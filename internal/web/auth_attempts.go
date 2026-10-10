package web

import (
	"errors"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"sync"
	"time"
)

const (
	authAttemptWindow  = time.Minute
	authSourceLimit    = 10
	authGlobalLimit    = 60
	authSourceCapacity = 4096
	authFormLimit      = 8 << 10
)

// ErrAuthenticationBusy는 인가 계층이 비밀번호 작업을 시작하지 못했음을 나타낸다.
var ErrAuthenticationBusy = errors.New("authentication_busy")

type attemptWindow struct {
	until time.Time
	count int
}

type authAttempts struct {
	mu      sync.Mutex
	now     func() time.Time
	global  attemptWindow
	sources map[netip.Prefix]attemptWindow
}

func newAuthAttempts() *authAttempts {
	return &authAttempts{now: time.Now, sources: make(map[netip.Prefix]attemptWindow)}
}

func (attempts *authAttempts) allow(address netip.Addr) time.Duration {
	attempts.mu.Lock()
	defer attempts.mu.Unlock()
	now := attempts.now()
	address = address.Unmap()
	key := netip.Prefix{}
	if address.IsValid() && address.Zone() == "" {
		bits := 64
		if address.Is4() {
			bits = 32
		}
		key = netip.PrefixFrom(address, bits).Masked()
	}
	source, exists := attempts.sources[key]
	if !exists || !now.Before(source.until) {
		// 만료 항목만 회수해 공격으로 활성 제한을 밀어내지 못하게 한다.
		for key, window := range attempts.sources {
			if !now.Before(window.until) {
				delete(attempts.sources, key)
			}
		}
		if len(attempts.sources) >= authSourceCapacity {
			return authAttemptWindow
		}
		source = attemptWindow{until: now.Add(authAttemptWindow)}
	}
	global := attempts.global
	if !now.Before(global.until) {
		global = attemptWindow{until: now.Add(authAttemptWindow)}
	}
	var retry time.Duration
	if source.count >= authSourceLimit {
		retry = source.until.Sub(now)
	}
	if global.count >= authGlobalLimit {
		retry = max(retry, global.until.Sub(now))
	}
	if retry > 0 {
		return retry
	}
	source.count++
	global.count++
	attempts.sources[key], attempts.global = source, global
	return 0
}

func requestAddress(request *http.Request) netip.Addr {
	host, _, err := net.SplitHostPort(request.RemoteAddr)
	if err != nil {
		host = request.RemoteAddr
	}
	address, _ := netip.ParseAddr(host)
	return address.Unmap()
}

func (server *Server) allowAuthentication(writer http.ResponseWriter, request *http.Request) bool {
	if request.Context().Err() != nil {
		return false
	}
	address := requestAddress(request)
	if server.config.ClientAddress != nil {
		address = server.config.ClientAddress(request)
	}
	if retry := server.attempts.allow(address); retry > 0 {
		server.authenticationLimited(writer, retry)
		return false
	}
	request.Body = http.MaxBytesReader(writer, request.Body, authFormLimit)
	return true
}

func (server *Server) authenticationLimited(writer http.ResponseWriter, retry time.Duration) {
	seconds := max(1, int((retry+time.Second-1)/time.Second))
	writer.Header().Set("Retry-After", strconv.Itoa(seconds))
	writer.Header().Set("Cache-Control", "no-store")
	server.render(writer, http.StatusTooManyRequests, "message", pageData{Title: "재시도 안내", Error: "요청이 많아 지금 처리할 수 없습니다. 잠시 후 다시 시도해 주세요."})
}

func (server *Server) authenticationFormError(writer http.ResponseWriter, template string, err error) {
	status := http.StatusBadRequest
	message := "입력을 처리할 수 없습니다."
	if _, tooLarge := errors.AsType[*http.MaxBytesError](err); tooLarge {
		status, message = http.StatusRequestEntityTooLarge, "입력 크기가 허용 범위를 초과했습니다."
	}
	server.render(writer, status, template, pageData{Error: message})
}
