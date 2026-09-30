package auth

import (
	"crypto/sha256"
	"crypto/subtle"
	"net/http"
	"strings"
)

func Bearer(token string, next http.Handler) http.Handler {
	expected := sha256.Sum256([]byte(token))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		header := r.Header.Get("Authorization")
		value, ok := strings.CutPrefix(header, "Bearer ")
		actual := sha256.Sum256([]byte(value))
		if token == "" || !ok || subtle.ConstantTimeCompare(expected[:], actual[:]) != 1 {
			w.Header().Set("WWW-Authenticate", `Bearer realm="mail-mcp"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		// Browser-origin requests are rejected: this server is a private machine-to-machine endpoint.
		if r.Header.Get("Origin") != "" {
			http.Error(w, "browser origin not allowed", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}
