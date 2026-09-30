package mcp

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"mail-mcp/internal/app"
	"mail-mcp/internal/auth"
	"mail-mcp/internal/mcp/tools"
	"mail-mcp/internal/observability"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

type Readiness interface{ Ping(context.Context) error }
type Security interface {
	Protect(http.Handler) http.Handler
}

func Handler(service *app.Service, ready Readiness, token string, metrics *observability.Metrics, logger *slog.Logger, security Security) http.Handler {
	server := sdk.NewServer(&sdk.Implementation{Name: "multi-account-mail", Version: "0.2.0"}, nil)
	tools.Register(server, service, metrics, logger)
	transport := sdk.NewStreamableHTTPHandler(func(*http.Request) *sdk.Server { return server }, &sdk.StreamableHTTPOptions{JSONResponse: true, SessionTimeout: 10 * time.Minute})
	mux := http.NewServeMux()
	protect := func(next http.Handler) http.Handler { return auth.Bearer(token, next) }
	if security != nil {
		protect = security.Protect
		if metadata, ok := security.(interface {
			Metadata(http.ResponseWriter, *http.Request)
		}); ok {
			mux.HandleFunc("GET /.well-known/oauth-protected-resource", metadata.Metadata)
			mux.HandleFunc("GET /.well-known/oauth-protected-resource/mcp", metadata.Metadata)
		}
		if oauth, ok := security.(interface{ Routes() http.Handler }); ok {
			routes := oauth.Routes()
			mux.Handle("/.well-known/oauth-authorization-server", routes)
			mux.Handle("/oauth/mcp/", routes)
		}
	}
	mux.Handle("/mcp", protect(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, 64*1024)
		transport.ServeHTTP(w, r)
	})))
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})
	mux.HandleFunc("GET /ready", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		w.Header().Set("Content-Type", "application/json")
		if ready.Ping(ctx) != nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"status":"unavailable"}`))
			return
		}
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})
	mux.Handle("GET /metrics", auth.Bearer(token, metrics.Handler()))
	return observability.WithRequestID(mux)
}
