package authorize

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"

	"agent_context_sharing/client/internal/client/config"
	"agent_context_sharing/client/internal/client/contract"
)

// ErrAuthorization은 비밀을 포함하지 않는 client_authorization 오류다.
var ErrAuthorization = contract.ErrAuthorization

// ErrBusy는 공유 인가 대기자 상한의 client_busy 오류다.
var ErrBusy = contract.ErrBusy

// Options는 외부 접근과 비결정적 시험 경계를 교체한다. nil은 운영 기본값이다.
type Options struct {
	Transport http.RoundTripper
	// OpenBrowser는 context 취소 시 반환해야 한다. callback은 반환과 독립적으로 처리된다.
	OpenBrowser func(context.Context, string) error
	Listen      func(context.Context, string, string) (net.Listener, error)
	Now         func() time.Time
}

// Credential은 검증된 토큰의 요청별 스냅샷이다. 원문은 HTTP 헤더에만 사용한다.
type Credential struct {
	raw      string
	expires  time.Time
	identity string
}

const expiryLeeway = 5 * time.Second

func (c Credential) usable(now time.Time) bool {
	return c.raw != "" && now.Add(expiryLeeway).Before(c.expires)
}

// String은 로그나 오류의 기본 표현에 자격 증명을 노출하지 않는다.
func (Credential) String() string { return "[자격 증명 비공개]" }

// GoString은 상세 디버그 표현도 비밀 없이 반환한다.
func (c Credential) GoString() string { return c.String() }

// MarshalJSON은 자격 증명의 직렬화를 금지한다.
func (Credential) MarshalJSON() ([]byte, error) { return nil, contract.ErrProtocol }

type flight struct {
	ctx        context.Context
	cancel     context.CancelFunc
	done       chan struct{}
	waiters    int
	credential Credential
	err        error
}

// Manager는 프로세스 키·단일 계정 결합·현재 토큰·공유 인가 하나를 관리한다.
type Manager struct {
	cfg            config.Config
	transport      http.RoundTripper
	ownedTransport *http.Transport
	open           func(context.Context, string) error
	listen         func(context.Context, string, string) (net.Listener, error)
	now            func() time.Time
	mu             sync.Mutex
	key            *ecdsa.PrivateKey
	jkt            string
	boundIdentity  string
	current        Credential
	metadata       metadata
	flight         *flight
	closed         bool
}

// New는 네트워크 접근 없이 프로세스 수명의 ES256 키를 생성한다.
func New(cfg config.Config, options Options) (*Manager, error) {
	if cfg.RemoteURL() == "" || cfg.ClientID() == "" || cfg.AuthTimeout() <= 0 {
		return nil, contract.ErrConfiguration
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, ErrAuthorization
	}
	var ownedTransport *http.Transport
	if options.Transport == nil {
		if standard, ok := http.DefaultTransport.(*http.Transport); ok {
			ownedTransport = standard.Clone()
			options.Transport = ownedTransport
		} else {
			options.Transport = http.DefaultTransport
		}
	}
	if options.OpenBrowser == nil {
		options.OpenBrowser = openBrowser
	}
	if options.Listen == nil {
		options.Listen = (&net.ListenConfig{}).Listen
	}
	if options.Now == nil {
		options.Now = time.Now
	}
	return &Manager{cfg: cfg, transport: options.Transport, ownedTransport: ownedTransport, open: options.OpenBrowser, listen: options.Listen, now: options.Now, key: key, jkt: thumbprint(publicJWK(key))}, nil
}

// String은 조정자의 비밀·URL·구성을 출력하지 않는다.
func (*Manager) String() string { return "[인증 상태 비공개]" }

// GoString은 조정자의 상세 디버그 표현도 차단한다.
func (m *Manager) GoString() string { return m.String() }

// Credentials는 유효 토큰을 반환하거나 대기자별 취소 가능한 공유 인가를 기다린다.
func (m *Manager) Credentials(ctx context.Context, challenge Challenge) (Credential, error) {
	for {
		if err := ctx.Err(); err != nil {
			return Credential{}, err
		}
		m.mu.Lock()
		if m.closed {
			m.mu.Unlock()
			return Credential{}, ErrAuthorization
		}
		if m.current.usable(m.now()) {
			result := m.current
			m.mu.Unlock()
			return result, nil
		}
		f := m.flight
		if f != nil && f.waiters == 0 {
			m.mu.Unlock()
			select {
			case <-ctx.Done():
				return Credential{}, ctx.Err()
			case <-f.done:
				continue
			}
		}
		if f != nil && f.waiters >= 128 {
			m.mu.Unlock()
			return Credential{}, ErrBusy
		}
		if f == nil {
			flowCtx, cancel := context.WithTimeout(context.Background(), m.cfg.AuthTimeout())
			f = &flight{ctx: flowCtx, cancel: cancel, done: make(chan struct{})}
			m.flight = f
			// 결과 게시와 cleanup까지 done이 수명 경계를 소유한다.
			go m.run(f, challenge)
		}
		f.waiters++
		m.mu.Unlock()
		var result Credential
		var err error
		select {
		case <-ctx.Done():
			err = ctx.Err()
		case <-f.done:
			result, err = f.credential, f.err
		}
		m.mu.Lock()
		f.waiters--
		last := f.waiters == 0 && m.flight == f
		if last {
			f.cancel()
		}
		m.mu.Unlock()
		if last {
			<-f.done
		}
		if ctx.Err() != nil {
			return Credential{}, ctx.Err()
		}
		m.mu.Lock()
		closed := m.closed
		m.mu.Unlock()
		if closed {
			return Credential{}, ErrAuthorization
		}
		return result, err
	}
}

func (m *Manager) run(f *flight, challenge Challenge) {
	md, credential, err := m.authorize(f.ctx, challenge)
	m.mu.Lock()
	if f.ctx.Err() != nil {
		err = f.ctx.Err()
	}
	if err == nil && !credential.usable(m.now()) {
		err = contract.ErrProtocol
	}
	if err == nil && !m.closed && f.waiters > 0 {
		if m.boundIdentity != "" && m.boundIdentity != credential.identity {
			err = contract.ErrIdentityChanged
		} else {
			m.boundIdentity = credential.identity
			m.current = credential
			m.metadata = md
		}
	} else if err == nil {
		err = ErrAuthorization
	}
	if err != nil {
		credential = Credential{}
	}
	f.credential, f.err = credential, err
	if m.flight == f {
		m.flight = nil
	}
	f.cancel()
	close(f.done)
	m.mu.Unlock()
}

// Identity는 인가를 시작하거나 기다리지 않고 사용 가능한 현재 주체만 조회한다.
func (m *Manager) Identity(ctx context.Context) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if m.closed || !m.current.usable(m.now()) {
		return "", ErrAuthorization
	}
	return m.current.identity, nil
}

// Authenticate는 구성된 MCP POST에만 현재 토큰과 새 proof를 설정한다.
func (m *Manager) Authenticate(ctx context.Context, request *http.Request) (Credential, error) {
	if request == nil || request.URL == nil || request.Method != http.MethodPost {
		return Credential{}, contract.ErrProtocol
	}
	actual, e1 := normalizedURL(request.URL.String(), false)
	expected, e2 := normalizedURL(m.cfg.RemoteURL(), false)
	if e1 != nil || e2 != nil || actual != expected {
		return Credential{}, contract.ErrProtocol
	}
	if _, err := m.Credentials(ctx, Challenge{}); err != nil {
		return Credential{}, err
	}
	return m.Apply(ctx, request)
}

// Apply는 인가를 시작하지 않고 사용 가능한 현재 토큰과 새 proof만 적용한다.
func (m *Manager) Apply(ctx context.Context, request *http.Request) (Credential, error) {
	if request == nil || request.URL == nil || request.Method != http.MethodPost {
		return Credential{}, contract.ErrProtocol
	}
	actual, e1 := normalizedURL(request.URL.String(), false)
	expected, e2 := normalizedURL(m.cfg.RemoteURL(), false)
	if e1 != nil || e2 != nil || actual != expected {
		return Credential{}, contract.ErrProtocol
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return Credential{}, err
	}
	if m.closed || !m.current.usable(m.now()) {
		return Credential{}, ErrAuthorization
	}
	credential := m.current
	proof, err := makeProof(m.key, m.now(), request.URL.String(), credential.raw)
	if err != nil {
		return Credential{}, err
	}
	if request.Header == nil {
		request.Header = make(http.Header)
	}
	request.Header.Set("Authorization", "DPoP "+credential.raw)
	request.Header.Set("DPoP", proof)
	return credential, nil
}

// Reauthorize는 실패한 토큰만 무효화해 늦은 401이 새 토큰을 지우지 못하게 한다.
// 호출 전 ParseChallenge 판정과 최대 한 번의 재전송은 remote가 소유한다.
func (m *Manager) Reauthorize(ctx context.Context, failed Credential, challenge Challenge) (Credential, error) {
	if err := ctx.Err(); err != nil {
		return Credential{}, err
	}
	m.mu.Lock()
	if failed.raw != "" && m.current.raw == failed.raw {
		m.current = Credential{}
	}
	m.mu.Unlock()
	return m.Credentials(ctx, challenge)
}

// Refresh는 두 갱신 헤더와 JWT를 검증한 뒤 현재 토큰을 원자적으로 교체한다.
func (m *Manager) Refresh(ctx context.Context, header http.Header) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	tokens, expirations := header.Values("Mcp-Access-Token"), header.Values("Mcp-Access-Token-Expires-At")
	if len(tokens) == 0 && len(expirations) == 0 {
		return nil
	}
	if len(tokens) != 1 || len(expirations) != 1 || tokens[0] == "" {
		return contract.ErrProtocol
	}
	expires, err := strconv.ParseInt(expirations[0], 10, 64)
	if err != nil {
		return contract.ErrProtocol
	}
	m.mu.Lock()
	md, jkt := m.metadata, m.jkt
	m.mu.Unlock()
	claims, err := verifyToken(tokens[0], md.keys, md.Issuer, md.resource, jkt, m.now())
	if err != nil || expires != claims.Expires {
		return contract.ErrProtocol
	}
	credential := credentialFrom(tokens[0], claims)
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if m.closed {
		return ErrAuthorization
	}
	if m.boundIdentity == "" || credential.identity != m.boundIdentity {
		return contract.ErrIdentityChanged
	}
	if !credential.usable(m.now()) {
		return contract.ErrProtocol
	}
	if credential.expires.Before(m.current.expires) {
		return nil
	}
	m.current = credential
	return nil
}

func credentialFrom(raw string, claims tokenClaims) Credential {
	hash := sha256.Sum256([]byte(claims.Issuer + "\x00" + claims.Subject))
	return Credential{raw: raw, expires: time.Unix(claims.Expires, 0), identity: encoding.EncodeToString(hash[:])}
}

func tokenHash(value string) string {
	hash := sha256.Sum256([]byte(value))
	return encoding.EncodeToString(hash[:])
}

// Close는 진행 중 인가를 취소하고 수신기 정리 뒤 비밀 참조를 폐기한다.
func (m *Manager) Close() {
	m.mu.Lock()
	m.closed = true
	f := m.flight
	if f != nil {
		f.cancel()
	}
	m.mu.Unlock()
	if f != nil {
		<-f.done
	}
	m.mu.Lock()
	m.current = Credential{}
	m.metadata = metadata{}
	m.key = nil
	m.jkt = ""
	m.boundIdentity = ""
	m.mu.Unlock()
	if m.ownedTransport != nil {
		m.ownedTransport.CloseIdleConnections()
	}
}
