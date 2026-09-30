package auth

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestBearer(t *testing.T) {
	token := strings.Repeat("a", 32)
	h := Bearer(token, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) }))
	for _, tc := range []struct {
		header, origin string
		code           int
	}{{"", "", 401}, {"Bearer wrong", "", 401}, {"Bearer " + token, "", 204}, {"Bearer " + token, "https://evil.test", 403}} {
		r := httptest.NewRequest("POST", "/mcp", nil)
		r.Header.Set("Authorization", tc.header)
		r.Header.Set("Origin", tc.origin)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != tc.code {
			t.Fatalf("code=%d want=%d", w.Code, tc.code)
		}
	}
}
