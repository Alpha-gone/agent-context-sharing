package authorize

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"agent_context_sharing/client/internal/client/contract"
)

func secret() string {
	var bytes [32]byte
	rand.Read(bytes[:])
	return encoding.EncodeToString(bytes[:])
}

func (m *Manager) authorize(ctx context.Context, challenge Challenge) (metadata, Credential, error) {
	md, err := m.discover(ctx, challenge)
	if err != nil {
		return md, Credential{}, err
	}
	code, redirect, verifier, err := m.callback(ctx, md)
	if err != nil {
		return md, Credential{}, err
	}
	defer func() { code, verifier = "", "" }()
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return md, Credential{}, ErrAuthorization
	}
	proof, err := makeProof(m.key, m.now(), md.Token, "")
	m.mu.Unlock()
	if err != nil {
		return md, Credential{}, err
	}
	form := url.Values{"grant_type": {"authorization_code"}, "client_id": {m.cfg.ClientID()}, "code": {code}, "redirect_uri": {redirect}, "code_verifier": {verifier}, "resource": {m.cfg.RemoteURL()}}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, md.Token, strings.NewReader(form.Encode()))
	if err != nil {
		return md, Credential{}, ErrAuthorization
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("Accept", "application/json")
	request.Header.Set("DPoP", proof)
	var response struct {
		AccessToken string `json:"access_token"`
		Type        string `json:"token_type"`
		Expires     int64  `json:"expires_in"`
		Scope       string `json:"scope"`
	}
	if err := readJSON(ctx, m.httpClient(false), request, &response); err != nil {
		if ctx.Err() != nil {
			return md, Credential{}, ctx.Err()
		}
		if _, ok := errors.AsType[statusError](err); ok {
			return md, Credential{}, ErrAuthorization
		}
		return md, Credential{}, err
	}
	if response.Type != "DPoP" || response.Expires <= 0 || response.Scope != "agent-context" {
		return md, Credential{}, contract.ErrProtocol
	}
	m.mu.Lock()
	jkt := m.jkt
	m.mu.Unlock()
	claims, err := verifyToken(response.AccessToken, md.keys, md.Issuer, m.cfg.RemoteURL(), jkt, m.now())
	if err != nil {
		return md, Credential{}, err
	}
	return md, credentialFrom(response.AccessToken, claims), nil
}

func (m *Manager) callback(ctx context.Context, md metadata) (string, string, string, error) {
	if err := ctx.Err(); err != nil {
		return "", "", "", err
	}
	var listener net.Listener
	var err error
	for _, bind := range []struct{ network, address string }{{"tcp4", "127.0.0.1:0"}, {"tcp6", "[::1]:0"}} {
		listener, err = m.listen(ctx, bind.network, bind.address)
		if err == nil {
			break
		}
		if ctx.Err() != nil {
			return "", "", "", ctx.Err()
		}
	}
	if err != nil || listener == nil {
		return "", "", "", ErrAuthorization
	}
	defer listener.Close()
	address, ok := listener.Addr().(*net.TCPAddr)
	if !ok || !address.IP.IsLoopback() || address.Port == 0 || address.IP.String() != "127.0.0.1" && address.IP.String() != "::1" {
		return "", "", "", ErrAuthorization
	}
	redirect := "http://" + listener.Addr().String() + "/callback"
	state, verifier := secret(), secret()
	defer func() { state, verifier = "", "" }()
	type outcome struct {
		code string
		err  error
	}
	completed := make(chan outcome, 1)
	var mu sync.Mutex
	consumed := false
	handler := http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Cache-Control", "no-store")
		writer.Header().Set("Content-Type", "text/plain; charset=utf-8")
		mu.Lock()
		defer mu.Unlock()
		if consumed {
			writer.WriteHeader(http.StatusGone)
			return
		}
		query, parseErr := url.ParseQuery(request.URL.RawQuery)
		remote, _, remoteErr := net.SplitHostPort(request.RemoteAddr)
		ip := net.ParseIP(remote)
		valid := parseErr == nil && remoteErr == nil && ip != nil && ip.IsLoopback() && request.Method == http.MethodGet && request.Host == listener.Addr().String() && request.URL.Path == "/callback" && request.URL.EscapedPath() == "/callback" && len(query["state"]) == 1 && subtle.ConstantTimeCompare([]byte(query.Get("state")), []byte(state)) == 1 && len(query["iss"]) == 1 && query.Get("iss") == md.Issuer
		valid = valid && (len(query["code"]) == 1 && query.Get("code") != "" && len(query["error"]) == 0 || len(query["error"]) == 1 && query.Get("error") != "" && len(query["code"]) == 0)
		if !valid {
			writer.WriteHeader(http.StatusBadRequest)
			return
		}
		if ctx.Err() != nil {
			writer.WriteHeader(http.StatusGone)
			return
		}
		consumed = true
		result := outcome{code: query.Get("code")}
		if query.Get("error") != "" {
			result.err = ErrAuthorization
		}
		completed <- result
		writer.WriteHeader(http.StatusOK)
	})
	server := &http.Server{Handler: handler, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 5 * time.Second, WriteTimeout: 5 * time.Second, MaxHeaderBytes: 256 << 10, ErrorLog: discardLog()}
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := server.Serve(listener); err != nil && ctx.Err() == nil {
			select {
			case completed <- outcome{err: ErrAuthorization}:
			default:
			}
		}
	}()
	defer func() { server.Shutdown(context.Background()); server.Close(); <-done }()
	if err := ctx.Err(); err != nil {
		return "", "", "", err
	}
	if err := m.open(ctx, authorizationURL(md, m.cfg.ClientID(), m.cfg.RemoteURL(), redirect, state, verifier)); err != nil {
		if ctx.Err() != nil {
			return "", "", "", ctx.Err()
		}
		return "", "", "", ErrAuthorization
	}
	select {
	case <-ctx.Done():
		return "", "", "", ctx.Err()
	case result := <-completed:
		return result.code, redirect, verifier, result.err
	}
}

func discardLog() *log.Logger { return log.New(io.Discard, "", 0) }
