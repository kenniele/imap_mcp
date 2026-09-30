//go:build integration

package oauth

import (
	"context"
	"crypto/rand"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"mail-mcp/migrations"
)

func integrationServer(t *testing.T) (*Server, *pgxpool.Pool) {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	admin, err := pgxpool.New(t.Context(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(admin.Close)
	schema := "oauth_test_" + strings.ToLower(rand.Text())
	ident := pgx.Identifier{schema}.Sanitize()
	if _, err := admin.Exec(t.Context(), "CREATE SCHEMA "+ident); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := admin.Exec(ctx, "DROP SCHEMA "+ident+" CASCADE"); err != nil {
			t.Error(err)
		}
	})
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	q.Set("search_path", schema)
	u.RawQuery = q.Encode()
	pool, err := pgxpool.New(t.Context(), u.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	// A repeat migration must be safe on an existing deployment.
	for range 2 {
		if _, err := pool.Exec(t.Context(), migrations.AccountsSQL+"\n"+migrations.OAuthSQL); err != nil {
			t.Fatal(err)
		}
	}
	s, err := NewServer(testConfig(), pool, nil)
	if err != nil {
		t.Fatal(err)
	}
	return s, pool
}
func TestPostgresOAuthAtomicExchange(t *testing.T) {
	s, pool := integrationServer(t)
	code := approvedCode(t, s)
	f := codeForm(s, code)
	var mu sync.Mutex
	var replies []*httptest.ResponseRecorder
	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() { defer wg.Done(); r := tokenRequest(s, f); mu.Lock(); replies = append(replies, r); mu.Unlock() }()
	}
	wg.Wait()
	successes := 0
	var token string
	for _, reply := range replies {
		if reply.Code == 200 {
			successes++
			token = tokens(t, reply)["access_token"].(string)
		} else if reply.Code != 400 {
			t.Fatalf("unexpected response %d: %s", reply.Code, reply.Body.String())
		}
	}
	if successes != 1 {
		t.Fatalf("code exchanged %d times", successes)
	}
	reloaded, err := NewServer(s.cfg, pool, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reloaded.store.Validate(t.Context(), digest(token), reloaded.binding); err != nil {
		t.Fatal("grant lost on restart", err)
	}
	if _, err := pool.Exec(t.Context(), `UPDATE mcp_oauth_grants SET access_expires_at=now()-interval '1 second' WHERE binding=$1`, s.binding); err != nil {
		t.Fatal(err)
	}
	if _, err := s.store.Validate(t.Context(), digest(token), s.binding); err == nil {
		t.Fatal("expired access token accepted")
	}
}

func TestPostgresNativeClientIsolation(t *testing.T) {
	s, _ := integrationServer(t)
	exerciseClientIsolation(t, s)
}
