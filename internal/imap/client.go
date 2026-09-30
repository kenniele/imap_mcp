package imap

import (
	"context"
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

func (p *Provider) with(ctx context.Context, a domain.Account, op string, fn func(*connection) error) (err error) {
	start := time.Now()
	defer func() {
		if p.Metrics != nil {
			p.Metrics.ObserveIMAP(op, start, err)
		}
		errorText := ""
		if err != nil {
			errorText = appSafeError(err)
		}
		slog.InfoContext(ctx, "imap operation completed", "request_id", observability.RequestID(ctx), "tool", observability.Tool(ctx), "account", a.Alias, "duration_ms", time.Since(start).Milliseconds(), "error", errorText)
	}()
	c, err := p.Pool.Acquire(ctx, a)
	if err != nil {
		return err
	}
	stop := watch(ctx, c)
	err = fn(c)
	stop()
	if ctx.Err() != nil {
		err = ctx.Err()
	}
	if err != nil {
		p.Pool.Invalidate(a, c)
	} else {
		p.Pool.Release(a, c)
	}
	return err
}
func (p *Provider) Test(ctx context.Context, a domain.Account) error {
	return p.with(ctx, a, "test", func(c *connection) error {
		if err := c.client.Noop().Wait(); err != nil {
			return errors.New("imap operation failed")
		}
		return nil
	})
}
func (p *Provider) Folders(ctx context.Context, a domain.Account) (out []domain.Folder, err error) {
	out = []domain.Folder{}
	err = p.with(ctx, a, "folders", func(c *connection) error {
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
		if cmd.Close() != nil {
			return errors.New("imap operation failed")
		}
		return nil
	})
	return out, err
}
func selectFolder(c *connection, folder string, validity uint32) (*imaplib.SelectData, error) {
	data, err := c.client.Select(folder, &imaplib.SelectOptions{ReadOnly: true}).Wait()
	if err != nil {
		return nil, errors.New("imap operation failed")
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
