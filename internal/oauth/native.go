package oauth

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"mime"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"
)

const (
	nativeClientPrefix = "mail-mcp-native-v1."
	maxClientID        = 4096
)

type oauthClient struct {
	name, hash   string
	redirectURIs []string
	public       bool
	refresh      bool
}

type nativeMetadata struct {
	ClientName    string   `json:"client_name"`
	RedirectURIs  []string `json:"redirect_uris"`
	GrantTypes    []string `json:"grant_types"`
	ResponseTypes []string `json:"response_types"`
	AuthMethod    string   `json:"token_endpoint_auth_method"`
}

type nativeRegistration struct {
	nativeMetadata
	Nonce string `json:"nonce"`
}

// register implements RFC 7591 for public native clients. Registration grants
// no data access: the owner must still approve each authorization with PKCE.
// Signed client IDs keep unauthenticated registrations out of the database
// (RFC 7591 A.5.2). No client-provided URLs are fetched by the server.
func (s *Server) register(w http.ResponseWriter, r *http.Request) {
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		oauthError(w, 400, "invalid_client_metadata")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 8192)
	decoder := json.NewDecoder(r.Body)
	var metadata nativeMetadata
	if err := decoder.Decode(&metadata); err != nil {
		oauthError(w, 400, "invalid_client_metadata")
		return
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		oauthError(w, 400, "invalid_client_metadata")
		return
	}
	if len(metadata.RedirectURIs) == 0 || len(metadata.RedirectURIs) > 4 {
		oauthError(w, 400, "invalid_redirect_uri")
		return
	}
	for _, raw := range metadata.RedirectURIs {
		if _, ok := loopbackRedirect(raw); !ok {
			oauthError(w, 400, "invalid_redirect_uri")
			return
		}
	}
	if len(metadata.ClientName) > 128 || (metadata.AuthMethod != "" && metadata.AuthMethod != "none") {
		oauthError(w, 400, "invalid_client_metadata")
		return
	}
	if metadata.ClientName == "" {
		metadata.ClientName = "MCP desktop client"
	}
	metadata.AuthMethod = "none"
	if len(metadata.GrantTypes) == 0 {
		metadata.GrantTypes = []string{"authorization_code", "refresh_token"}
	}
	if len(metadata.ResponseTypes) == 0 {
		metadata.ResponseTypes = []string{"code"}
	}
	if !slices.Contains(metadata.GrantTypes, "authorization_code") || len(metadata.GrantTypes) > 2 || len(metadata.ResponseTypes) != 1 || metadata.ResponseTypes[0] != "code" {
		oauthError(w, 400, "invalid_client_metadata")
		return
	}
	for _, kind := range metadata.GrantTypes {
		if kind != "authorization_code" && kind != "refresh_token" {
			oauthError(w, 400, "invalid_client_metadata")
			return
		}
	}
	registration := nativeRegistration{nativeMetadata: metadata, Nonce: randomToken()}
	raw, _ := json.Marshal(registration)
	payload := base64.RawURLEncoding.EncodeToString(raw)
	id := nativeClientPrefix + payload + "." + s.signClient(payload)
	if len(id) > maxClientID {
		oauthError(w, 400, "invalid_client_metadata")
		return
	}
	writeJSON(w, http.StatusCreated, struct {
		nativeMetadata
		ClientID string `json:"client_id"`
		IssuedAt int64  `json:"client_id_issued_at"`
	}{metadata, id, time.Now().Unix()})
}

func (s *Server) signClient(payload string) string {
	mac := hmac.New(sha256.New, []byte(s.cfg.LoginToken))
	_, _ = mac.Write([]byte("mail-mcp-native-client-v1\x00" + s.binding + "\x00" + payload))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func (s *Server) resolveClient(id string) (oauthClient, bool) {
	if id == s.cfg.ClientID {
		// Empty hash preserves grants issued before native clients were supported.
		return oauthClient{name: id, redirectURIs: s.cfg.RedirectURIs, refresh: true}, true
	}
	if len(id) > maxClientID || !strings.HasPrefix(id, nativeClientPrefix) {
		return oauthClient{}, false
	}
	payload, signature, ok := strings.Cut(strings.TrimPrefix(id, nativeClientPrefix), ".")
	if !ok || !hmac.Equal([]byte(signature), []byte(s.signClient(payload))) {
		return oauthClient{}, false
	}
	raw, err := base64.RawURLEncoding.DecodeString(payload)
	if err != nil {
		return oauthClient{}, false
	}
	var registration nativeRegistration
	if json.Unmarshal(raw, &registration) != nil || registration.Nonce == "" || registration.AuthMethod != "none" {
		return oauthClient{}, false
	}
	return oauthClient{name: registration.ClientName, hash: digest(id), redirectURIs: registration.RedirectURIs,
		public: true, refresh: slices.Contains(registration.GrantTypes, "refresh_token")}, true
}

func (c oauthClient) allowsRedirect(raw string) bool {
	if !c.public {
		return slices.Contains(c.redirectURIs, raw)
	}
	requested, ok := loopbackRedirect(raw)
	if !ok {
		return false
	}
	for _, registered := range c.redirectURIs {
		candidate, valid := loopbackRedirect(registered)
		// RFC 8252 allows the native app to select a new listener port. Only
		// the port varies; scheme, IP literal, path and query must match exactly.
		if valid && requested == candidate {
			return true
		}
	}
	return false
}

func loopbackRedirect(raw string) (string, bool) {
	if len(raw) > 512 {
		return "", false
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "http" || u.User != nil || u.Opaque != "" || strings.Contains(raw, "#") || u.Path == "" {
		return "", false
	}
	host := u.Hostname()
	if host != "127.0.0.1" && host != "::1" {
		return "", false
	}
	if port := u.Port(); port != "" {
		number, err := strconv.Atoi(port)
		if err != nil || number < 1 || number > 65535 {
			return "", false
		}
	} else if strings.HasSuffix(u.Host, ":") {
		return "", false
	}
	canonicalHost := host
	if host == "::1" {
		canonicalHost = "[::1]"
	}
	authority := canonicalHost
	if u.Port() != "" {
		authority += ":" + u.Port()
	}
	if u.Host != authority {
		return "", false
	}
	// Remove precisely the authority port, retaining the original URI bytes.
	return "http://" + canonicalHost + strings.TrimPrefix(raw, "http://"+authority), true
}
