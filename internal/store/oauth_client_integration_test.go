package store

import (
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestDCRClientPersistenceIntegration(t *testing.T) {
	database := newIsolatedIntegrationStore(t)
	client := OAuthClient{ID: "dcr_persistence", Name: "Codex", RedirectURIs: []string{"http://127.0.0.1:49152/callback"}, Resources: []string{"https://resource.test/mcp"}, LoopbackPortPolicy: "same_ip", CreatedAt: time.Now().UTC().Truncate(time.Second)}
	if err := database.RegisterOAuthClient(t.Context(), client); err != nil {
		t.Fatal(err)
	}
	second := &Store{pool: database.pool}
	stored, err := second.OAuthClient(t.Context(), client.ID)
	if err != nil || stored.ID != client.ID || stored.Name != client.Name || stored.RedirectURIs[0] != client.RedirectURIs[0] || stored.Resources[0] != client.Resources[0] || !stored.CreatedAt.Equal(client.CreatedAt) {
		t.Fatalf("shared registration = %+v %v", stored, err)
	}
	if _, err := second.OAuthClient(t.Context(), "absent"); !errors.Is(err, ErrNotFound) {
		t.Fatal("unknown client", err)
	}
	if err := database.RegisterOAuthClient(t.Context(), client); err == nil {
		t.Fatal("duplicate ID overwrote registration")
	}
}

func TestDCRClientCapacityConcurrentIntegration(t *testing.T) {
	database := newIsolatedIntegrationStore(t)
	_, err := database.pool.Exec(t.Context(), `INSERT INTO public.oauth_client (client_id,client_name,redirect_uris,resource,loopback_port_policy,created_at) SELECT 'capacity_' || n,'',ARRAY['http://127.0.0.1:49152/callback'],'https://resource.test/mcp','same_ip',now() FROM generate_series(1,9999) n`)
	if err != nil {
		t.Fatal(err)
	}
	var success atomic.Int64
	var wg sync.WaitGroup
	for i := range 12 {
		wg.Go(func() {
			err := database.RegisterOAuthClient(t.Context(), OAuthClient{ID: fmt.Sprintf("dcr_concurrent_%d", i), RedirectURIs: []string{"http://127.0.0.1:49152/callback"}, Resources: []string{"https://resource.test/mcp"}, LoopbackPortPolicy: "same_ip", CreatedAt: time.Now().UTC()})
			if err == nil {
				success.Add(1)
			} else if !errors.Is(err, ErrOAuthClientCapacity) {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	var count int
	if err := database.pool.QueryRow(t.Context(), `SELECT count(*) FROM public.oauth_client`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if success.Load() != 1 || count != 10000 {
		t.Fatalf("registration cap: successes=%d count=%d", success.Load(), count)
	}
}
