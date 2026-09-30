package config

import (
	"errors"
	"log/slog"
	"net/url"
	"os"
	"strings"

	"mail-mcp/internal/oauth"
	"mail-mcp/internal/secrets"
)

type Config struct {
	HTTPAddr, DatabaseURL, AuthToken                  string
	AuthMode, OIDCIssuer, PublicURL, OIDCSubject      string
	OAuthClientID, OAuthClientSecret, OAuthLoginToken string
	OAuthRedirectURIs                                 []string
	Key                                               []byte
	LogLevel                                          slog.Level
}

func Load(server bool) (Config, error) {
	c := Config{HTTPAddr: os.Getenv("HTTP_ADDR"), DatabaseURL: os.Getenv("DATABASE_URL"), AuthToken: os.Getenv("MCP_AUTH_TOKEN")}
	if c.HTTPAddr == "" {
		c.HTTPAddr = ":8080"
	}
	u, err := url.Parse(c.DatabaseURL)
	if err != nil || u.Host == "" || (u.Scheme != "postgres" && u.Scheme != "postgresql") {
		return c, errors.New("DATABASE_URL must be a PostgreSQL URL")
	}
	c.Key, err = secrets.ParseKey(os.Getenv("MCP_SECRET_KEY"))
	if err != nil {
		return c, err
	}
	if server && (len(c.AuthToken) < 32 || strings.ContainsAny(c.AuthToken, " \t\r\n")) {
		return c, errors.New("MCP_AUTH_TOKEN must be at least 32 characters without whitespace")
	}
	if v := os.Getenv("LOG_LEVEL"); v != "" {
		if err := c.LogLevel.UnmarshalText([]byte(v)); err != nil {
			return c, errors.New("invalid LOG_LEVEL")
		}
	}
	c.AuthMode = os.Getenv("AUTH_MODE")
	if c.AuthMode == "" {
		c.AuthMode = "bearer"
	}
	if c.AuthMode != "bearer" && c.AuthMode != "oidc" && c.AuthMode != "oauth" {
		return c, errors.New("AUTH_MODE must be bearer, oauth or oidc")
	}
	if c.AuthMode == "oauth" && server {
		c.PublicURL = os.Getenv("MCP_PUBLIC_URL")
		u, err := url.Parse(c.PublicURL)
		if err != nil || u.Scheme != "https" || u.Host == "" || u.Path != "/mcp" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
			return c, errors.New("MCP_PUBLIC_URL must be an HTTPS URL with the exact /mcp path")
		}
		c.OAuthClientID = os.Getenv("MCP_OAUTH_CLIENT_ID")
		c.OAuthClientSecret = os.Getenv("MCP_OAUTH_CLIENT_SECRET")
		c.OAuthLoginToken = os.Getenv("MCP_OAUTH_LOGIN_TOKEN")
		for _, uri := range strings.Split(os.Getenv("MCP_OAUTH_REDIRECT_URIS"), ",") {
			if uri = strings.TrimSpace(uri); uri != "" {
				c.OAuthRedirectURIs = append(c.OAuthRedirectURIs, uri)
			}
		}
		if c.OAuthClientID == "" || len(c.OAuthClientSecret) < 32 || len(c.OAuthLoginToken) < 32 || c.OAuthClientSecret == c.OAuthLoginToken {
			return c, errors.New("OAuth requires MCP_OAUTH_CLIENT_ID and distinct client/login secrets of at least 32 characters")
		}
		if c.OAuthClientSecret == c.AuthToken || c.OAuthLoginToken == c.AuthToken {
			return c, errors.New("OAuth secrets must differ from MCP_AUTH_TOKEN")
		}
		if len(c.OAuthRedirectURIs) == 0 {
			return c, errors.New("MCP_OAUTH_REDIRECT_URIS must contain at least one exact HTTPS callback")
		}
		if _, err := oauth.ValidateConfig(oauth.Config{
			BaseURL:  strings.TrimSuffix(c.PublicURL, "/mcp"),
			ClientID: c.OAuthClientID, ClientSecret: c.OAuthClientSecret,
			LoginToken: c.OAuthLoginToken, RedirectURIs: c.OAuthRedirectURIs,
		}); err != nil {
			return c, err
		}
	}
	if c.AuthMode == "oidc" && server {
		c.OIDCIssuer = os.Getenv("OIDC_ISSUER")
		c.PublicURL = os.Getenv("MCP_PUBLIC_URL")
		c.OIDCSubject = os.Getenv("OIDC_ALLOWED_SUBJECT")
		for _, raw := range []string{c.OIDCIssuer, c.PublicURL} {
			v, e := url.Parse(raw)
			if e != nil || v.Scheme != "https" || v.Host == "" || v.User != nil || v.RawQuery != "" || v.Fragment != "" {
				return c, errors.New("OIDC_ISSUER and MCP_PUBLIC_URL must be public HTTPS URLs")
			}
		}
		resource, _ := url.Parse(c.PublicURL)
		if resource.Path != "/mcp" || c.OIDCSubject == "" {
			return c, errors.New("OIDC requires MCP_PUBLIC_URL with the exact /mcp path and OIDC_ALLOWED_SUBJECT")
		}
	}
	return c, nil
}
