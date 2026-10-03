// Package remote는 인증된 원격 경계의 발견 결과와 도구 계약 캐시를 소유한다.
package remote

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"math"
	"slices"
	"strings"
	"time"

	"agent_context_sharing/client/internal/client/authorize"
	"agent_context_sharing/client/internal/client/contract"
)

const protocolVersion = "2026-07-28"
const idempotencyExtension = "io.github.alpha-gone/write-idempotency"

// Source는 인증·HTTP 전송을 발견·도구 계약 검증에서 분리한다.
// Identity는 토큰 원문이 아닌 인증 주체의 안정된 불투명 식별자를 반환한다.
// 각 응답은 JSON-RPC 외피 검증을 마친 result 객체다.
type Source interface {
	Identity(context.Context) (string, error)
	Discover(context.Context) (jsontext.Value, error)
	ListTools(context.Context) (jsontext.Value, error)
	CallTool(context.Context, string, jsontext.Value) (jsontext.Value, error)
}

// Discovery는 검증한 원격 발견 결과와 쓰기 멱등성 협상 상태다.
type Discovery struct {
	Result           jsontext.Value
	WriteIdempotency bool
}

type cacheEntry struct {
	expires  time.Time
	scope    string
	identity string
}

func (e cacheEntry) fresh(now time.Time, identity string) bool {
	return now.Before(e.expires) && (e.scope == "public" || e.identity == identity)
}

// Client는 프로세스 메모리에서만 발견·목록과 최초 도구 지문을 보관한다.
// 재검증은 취소 가능한 단일 잠금으로 직렬화하며 실패한 목록은 공개하지 않는다.
type Client struct {
	source         Source
	now            func() time.Time
	gate           chan struct{}
	discovery      Discovery
	discoveryEntry cacheEntry
	catalog        *contract.Catalog
	catalogEntry   cacheEntry
	fingerprint    [sha256.Size]byte
	hasFingerprint bool
	rejected       bool
	timeout        time.Duration
	closed         context.Context
	close          func()
	prepare        func(context.Context) error
	reauthorize    func(context.Context, *authorizationRequired) error
	negotiation    func(bool)
	admission      *callQueue
}

// New는 인증된 전송 경계와 시험용 시계를 연결한다. nil 시계는 실제 시각을 사용한다.
func New(source Source, now func() time.Time) *Client {
	if now == nil {
		now = time.Now
	}
	return &Client{source: source, now: now, gate: make(chan struct{}, 1)}
}

func (c *Client) lock(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case c.gate <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (c *Client) identity(ctx context.Context) (string, error) {
	if c.source == nil || c.rejected {
		return "", contract.ErrProtocol
	}
	identity, err := c.source.Identity(ctx)
	if err != nil {
		if c.reauthorize != nil && errors.Is(err, authorize.ErrAuthorization) {
			return "", &authorizationRequired{}
		}
		return "", err
	}
	if identity == "" {
		return "", contract.ErrProtocol
	}
	return identity, nil
}

// Discover는 revision·tools·서버 식별자와 캐시 힌트를 검증해 반환한다.
func (c *Client) Discover(ctx context.Context) (Discovery, error) {
	ctx, done := c.callContext(ctx)
	defer done()
	if err := c.enter(ctx); err != nil {
		return Discovery{}, err
	}
	for {
		result, err := c.discoverPrepared(requestContext(ctx))
		if recovered, nextErr := c.recoverAuthorization(ctx, err); !recovered {
			return result, nextErr
		}
	}
}

func (c *Client) discoverPrepared(ctx context.Context) (Discovery, error) {
	if err := c.lock(ctx); err != nil {
		return Discovery{}, err
	}
	defer func() { <-c.gate }()
	identity, err := c.identity(ctx)
	if err != nil {
		return Discovery{}, err
	}
	if err := c.discover(ctx, identity); err != nil {
		return Discovery{}, err
	}
	result := c.discovery
	result.Result = bytes.Clone(result.Result)
	return result, nil
}

func (c *Client) discover(ctx context.Context, identity string) error {
	if c.discoveryEntry.fresh(c.now(), identity) {
		return nil
	}
	raw, err := c.source.Discover(context.WithValue(ctx, cacheKey{}, true))
	if err != nil {
		return err
	}
	if err := c.sameIdentity(ctx, identity); err != nil {
		return err
	}
	var result struct {
		SupportedVersions []string `json:"supportedVersions"`
		Capabilities      struct {
			Tools      map[string]jsontext.Value `json:"tools"`
			Extensions map[string]jsontext.Value `json:"extensions"`
		} `json:"capabilities"`
		Meta map[string]jsontext.Value `json:"_meta"`
	}
	if json.Unmarshal(raw, &result) != nil || !slices.Contains(result.SupportedVersions, protocolVersion) || result.Capabilities.Tools == nil {
		return contract.ErrProtocol
	}
	var serverInfo struct {
		Name    string `json:"name"`
		Version string `json:"version"`
	}
	if json.Unmarshal(result.Meta["io.modelcontextprotocol/serverInfo"], &serverInfo) != nil || strings.TrimSpace(serverInfo.Name) == "" || strings.TrimSpace(serverInfo.Version) == "" {
		return contract.ErrProtocol
	}
	entry, err := c.cacheHints(raw, identity)
	if err != nil {
		return err
	}
	var extension struct {
		RetentionMS int64 `json:"retentionMs"`
	}
	negotiated := json.Unmarshal(result.Capabilities.Extensions[idempotencyExtension], &extension) == nil && extension.RetentionMS == 86400000
	if err := c.sameIdentity(ctx, identity); err != nil {
		return err
	}
	c.discovery = Discovery{Result: bytes.Clone(raw), WriteIdempotency: negotiated}
	c.discoveryEntry = entry
	if c.negotiation != nil {
		c.negotiation(negotiated)
	}
	return nil
}

func (c *Client) sameIdentity(ctx context.Context, identity string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	current, err := c.source.Identity(ctx)
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	if err != nil {
		return err
	}
	if current != identity {
		return contract.ErrIdentityChanged
	}
	return nil
}

func (c *Client) cacheHints(raw jsontext.Value, identity string) (cacheEntry, error) {
	var object map[string]jsontext.Value
	if json.Unmarshal(raw, &object) != nil || object == nil {
		return cacheEntry{}, contract.ErrProtocol
	}
	var ttl int64
	if value, ok := object["ttlMs"]; ok {
		if value.Kind() != '0' || json.Unmarshal(value, &ttl) != nil || ttl < 0 || ttl > math.MaxInt64/int64(time.Millisecond) {
			return cacheEntry{}, contract.ErrProtocol
		}
	}
	scope := "public"
	if value, ok := object["cacheScope"]; ok {
		if value.Kind() != '"' || json.Unmarshal(value, &scope) != nil {
			return cacheEntry{}, contract.ErrProtocol
		}
	}
	if scope != "public" && scope != "private" {
		return cacheEntry{}, contract.ErrProtocol
	}
	return cacheEntry{expires: c.now().Add(time.Duration(ttl) * time.Millisecond), scope: scope, identity: identity}, nil
}

// ListTools는 발견과 도구 캐시를 각각 갱신한 뒤 검증한 목록만 반환한다.
// 최초 정상 지문이 바뀌면 프로세스를 재시작할 때까지 공개를 거부한다.
func (c *Client) ListTools(ctx context.Context) (*contract.Catalog, error) {
	ctx, done := c.callContext(ctx)
	defer done()
	if err := c.enter(ctx); err != nil {
		return nil, err
	}
	for {
		result, err := c.listToolsPrepared(requestContext(ctx))
		if recovered, nextErr := c.recoverAuthorization(ctx, err); !recovered {
			return result, nextErr
		}
	}
}

func (c *Client) listToolsPrepared(ctx context.Context) (*contract.Catalog, error) {
	if err := c.lock(ctx); err != nil {
		return nil, err
	}
	defer func() { <-c.gate }()
	identity, err := c.identity(ctx)
	if err != nil {
		return nil, err
	}
	if err := c.discover(ctx, identity); err != nil {
		return nil, err
	}
	if c.catalog != nil && c.catalogEntry.fresh(c.now(), identity) {
		return c.catalog, nil
	}
	raw, err := c.source.ListTools(context.WithValue(ctx, cacheKey{}, true))
	if err != nil {
		return nil, err
	}
	if err := c.sameIdentity(ctx, identity); err != nil {
		return nil, err
	}
	var result struct {
		Tools      []contract.Tool `json:"tools"`
		NextCursor jsontext.Value  `json:"nextCursor"`
	}
	if json.Unmarshal(raw, &result) != nil {
		return nil, contract.ErrProtocol
	}
	if len(result.NextCursor) != 0 {
		var cursor string
		if result.NextCursor.Kind() != '"' || json.Unmarshal(result.NextCursor, &cursor) != nil || cursor != "" {
			return nil, contract.ErrProtocol
		}
	}
	entry, err := c.cacheHints(raw, identity)
	if err != nil {
		return nil, err
	}
	catalog, err := contract.ValidateTools(result.Tools)
	if err != nil {
		return nil, err
	}
	if err := c.sameIdentity(ctx, identity); err != nil {
		return nil, err
	}
	if c.hasFingerprint && c.fingerprint != catalog.Fingerprint() {
		c.rejected = true
		return nil, contract.ErrProtocol
	}
	c.fingerprint, c.hasFingerprint = catalog.Fingerprint(), true
	c.catalog, c.catalogEntry = catalog, entry
	return catalog, nil
}

// CallTool는 캐시 만료를 다시 확인하고 원격 스키마를 통과한 인자만 전달한다.
// 재시도·DPoP·JSON-RPC 외피 검증은 Source의 전송 구현이 담당한다.
func (c *Client) CallTool(ctx context.Context, name string, arguments jsontext.Value) (*contract.Result, error) {
	ctx, done := c.callContext(ctx)
	defer done()
	if len(arguments) > maxRequestBytes {
		return nil, contract.ErrBusy
	}
	if err := c.enter(ctx); err != nil {
		return nil, err
	}
	catalog, err := c.ListTools(ctx)
	if err != nil {
		return nil, err
	}
	if err := catalog.ValidateRemoteArguments(name, arguments); err != nil {
		return nil, err
	}
	if err := requestContext(ctx).Err(); err != nil {
		return nil, err
	}
	raw, err := c.source.CallTool(requestContext(ctx), name, arguments)
	if err != nil {
		return nil, err
	}
	if err := requestContext(ctx).Err(); err != nil {
		return nil, err
	}
	return contract.NewResult(raw)
}

func (c *Client) callContext(ctx context.Context) (context.Context, func()) {
	if state, ok := ctx.Value(stateKey{}).(*callState); ok && state.owner == c {
		return state.base, func() {}
	}
	state := &callState{owner: c, remaining: c.timeout}
	ctx = context.WithValue(ctx, stateKey{}, state)
	ctx, cancel := context.WithCancel(ctx)
	state.base = ctx
	state.resume()
	cleanup := func() {
		if state.lease != nil {
			state.lease.release()
			state.lease = nil
		}
		if state.cancel != nil {
			state.cancel()
		}
	}
	if c.closed == nil {
		return ctx, func() { cleanup(); cancel() }
	}
	stop := context.AfterFunc(c.closed, cancel)
	if c.closed.Err() != nil {
		cancel()
	}
	return ctx, func() { cleanup(); stop(); cancel() }
}

func (c *Client) enter(ctx context.Context) error {
	state := ctx.Value(stateKey{}).(*callState)
	if state.entered {
		return requestContext(ctx).Err()
	}
	if c.prepare != nil {
		if err := c.prepare(ctx); err != nil {
			return err
		}
	}
	if c.admission != nil {
		if err := c.admission.acquire(requestContext(ctx)); err != nil {
			return err
		}
		state.lease = c.admission
	}
	state.entered = true
	return requestContext(ctx).Err()
}

func (c *Client) recoverAuthorization(ctx context.Context, err error) (bool, error) {
	if ctxErr := requestContext(ctx).Err(); ctxErr != nil {
		return false, ctxErr
	}
	if required, ok := errors.AsType[*authorizationRequired](err); ok && c.reauthorize != nil {
		if err := c.reauthorize(ctx, required); err != nil {
			return false, err
		}
		return true, nil
	}
	return false, err
}

// Close는 HTTP 클라이언트의 호출을 취소하고 소유한 transport를 정리한다.
// 주입된 Source·인증 조정자의 종료는 주입자가 소유한다.
func (c *Client) Close() {
	if c.close != nil {
		c.close()
	}
}
