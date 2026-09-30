package auth

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
)

// OIDC validates JWT access tokens issued by an external OAuth 2.1 authorization server.
// One server belongs to one owner; the subject allowlist protects every configured mailbox.
type OIDC struct {
	verifier                  *oidc.IDTokenVerifier
	issuer, resource, subject string
}

func NewOIDC(ctx context.Context, issuer, resource, subject string) (*OIDC, error) {
	client := &http.Client{Timeout: 5 * time.Second}
	provider, err := oidc.NewProvider(oidc.ClientContext(ctx, client), issuer)
	if err != nil {
		return nil, errors.New("OIDC discovery failed")
	}
	verifier := provider.VerifierContext(oidc.ClientContext(context.WithoutCancel(ctx), client), &oidc.Config{ClientID: resource, SupportedSigningAlgs: []string{"RS256", "ES256", "EdDSA"}})
	return &OIDC{verifier: verifier, issuer: issuer, resource: resource, subject: subject}, nil
}
func (o *OIDC) Protect(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok || len(raw) > 16*1024 {
			o.challenge(w)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		token, err := o.verifier.Verify(ctx, raw)
		if err != nil || token.Subject != o.subject {
			o.challenge(w)
			return
		}
		var claims struct {
			Scope     string `json:"scope"`
			NotBefore int64  `json:"nbf"`
		}
		if token.Claims(&claims) != nil || claims.NotBefore > time.Now().Unix() {
			o.challenge(w)
			return
		}
		allowed := false
		for _, scope := range strings.Fields(claims.Scope) {
			if scope == "mail.read" {
				allowed = true
			}
		}
		if !allowed {
			o.challenge(w)
			return
		}
		if r.Header.Get("Origin") != "" {
			http.Error(w, "browser origin not allowed", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}
func (o *OIDC) challenge(w http.ResponseWriter) {
	w.Header().Set("WWW-Authenticate", `Bearer resource_metadata="`+o.metadataURL()+`", scope="mail.read"`)
	http.Error(w, "unauthorized", http.StatusUnauthorized)
}
func (o *OIDC) metadataURL() string {
	return strings.TrimSuffix(o.resource, "/mcp") + "/.well-known/oauth-protected-resource/mcp"
}
func (o *OIDC) Metadata(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"resource": o.resource, "authorization_servers": []string{o.issuer}, "scopes_supported": []string{"mail.read"}, "bearer_methods_supported": []string{"header"}})
}
