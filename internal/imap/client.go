package imap

import (
	"context"
	"crypto/rand"
	"errors"
	"log/slog"
	"time"

	"mail-mcp/internal/domain"
	"mail-mcp/internal/observability"

	imaplib "github.com/emersion/go-imap/v2"
)

type Provider struct {
	Pool    *Pool
	Metrics *observability.Metrics
}

func (p *Provider) with(ctx context.Context, a domain.Account, op string, fn func(context.Context, *connection) error) (err error) {
	if observability.RequestID(ctx) == "" {
		ctx = observability.ContextWithRequestID(ctx, rand.Text())
	}
	ctx = context.WithValue(ctx, traceKey{}, traceContext{account: a.Alias, operation: op})
	start := time.Now()
	defer func() {
		if p.Metrics != nil {
			p.Metrics.ObserveIMAP(op, start, err)
		}
		errorText := ""
		if err != nil {
			errorText = appSafeError(err)
		}
		result := "ok"
		if err != nil {
			result = "error"
		}
		fields := append(traceAttrs(ctx), "duration_ms", time.Since(start).Milliseconds(), "result", result, "error", errorText)
		fields = append(fields, errorAttrs(err)...)
		slog.InfoContext(ctx, "imap operation completed", fields...)
	}()
	c, err := step(ctx, "pool_acquire", func() (*connection, error) { return p.Pool.Acquire(ctx, a) })
	if err != nil {
		return err
	}
	stop := watch(ctx, c)
	err = fn(ctx, c)
	stop()
	if ctx.Err() != nil {
		if failure, ok := errors.AsType[*operationError](err); ok {
			err = &operationError{stage: failure.stage, message: failure.message, cause: ctx.Err()}
		} else {
			err = ctx.Err()
		}
	}
	if err != nil {
		p.Pool.Invalidate(a, c)
	} else {
		p.Pool.Release(a, c)
	}
	return err
}
func (p *Provider) Test(ctx context.Context, a domain.Account) error {
	return p.with(ctx, a, "test", func(ctx context.Context, c *connection) error {
		_, err := step(ctx, "noop", func() (struct{}, error) { return struct{}{}, c.client.Noop().Wait() })
		return err
	})
}
func (p *Provider) Folders(ctx context.Context, a domain.Account) (out []domain.Folder, err error) {
	out = []domain.Folder{}
	err = p.with(ctx, a, "folders", func(ctx context.Context, c *connection) error {
		return commandStep(ctx, "list_folders", func() error {
			cmd := c.client.List("", "*", nil)
			defer cmd.Close()
			for m := cmd.Next(); m != nil; m = cmd.Next() {
				if len(out) >= 1000 {
					return errors.New("too many IMAP folders")
				}
				name := m.Mailbox
				display := name
				switch name {
				case "INBOX":
					display = "Входящие"
				case "Sent", "Sent Messages":
					display = "Отправленные"
				case "Drafts":
					display = "Черновики"
				case "Trash":
					display = "Корзина"
				}
				out = append(out, domain.Folder{Name: name, DisplayName: display})
			}
			return cmd.Close()
		})
	})
	return out, err
}
func selectFolder(ctx context.Context, c *connection, folder string, validity uint32) (*imaplib.SelectData, error) {
	data, err := step(ctx, "examine", func() (*imaplib.SelectData, error) {
		return c.client.Select(folder, &imaplib.SelectOptions{ReadOnly: true}).Wait()
	}, "readonly", true)
	if err != nil {
		return nil, err
	}
	if validity != 0 && data.UIDValidity != validity {
		return nil, errors.New("mailbox UIDVALIDITY changed; start a new search")
	}
	return data, nil
}

func appSafeError(err error) string {
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return "timeout or cancellation"
	}
	return "imap account operation failed"
}
