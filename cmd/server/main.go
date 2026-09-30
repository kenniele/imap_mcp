package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"mail-mcp/internal/app"
	"mail-mcp/internal/auth"
	"mail-mcp/internal/cli"
	"mail-mcp/internal/config"
	"mail-mcp/internal/imap"
	mailmcp "mail-mcp/internal/mcp"
	"mail-mcp/internal/oauth"
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
	checkConfig := len(args) == 1 && args[0] == "check-config"
	if !serve && !checkConfig && (len(args) == 0 || (args[0] != "account" && args[0] != "migrate")) {
		return errors.New("usage: mail-mcp [serve|check-config|migrate|account add|list|test|disable|delete]")
	}
	cfg, err := config.Load(serve || checkConfig)
	if err != nil {
		return err
	}
	if checkConfig {
		fmt.Fprintln(os.Stdout, "Server configuration valid")
		return nil
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
		_, err := store.DB.Exec(migrationCtx, migrations.AccountsSQL+"\n"+migrations.OAuthSQL)
		if err != nil {
			return errors.New("database migration failed")
		}
		fmt.Fprintln(os.Stdout, "Account and OAuth schemas ready")
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
	var security mailmcp.Security
	if cfg.AuthMode == "oidc" {
		discoveryCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		security, err = auth.NewOIDC(discoveryCtx, cfg.OIDCIssuer, cfg.PublicURL, cfg.OIDCSubject)
		cancel()
		if err != nil {
			return err
		}
	}
	if cfg.AuthMode == "oauth" {
		security, err = oauth.NewServer(oauth.Config{
			BaseURL:  strings.TrimSuffix(cfg.PublicURL, "/mcp"),
			ClientID: cfg.OAuthClientID, ClientSecret: cfg.OAuthClientSecret,
			LoginToken: cfg.OAuthLoginToken, RedirectURIs: cfg.OAuthRedirectURIs,
		}, store.DB, logger)
		if err != nil {
			return err
		}
		checkCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		_, err = store.DB.Exec(checkCtx, "SELECT 1 FROM mcp_oauth_grants LIMIT 0")
		cancel()
		if err != nil {
			return errors.New("OAuth schema unavailable; run mail-mcp migrate")
		}
	}
	service := app.New(store, provider, cipher)
	server := &http.Server{Addr: cfg.HTTPAddr, Handler: mailmcp.Handler(service, store, cfg.AuthToken, metrics, logger, security), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 20 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 16 * 1024}
	// No global WriteTimeout: Streamable HTTP GET streams can outlive an individual tool call.
	result := make(chan error, 1)
	go func() { result <- server.ListenAndServe() }()
	logger.Info("mail-mcp started", "address", cfg.HTTPAddr, "auth_mode", cfg.AuthMode)
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
