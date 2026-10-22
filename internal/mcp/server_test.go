package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"mail-mcp/internal/app"
	"mail-mcp/internal/domain"
	"mail-mcp/internal/imap"
	"mail-mcp/internal/observability"
	"mail-mcp/internal/secrets"
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

type manyFoldersProvider struct{ app.MailProvider }

func (manyFoldersProvider) Folders(context.Context, domain.Account) ([]domain.Folder, error) {
	folders := make([]domain.Folder, 400)
	for i := range folders {
		folders[i].Name = "Archive/" + strconv.Itoa(i) + "/" + strings.Repeat("я", 80)
	}
	return folders, nil
}

func (manyFoldersProvider) Search(_ context.Context, a domain.Account, q domain.SearchQuery, _ domain.Position) (domain.Page, error) {
	return domain.Page{
		Messages: []domain.Message{{Account: a.Alias, ID: a.ID + ":" + q.Folder + ":1", Folder: q.Folder, UID: 1, UIDValidity: 1}},
		Start:    domain.Position{BeforeUID: 2, UIDValidity: 1},
		Next:     domain.Position{BeforeUID: 1, UIDValidity: 1, Done: true},
	}, nil
}

func TestLargeFolderCursorOverHTTP(t *testing.T) {
	cipher, err := secrets.New(bytes.Repeat([]byte{42}, 32))
	if err != nil {
		t.Fatal(err)
	}
	accounts := testStore{{ID: "one", Alias: "one", Enabled: true}}
	service := app.New(accounts, manyFoldersProvider{}, cipher)
	token := strings.Repeat("t", 32)
	handler := Handler(service, accounts, token, observability.NewMetrics(), nil, nil)
	cursor := ""
	previousID := ""
	for range 2 {
		body, err := json.Marshal(map[string]any{
			"jsonrpc": "2.0", "id": 1, "method": "tools/call",
			"params": map[string]any{"name": "mail_recent", "arguments": map[string]any{"limit": 1, "cursor": cursor}},
		})
		if err != nil {
			t.Fatal(err)
		}
		req := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		req.Header.Set("MCP-Protocol-Version", "2025-06-18")
		req.Header.Set("Authorization", "Bearer "+token)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, req)
		var reply struct {
			Error  json.RawMessage `json:"error"`
			Result struct {
				IsError bool                `json:"isError"`
				Search  domain.SearchResult `json:"structuredContent"`
			} `json:"result"`
		}
		if err := json.Unmarshal(response.Body.Bytes(), &reply); err != nil {
			t.Fatal(err)
		}
		result := reply.Result.Search
		if response.Code != http.StatusOK || len(reply.Error) != 0 || reply.Result.IsError || len(result.Errors) != 0 || len(result.Messages) != 1 {
			t.Fatalf("large cursor request failed: status=%d rpc_error=%s tool_error=%v", response.Code, reply.Error, reply.Result.IsError)
		}
		if result.Messages[0].ID == previousID {
			t.Fatal("cursor repeated a message")
		}
		previousID = result.Messages[0].ID
		cursor = result.NextCursor
		if len(cursor) <= 64*1024 || len(cursor) > app.MaxSearchCursorSize {
			t.Fatalf("expected a supported cursor exceeding the old HTTP limit, got %d bytes", len(cursor))
		}
	}
}

func TestDiscoveryWithoutSession(t *testing.T) {
	accounts := testStore{}
	metrics := observability.NewMetrics()
	service := app.New(accounts, nil, nil)
	token := strings.Repeat("t", 32)
	handler := Handler(service, accounts, token, metrics, slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
	for _, version := range []string{"2025-03-26", "2025-06-18", "2025-11-25"} {
		t.Run(version, func(t *testing.T) {
			request := func(body string, authenticated bool) *httptest.ResponseRecorder {
				req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(body))
				req.Header.Set("Content-Type", "application/json")
				req.Header.Set("Accept", "application/json, text/event-stream")
				req.Header.Set("MCP-Protocol-Version", version)
				if authenticated {
					req.Header.Set("Authorization", "Bearer "+token)
				}
				response := httptest.NewRecorder()
				handler.ServeHTTP(response, req)
				return response
			}
			initialize := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"` + version + `","capabilities":{},"clientInfo":{"name":"discovery-test","version":"1"}}}`
			response := request(initialize, true)
			if response.Code != http.StatusOK {
				t.Fatalf("initialization status=%d", response.Code)
			}
			// Discovery and subsequent calls must not depend on a process-local session.
			for range 2 {
				response = request(`{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}`, true)
				var reply struct {
					Error  json.RawMessage `json:"error"`
					Result struct {
						Tools []*sdk.Tool `json:"tools"`
					} `json:"result"`
				}
				if err := json.Unmarshal(response.Body.Bytes(), &reply); err != nil {
					t.Fatal(err)
				}
				if response.Code != http.StatusOK || len(reply.Error) != 0 || len(reply.Result.Tools) != 6 {
					t.Fatalf("sessionless discovery: status=%d error=%s tool_count=%d", response.Code, reply.Error, len(reply.Result.Tools))
				}
				for _, tool := range reply.Result.Tools {
					if tool.Annotations == nil || !tool.Annotations.ReadOnlyHint {
						t.Fatalf("tool is not read-only: %s", tool.Name)
					}
					encoded, err := json.Marshal(tool.InputSchema)
					if err != nil {
						t.Fatal(err)
					}
					var schema struct {
						Properties map[string]json.RawMessage `json:"properties"`
					}
					if err := json.Unmarshal(encoded, &schema); err != nil {
						t.Fatal(err)
					}
					if schema.Properties == nil {
						t.Fatalf("tool schema omits properties: %s", tool.Name)
					}
				}
			}
			response = request(`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"mail_accounts","arguments":{}}}`, true)
			var called struct {
				Error  json.RawMessage     `json:"error"`
				Result *sdk.CallToolResult `json:"result"`
			}
			if err := json.Unmarshal(response.Body.Bytes(), &called); err != nil {
				t.Fatal(err)
			}
			if response.Code != http.StatusOK || len(called.Error) != 0 || called.Result == nil || called.Result.IsError {
				t.Fatal("sessionless mail_accounts call failed")
			}
			if response = request(`{"jsonrpc":"2.0","id":4,"method":"tools/list","params":{}}`, false); response.Code != http.StatusUnauthorized {
				t.Fatal("unauthenticated discovery was accepted")
			}
		})
	}
}

func TestToolLogReportsAccountFailure(t *testing.T) {
	f := testutil.NewIMAP(t)
	good, _ := f.Account(t, "personal", []string{testutil.Plain})
	broken, _ := f.Account(t, "university", nil)
	var err error
	broken.Secret, err = f.Cipher.Encrypt([]byte("wrong-password"), "account:"+broken.ID)
	if err != nil {
		t.Fatal(err)
	}
	accounts := testStore{good, broken}
	pool := imap.NewPool(f.Cipher, f.Roots)
	t.Cleanup(pool.Close)
	metrics := observability.NewMetrics()
	service := app.New(accounts, &imap.Provider{Pool: pool, Metrics: metrics}, f.Cipher)
	var output bytes.Buffer
	token := strings.Repeat("t", 32)
	handler := Handler(service, accounts, token, metrics, slog.New(slog.NewJSONHandler(&output, nil)), nil)
	req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"mail_recent","arguments":{"limit":1}}}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("MCP-Protocol-Version", "2025-06-18")
	req.Header.Set("Authorization", "Bearer "+token)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, req)
	var row map[string]any
	if err := json.Unmarshal(output.Bytes(), &row); err != nil {
		t.Fatal(err)
	}
	if row["result"] != "partial" || row["account_error_count"] != float64(1) || row["result_count"] != float64(1) {
		t.Fatal("partial account failure was logged as a successful tool")
	}
	if row["request_id"] == "" || row["tool"] != "mail_recent" {
		t.Fatal("tool completion lacks request correlation")
	}
}

func TestStreamableHTTPTools(t *testing.T) {
	fixture := testutil.NewIMAP(t)
	accounts := testStore{}
	for _, alias := range []string{"personal", "university", "work"} {
		a, user := fixture.Account(t, alias, []string{testutil.Plain, testutil.HTML, testutil.Multipart})
		if alias == "university" {
			if err := user.Create("Archive/Учёба", nil); err != nil {
				t.Fatal(err)
			}
			fixture.AppendFolder(t, user, "Archive/Учёба", strings.Replace(testutil.Plain, "08:42:00", "11:42:00", 1))
		}
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
			if (tc.name == "mail_recent" || tc.name == "mail_search") && !strings.Contains(string(b), "Archive/Учёба") {
				t.Fatal("default tool search omitted a nested folder")
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
