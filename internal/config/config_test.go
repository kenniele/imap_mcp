package config

import (
	"strings"
	"testing"
)

func TestConfig(t *testing.T) {
	t.Setenv("AUTH_MODE", "bearer")
	t.Setenv("DATABASE_URL", "postgres://localhost/mailmcp")
	t.Setenv("MCP_SECRET_KEY", strings.Repeat("k", 32))
	t.Setenv("MCP_AUTH_TOKEN", strings.Repeat("t", 32))
	t.Setenv("LOG_LEVEL", "info")
	if _, e := Load(true); e != nil {
		t.Fatal(e)
	}
	t.Setenv("MCP_AUTH_TOKEN", "")
	if _, e := Load(true); e == nil {
		t.Fatal("public MCP enabled")
	}
	if _, e := Load(false); e != nil {
		t.Fatal("CLI unnecessarily requires MCP token", e)
	}
	t.Setenv("MCP_SECRET_KEY", "short")
	if _, e := Load(false); e == nil {
		t.Fatal("short master key accepted")
	}
}

func TestBuiltinOAuthConfig(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://localhost/mailmcp")
	t.Setenv("MCP_SECRET_KEY", strings.Repeat("k", 32))
	t.Setenv("MCP_AUTH_TOKEN", strings.Repeat("t", 32))
	t.Setenv("AUTH_MODE", "oauth")
	t.Setenv("MCP_PUBLIC_URL", "https://imap-mcp.example/mcp")
	t.Setenv("MCP_OAUTH_CLIENT_ID", "mail-mcp-chatgpt")
	t.Setenv("MCP_OAUTH_CLIENT_SECRET", strings.Repeat("c", 32))
	t.Setenv("MCP_OAUTH_LOGIN_TOKEN", strings.Repeat("l", 32))
	t.Setenv("MCP_OAUTH_REDIRECT_URIS", "https://chatgpt.com/connector_platform_oauth_redirect")
	if _, err := Load(true); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct{ key, value string }{
		{"MCP_PUBLIC_URL", "http://imap-mcp.example/mcp"},
		{"MCP_PUBLIC_URL", "https://imap-mcp.example/"},
		{"MCP_OAUTH_CLIENT_SECRET", strings.Repeat("l", 32)},
		{"MCP_OAUTH_LOGIN_TOKEN", strings.Repeat("t", 32)},
		{"MCP_OAUTH_LOGIN_TOKEN", "short"},
		{"MCP_OAUTH_REDIRECT_URIS", ""},
		{"MCP_OAUTH_REDIRECT_URIS", "http://chatgpt.com/callback"},
		{"MCP_OAUTH_REDIRECT_URIS", "https://chatgpt.com/callback#fragment"},
	} {
		t.Run(test.key+test.value, func(t *testing.T) {
			t.Setenv(test.key, test.value)
			if _, err := Load(true); err == nil {
				t.Fatal("invalid OAuth config accepted")
			}
		})
	}
	t.Setenv("MCP_OAUTH_LOGIN_TOKEN", "")
	if _, err := Load(false); err != nil {
		t.Fatal("account CLI requires OAuth login", err)
	}
}
