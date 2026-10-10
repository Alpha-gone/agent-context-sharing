package authorize

import (
	"context"
	"time"

	"agent_context_sharing/client/internal/client/contract"
)

const (
	keyRefreshInterval = 30 * time.Second
	keyRefreshTimeout  = 10 * time.Second
	keyRefreshWaiters  = 128
)

type keyFlight struct {
	ctx     context.Context
	cancel  context.CancelFunc
	done    chan struct{}
	waiters int
	version uint64
	keys    []jwk
	err     error
}

// refreshKeys는 검증된 발견 정보의 주소만 조회하며 호출자별 취소를 공유 실행과 분리한다.
func (m *Manager) refreshKeys(ctx context.Context, md metadata, version uint64) ([]jwk, error) {
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		m.mu.Lock()
		if m.closed {
			m.mu.Unlock()
			return nil, ErrAuthorization
		}
		if m.metadataVersion != version {
			current := m.metadata
			m.mu.Unlock()
			if current.Issuer != md.Issuer || current.JWKS != md.JWKS || current.resource != md.resource {
				return nil, contract.ErrProtocol
			}
			return current.keys, nil
		}
		f := m.keyFlight
		if f != nil && (f.waiters == 0 || f.version != version) {
			m.mu.Unlock()
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-f.done:
				continue
			}
		}
		if f != nil && f.waiters >= keyRefreshWaiters {
			m.mu.Unlock()
			return nil, contract.ErrProtocol
		}
		if f == nil {
			now := m.now()
			if now.Before(m.nextKeyRefresh) {
				m.mu.Unlock()
				return nil, contract.ErrProtocol
			}
			m.nextKeyRefresh = now.Add(keyRefreshInterval)
			flowCtx, cancel := context.WithTimeout(context.Background(), keyRefreshTimeout)
			f = &keyFlight{ctx: flowCtx, cancel: cancel, done: make(chan struct{}), version: version}
			m.keyFlight = f
			go m.runKeyRefresh(f, md.JWKS)
		}
		f.waiters++
		m.mu.Unlock()

		select {
		case <-ctx.Done():
		case <-f.done:
		}
		m.mu.Lock()
		f.waiters--
		last := f.waiters == 0 && m.keyFlight == f
		if last {
			f.cancel()
		}
		m.mu.Unlock()
		if last {
			<-f.done
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		return f.keys, f.err
	}
}

func (m *Manager) runKeyRefresh(f *keyFlight, target string) {
	keys, err := m.signingKeys(f.ctx, target)
	m.mu.Lock()
	defer m.mu.Unlock()
	if f.ctx.Err() != nil {
		err = f.ctx.Err()
	}
	if m.closed || f.waiters == 0 {
		err = contract.ErrProtocol
	}
	if err == nil && m.metadataVersion == f.version {
		m.metadata.keys = keys
		m.metadataVersion++
	}
	f.keys, f.err = keys, err
	f.cancel()
	if m.keyFlight == f {
		m.keyFlight = nil
	}
	close(f.done)
}
