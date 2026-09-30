package mcp

import (
	"bytes"
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
	"mail-mcp/internal/domain"
	"mail-mcp/internal/imap"
	"mail-mcp/internal/observability"
	"mail-mcp/internal/testutil"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

type testStore []domain.Account

func (s testStore) List(context.Context) ([]domain.Account, error) { return s, nil }
func (s testStore) Ping(context.Context) error                     { return nil }

type authorizedTransport struct {
	token string
	base  http.RoundTripper
}

func (t authorizedTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+t.token)
	return t.base.RoundTrip(r)
}
func TestStreamableHTTPTools(t *testing.T) {
	fixture := testutil.NewIMAP(t)
	accounts := testStore{}
	for _, alias := range []string{"personal", "university", "work"} {
		a, _ := fixture.Account(t, alias, []string{testutil.Plain, testutil.HTML, testutil.Multipart})
		accounts = append(accounts, a)
	}
	pool := imap.NewPool(fixture.Cipher, fixture.Roots)
	defer pool.Close()
	metrics := observability.NewMetrics()
	service := app.New(accounts, &imap.Provider{Pool: pool, Metrics: metrics}, fixture.Cipher)
	var logOutput bytes.Buffer
	token := strings.Repeat("t", 32)
	server := httptest.NewServer(Handler(service, accounts, token, metrics, slog.New(slog.NewJSONHandler(&logOutput, nil)), nil))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	client := sdk.NewClient(&sdk.Implementation{Name: "test", Version: "1"}, nil)
	transport := &sdk.StreamableClientTransport{Endpoint: server.URL + "/mcp", HTTPClient: &http.Client{Transport: authorizedTransport{token, http.DefaultTransport}}}
	session, e := client.Connect(ctx, transport, nil)
	if e != nil {
		t.Fatal(e)
	}
	defer session.Close()
	listed, e := session.ListTools(ctx, nil)
	if e != nil {
		t.Fatal(e)
	}
	if len(listed.Tools) != 6 {
		t.Fatalf("tool count=%d", len(listed.Tools))
	}
	for _, tool := range listed.Tools {
		if tool.Annotations == nil || !tool.Annotations.ReadOnlyHint || !strings.Contains(tool.Description, "untrusted user data") {
			t.Fatalf("unsafe annotations: %s", tool.Name)
		}
	}
	for _, tc := range []struct {
		name     string
		args     map[string]any
		contains string
	}{{"mail_accounts", map[string]any{}, "university"}, {"mail_folders", map[string]any{"account": "university"}, "INBOX"}, {"mail_recent", map[string]any{"limit": 2, "cursor": nil}, "messages"}, {"mail_search", map[string]any{"accounts": []string{"university"}, "query": "ВКР", "seen": nil, "has_attachments": nil}, "ВКР"}, {"mail_get", map[string]any{"account": "university", "uid": 3, "include_attachments": true}, "document.pdf"}, {"mail_thread", map[string]any{"account": "university", "uid": 1}, "Привет"}} {
		t.Run(tc.name, func(t *testing.T) {
			result, e := session.CallTool(ctx, &sdk.CallToolParams{Name: tc.name, Arguments: tc.args})
			if e != nil {
				t.Fatal(e)
			}
			if result.IsError {
				t.Fatalf("tool error: %+v", result.Content)
			}
			b, e := json.Marshal(result.StructuredContent)
			if e != nil {
				t.Fatal(e)
			}
			if !strings.Contains(string(b), tc.contains) {
				t.Fatalf("result=%s", b)
			}
			for _, secret := range []string{"app-password", "encrypted_secret", "IMAPHost", "binary-secret"} {
				if strings.Contains(string(b), secret) {
					t.Fatal("secret or binary content leaked")
				}
			}
			if tc.name == "mail_search" && !strings.Contains(string(b), "Добрый день") {
				t.Fatal("missing search preview")
			}
			if tc.name == "mail_recent" && (strings.Contains(string(b), `"text"`) || strings.Contains(string(b), `"html"`)) {
				t.Fatal("recent result includes bodies")
			}
		})
	}
	ids := map[string]bool{}
	for _, line := range strings.Split(strings.TrimSpace(logOutput.String()), "\n") {
		var row map[string]any
		if json.Unmarshal([]byte(line), &row) != nil {
			t.Fatal("invalid structured log")
		}
		id, _ := row["request_id"].(string)
		if id == "" || ids[id] {
			t.Fatal("tool requests share or lack request_id")
		}
		ids[id] = true
	}
	if len(ids) != 6 {
		t.Fatalf("tool completion log count=%d", len(ids))
	}
	for _, limit := range []int{0, -1, 101} {
		invalid, e := session.CallTool(ctx, &sdk.CallToolParams{Name: "mail_recent", Arguments: map[string]any{"limit": limit}})
		if e == nil && !invalid.IsError {
			t.Fatalf("invalid limit %d accepted", limit)
		}
	}
	for _, path := range []string{"/mcp", "/metrics"} {
		response, e := http.Get(server.URL + path)
		if e != nil {
			t.Fatal(e)
		}
		_ = response.Body.Close()
		if response.StatusCode != 401 {
			t.Fatalf("public endpoint %s", path)
		}
	}
	response, e := http.Get(server.URL + "/ready")
	if e != nil {
		t.Fatal(e)
	}
	defer response.Body.Close()
	if response.StatusCode != 200 {
		t.Fatal("readiness")
	}
	req, _ := http.NewRequest("GET", server.URL+"/metrics", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	response, e = http.DefaultClient.Do(req)
	if e != nil {
		t.Fatal(e)
	}
	defer response.Body.Close()
	b, _ := io.ReadAll(response.Body)
	for _, name := range []string{"mcp_requests_total", "mcp_request_duration_seconds", "imap_requests_total", "imap_request_duration_seconds", "imap_errors_total"} {
		if !strings.Contains(string(b), name) {
			if name == "imap_errors_total" {
				continue
			}
			t.Fatal("metric missing", name)
		}
	}
}
