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

	imaplib "github.com/emersion/go-imap/v2"
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
	last := first
	for pages := 0; last.NextCursor != ""; pages++ {
		if pages > imap.MaxScan+1 {
			t.Fatal("filtered cursor did not terminate")
		}
		q.Cursor = last.NextCursor
		last, e = service.Search(context.Background(), q)
		if e != nil {
			t.Fatal(e)
		}
		if len(last.Messages) > 0 && last.NextCursor != "" {
			t.Fatal("unexpected matching page")
		}
	}
	if len(last.Messages) != 1 || last.Messages[0].UID != 1 {
		t.Fatalf("last=%+v", last)
	}
}

func TestAllFoldersPagination(t *testing.T) {
	f := testutil.NewIMAP(t)
	f.ListEntries = []imaplib.ListData{
		{Mailbox: "Учёба", Attrs: []imaplib.MailboxAttr{imaplib.MailboxAttrNoSelect}},
		{Mailbox: "Deleted folder", Attrs: []imaplib.MailboxAttr{imaplib.MailboxAttrNonExistent}},
	}
	a, user := f.Account(t, "one", []string{testutil.Plain, testutil.HTML})
	const nested = "Учёба/Задания"
	if err := user.Create(nested, nil); err != nil {
		t.Fatal(err)
	}
	for _, folder := range []string{"Sent", nested} {
		f.AppendFolder(t, user, folder, testutil.Plain)
		f.AppendFolder(t, user, folder, testutil.HTML)
	}
	pool := imap.NewPool(f.Cipher, f.Roots)
	t.Cleanup(pool.Close)
	service := app.New(store{a}, &imap.Provider{Pool: pool}, f.Cipher)
	q := domain.SearchQuery{Limit: 1, Preview: true, Query: "ВКР"}
	seen := map[string]bool{}
	folders := map[string]bool{}
	for pages := 0; ; pages++ {
		if pages > 10 {
			t.Fatal("folder pagination did not terminate")
		}
		result, err := service.Search(t.Context(), q)
		if err != nil || len(result.Errors) != 0 {
			t.Fatalf("search error=%v account errors=%v", err, result.Errors)
		}
		for _, m := range result.Messages {
			if seen[m.ID] {
				t.Fatalf("duplicate message %s", m.ID)
			}
			seen[m.ID] = true
			folders[m.Folder] = true
			if m.UID != 1 || m.UIDValidity == 0 || m.Preview == "" {
				t.Fatalf("missing mailbox identity or preview: %+v", m)
			}
			content, err := service.Get(t.Context(), m.Account, m.Folder, m.UID, m.UIDValidity, false)
			if err != nil || !strings.Contains(content.Text, "Добрый день") {
				t.Fatalf("cannot read folder result: %v", err)
			}
		}
		if pages == 0 {
			for _, folder := range []string{"INBOX", "Sent", nested} {
				f.AppendFolder(t, user, folder, testutil.Plain)
			}
			if err := user.Create("New folder", nil); err != nil {
				t.Fatal(err)
			}
			f.AppendFolder(t, user, "New folder", testutil.Plain)
			if _, err := service.Search(t.Context(), domain.SearchQuery{Folder: "INBOX", Limit: 1, Preview: true, Query: q.Query, Cursor: result.NextCursor}); err == nil {
				t.Fatal("cursor accepted a changed folder scope")
			}
		}
		if result.NextCursor == "" {
			break
		}
		q.Cursor = result.NextCursor
	}
	if len(seen) != 3 || !folders["INBOX"] || !folders["Sent"] || !folders[nested] {
		t.Fatalf("expected original matches in all three folders, got %v", folders)
	}
	for _, folder := range []string{"INBOX", "Sent", nested} {
		status, err := user.Status(folder, &imaplib.StatusOptions{NumUnseen: true})
		if err != nil || *status.NumUnseen != 3 {
			t.Fatalf("read changed Seen in %s: %v", folder, err)
		}
	}
	result, err := service.Search(t.Context(), domain.SearchQuery{Folder: nested, Limit: 20})
	if err != nil || len(result.Errors) != 0 || len(result.Messages) != 3 {
		t.Fatalf("explicit folder search: %+v, %v", result, err)
	}
	for _, m := range result.Messages {
		if m.Folder != nested {
			t.Fatal("explicit folder search included another folder")
		}
	}
}

type folderFailureProvider struct {
	app.MailProvider
	folder string
}

func (p *folderFailureProvider) Search(ctx context.Context, a domain.Account, q domain.SearchQuery, pos domain.Position) (domain.Page, error) {
	if q.Folder == p.folder {
		return domain.Page{}, errors.New("private provider error with credentials")
	}
	return p.MailProvider.Search(ctx, a, q, pos)
}

func TestFolderFailureRetainsOtherResultsAndRetries(t *testing.T) {
	f := testutil.NewIMAP(t)
	a, user := f.Account(t, "one", []string{testutil.Plain})
	f.AppendFolder(t, user, "Sent", testutil.HTML)
	if err := user.Create("Учёба", nil); err != nil {
		t.Fatal(err)
	}
	f.AppendFolder(t, user, "Учёба", testutil.Multipart)
	pool := imap.NewPool(f.Cipher, f.Roots)
	t.Cleanup(pool.Close)
	provider := &folderFailureProvider{MailProvider: &imap.Provider{Pool: pool}, folder: "Sent"}
	service := app.New(store{a}, provider, f.Cipher)
	q := domain.SearchQuery{Limit: 20}
	first, err := service.Search(t.Context(), q)
	if err != nil || len(first.Messages) != 2 || len(first.Errors) != 1 || first.NextCursor == "" {
		t.Fatalf("partial folder results=%+v, err=%v", first, err)
	}
	if first.Errors[0].Account != a.Alias || first.Errors[0].Folder != "Sent" || first.Errors[0].Error != "imap account unavailable" {
		t.Fatalf("unsafe or unscoped folder error: %+v", first.Errors)
	}
	if first.Messages[0].Folder != "Учёба" || first.Messages[1].Folder != "INBOX" {
		t.Fatal("folder heads were not merged by Date")
	}
	provider.folder = ""
	q.Cursor = first.NextCursor
	last, err := service.Search(t.Context(), q)
	if err != nil || len(last.Errors) != 0 || len(last.Messages) != 1 || last.Messages[0].Folder != "Sent" || last.NextCursor != "" {
		t.Fatalf("folder retry=%+v, err=%v", last, err)
	}
}

func TestFolderUIDValidityChange(t *testing.T) {
	f := testutil.NewIMAP(t)
	a, user := f.Account(t, "one", []string{testutil.Plain, testutil.HTML})
	f.AppendFolder(t, user, "Sent", testutil.Plain)
	pool := imap.NewPool(f.Cipher, f.Roots)
	t.Cleanup(pool.Close)
	service := app.New(store{a}, &imap.Provider{Pool: pool}, f.Cipher)
	q := domain.SearchQuery{Limit: 1}
	first, err := service.Search(t.Context(), q)
	if err != nil || len(first.Errors) != 0 || first.NextCursor == "" {
		t.Fatalf("initial search=%+v, err=%v", first, err)
	}
	if err := user.Delete("Sent"); err != nil {
		t.Fatal(err)
	}
	if err := user.Create("Sent", nil); err != nil {
		t.Fatal(err)
	}
	f.AppendFolder(t, user, "Sent", testutil.Plain)
	q.Cursor = first.NextCursor
	next, err := service.Search(t.Context(), q)
	if err != nil || len(next.Errors) != 1 || next.Errors[0].Folder != "Sent" || next.Errors[0].Error != "mailbox UIDVALIDITY changed; start a new search" {
		t.Fatalf("recreated mailbox not rejected: %+v, %v", next, err)
	}
	if len(next.Messages) != 1 || next.Messages[0].Folder != "INBOX" {
		t.Fatal("UIDVALIDITY error discarded another folder's results")
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
