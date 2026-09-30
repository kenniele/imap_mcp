package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"mail-mcp/internal/app"
	"mail-mcp/internal/auth"
	"mail-mcp/internal/cli"
	"mail-mcp/internal/config"
	"mail-mcp/internal/imap"
	mailmcp "mail-mcp/internal/mcp"
	"mail-mcp/internal/observability"
	"mail-mcp/internal/secrets"
	"mail-mcp/internal/storage/postgres"
	"mail-mcp/migrations"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
func run() error {
	args := os.Args[1:]
	serve := len(args) == 0 || (len(args) == 1 && args[0] == "serve")
	if !serve && (len(args) == 0 || (args[0] != "account" && args[0] != "migrate")) {
		return errors.New("usage: mail-mcp [serve|migrate|account add|list|test|disable|delete]")
	}
	cfg, err := config.Load(serve)
	if err != nil {
		return err
	}
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: cfg.LogLevel}))
	slog.SetDefault(logger)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	startup, cancel := context.WithTimeout(ctx, 10*time.Second)
	store, err := postgres.Open(startup, cfg.DatabaseURL)
	cancel()
	if err != nil {
		return err
	}
	defer store.Close()
	if !serve && args[0] == "migrate" {
		if len(args) != 1 {
			return errors.New("usage: mail-mcp migrate")
		}
		migrationCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		_, err := store.DB.Exec(migrationCtx, migrations.AccountsSQL)
		if err != nil {
			return errors.New("account migration failed")
		}
		fmt.Fprintln(os.Stdout, "Account schema ready")
		return nil
	}
	cipher, err := secrets.New(cfg.Key)
	if err != nil {
		return err
	}
	pool := imap.NewPool(cipher, nil)
	defer pool.Close()
	metrics := observability.NewMetrics()
	provider := &imap.Provider{Pool: pool, Metrics: metrics}
	if !serve {
		return cli.Run(ctx, args[1:], store, provider, cipher)
	}
	startup, cancel = context.WithTimeout(ctx, 5*time.Second)
	_, err = store.List(startup)
	cancel()
	if err != nil {
		return errors.New("account schema unavailable; run mail-mcp migrate")
	}
	var security *auth.OIDC
	if cfg.AuthMode == "oidc" {
		discoveryCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		security, err = auth.NewOIDC(discoveryCtx, cfg.OIDCIssuer, cfg.PublicURL, cfg.OIDCSubject)
		cancel()
		if err != nil {
			return err
		}
	}
	service := app.New(store, provider, cipher)
	server := &http.Server{Addr: cfg.HTTPAddr, Handler: mailmcp.Handler(service, store, cfg.AuthToken, metrics, logger, security), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 20 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 16 * 1024}
	// No global WriteTimeout: Streamable HTTP GET streams can outlive an individual tool call.
	result := make(chan error, 1)
	go func() { result <- server.ListenAndServe() }()
	logger.Info("mail-mcp started", "address", cfg.HTTPAddr)
	select {
	case err := <-result:
		if !errors.Is(err, http.ErrServerClosed) {
			return errors.New("HTTP server failed to listen")
		}
		return nil
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdown); err != nil {
			_ = server.Close()
			return errors.New("HTTP graceful shutdown timed out")
		}
		return nil
	}
}
