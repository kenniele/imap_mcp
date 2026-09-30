package oauth

import (
	"encoding/json"
	"html"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"testing"
)

func registrationRequest(s *Server, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest("POST", s.cfg.BaseURL+"/oauth/mcp/register", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.Routes().ServeHTTP(w, r)
	return w
}

func registerNative(t *testing.T, s *Server, callback string) string {
	t.Helper()
	body, _ := json.Marshal(nativeMetadata{ClientName: "ChatGPT desktop", RedirectURIs: []string{callback},
		AuthMethod: "none", GrantTypes: []string{"authorization_code", "refresh_token"}, ResponseTypes: []string{"code"}})
	w := registrationRequest(s, string(body))
	if w.Code != http.StatusCreated {
		t.Fatalf("register: %d %s", w.Code, w.Body.String())
	}
	var result struct {
		nativeMetadata
		ClientID     string `json:"client_id"`
		ClientSecret string `json:"client_secret"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.AuthMethod != "none" || result.ClientSecret != "" || result.ClientID == "" {
		t.Fatalf("unexpected public client: %s", w.Body.String())
	}
	return result.ClientID
}

func nativeQuery(s *Server, id, callback string) url.Values {
	q := authorizeQuery(s)
	q.Set("client_id", id)
	q.Set("redirect_uri", callback)
	return q
}

func approveQuery(t *testing.T, s *Server, q url.Values) string {
	t.Helper()
	cookie, csrf := beginConsent(t, s, q)
	w := approve(t, s, q, cookie, csrf, s.cfg.LoginToken)
	if w.Code != http.StatusSeeOther {
		t.Fatalf("approve: %d %s", w.Code, w.Body.String())
	}
	u, err := url.Parse(w.Header().Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	if u.Query().Get("state") != q.Get("state") || u.Query().Get("iss") != s.cfg.BaseURL || u.Query().Get("code") == "" {
		t.Fatalf("invalid callback: %s", u)
	}
	return u.Query().Get("code")
}

func publicRequest(s *Server, path string, form url.Values) *httptest.ResponseRecorder {
	r := httptest.NewRequest("POST", s.cfg.BaseURL+path, strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	s.Routes().ServeHTTP(w, r)
	return w
}

func nativeCodeForm(s *Server, id, callback, code string) url.Values {
	f := codeForm(s, code)
	f.Set("client_id", id)
	f.Set("redirect_uri", callback)
	return f
}

func TestNativeDiscoveryAndRegistration(t *testing.T) {
	s, store := testServer(t)
	w := httptest.NewRecorder()
	s.Routes().ServeHTTP(w, httptest.NewRequest("GET", s.cfg.BaseURL+"/.well-known/oauth-authorization-server", nil))
	var metadata struct {
		Registration string   `json:"registration_endpoint"`
		AuthMethods  []string `json:"token_endpoint_auth_methods_supported"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &metadata); err != nil {
		t.Fatal(err)
	}
	if metadata.Registration != s.cfg.BaseURL+"/oauth/mcp/register" || !slices.Contains(metadata.AuthMethods, "none") || !slices.Contains(metadata.AuthMethods, "client_secret_basic") {
		t.Fatalf("bad metadata: %s", w.Body.String())
	}
	id := registerNative(t, s, "http://127.0.0.1/callback")
	if id == registerNative(t, s, "http://127.0.0.1/callback") || len(store.grants) != 0 {
		t.Fatal("registration must be unique and must not create a grant")
	}
	restarted, err := newServer(s.cfg, store, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := restarted.resolveClient(id); !ok {
		t.Fatal("registration lost on restart")
	}
	for _, edit := range []func(*Config){
		func(c *Config) { c.BaseURL = "https://other.example" },
		func(c *Config) { c.LoginToken = strings.Repeat("n", 32) },
		func(c *Config) { c.ClientSecret = strings.Repeat("c", 32) },
	} {
		cfg := s.cfg
		edit(&cfg)
		changed, err := newServer(cfg, store, nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := changed.resolveClient(id); ok {
			t.Fatal("registration crossed configuration boundary")
		}
	}
}

func TestNativeRegistrationRejectsUnsafeMetadata(t *testing.T) {
	s, _ := testServer(t)
	for _, callback := range []string{
		"https://evil.example/callback", "http://192.168.1.1/callback", "http://localhost/callback",
		"http://127.0.0.1.evil.example/callback", "http://127.1/callback", "http://[::ffff:127.0.0.1]/callback",
		"http://user@127.0.0.1/callback", "http://127.0.0.1/callback#fragment", "http://127.0.0.1/callback#",
		"http://127.0.0.1:0/callback", "http://127.0.0.1:65536/callback", "http://127.0.0.1:/callback",
		"http://127.0.0.1", "http://::1/callback", "http://127.0.0.1/" + strings.Repeat("x", 512),
	} {
		t.Run(callback, func(t *testing.T) {
			body, _ := json.Marshal(nativeMetadata{RedirectURIs: []string{callback}})
			if w := registrationRequest(s, string(body)); w.Code != 400 || !strings.Contains(w.Body.String(), "invalid_redirect_uri") {
				t.Fatalf("accepted unsafe callback: %d %s", w.Code, w.Body.String())
			}
		})
	}
	for _, body := range []string{
		`{`, `{}`, `null`, `{"redirect_uris":[]}`, `{"redirect_uris":["http://127.0.0.1/callback"]} {}`,
		`{"redirect_uris":["http://127.0.0.1/callback"],"token_endpoint_auth_method":"client_secret_post"}`,
		`{"redirect_uris":["http://127.0.0.1/callback"],"grant_types":["client_credentials"]}`,
		`{"redirect_uris":["http://127.0.0.1/callback"],"response_types":["token"]}`,
		`{"redirect_uris":["http://127.0.0.1/callback"],"client_name":"` + strings.Repeat("x", 9000) + `"}`,
	} {
		if w := registrationRequest(s, body); w.Code != 400 {
			t.Errorf("bad metadata accepted: %d %s", w.Code, w.Body.String())
		}
	}
}

func TestNativeRedirectMatching(t *testing.T) {
	s, _ := testServer(t)
	id := registerNative(t, s, "http://127.0.0.1:3000/callback/client?test=1")
	client, _ := s.resolveClient(id)
	for _, callback := range []string{"http://127.0.0.1:54321/callback/client?test=1", "http://127.0.0.1/callback/client?test=1"} {
		if !client.allowsRedirect(callback) {
			t.Errorf("variable port rejected: %s", callback)
		}
	}
	for _, callback := range []string{
		"http://[::1]:3000/callback/client?test=1", "https://127.0.0.1:3000/callback/client?test=1",
		"http://127.0.0.1:3000/callback/other?test=1", "http://127.0.0.1:3000/callback/client?test=2",
		"http://127.0.0.1:3000/callback/client", "http://127.0.0.1:3000/%63allback/client?test=1",
	} {
		if client.allowsRedirect(callback) {
			t.Errorf("non-matching callback accepted: %s", callback)
		}
	}
	client, _ = s.resolveClient(registerNative(t, s, "http://[::1]/callback"))
	if !client.allowsRedirect("http://[::1]:54321/callback") {
		t.Fatal("IPv6 loopback callback rejected")
	}
}

func TestConsentPolicyAllowsCallbackNavigation(t *testing.T) {
	s, _ := testServer(t)
	for _, callback := range []string{s.cfg.RedirectURIs[0], "http://127.0.0.1:54321/callback", "http://[::1]:54321/callback"} {
		q := authorizeQuery(s)
		if callback != s.cfg.RedirectURIs[0] {
			q = nativeQuery(s, registerNative(t, s, callback), callback)
		}
		requestURI := "/oauth/mcp/authorize?" + q.Encode()
		w := httptest.NewRecorder()
		s.Routes().ServeHTTP(w, httptest.NewRequest("GET", s.cfg.BaseURL+requestURI, nil))
		policy := w.Header().Get("Content-Security-Policy")
		if w.Code != 200 || strings.Contains(policy, "form-action") || !strings.Contains(policy, "default-src 'none'") || !strings.Contains(policy, "frame-ancestors 'none'") || !strings.Contains(policy, "base-uri 'none'") {
			t.Fatalf("consent policy blocks OAuth navigation or lost protections: %s", policy)
		}
		form := regexp.MustCompile(`<form method="post" action="([^"]+)"`).FindStringSubmatch(w.Body.String())
		if len(form) != 2 || html.UnescapeString(form[1]) != requestURI {
			t.Fatal("consent form no longer posts to the validated Mail MCP request")
		}
	}
	w := httptest.NewRecorder()
	s.Routes().ServeHTTP(w, httptest.NewRequest("GET", s.cfg.BaseURL+"/oauth/mcp/authorize?redirect_uri=https://evil.example", nil))
	if w.Code != 400 || !strings.Contains(w.Header().Get("Content-Security-Policy"), "form-action 'self'") {
		t.Fatal("consent policy applied to invalid authorization request")
	}
}

func TestNativeLoginPKCEAndConsent(t *testing.T) {
	s, _ := testServer(t)
	id := registerNative(t, s, "http://127.0.0.1/callback")
	callback := "http://127.0.0.1:54321/callback"
	q := nativeQuery(s, id, callback)
	for _, edit := range []func(url.Values){
		func(q url.Values) { q.Set("client_id", id+"x") },
		func(q url.Values) { q.Set("redirect_uri", "http://127.0.0.1:54321/other") },
		func(q url.Values) { q.Set("code_challenge_method", "plain") },
		func(q url.Values) { q.Del("code_challenge") },
	} {
		bad := cloneValues(q)
		edit(bad)
		w := httptest.NewRecorder()
		s.Routes().ServeHTTP(w, httptest.NewRequest("GET", s.cfg.BaseURL+"/oauth/mcp/authorize?"+bad.Encode(), nil))
		if w.Code != 400 || w.Header().Get("Location") != "" {
			t.Fatal("unsafe authorization request accepted or redirected")
		}
	}
	cookie, csrf := beginConsent(t, s, q)
	if approve(t, s, q, cookie, csrf, "wrong").Code != 401 {
		t.Fatal("owner login not enforced")
	}
	bad := cloneValues(q)
	bad.Set("client_id", registerNative(t, s, "http://127.0.0.1/callback"))
	if approve(t, s, bad, cookie, csrf, s.cfg.LoginToken).Code != 403 {
		t.Fatal("consent changed clients")
	}
	form := nativeCodeForm(s, id, callback, approveQuery(t, s, q))
	for _, edit := range []func(url.Values){
		func(f url.Values) { f.Set("code_verifier", strings.Repeat("b", 43)) },
		func(f url.Values) { f.Del("code_verifier") },
		func(f url.Values) { f.Set("redirect_uri", "http://127.0.0.1:12345/callback") },
		func(f url.Values) { f.Set("resource", "https://other.example/mcp") },
	} {
		bad := cloneValues(form)
		edit(bad)
		if publicRequest(s, "/oauth/mcp/token", bad).Code != 400 {
			t.Fatal("invalid public token exchange accepted")
		}
	}
	_ = tokens(t, publicRequest(s, "/oauth/mcp/token", form))
	if publicRequest(s, "/oauth/mcp/token", form).Code != 400 {
		t.Fatal("public code replay accepted")
	}
}

// Shared with the opt-in PostgreSQL test so the same client isolation contract
// is exercised against the real atomic UPDATE/DELETE predicates when enabled.
func exerciseClientIsolation(t *testing.T, s *Server) {
	t.Helper()
	callback := "http://127.0.0.1:54321/callback"
	ids := []string{registerNative(t, s, callback), registerNative(t, s, callback), s.cfg.ClientID}
	request := func(path, id string, form url.Values) *httptest.ResponseRecorder {
		f := cloneValues(form)
		f.Set("client_id", id)
		if id == s.cfg.ClientID {
			f.Set("client_secret", s.cfg.ClientSecret)
		}
		return publicRequest(s, path, f)
	}
	for _, id := range ids {
		q := nativeQuery(s, id, callback)
		if id == s.cfg.ClientID {
			q = authorizeQuery(s)
		}
		code := approveQuery(t, s, q)
		form := codeForm(s, code)
		form.Set("redirect_uri", q.Get("redirect_uri"))
		for _, other := range ids {
			if other != id && request("/oauth/mcp/token", other, form).Code != 400 {
				t.Fatal("code crossed client boundary")
			}
		}
		first := tokens(t, request("/oauth/mcp/token", id, form))
		refresh := url.Values{"grant_type": {"refresh_token"}, "resource": {s.resource()}, "refresh_token": {first["refresh_token"].(string)}}
		for _, other := range ids {
			if other == id {
				continue
			}
			if request("/oauth/mcp/token", other, refresh).Code != 400 {
				t.Fatal("refresh crossed client boundary")
			}
			for _, token := range []string{first["access_token"].(string), first["refresh_token"].(string)} {
				if request("/oauth/mcp/revoke", other, url.Values{"token": {token}}).Code != 200 {
					t.Fatal("revocation leaked other client's token existence")
				}
				if _, err := s.store.Validate(t.Context(), digest(first["access_token"].(string)), s.binding); err != nil {
					t.Fatal("other client revoked grant", err)
				}
			}
		}
		second := tokens(t, request("/oauth/mcp/token", id, refresh))
		if second["refresh_token"] == first["refresh_token"] || request("/oauth/mcp/token", id, refresh).Code != 400 {
			t.Fatal("refresh token not rotated or replayed")
		}
		if _, err := s.store.Validate(t.Context(), digest(first["access_token"].(string)), s.binding); err == nil {
			t.Fatal("old access token still valid")
		}
		if request("/oauth/mcp/revoke", id, url.Values{"token": {second["access_token"].(string)}}).Code != 200 {
			t.Fatal("own access revocation failed")
		}
		if _, err := s.store.Validate(t.Context(), digest(second["access_token"].(string)), s.binding); err == nil {
			t.Fatal("revoked access token still valid")
		}
		refresh.Set("refresh_token", second["refresh_token"].(string))
		if request("/oauth/mcp/token", id, refresh).Code != 400 {
			t.Fatal("revoked grant refreshed")
		}
	}
}

func TestNativeClientIsolation(t *testing.T) {
	s, _ := testServer(t)
	exerciseClientIsolation(t, s)
}

func TestClientAuthenticationRejectsMixedCredentials(t *testing.T) {
	s, _ := testServer(t)
	id := registerNative(t, s, "http://127.0.0.1/callback")
	for _, tt := range []struct {
		name        string
		editForm    func(url.Values)
		editRequest func(*http.Request)
		status      int
	}{
		{"public basic", nil, func(r *http.Request) { r.SetBasicAuth(id, "") }, 401},
		{"public empty secret", func(f url.Values) { f.Set("client_secret", "") }, nil, 401},
		{"public secret", func(f url.Values) { f.Set("client_secret", s.cfg.ClientSecret) }, nil, 401},
		{"web missing secret", func(f url.Values) { f.Set("client_id", s.cfg.ClientID) }, nil, 401},
		{"basic and different form ID", nil, func(r *http.Request) { r.SetBasicAuth(s.cfg.ClientID, s.cfg.ClientSecret) }, 400},
		{"basic and form secret", func(f url.Values) { f.Set("client_secret", "") }, func(r *http.Request) { r.SetBasicAuth(id, "") }, 400},
		{"invalid authorization", nil, func(r *http.Request) { r.Header.Set("Authorization", "Basic invalid") }, 401},
		{"unexpected bearer", nil, func(r *http.Request) { r.Header.Set("Authorization", "Bearer invalid") }, 401},
		{"duplicate authorization", nil, func(r *http.Request) { r.SetBasicAuth(id, ""); r.Header.Add("Authorization", "Basic invalid") }, 400},
		{"duplicate client ID", func(f url.Values) { f.Add("client_id", s.cfg.ClientID) }, nil, 400},
		{"tampered registration", func(f url.Values) { f.Set("client_id", id+"x") }, nil, 401},
	} {
		t.Run(tt.name, func(t *testing.T) {
			form := url.Values{"client_id": {id}, "grant_type": {"refresh_token"}, "resource": {s.resource()}, "refresh_token": {"unused"}}
			if tt.editForm != nil {
				tt.editForm(form)
			}
			r := httptest.NewRequest("POST", s.cfg.BaseURL+"/oauth/mcp/token", strings.NewReader(form.Encode()))
			r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			if tt.editRequest != nil {
				tt.editRequest(r)
			}
			w := httptest.NewRecorder()
			s.Routes().ServeHTTP(w, r)
			if w.Code != tt.status || !strings.Contains(w.Body.String(), "invalid_client") && !strings.Contains(w.Body.String(), "invalid_request") {
				t.Fatalf("invalid credentials accepted: %d %s", w.Code, w.Body.String())
			}
		})
	}
}

func TestNativeClientWithoutRefreshGrant(t *testing.T) {
	s, _ := testServer(t)
	w := registrationRequest(s, `{"redirect_uris":["http://127.0.0.1/callback"],"grant_types":["authorization_code"],"token_endpoint_auth_method":"none"}`)
	if w.Code != http.StatusCreated {
		t.Fatal(w.Body.String())
	}
	var registration struct {
		ClientID string `json:"client_id"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &registration); err != nil {
		t.Fatal(err)
	}
	q := nativeQuery(s, registration.ClientID, "http://127.0.0.1:54321/callback")
	w = httptest.NewRecorder()
	s.Routes().ServeHTTP(w, httptest.NewRequest("GET", s.cfg.BaseURL+"/oauth/mcp/authorize?"+q.Encode(), nil))
	if w.Code != 400 || !strings.Contains(w.Body.String(), "invalid_scope") {
		t.Fatal("offline access accepted without refresh grant")
	}
	q.Del("scope")
	form := nativeCodeForm(s, registration.ClientID, q.Get("redirect_uri"), approveQuery(t, s, q))
	result := tokens(t, publicRequest(s, "/oauth/mcp/token", form))
	if _, ok := result["refresh_token"]; ok || hasScope(result["scope"].(string), "offline_access") {
		t.Fatal("code-only client received refresh grant")
	}
	form.Set("grant_type", "refresh_token")
	if w := publicRequest(s, "/oauth/mcp/token", form); w.Code != 400 || !strings.Contains(w.Body.String(), "unauthorized_client") {
		t.Fatal("code-only client allowed to refresh")
	}
}
