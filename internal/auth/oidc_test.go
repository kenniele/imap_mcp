package auth

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/golang-jwt/jwt/v5"
)

func TestOIDCAccessTokenValidation(t *testing.T) {
	key, e := rsa.GenerateKey(rand.Reader, 2048)
	if e != nil {
		t.Fatal(e)
	}
	issuer := ""
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/.well-known/openid-configuration":
			_ = json.NewEncoder(w).Encode(map[string]any{"issuer": issuer, "jwks_uri": issuer + "/jwks", "authorization_endpoint": issuer + "/authorize", "token_endpoint": issuer + "/token", "id_token_signing_alg_values_supported": []string{"RS256"}})
		case "/jwks":
			_ = json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{Key: &key.PublicKey, KeyID: "test", Algorithm: "RS256", Use: "sig"}}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	issuer = server.URL
	security, e := NewOIDC(context.Background(), issuer, "https://mail.example.com/mcp", "owner")
	if e != nil {
		t.Fatal(e)
	}
	handler := security.Protect(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) }))
	valid := func() jwt.MapClaims {
		return jwt.MapClaims{"iss": issuer, "aud": "https://mail.example.com/mcp", "sub": "owner", "scope": "mail.read", "exp": time.Now().Add(time.Hour).Unix()}
	}
	for _, tc := range []struct {
		name, claim string
		value       any
		code        int
	}{{"valid", "", nil, 204}, {"issuer", "iss", "https://other.test", 401}, {"audience", "aud", "other-client", 401}, {"other owner", "sub", "intruder", 401}, {"scope", "scope", "profile", 401}, {"expired", "exp", time.Now().Add(-time.Hour).Unix(), 401}, {"not active", "nbf", time.Now().Add(time.Hour).Unix(), 401}} {
		t.Run(tc.name, func(t *testing.T) {
			claims := valid()
			if tc.claim != "" {
				claims[tc.claim] = tc.value
			}
			token := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
			token.Header["kid"] = "test"
			raw, e := token.SignedString(key)
			if e != nil {
				t.Fatal(e)
			}
			r := httptest.NewRequest("POST", "/mcp", nil)
			r.Header.Set("Authorization", "Bearer "+raw)
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, r)
			if w.Code != tc.code {
				t.Fatalf("status=%d want=%d", w.Code, tc.code)
			}
			if tc.code == 401 && w.Header().Get("WWW-Authenticate") == "" {
				t.Fatal("missing OAuth challenge")
			}
		})
	}
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, valid())
	raw, e := token.SignedString([]byte("test"))
	if e != nil {
		t.Fatal(e)
	}
	r := httptest.NewRequest("POST", "/mcp", nil)
	r.Header.Set("Authorization", "Bearer "+raw)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	if w.Code != 401 {
		t.Fatal("HS256 algorithm confusion")
	}
	w = httptest.NewRecorder()
	security.Metadata(w, httptest.NewRequest("GET", "/.well-known/oauth-protected-resource", nil))
	if w.Code != 200 {
		t.Fatal("OAuth metadata missing")
	}
}
