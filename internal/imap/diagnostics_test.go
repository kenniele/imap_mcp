package imap

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strings"
	"syscall"
	"testing"
	"time"

	"mail-mcp/internal/domain"
	"mail-mcp/internal/observability"
	"mail-mcp/internal/testutil"

	imaplib "github.com/emersion/go-imap/v2"
)

func TestDiagnosticClassification(t *testing.T) {
	for _, tc := range []struct {
		name, kind, status, code string
		err                      error
	}{
		{name: "timeout", err: context.DeadlineExceeded, kind: "timeout"},
		{name: "cancel", err: context.Canceled, kind: "canceled"},
		{name: "closed", err: io.EOF, kind: "connection_closed"},
		{name: "truncated", err: io.ErrUnexpectedEOF, kind: "unexpected_eof"},
		{name: "reset", err: &net.OpError{Op: "read", Err: syscall.ECONNRESET}, kind: "connection_reset"},
		{name: "dns", err: &net.DNSError{Err: "secret", Name: "private.invalid"}, kind: "dns"},
		{name: "denied", err: &imaplib.Error{Type: imaplib.StatusResponseTypeNo, Code: imaplib.ResponseCodeNoPerm, Text: "credential-secret"}, kind: "server_rejected", status: "NO", code: "NOPERM"},
		{name: "unknown_code", err: &imaplib.Error{Type: imaplib.StatusResponseTypeBad, Code: "token-secret", Text: "private mail text"}, kind: "server_rejected", status: "BAD", code: "OTHER"},
		{name: "unknown_status", err: &imaplib.Error{Type: "private-status", Text: "secret"}, kind: "server_rejected", status: "OTHER"},
		{name: "unknown", err: errors.New("private response"), kind: "other"},
		{name: "date", err: &time.ParseError{Layout: "private-layout", Value: "private-date"}, kind: "date_parse"},
		{name: "flattened_envelope", err: errors.New("in response-data: in envelope: private address"), kind: "response_parse"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			wrapped := &operationError{stage: "uid_search", message: "imap operation failed", cause: fmt.Errorf("private context: %w", tc.err)}
			kind, status, code := classifyError(wrapped)
			if kind != tc.kind || status != tc.status || code != tc.code {
				t.Fatalf("classification = %q, %q, %q", kind, status, code)
			}
			if wrapped.Error() != "imap operation failed" || !errors.Is(wrapped, tc.err) {
				t.Fatal("safe error does not retain its cause")
			}
		})
	}
}

func TestFlattenedDecoderDiagnosticsAreRedacted(t *testing.T) {
	output := captureDiagnosticLogs(t, slog.LevelDebug)
	_, err := step(t.Context(), "uid_fetch_metadata", func() (int, error) {
		return 0, errors.New("in response-data: in body-type-mpart: in body-fld-param: private-address password-secret private-body")
	})
	if err == nil || parserArea(err) != "body_structure" {
		t.Fatal("flattened parser cause was not identified")
	}
	rows := logRows(t, output)
	last := rows[len(rows)-1]
	if last["error_kind"] != "response_parse" || last["parser_area"] != "body_structure" || last["error_types"] == nil {
		t.Fatal("missing safe parser details")
	}
	for _, private := range []string{"private-address", "password-secret", "private-body", "in body-fld-param"} {
		if strings.Contains(output.String(), private) {
			t.Fatal("raw parser response leaked")
		}
	}
}

func captureDiagnosticLogs(t *testing.T, level slog.Level) *bytes.Buffer {
	t.Helper()
	var output bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&output, &slog.HandlerOptions{Level: level})))
	t.Cleanup(func() { slog.SetDefault(previous) })
	return &output
}

func logRows(t *testing.T, output *bytes.Buffer) []map[string]any {
	t.Helper()
	var rows []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(output.String()), "\n") {
		var row map[string]any
		if err := json.Unmarshal([]byte(line), &row); err != nil {
			t.Fatal(err)
		}
		rows = append(rows, row)
	}
	return rows
}

func TestNestedDiagnosticsRedactProviderData(t *testing.T) {
	output := captureDiagnosticLogs(t, slog.LevelDebug)
	ctx := observability.ContextWithRequestID(t.Context(), "nested-request")
	ctx = context.WithValue(ctx, traceKey{}, traceContext{account: "personal", operation: "search"})
	ctx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	_, err := step(ctx, "pool_acquire", func() (int, error) {
		return step(ctx, "login", func() (int, error) {
			return 0, &imaplib.Error{Type: imaplib.StatusResponseTypeNo, Code: "SECRET_CODE", Text: "password-secret token-secret private-message"}
		})
	})
	failure, ok := errors.AsType[*operationError](err)
	if !ok || failure.stage != "login" || err.Error() != "imap authentication failed" {
		t.Fatal("outer stage hid the failing login stage")
	}
	for _, secret := range []string{"SECRET_CODE", "password-secret", "token-secret", "private-message"} {
		if strings.Contains(output.String(), secret) {
			t.Fatalf("diagnostic log leaked %s", secret)
		}
	}
	rows := logRows(t, output)
	if len(rows) != 4 || rows[3]["error_stage"] != "login" || rows[3]["imap_status"] != "NO" || rows[3]["imap_code"] != "OTHER" {
		t.Fatal("missing nested stage diagnostics")
	}
}

func TestIMAPDiagnosticStagesAndCorrelation(t *testing.T) {
	f := testutil.NewIMAP(t)
	a, _ := f.Account(t, "personal", []string{testutil.Plain})
	pool := NewPool(f.Cipher, f.Roots)
	t.Cleanup(pool.Close)
	p := &Provider{Pool: pool}
	output := captureDiagnosticLogs(t, slog.LevelDebug)
	ctx := observability.ContextWithRequestID(t.Context(), "diagnostic-request")
	ctx = observability.WithTool(ctx, "mail_search")
	_, err := p.Search(ctx, a, domain.SearchQuery{Folder: "INBOX", Limit: 1, Query: "Добрый день", Preview: true}, domain.Position{})
	if err != nil {
		t.Fatal(err)
	}
	stages := map[string]bool{}
	for _, row := range logRows(t, output) {
		if row["request_id"] != "diagnostic-request" || row["tool"] != "mail_search" || row["account"] != "personal" {
			t.Fatal("stage lost request correlation")
		}
		if stage, ok := row["stage"].(string); ok {
			stages[stage] = true
		}
	}
	for _, stage := range []string{"pool_acquire", "decrypt_credentials", "tls_connect", "greeting", "login", "examine", "uid_search", "uid_fetch_metadata", "uid_fetch_section", "mime_parse"} {
		if !stages[stage] {
			t.Fatalf("missing stage %s", stage)
		}
	}
	for _, private := range []string{"app-password", a.Email, "Добрый день", "ВКР", "teacher@example.com", "root@example.com"} {
		if strings.Contains(output.String(), private) {
			t.Fatalf("log leaked private data: %s", private)
		}
	}
	output.Reset()
	if _, err := p.Folders(ctx, a); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), `"connection_reused":true`) || !strings.Contains(output.String(), `"stage":"list_folders"`) {
		t.Fatal("missing reuse/list diagnostics")
	}
}

func TestDiagnosticTimeoutPreservesLoginStage(t *testing.T) {
	f := testutil.NewIMAP(t)
	a, _ := f.Account(t, "personal", nil)
	f.Delays.Store(a.Username, time.Second)
	pool := NewPool(f.Cipher, f.Roots)
	t.Cleanup(pool.Close)
	output := captureDiagnosticLogs(t, slog.LevelInfo)
	ctx, cancel := context.WithTimeout(t.Context(), 300*time.Millisecond)
	defer cancel()
	err := (&Provider{Pool: pool}).Test(ctx, a)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("timeout cause was lost")
	}
	rows := logRows(t, output)
	if len(rows) != 1 || rows[0]["error_stage"] != "login" || rows[0]["error_kind"] != "timeout" {
		t.Fatal("timeout lost the failing stage")
	}
}

func TestIMAPInfoLogIdentifiesFailure(t *testing.T) {
	f := testutil.NewIMAP(t)
	a, _ := f.Account(t, "personal", nil)
	pool := NewPool(f.Cipher, f.Roots)
	t.Cleanup(pool.Close)
	output := captureDiagnosticLogs(t, slog.LevelInfo)
	_, err := (&Provider{Pool: pool}).Search(t.Context(), a, domain.SearchQuery{Folder: "private-missing-folder", Limit: 1}, domain.Position{})
	if err == nil || err.Error() != "imap operation failed" {
		t.Fatal("expected safe EXAMINE failure")
	}
	rows := logRows(t, output)
	if len(rows) != 1 || rows[0]["error_stage"] != "examine" || rows[0]["error_kind"] != "server_rejected" || rows[0]["imap_status"] != "NO" || rows[0]["request_id"] == "" {
		t.Fatal("INFO completion lacks detailed failure classification")
	}
	if strings.Contains(output.String(), "private-missing-folder") {
		t.Fatal("private folder leaked")
	}
}
