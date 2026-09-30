package config

import (
	"strings"
	"testing"
)

func TestConfig(t *testing.T) {
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
