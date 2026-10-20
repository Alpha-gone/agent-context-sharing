package web

import (
	"errors"
	"mime"
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

// ErrAuthenticationUnavailable는 자격 불일치와 구분할 인증 처리 장애다.
var ErrAuthenticationUnavailable = errors.New("authentication_unavailable")

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
		var remaining time.Duration
		for key, window := range attempts.sources {
			if !now.Before(window.until) {
				delete(attempts.sources, key)
			} else {
				remaining = max(remaining, window.until.Sub(now))
			}
		}
		if len(attempts.sources) >= authSourceCapacity {
			if attempts.global.count >= authGlobalLimit {
				remaining = max(remaining, attempts.global.until.Sub(now))
			}
			return remaining
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
	peer, err := netip.ParseAddrPort(request.RemoteAddr)
	if err != nil {
		return netip.Addr{}
	}
	address := peer.Addr()
	if address.Zone() != "" {
		return netip.Addr{}
	}
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
	if request.Context().Err() != nil {
		return false
	}
	retry := server.attempts.allow(address)
	if request.Context().Err() != nil {
		return false
	}
	if retry > 0 {
		server.authenticationLimited(writer, retry)
		return false
	}
	request.Body = http.MaxBytesReader(writer, request.Body, authFormLimit)
	return true
}

func (server *Server) parseAuthenticationForm(writer http.ResponseWriter, request *http.Request, template string) bool {
	if !server.allowAuthentication(writer, request) {
		return false
	}
	if request.ContentLength > authFormLimit {
		server.authenticationFormError(writer, template, &http.MaxBytesError{Limit: authFormLimit})
		return false
	}
	types := request.Header.Values("Content-Type")
	if len(types) != 1 {
		server.authenticationFormError(writer, template, http.ErrNotSupported)
		return false
	}
	mediaType, _, err := mime.ParseMediaType(types[0])
	if err != nil || mediaType != "application/x-www-form-urlencoded" {
		server.authenticationFormError(writer, template, http.ErrNotSupported)
		return false
	}
	err = request.ParseForm()
	if request.Context().Err() != nil {
		return false
	}
	if err != nil {
		server.authenticationFormError(writer, template, err)
		return false
	}
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
