package store

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// ErrOAuthClientCapacity는 공유 동적 등록 총량이 소진됐음을 나타낸다.
var ErrOAuthClientCapacity = errors.New("oauth_client_capacity")

// OAuthClient는 서버가 발급하고 공유 DB에 보존하는 공개 클라이언트다.
type OAuthClient struct {
	ID, Name           string
	RedirectURIs       []string
	Resources          []string
	LoopbackPortPolicy string
	CreatedAt          time.Time
}

// RegisterOAuthClient는 등록 총량 확인과 삽입을 같은 잠금 아래 직렬화한다.
func (s *Store) RegisterOAuthClient(ctx context.Context, client OAuthClient) (resultErr error) {
	if client.ID == "" || len(client.RedirectURIs) == 0 || len(client.RedirectURIs) > 8 || len(client.Resources) != 1 || client.CreatedAt.IsZero() {
		return ErrInvalidState
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return storageError("oauth_client_begin", err)
	}
	defer func() {
		if err := rollbackTransaction(ctx, tx); err != nil {
			resultErr = storageError("oauth_client_rollback", errors.Join(resultErr, err))
		}
	}()
	// 두 인자의 전용 namespace이며 업무 그래프·인가 코드 잠금과 공유하지 않는다.
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(1094931276, 1145262592)`); err != nil {
		return storageError("oauth_client_lock", err)
	}
	var count int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM public.oauth_client`).Scan(&count); err != nil {
		return storageError("oauth_client_count", err)
	}
	if count >= 10000 {
		return ErrOAuthClientCapacity
	}
	_, err = tx.Exec(ctx, `INSERT INTO public.oauth_client (client_id,client_name,redirect_uris,resource,loopback_port_policy,created_at) VALUES ($1,$2,$3,$4,$5,$6)`, client.ID, client.Name, client.RedirectURIs, client.Resources[0], client.LoopbackPortPolicy, client.CreatedAt)
	if err != nil {
		return storageError("oauth_client_register", err)
	}
	return storageError("oauth_client_commit", tx.Commit(ctx))
}

// OAuthClient는 등록 ID에 결합된 callback·resource를 요청 컨텍스트로 조회한다.
func (s *Store) OAuthClient(ctx context.Context, id string) (OAuthClient, error) {
	var client OAuthClient
	var resource string
	err := s.pool.QueryRow(ctx, `SELECT client_id,client_name,redirect_uris,resource,loopback_port_policy,created_at FROM public.oauth_client WHERE client_id=$1`, id).Scan(&client.ID, &client.Name, &client.RedirectURIs, &resource, &client.LoopbackPortPolicy, &client.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return OAuthClient{}, ErrNotFound
	}
	if err != nil {
		return OAuthClient{}, storageError("oauth_client_lookup", err)
	}
	client.Resources = []string{resource}
	return client, nil
}
