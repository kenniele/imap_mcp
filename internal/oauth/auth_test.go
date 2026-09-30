package oauth

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"html"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

type memoryGrant struct {
	grant
	access, refresh             string
	accessExpiry, refreshExpiry time.Time
}
type memoryStore struct {
	mu      sync.Mutex
	grants  []memoryGrant
	failure error
}

func (s *memoryStore) SaveCode(_ context.Context, g grant) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.grants = append(s.grants, memoryGrant{grant: g})
	return s.failure
}
func (s *memoryStore) Exchange(_ context.Context, e exchange) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.grants {
		g := &s.grants[i]
		if g.Binding != e.Binding || g.ClientHash != e.ClientHash {
			continue
		}
		ok := e.Kind == "authorization_code" && g.CodeHash == e.Hash && time.Now().Before(g.CodeExpiresAt) && g.RedirectURI == e.RedirectURI && g.Challenge == e.Challenge
		ok = ok || e.Kind == "refresh_token" && g.refresh == e.Hash && time.Now().Before(g.refreshExpiry) && hasScope(g.Scope, "offline_access")
		if !ok {
			continue
		}
		if e.RequestedScope != "" {
			for _, scope := range strings.Fields(e.RequestedScope) {
				if !hasScope(g.Scope, scope) {
					return "", errInvalidGrant
				}
			}
			g.Scope = e.RequestedScope
		}
		g.CodeHash = ""
		g.access, g.refresh, g.accessExpiry = e.AccessHash, e.RefreshHash, e.AccessExpiresAt
		if e.Kind == "authorization_code" {
			g.refreshExpiry = e.RefreshExpiresAt
		}
		return g.Scope, nil
	}
	return "", errInvalidGrant
}
func (s *memoryStore) Validate(_ context.Context, hash, binding string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failure != nil {
		return "", s.failure
	}
	for _, g := range s.grants {
		if g.Binding == binding && g.access == hash && time.Now().Before(g.accessExpiry) {
			return g.Scope, nil
		}
	}
	return "", errInvalidGrant
}
func (s *memoryStore) Revoke(_ context.Context, hash, binding, clientHash string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, g := range s.grants {
		if g.Binding == binding && g.ClientHash == clientHash && (g.access == hash || g.refresh == hash) {
			s.grants = append(s.grants[:i], s.grants[i+1:]...)
			break
		}
	}
	return nil
}
func testConfig() Config {
	return Config{BaseURL: "https://mail-mcp.example", ClientID: "mail-mcp-chatgpt", ClientSecret: strings.Repeat("s", 32), LoginToken: strings.Repeat("l", 32), RedirectURIs: []string{"https://chatgpt.com/connector/oauth/callback"}}
}
func testServer(t *testing.T) (*Server, *memoryStore) {
	t.Helper()
	store := &memoryStore{}
	s, err := newServer(testConfig(), store, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	return s, store
}
func authorizeQuery(s *Server) url.Values {
	sum := sha256.Sum256([]byte(strings.Repeat("v", 43)))
	return url.Values{"client_id": {s.cfg.ClientID}, "redirect_uri": {s.cfg.RedirectURIs[0]}, "resource": {s.resource()}, "response_type": {"code"}, "state": {"state-from-chatgpt"}, "scope": {ReadScope + " offline_access"}, "code_challenge_method": {"S256"}, "code_challenge": {base64.RawURLEncoding.EncodeToString(sum[:])}}
}
func beginConsent(t *testing.T, s *Server, q url.Values) (*http.Cookie, string) {
	t.Helper()
	req := httptest.NewRequest("GET", s.cfg.BaseURL+"/oauth/mcp/authorize?"+q.Encode(), nil)
	w := httptest.NewRecorder()
	s.Routes().ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("consent %d: %s", w.Code, w.Body.String())
	}
	cookies := w.Result().Cookies()
	if len(cookies) != 1 || !cookies[0].Secure || !cookies[0].HttpOnly {
		t.Fatalf("unsafe cookie: %v", cookies)
	}
	matches := regexp.MustCompile(`name="csrf" value="([^"]+)"`).FindStringSubmatch(w.Body.String())
	if len(matches) != 2 {
		t.Fatal("consent form has no CSRF field")
	}
	csrf := html.UnescapeString(matches[1])
	if csrf != s.consentCSRF(cookies[0].Value, q) {
		t.Fatal("form CSRF does not bind consent parameters")
	}
	return cookies[0], csrf
}
func approve(t *testing.T, s *Server, q url.Values, cookie *http.Cookie, csrf, secret string) *httptest.ResponseRecorder {
	t.Helper()
	f := url.Values{"csrf": {csrf}, "login_token": {secret}, "decision": {"allow"}}
	r := httptest.NewRequest("POST", s.cfg.BaseURL+"/oauth/mcp/authorize?"+q.Encode(), strings.NewReader(f.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.Header.Set("Origin", s.cfg.BaseURL)
	r.AddCookie(cookie)
	w := httptest.NewRecorder()
	s.Routes().ServeHTTP(w, r)
	return w
}
func approvedCode(t *testing.T, s *Server) string {
	t.Helper()
	q := authorizeQuery(s)
	cookie, csrf := beginConsent(t, s, q)
	w := approve(t, s, q, cookie, csrf, s.cfg.LoginToken)
	if w.Code != 303 {
		t.Fatalf("approve %d: %s", w.Code, w.Body.String())
	}
	u, err := url.Parse(w.Header().Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	if u.Query().Get("state") != "state-from-chatgpt" || u.Query().Get("iss") != s.cfg.BaseURL {
		t.Fatal("lost state or issuer")
	}
	return u.Query().Get("code")
}
func tokenRequest(s *Server, f url.Values) *httptest.ResponseRecorder {
	r := httptest.NewRequest("POST", s.cfg.BaseURL+"/oauth/mcp/token", strings.NewReader(f.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.SetBasicAuth(s.cfg.ClientID, s.cfg.ClientSecret)
	w := httptest.NewRecorder()
	s.Routes().ServeHTTP(w, r)
	return w
}
func codeForm(s *Server, code string) url.Values {
	return url.Values{"grant_type": {"authorization_code"}, "code": {code}, "code_verifier": {strings.Repeat("v", 43)}, "redirect_uri": {s.cfg.RedirectURIs[0]}, "resource": {s.resource()}}
}
func tokens(t *testing.T, w *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	if w.Code != 200 {
		t.Fatalf("tokens %d: %s", w.Code, w.Body.String())
	}
	var data map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &data); err != nil {
		t.Fatal(err)
	}
	return data
}

func TestConfigValidation(t *testing.T) {
	for _, tt := range []struct {
		name string
		edit func(*Config)
	}{
		{"http", func(c *Config) { c.BaseURL = "http://mail-mcp.example" }},
		{"weak login", func(c *Config) { c.LoginToken = "short" }},
		{"same secrets", func(c *Config) { c.LoginToken = c.ClientSecret }},
		{"no redirects", func(c *Config) { c.RedirectURIs = nil }},
		{"fragment", func(c *Config) { c.RedirectURIs = []string{"https://chatgpt.com/cb#secret"} }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cfg := testConfig()
			tt.edit(&cfg)
			if _, err := newServer(cfg, &memoryStore{}, nil); err == nil {
				t.Fatal("unsafe config accepted")
			}
		})
	}
}
func TestDiscoveryAndAuthentication(t *testing.T) {
	s, store := testServer(t)
	for _, path := range []string{"/.well-known/oauth-protected-resource", "/.well-known/oauth-protected-resource/mcp", "/.well-known/oauth-authorization-server"} {
		w := httptest.NewRecorder()
		s.Routes().ServeHTTP(w, httptest.NewRequest("GET", s.cfg.BaseURL+path, nil))
		if w.Code != 200 || !json.Valid(w.Body.Bytes()) {
			t.Fatalf("bad discovery: %s", w.Body.String())
		}
	}
	h := s.Protect(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(204)
	}))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("POST", s.resource(), nil))
	if w.Code != 401 || !strings.Contains(w.Header().Get("WWW-Authenticate"), "resource_metadata=") {
		t.Fatal("missing OAuth challenge")
	}
	store.grants = append(store.grants, memoryGrant{grant: grant{Binding: s.binding, Scope: ReadScope}, access: digest("token"), accessExpiry: time.Now().Add(time.Hour)})
	r := httptest.NewRequest("POST", s.resource(), nil)
	r.Header.Set("Authorization", "Bearer token")
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 204 {
		t.Fatal(w.Code)
	}
	r.Header.Set("Origin", "https://evil.example")
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 403 {
		t.Fatal("foreign origin accepted")
	}
	r.Header.Del("Origin")
	store.failure = errors.New("postgres secret")
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 500 || strings.Contains(w.Body.String(), "secret") {
		t.Fatal("internal error leaked")
	}
}
func TestOAuthExchangeRefreshAndRevocation(t *testing.T) {
	s, _ := testServer(t)
	code := approvedCode(t, s)
	f := codeForm(s, code)
	bad := cloneValues(f)
	bad.Set("code_verifier", strings.Repeat("b", 43))
	if tokenRequest(s, bad).Code != 400 {
		t.Fatal("bad PKCE accepted")
	}
	bad = cloneValues(f)
	bad.Set("resource", "https://other.example/mcp")
	if tokenRequest(s, bad).Code != 400 {
		t.Fatal("wrong audience accepted")
	}
	first := tokens(t, tokenRequest(s, f))
	if tokenRequest(s, f).Code != 400 {
		t.Fatal("code replay accepted")
	}
	refresh := url.Values{"grant_type": {"refresh_token"}, "refresh_token": {first["refresh_token"].(string)}, "resource": {s.resource()}}
	second := tokens(t, tokenRequest(s, refresh))
	if second["refresh_token"] == first["refresh_token"] {
		t.Fatal("refresh not rotated")
	}
	if tokenRequest(s, refresh).Code != 400 {
		t.Fatal("refresh replay accepted")
	}
	if _, err := s.store.Validate(t.Context(), digest(first["access_token"].(string)), s.binding); err == nil {
		t.Fatal("old access still valid")
	}
	if err := s.store.Revoke(t.Context(), digest(second["refresh_token"].(string)), s.binding, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.store.Validate(t.Context(), digest(second["access_token"].(string)), s.binding); err == nil {
		t.Fatal("revoked token accepted")
	}
}
func TestConsentRejectsTampering(t *testing.T) {
	for _, tt := range []struct {
		name        string
		edit        func(url.Values)
		csrf, login bool
	}{
		{"redirect", func(q url.Values) { q.Set("redirect_uri", "https://evil.example") }, true, true},
		{"PKCE downgrade", func(q url.Values) { q.Set("code_challenge_method", "plain") }, true, true},
		{"consent scope tampering", func(q url.Values) { q.Set("scope", ReadScope) }, true, true},
		{"missing csrf", func(q url.Values) {}, false, true},
		{"wrong login", func(q url.Values) {}, true, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			s, store := testServer(t)
			q := authorizeQuery(s)
			cookie, csrf := beginConsent(t, s, q)
			tt.edit(q)
			login := s.cfg.LoginToken
			if !tt.csrf {
				csrf = "wrong"
			}
			if !tt.login {
				login = "wrong"
			}
			w := approve(t, s, q, cookie, csrf, login)
			if w.Code < 400 || len(store.grants) > 0 {
				t.Fatalf("unsafe consent: %d", w.Code)
			}
		})
	}
}

func TestConsentReferrerPolicyPreservesFormOrigin(t *testing.T) {
	s, _ := testServer(t)
	q := authorizeQuery(s)
	w := httptest.NewRecorder()
	s.Routes().ServeHTTP(w, httptest.NewRequest("GET", s.cfg.BaseURL+"/oauth/mcp/authorize?"+q.Encode(), nil))
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	// HTML form POSTs send Origin:null under no-referrer, even to the same
	// origin (Fetch's "append a request Origin header" algorithm).
	// strict-origin preserves Origin without disclosing the OAuth URL query.
	if got := w.Header().Get("Referrer-Policy"); got != "strict-origin" {
		t.Fatalf("consent policy must preserve form Origin without leaking the query: %q", got)
	}
	cookie, csrf := beginConsent(t, s, q)
	w = approve(t, s, q, cookie, csrf, s.cfg.LoginToken)
	if w.Code != http.StatusSeeOther {
		t.Fatalf("same-origin form rejected: %d %s", w.Code, w.Body.String())
	}
	// The callback response must still suppress the referrer on navigation.
	if got := w.Header().Get("Referrer-Policy"); got != "no-referrer" {
		t.Fatalf("OAuth callback response lost its referrer policy: %q", got)
	}
}

func TestConsentRejectsMissingNullAndForeignOrigin(t *testing.T) {
	for _, origin := range []string{"", "null", "https://evil.example"} {
		t.Run(origin, func(t *testing.T) {
			s, store := testServer(t)
			q := authorizeQuery(s)
			cookie, csrf := beginConsent(t, s, q)
			form := url.Values{"csrf": {csrf}, "login_token": {s.cfg.LoginToken}, "decision": {"allow"}}
			r := httptest.NewRequest("POST", s.cfg.BaseURL+"/oauth/mcp/authorize?"+q.Encode(), strings.NewReader(form.Encode()))
			r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			if origin != "" {
				r.Header.Set("Origin", origin)
			}
			r.AddCookie(cookie)
			w := httptest.NewRecorder()
			s.Routes().ServeHTTP(w, r)
			if w.Code != 403 || len(store.grants) != 0 || w.Header().Get("Location") != "" {
				t.Fatalf("untrusted form origin accepted: %d", w.Code)
			}
		})
	}
}
func TestScopeAndOwnerIsolation(t *testing.T) {
	s, store := testServer(t)
	code := approvedCode(t, s)
	f := codeForm(s, code)
	f.Set("scope", ReadScope+" offline_access")
	result := tokens(t, tokenRequest(s, f))
	refresh := url.Values{"grant_type": {"refresh_token"}, "refresh_token": {result["refresh_token"].(string)}, "resource": {s.resource()}, "scope": {ReadScope + " mail.write"}}
	if tokenRequest(s, refresh).Code != 400 {
		t.Fatal("scope escalation accepted")
	}
	for _, edit := range []func(*Config){func(c *Config) { c.BaseURL = "https://other.example" }, func(c *Config) { c.LoginToken = strings.Repeat("z", 32) }} {
		cfg := s.cfg
		edit(&cfg)
		other, err := newServer(cfg, store, nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := store.Validate(t.Context(), digest(result["access_token"].(string)), other.binding); err == nil {
			t.Fatal("token crossed owner/secret boundary")
		}
	}
}

func cloneValues(v url.Values) url.Values {
	c := url.Values{}
	for k, vs := range v {
		c[k] = append([]string(nil), vs...)
	}
	return c
}

func TestClientSecretPostAndExpiredCode(t *testing.T) {
	s, store := testServer(t)
	code := approvedCode(t, s)
	f := codeForm(s, code)
	f.Set("client_id", s.cfg.ClientID)
	f.Set("client_secret", s.cfg.ClientSecret)
	request := func() *httptest.ResponseRecorder {
		r := httptest.NewRequest("POST", s.cfg.BaseURL+"/oauth/mcp/token", strings.NewReader(f.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		w := httptest.NewRecorder()
		s.Routes().ServeHTTP(w, r)
		return w
	}
	wrongSecret := f.Get("client_secret")
	f.Set("client_secret", "wrong")
	if request().Code != 401 {
		t.Fatal("wrong client secret accepted")
	}
	f.Set("client_secret", wrongSecret)
	store.grants[0].CodeExpiresAt = time.Now().Add(-time.Second)
	if request().Code != 400 {
		t.Fatal("expired code accepted")
	}
	store.grants[0].CodeExpiresAt = time.Now().Add(time.Minute)
	_ = tokens(t, request())
}

func TestOAuthErrorsDoNotLogSecretsAndProtectionKeepsRequestContext(t *testing.T) {
	var logs bytes.Buffer
	store := &memoryStore{}
	s, err := newServer(testConfig(), store, slog.New(slog.NewJSONHandler(&logs, nil)))
	if err != nil {
		t.Fatal(err)
	}
	store.grants = append(store.grants, memoryGrant{grant: grant{Binding: s.binding, Scope: ReadScope}, access: digest("access-token"), accessExpiry: time.Now().Add(time.Hour)})
	h := s.Protect(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.Context().Err(); err != nil {
			t.Fatal("database-check cancellation reached MCP", err)
		}
		if _, limited := r.Context().Deadline(); limited {
			t.Fatal("OAuth imposed a deadline on MCP streams")
		}
		w.WriteHeader(204)
	}))
	r := httptest.NewRequest("POST", s.resource(), nil)
	r.Header.Set("Authorization", "Bearer access-token")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 204 {
		t.Fatal(w.Code)
	}
	store.failure = errors.New("database password " + s.cfg.ClientSecret + " " + s.cfg.LoginToken)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 500 {
		t.Fatal(w.Code)
	}
	for _, secret := range []string{s.cfg.ClientSecret, s.cfg.LoginToken, "database password", "access-token"} {
		if strings.Contains(logs.String()+w.Body.String(), secret) {
			t.Fatal("OAuth leaked a secret")
		}
	}
}
