//go:build integration

package oauth

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"mail-mcp/internal/app"
	"mail-mcp/internal/imap"
	mailmcp "mail-mcp/internal/mcp"
	"mail-mcp/internal/observability"
	"mail-mcp/internal/storage/postgres"
	"mail-mcp/internal/testutil"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

type bearerTransport struct{ token string }

func (b bearerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.token)
	return http.DefaultTransport.RoundTrip(r)
}

func TestOAuthToMailMCP(t *testing.T) {
	security, database := integrationServer(t)
	fixture := testutil.NewIMAP(t)
	store := &postgres.Store{DB: database}
	account, _ := fixture.Account(t, "personal", []string{testutil.Plain})
	plain, err := fixture.Cipher.Decrypt(account.Secret, "account:"+account.ID)
	if err != nil {
		t.Fatal(err)
	}
	account.ID = "00000000-0000-4000-8000-000000000001"
	account.Secret, err = fixture.Cipher.Encrypt(plain, "account:"+account.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Add(t.Context(), account); err != nil {
		t.Fatal(err)
	}
	pool := imap.NewPool(fixture.Cipher, fixture.Roots)
	defer pool.Close()
	metrics := observability.NewMetrics()
	service := app.New(store, &imap.Provider{Pool: pool, Metrics: metrics}, fixture.Cipher)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	server := httptest.NewServer(mailmcp.Handler(service, store, strings.Repeat("m", 32), metrics, logger, security))
	defer server.Close()
	for _, path := range []string{"/.well-known/oauth-protected-resource/mcp", "/.well-known/oauth-authorization-server"} {
		r, err := http.Get(server.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(r.Body)
		_ = r.Body.Close()
		if r.StatusCode != 200 || !json.Valid(body) {
			t.Fatalf("discovery %s: %d", path, r.StatusCode)
		}
	}
	first := tokens(t, tokenRequest(security, codeForm(security, approvedCode(t, security))))
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	client := sdk.NewClient(&sdk.Implementation{Name: "oauth-mail-test", Version: "1"}, nil)
	session, err := client.Connect(ctx, &sdk.StreamableClientTransport{Endpoint: server.URL + "/mcp", HTTPClient: &http.Client{Transport: bearerTransport{first["access_token"].(string)}}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	listed, err := session.ListTools(ctx, nil)
	if err != nil || len(listed.Tools) != 6 {
		t.Fatalf("tools after OAuth: %v", err)
	}
	for _, tool := range listed.Tools {
		if tool.Annotations == nil || !tool.Annotations.ReadOnlyHint {
			t.Fatalf("mail tool permits writes: %s", tool.Name)
		}
	}
	result, err := session.CallTool(ctx, &sdk.CallToolParams{Name: "mail_recent", Arguments: map[string]any{"limit": 1}})
	if err != nil || result.IsError {
		t.Fatalf("read mail after OAuth: %v", err)
	}
	if err := security.store.Revoke(ctx, digest(first["access_token"].(string)), security.binding, ""); err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequestWithContext(ctx, "GET", server.URL+"/mcp", nil)
	req.Header.Set("Authorization", "Bearer "+first["access_token"].(string))
	r, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = r.Body.Close()
	if r.StatusCode != 401 {
		t.Fatal("revoked OAuth token still reaches MCP")
	}
}
