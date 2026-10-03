package authorize

import (
	"context"
	"net"
	"net/http"
	"time"
)

// CheckBrowserLoopback는 비밀 없는 임시 URL로 브라우저·루프백 도달성을 점검한다.
// 토큰·인가 코드·PKCE 값을 만들거나 보호 도구를 호출하지 않는다.
func (m *Manager) CheckBrowserLoopback(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	var listener net.Listener
	var err error
	for _, bind := range []struct{ network, address string }{{"tcp4", "127.0.0.1:0"}, {"tcp6", "[::1]:0"}} {
		listener, err = m.listen(ctx, bind.network, bind.address)
		if err == nil {
			break
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
	}
	if err != nil || listener == nil {
		return ErrAuthorization
	}
	defer listener.Close()
	address, ok := listener.Addr().(*net.TCPAddr)
	if !ok || !address.IP.IsLoopback() || address.Port == 0 || address.IP.String() != "127.0.0.1" && address.IP.String() != "::1" {
		return ErrAuthorization
	}
	completed := make(chan error, 1)
	server := &http.Server{
		ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 5 * time.Second, WriteTimeout: 5 * time.Second,
		MaxHeaderBytes: 256 << 10, ErrorLog: discardLog(),
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Cache-Control", "no-store")
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			remote, _, err := net.SplitHostPort(r.RemoteAddr)
			if err != nil || !net.ParseIP(remote).IsLoopback() || r.Method != http.MethodGet || r.Host != listener.Addr().String() || r.URL.EscapedPath() != "/diagnostic" || r.URL.RawQuery != "" || r.URL.ForceQuery {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			select {
			case completed <- nil:
			default:
			}
			w.WriteHeader(http.StatusOK)
		}),
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := server.Serve(listener); err != nil && ctx.Err() == nil {
			select {
			case completed <- ErrAuthorization:
			default:
			}
		}
	}()
	defer func() { server.Close(); <-done }()
	openCtx, cancel := context.WithCancel(ctx)
	opened := make(chan error, 1)
	openDone := make(chan struct{})
	go func() {
		defer close(openDone)
		opened <- m.open(openCtx, "http://"+listener.Addr().String()+"/diagnostic")
	}()
	defer func() { cancel(); <-openDone }()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case err := <-completed:
			return err
		case err := <-opened:
			if err == nil {
				opened = nil
				continue
			}
			select {
			case err := <-completed:
				return err
			default:
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return ErrAuthorization
		}
	}
}
