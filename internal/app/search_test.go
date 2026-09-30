package app_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"mail-mcp/internal/app"
	"mail-mcp/internal/domain"
	"mail-mcp/internal/imap"
	"mail-mcp/internal/testutil"
)

type store []domain.Account

func (s store) List(context.Context) ([]domain.Account, error) { return s, nil }

type timeoutProvider struct{ app.MailProvider }

func (p timeoutProvider) Search(ctx context.Context, a domain.Account, q domain.SearchQuery, pos domain.Position) (domain.Page, error) {
	if a.Alias == "timeout" {
		<-ctx.Done()
		return domain.Page{}, ctx.Err()
	}
	return p.MailProvider.Search(ctx, a, q, pos)
}
func TestSelectionAndValidation(t *testing.T) {
	accounts := []domain.Account{{ID: "1", Alias: "one", Enabled: true}, {ID: "2", Alias: "two", Enabled: false}}
	selected, e := app.SelectAccounts(accounts, nil)
	if e != nil || len(selected) != 1 {
		t.Fatal("enabled selection")
	}
	selected, e = app.SelectAccounts(accounts, []string{"one", "1"})
	if e != nil || len(selected) != 1 {
		t.Fatal("deduplication")
	}
	if _, e = app.SelectAccounts(accounts, []string{"two"}); e == nil {
		t.Fatal("disabled account selected")
	}
	for _, q := range []domain.SearchQuery{{Limit: 101}, {Limit: -1}, {After: "tomorrow"}, {After: "2026-10-01T00:00:00Z", Before: "2026-09-01T00:00:00Z"}, {Folder: "INBOX\r\nSTORE"}} {
		if _, e := app.Normalize(q); e == nil {
			t.Fatalf("invalid query accepted: %+v", q)
		}
	}
}
func TestMultiAccountPaginationAndPartialFailure(t *testing.T) {
	f := testutil.NewIMAP(t)
	messages := []string{}
	for i := 1; i <= 4; i++ {
		raw := strings.Replace(testutil.Plain, "<root@example.com>", fmt.Sprintf("<%d@example.com>", i), 1)
		messages = append(messages, raw)
	}
	a, u := f.Account(t, "university", messages)
	b, _ := f.Account(t, "personal", messages)
	bad, _ := f.Account(t, "broken", nil)
	bad.Secret, _ = f.Cipher.Encrypt([]byte("invalid"), "account:broken")
	timed := a
	timed.ID = "timeout"
	timed.Alias = "timeout"
	pool := imap.NewPool(f.Cipher, f.Roots)
	defer pool.Close()
	provider := &imap.Provider{Pool: pool}
	service := app.New(store{a, b, bad, timed}, timeoutProvider{provider}, f.Cipher)
	service.AccountTimeout = 50 * time.Millisecond
	result, e := service.Search(context.Background(), domain.SearchQuery{Limit: 3})
	if e != nil {
		t.Fatal(e)
	}
	if len(result.Messages) != 3 || len(result.Errors) != 2 {
		t.Fatalf("results=%+v", result)
	}
	// Test stable pagination without failing accounts, including an arrival between pages.
	service = app.New(store{a, b}, provider, f.Cipher)
	q := domain.SearchQuery{Limit: 3}
	seen := map[string]bool{}
	pages := 0
	for {
		result, e = service.Search(context.Background(), q)
		if e != nil {
			t.Fatal(e)
		}
		for _, m := range result.Messages {
			if seen[m.ID] {
				t.Fatal("duplicate", m.ID)
			}
			seen[m.ID] = true
		}
		pages++
		if pages == 1 {
			f.Append(t, u, strings.Replace(testutil.Plain, "root@example.com", "new@example.com", 1))
		}
		if result.NextCursor == "" {
			break
		}
		q.Cursor = result.NextCursor
		if pages > 10 {
			t.Fatal("pagination did not end")
		}
	}
	if len(seen) != 8 {
		t.Fatalf("snapshot expected 8 messages, got %d", len(seen))
	}
	first, e := service.Search(context.Background(), domain.SearchQuery{Limit: 1})
	if e != nil {
		t.Fatal(e)
	}
	changed := domain.SearchQuery{Limit: 1, Subject: "other", Cursor: first.NextCursor}
	if _, e = service.Search(context.Background(), changed); e == nil {
		t.Fatal("cursor accepts changed filters")
	}
	tampered := first.NextCursor
	byteToken := []byte(tampered)
	byteToken[len(byteToken)/2] ^= 1
	if _, e = service.Search(context.Background(), domain.SearchQuery{Limit: 1, Cursor: string(byteToken)}); e == nil {
		t.Fatal("tampered cursor accepted")
	}
}
func TestEmptyFilteredPageStillContinues(t *testing.T) {
	f := testutil.NewIMAP(t)
	raws := []string{testutil.Multipart}
	for range imap.MaxScan + 1 {
		raws = append(raws, testutil.Plain)
	}
	a, _ := f.Account(t, "one", raws)
	pool := imap.NewPool(f.Cipher, f.Roots)
	defer pool.Close()
	service := app.New(store{a}, &imap.Provider{Pool: pool}, f.Cipher)
	has := true
	q := domain.SearchQuery{Limit: 20, HasAttachments: &has}
	first, e := service.Search(context.Background(), q)
	if e != nil {
		t.Fatal(e)
	}
	if len(first.Messages) != 0 || first.NextCursor == "" {
		t.Fatalf("expected empty continuing window: %+v", first)
	}
	q.Cursor = first.NextCursor
	last, e := service.Search(context.Background(), q)
	if e != nil || len(last.Messages) != 1 || last.Messages[0].UID != 1 || last.NextCursor != "" {
		t.Fatalf("last=%+v err=%v", last, e)
	}
}
func TestThreadGrouping(t *testing.T) {
	now := time.Now()
	msgs := []domain.Content{{Message: domain.Message{MessageID: "<root>", Date: now}}, {Message: domain.Message{MessageID: "<child>", Date: now.Add(time.Hour)}, InReplyTo: []string{"<root>"}}, {Message: domain.Message{MessageID: "<grandchild>", Date: now.Add(2 * time.Hour)}, References: []string{"<child>"}}, {Message: domain.Message{MessageID: "<unrelated>", Subject: "Same subject"}}}
	grouped := app.GroupThread("<child>", msgs)
	if len(grouped) != 3 || grouped[0].MessageID != "<root>" {
		t.Fatalf("grouped=%+v", grouped)
	}
}
func TestNoRawErrors(t *testing.T) {
	service := app.New(errorStore{}, nil, nil)
	if _, e := service.Search(context.Background(), domain.SearchQuery{}); !errors.Is(e, errStore) {
		t.Fatal(e)
	}
}

var errStore = errors.New("controlled store failure")

type errorStore struct{}

func (errorStore) List(context.Context) ([]domain.Account, error) { return nil, errStore }
