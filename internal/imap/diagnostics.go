package imap

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"log/slog"
	"net"
	"reflect"
	"strconv"
	"strings"
	"syscall"
	"time"

	"mail-mcp/internal/observability"

	imaplib "github.com/emersion/go-imap/v2"
)

type traceKey struct{}
type traceContext struct{ account, operation string }

// operationError retains the cause for classification, but exposes only a
// controlled message. Raw provider text must never reach logs or MCP results.
type operationError struct {
	stage, message string
	cause          error
}

func (e *operationError) Error() string { return e.message }
func (e *operationError) Unwrap() error { return e.cause }

func traceAttrs(ctx context.Context) []any {
	trace, _ := ctx.Value(traceKey{}).(traceContext)
	return []any{"request_id", observability.RequestID(ctx), "tool", observability.Tool(ctx), "account", trace.account, "operation", trace.operation}
}

func step[T any](ctx context.Context, stage string, fn func() (T, error), attrs ...any) (out T, err error) {
	start := time.Now()
	fields := append(traceAttrs(ctx), "stage", stage)
	fields = append(fields, attrs...)
	if deadline, ok := ctx.Deadline(); ok {
		fields = append(fields, "remaining_ms", max(0, time.Until(deadline).Milliseconds()))
	}
	slog.DebugContext(ctx, "imap stage started", fields...)
	out, err = fn()
	if err != nil && ctx.Err() != nil {
		if failure, ok := errors.AsType[*operationError](err); ok {
			err = &operationError{stage: failure.stage, message: failure.message, cause: ctx.Err()}
		} else {
			err = ctx.Err()
		}
	}
	result := "ok"
	if err != nil {
		result = "error"
		message := "imap operation failed"
		switch stage {
		case "login":
			message = "imap authentication failed"
		case "tls_connect":
			message = "imap TLS connection failed"
		case "decrypt_credentials":
			message = "account credential decryption failed"
		case "mime_parse":
			message = "message MIME parsing failed"
		}
		if _, wrapped := errors.AsType[*operationError](err); !wrapped {
			err = &operationError{stage: stage, message: message, cause: err}
		}
	}
	fields = append(fields, "result", result, "duration_ms", time.Since(start).Milliseconds())
	fields = append(fields, errorAttrs(err)...)
	slog.DebugContext(ctx, "imap stage completed", fields...)
	return out, err
}

func commandStep(ctx context.Context, stage string, fn func() error, attrs ...any) error {
	_, err := step(ctx, stage, func() (struct{}, error) { return struct{}{}, fn() }, attrs...)
	return err
}

func errorAttrs(err error) []any {
	stage := ""
	if failure, ok := errors.AsType[*operationError](err); ok {
		stage = failure.stage
	}
	kind, status, code := classifyError(err)
	types := []string{}
	for cause := err; cause != nil && len(types) < 8; cause = errors.Unwrap(cause) {
		types = append(types, reflect.TypeOf(cause).String())
	}
	return []any{"error_stage", stage, "error_kind", kind, "imap_status", status, "imap_code", code, "error_types", types, "parser_area", parserArea(err)}
}

func classifyError(err error) (kind, status, code string) {
	switch {
	case err == nil:
		return "", "", ""
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout", "", ""
	case errors.Is(err, context.Canceled):
		return "canceled", "", ""
	case errors.Is(err, io.ErrUnexpectedEOF):
		return "unexpected_eof", "", ""
	case errors.Is(err, io.EOF):
		return "connection_closed", "", ""
	case errors.Is(err, syscall.ECONNRESET), errors.Is(err, syscall.EPIPE):
		return "connection_reset", "", ""
	case errors.Is(err, syscall.ECONNREFUSED):
		return "connection_refused", "", ""
	}
	if response, ok := errors.AsType[*imaplib.Error](err); ok {
		switch response.Type {
		case imaplib.StatusResponseTypeNo, imaplib.StatusResponseTypeBad, imaplib.StatusResponseTypeBye:
			status = string(response.Type)
		default:
			status = "OTHER"
		}
		return "server_rejected", status, safeResponseCode(response.Code)
	}
	if _, ok := errors.AsType[*tls.CertificateVerificationError](err); ok {
		return "tls_certificate", "", ""
	}
	if _, ok := errors.AsType[*net.DNSError](err); ok {
		return "dns", "", ""
	}
	if _, ok := errors.AsType[*time.ParseError](err); ok {
		return "date_parse", "", ""
	}
	if _, ok := errors.AsType[*strconv.NumError](err); ok {
		return "number_parse", "", ""
	}
	var network net.Error
	if errors.As(err, &network) && network.Timeout() {
		return "timeout", "", ""
	}
	// The dependency's decoder error is in an internal package. Inspect only
	// its type; its text can contain message data and must never be logged.
	for cause := err; cause != nil; cause = errors.Unwrap(cause) {
		if reflect.TypeOf(cause).String() == "*imapwire.DecoderExpectError" {
			return "response_parse", "", ""
		}
	}
	if parserArea(err) != "" {
		return "response_parse", "", ""
	}
	return "other", "", ""
}

func parserArea(err error) string {
	// go-imap sometimes formats decoder errors with %v and loses the typed
	// cause. Recognize only its fixed parser prefixes, and emit a fixed label.
	// Never emit any part of the error text or the provider's response.
	for cause := err; cause != nil; cause = errors.Unwrap(cause) {
		switch reflect.TypeOf(cause).String() {
		case "*errors.errorString", "*fmt.wrapError", "*imapwire.DecoderExpectError":
		default:
			continue
		}
		text := cause.Error()
		if !strings.HasPrefix(text, "in response-data: ") && !strings.HasPrefix(text, "in response-tagged: ") && !strings.HasPrefix(text, "in response: ") && !strings.HasPrefix(text, "imapwire: ") {
			continue
		}
		for _, area := range []struct{ marker, label string }{
			{"in envelope:", "envelope"},
			{"in date-time:", "internal_date"},
			{"in body-type-1part:", "body_structure"},
			{"in body-type-mpart:", "body_structure"},
			{"in body-fld-", "body_structure"},
			{"in body-ext-", "body_structure"},
			{"in section-spec:", "section"},
			{"in section-binary:", "section"},
			{"in resp-text-code:", "response_code"},
		} {
			if strings.Contains(text, area.marker) {
				return area.label
			}
		}
		return "response"
	}
	return ""
}

func safeResponseCode(code imaplib.ResponseCode) string {
	// Response codes are provider-controlled. Log only known constants.
	switch code {
	case "":
		return ""
	case imaplib.ResponseCodeAlert, imaplib.ResponseCodeAlreadyExists,
		imaplib.ResponseCodeAuthenticationFailed, imaplib.ResponseCodeAuthorizationFailed,
		imaplib.ResponseCodeBadCharset, imaplib.ResponseCodeCannot, imaplib.ResponseCodeClientBug,
		imaplib.ResponseCodeContactAdmin, imaplib.ResponseCodeCorruption, imaplib.ResponseCodeExpired,
		imaplib.ResponseCodeHasChildren, imaplib.ResponseCodeInUse, imaplib.ResponseCodeLimit,
		imaplib.ResponseCodeNonExistent, imaplib.ResponseCodeNoPerm, imaplib.ResponseCodeOverQuota,
		imaplib.ResponseCodeParse, imaplib.ResponseCodePrivacyRequired, imaplib.ResponseCodeServerBug,
		imaplib.ResponseCodeTryCreate, imaplib.ResponseCodeUnavailable, imaplib.ResponseCodeUnknownCTE,
		imaplib.ResponseCodeTooMany, imaplib.ResponseCodeNoPrivate, imaplib.ResponseCodeTooBig:
		return string(code)
	default:
		return "OTHER"
	}
}
