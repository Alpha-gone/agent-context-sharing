package main

import (
	"context"
	"errors"
	"net/http"
	"os"
	"time"
)

var errDeliveryPaused = errors.New("delivery paused")

// deliver는 네트워크 대기 중 세션 상태 잠금을 놓아 기록 제외 명령을 지연시키지 않는다.
func deliver(ctx context.Context, dir, name, graph string) error {
	return withDelivery(ctx, dir, name, func() error {
		endpoint, err := parseEndpoint(os.Getenv("INGEST_SERVER_URL"))
		if err != nil {
			return err
		}
		credential, err := readCredential()
		if err != nil {
			return err
		}
		client := &http.Client{Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
		for {
			var snapshot sessionState
			err := withState(dir, name, func(s *sessionState, _ func() error) error {
				if !s.Paused {
					snapshot = *s
				}
				return nil
			})
			if err != nil {
				return err
			}
			if len(snapshot.Pending) == 0 {
				return nil
			}
			previous := snapshot.Pending
			previousRejected := len(snapshot.Rejected)
			save := func() error {
				consumed := len(previous) - len(snapshot.Pending)
				if consumed <= 0 {
					return errors.New("delivery acknowledgement")
				}
				var paused bool
				err := withState(dir, name, func(current *sessionState, commit func() error) error {
					if len(current.Pending) < consumed {
						return errors.New("delivery state changed")
					}
					for i := range consumed {
						if current.Pending[i] != previous[i] {
							return errors.New("delivery state changed")
						}
					}
					current.Pending = current.Pending[consumed:]
					current.Rejected = append(current.Rejected, snapshot.Rejected[previousRejected:]...)
					paused = current.Paused
					return commit()
				})
				if err != nil {
					return err
				}
				previous = snapshot.Pending
				previousRejected = len(snapshot.Rejected)
				if paused {
					return errDeliveryPaused
				}
				return nil
			}
			err = flush(ctx, client, &snapshot, graph, endpoint, credential, save)
			if errors.Is(err, errDeliveryPaused) {
				return nil
			}
			if err != nil {
				return err
			}
		}
	})
}
