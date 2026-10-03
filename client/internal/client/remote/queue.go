package remote

import (
	"context"
	"sync"

	"agent_context_sharing/client/internal/client/contract"
)

type waiter struct {
	ready   chan struct{}
	granted bool
}

type callQueue struct {
	mu      sync.Mutex
	active  int
	waiters []*waiter
}

func (q *callQueue) acquire(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	q.mu.Lock()
	if q.active < 8 && len(q.waiters) == 0 {
		q.active++
		q.mu.Unlock()
		return nil
	}
	if len(q.waiters) >= 128 {
		q.mu.Unlock()
		return contract.ErrBusy
	}
	w := &waiter{ready: make(chan struct{})}
	q.waiters = append(q.waiters, w)
	q.mu.Unlock()
	select {
	case <-w.ready:
	case <-ctx.Done():
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	if err := ctx.Err(); err != nil {
		if w.granted {
			q.releaseLocked()
		} else {
			for index, candidate := range q.waiters {
				if candidate == w {
					copy(q.waiters[index:], q.waiters[index+1:])
					q.waiters[len(q.waiters)-1] = nil
					q.waiters = q.waiters[:len(q.waiters)-1]
					break
				}
			}
		}
		return err
	}
	return nil
}

func (q *callQueue) release() {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.releaseLocked()
}

func (q *callQueue) releaseLocked() {
	q.active--
	if len(q.waiters) == 0 {
		return
	}
	w := q.waiters[0]
	q.waiters[0] = nil
	q.waiters = q.waiters[1:]
	q.active++
	w.granted = true
	close(w.ready)
}
