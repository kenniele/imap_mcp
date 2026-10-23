package imap

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"mail-mcp/internal/app"
	"mail-mcp/internal/domain"
	"mail-mcp/internal/testutil"

	imaplib "github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapserver/imapmemserver"
)

type memoryDates struct {
	mu    sync.Mutex
	dates map[domain.DateIndexKey]map[uint32]domain.IndexedDate
	fail  bool
}

func (m *memoryDates) LoadDates(_ context.Context, key domain.DateIndexKey, uids []uint32) (map[uint32]domain.IndexedDate, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.fail {
		return nil, errors.New("index unavailable")
	}
	out := map[uint32]domain.IndexedDate{}
	for _, uid := range uids {
		if date, ok := m.dates[key][uid]; ok {
			out[uid] = date
		}
	}
	return out, nil
}
func (m *memoryDates) SaveDates(_ context.Context, key domain.DateIndexKey, dates map[uint32]domain.IndexedDate) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.dates == nil {
		m.dates = map[domain.DateIndexKey]map[uint32]domain.IndexedDate{}
	}
	if m.dates[key] == nil {
		m.dates[key] = map[uint32]domain.IndexedDate{}
	}
	for uid, date := range dates {
		m.dates[key][uid] = date
	}
	return nil
}

func TestDateIndexSkipsHistoryAndFindsNewImports(t *testing.T) {
	f := testutil.NewIMAP(t)
	f.IgnoreSentDates = true
	old := strings.Replace(testutil.Plain, "30 Sep 2026", "01 Sep 2026", 1)
	raws := []string{testutil.Plain}
	for range 400 {
		raws = append(raws, old)
	}
	a, user := f.Account(t, "indexed", raws)
	a.Provider = "yandex"
	pool := NewPool(f.Cipher, f.Roots)
	t.Cleanup(pool.Close)
	index := &memoryDates{}
	p := &Provider{Pool: pool, DateIndex: index}
	q := domain.SearchQuery{Folder: "INBOX", Limit: 100, After: "2026-09-30T00:00:00Z", Before: "2026-10-01T00:00:00Z"}
	scan := func(provider *Provider, wantPages int, wantUIDs ...uint32) {
		t.Helper()
		pos := domain.Position{}
		pages := 0
		seen := map[uint32]bool{}
		for !pos.Done {
			page, err := provider.Search(t.Context(), a, q, pos)
			if err != nil {
				t.Fatal(err)
			}
			for _, msg := range page.Messages {
				if seen[msg.UID] {
					t.Fatal("duplicate UID")
				}
				seen[msg.UID] = true
			}
			pos = page.Next
			pages++
			if pages > 20 {
				t.Fatal("indexing did not terminate")
			}
		}
		if wantPages > 0 && pages != wantPages {
			t.Fatalf("wanted %d pages, got %d", wantPages, pages)
		}
		if len(seen) != len(wantUIDs) {
			t.Fatalf("unexpected result set: %v", seen)
		}
		for _, uid := range wantUIDs {
			if !seen[uid] {
				t.Fatalf("lost UID %d", uid)
			}
		}
	}
	scan(p, 0, 1)
	reads := f.DateReads.Load()
	// Simulate a new provider/process using the same persisted dates.
	restarted := &Provider{Pool: pool, DateIndex: index}
	scan(restarted, 1, 1)
	if f.DateReads.Load() != reads {
		t.Fatal("warm search fetched historical Date headers")
	}
	q.After = "2026-10-01T00:00:00Z"
	q.Before = "2026-10-02T00:00:00Z"
	scan(restarted, 1)
	if f.DateReads.Load() != reads {
		t.Fatal("a new date window re-read history")
	}
	q.After = "2026-09-30T00:00:00Z"
	q.Before = "2026-10-01T00:00:00Z"
	// A newly appended old Date must not hide a following imported match.
	f.Append(t, user, old)
	f.Append(t, user, testutil.Plain)
	scan(restarted, 1, 1, 403)
	if got := f.DateReads.Load() - reads; got != 2 {
		t.Fatalf("expected only two new Date reads, got %d", got)
	}
	// Live UID membership must exclude externally removed messages.
	session := imapmemserver.NewUserSession(user)
	if _, err := session.Select("INBOX", nil); err != nil {
		t.Fatal(err)
	}
	if err := session.Store(nil, imaplib.UIDSetNum(1), &imaplib.StoreFlags{Op: imaplib.StoreFlagsAdd, Flags: []imaplib.Flag{imaplib.FlagDeleted}, Silent: true}, nil); err != nil {
		t.Fatal(err)
	}
	if err := session.Expunge(nil, nil); err != nil {
		t.Fatal(err)
	}
	_ = session.Close()
	scan(restarted, 1, 403)
	if err := user.Delete("INBOX"); err != nil {
		t.Fatal(err)
	}
	if err := user.Create("INBOX", nil); err != nil {
		t.Fatal(err)
	}
	f.Append(t, user, old)
	scan(restarted, 1)
	if err := user.Create("Учёба/Задания", nil); err != nil {
		t.Fatal(err)
	}
	f.AppendFolder(t, user, "Учёба/Задания", old)
	q.Folder = "Учёба/Задания"
	scan(restarted, 1)
	// A changed UIDVALIDITY/connection identity must not reuse these records.
	key := domain.DateIndexKey{AccountID: a.ID, MailboxKey: "other", Folder: q.Folder, UIDValidity: 1}
	dates, err := index.LoadDates(t.Context(), key, []uint32{1})
	if err != nil || len(dates) != 0 {
		t.Fatal("connection identities share dates")
	}
}

func TestDateIndexFailureDoesNotPretendSearchComplete(t *testing.T) {
	f := testutil.NewIMAP(t)
	a, _ := f.Account(t, "indexed", []string{testutil.Plain})
	a.Provider = "yandex"
	pool := NewPool(f.Cipher, f.Roots)
	t.Cleanup(pool.Close)
	p := &Provider{Pool: pool, DateIndex: &memoryDates{fail: true}}
	page, err := p.Search(t.Context(), a, domain.SearchQuery{Folder: "INBOX", Limit: 20, After: "2026-09-30T00:00:00Z"}, domain.Position{})
	if err == nil || page.Next.Done {
		t.Fatal("failed date index reported successful empty completion")
	}
}

type dateTestAccounts []domain.Account

func (s dateTestAccounts) List(context.Context) ([]domain.Account, error) { return s, nil }

func TestDateIndexRetainsAllFolderCoverageAcrossQueries(t *testing.T) {
	f := testutil.NewIMAP(t)
	a, user := f.Account(t, "indexed", []string{testutil.Plain})
	a.Provider = "yandex"
	if err := user.Create("Учёба/Задания", nil); err != nil {
		t.Fatal(err)
	}
	f.AppendFolder(t, user, "Sent", testutil.Plain)
	f.AppendFolder(t, user, "Учёба/Задания", testutil.Plain)
	pool := NewPool(f.Cipher, f.Roots)
	t.Cleanup(pool.Close)
	index := &memoryDates{}
	service := app.New(dateTestAccounts{a}, &Provider{Pool: pool, DateIndex: index}, f.Cipher)
	query := domain.SearchQuery{Limit: 100, After: "2026-09-30T00:00:00Z", Before: "2026-10-01T00:00:00Z"}
	first, err := service.Search(t.Context(), query)
	if err != nil || len(first.Errors) != 0 || first.NextCursor != "" || len(first.Messages) != 3 {
		t.Fatalf("cold all-folder search: %+v, %v", first, err)
	}
	reads := f.DateReads.Load()
	warm, err := service.Search(t.Context(), query)
	if err != nil || len(warm.Errors) != 0 || warm.NextCursor != "" || len(warm.Messages) != 3 || f.DateReads.Load() != reads {
		t.Fatalf("warm all-folder search: %+v, %v", warm, err)
	}
	// Folder discovery is never cached: a new nested mailbox is visible next run.
	if err := user.Create("Учёба/Новые", nil); err != nil {
		t.Fatal(err)
	}
	f.AppendFolder(t, user, "Учёба/Новые", testutil.Plain)
	next, err := service.Search(t.Context(), query)
	if err != nil || len(next.Errors) != 0 || next.NextCursor != "" || len(next.Messages) != 4 || f.DateReads.Load() != reads+1 {
		t.Fatalf("new nested folder lost: %+v, %v", next, err)
	}
}

func TestDateIndexWarnsAboutUnavailableBodiesAndRecovers(t *testing.T) {
	f := testutil.NewIMAP(t)
	a, _ := f.Account(t, "indexed", []string{testutil.Plain, testutil.Plain})
	a.Provider = "yandex"
	f.UnavailableBodies.Store(uint32(1), true)
	pool := NewPool(f.Cipher, f.Roots)
	t.Cleanup(pool.Close)
	index := &memoryDates{}
	service := app.New(dateTestAccounts{a}, &Provider{Pool: pool, DateIndex: index}, f.Cipher)
	query := domain.SearchQuery{Limit: 100, After: "2026-09-30T00:00:00Z", Before: "2026-10-01T00:00:00Z"}
	first, err := service.Search(t.Context(), query)
	if err != nil || len(first.Errors) != 0 || len(first.Warnings) != 1 || first.NextCursor != "" || len(first.Messages) != 1 || first.Messages[0].UID != 2 {
		t.Fatalf("unavailable UID lost or caused infinite retry: %+v, %v", first, err)
	}
	reads := f.DateReads.Load()
	next, err := service.Search(t.Context(), query)
	if err != nil || len(next.Errors) != 0 || len(next.Warnings) != 1 || next.NextCursor != "" || len(next.Messages) != 1 || f.DateReads.Load() != reads {
		t.Fatalf("warm lookup retried unavailable headers: %+v, %v", next, err)
	}
	// Expired negative status must notice a repair with unchanged UIDVALIDITY.
	f.UnavailableBodies.Delete(uint32(1))
	for _, folder := range index.dates {
		for uid, record := range folder {
			record.RecheckAfter = time.Time{}
			folder[uid] = record
		}
	}
	repaired, err := service.Search(t.Context(), query)
	if err != nil || len(repaired.Errors) != 0 || len(repaired.Warnings) != 0 || len(repaired.Messages) != 2 || repaired.NextCursor != "" {
		t.Fatalf("repaired body stayed hidden: %+v, %v", repaired, err)
	}
}

func TestUnavailableRechecksStayBoundedAndTerminate(t *testing.T) {
	f := testutil.NewIMAP(t)
	raws := make([]string, 13)
	for i := range raws {
		raws[i] = testutil.Plain
	}
	a, _ := f.Account(t, "indexed", raws)
	a.Provider = "yandex"
	for uid := uint32(1); uid <= 12; uid++ {
		f.UnavailableBodies.Store(uid, true)
	}
	pool := NewPool(f.Cipher, f.Roots)
	t.Cleanup(pool.Close)
	index := &memoryDates{}
	service := app.New(dateTestAccounts{a}, &Provider{Pool: pool, DateIndex: index}, f.Cipher)
	query := domain.SearchQuery{Limit: 100, After: "2026-09-30T00:00:00Z", Before: "2026-10-01T00:00:00Z"}
	collect := func() int {
		t.Helper()
		q := query
		seen := map[uint32]bool{}
		for pages := 1; pages <= 10; pages++ {
			result, err := service.Search(t.Context(), q)
			if err != nil || len(result.Errors) != 0 {
				t.Fatalf("failed bounded recheck: %+v, %v", result, err)
			}
			for _, message := range result.Messages {
				if seen[message.UID] {
					t.Fatal("repeated UID")
				}
				seen[message.UID] = true
			}
			if result.NextCursor == "" {
				if len(seen) != 1 || !seen[13] {
					t.Fatalf("available messages lost: %v", seen)
				}
				return pages
			}
			q.Cursor = result.NextCursor
		}
		t.Fatal("rechecks held cursor open")
		return 0
	}
	collect()
	reads := f.DateReads.Load()
	if pages := collect(); pages != 1 {
		t.Fatalf("fresh unavailable cache took %d pages", pages)
	}
	if f.DateReads.Load() != reads {
		t.Fatal("fresh negative cache reread headers")
	}
	for _, folder := range index.dates {
		for uid, record := range folder {
			record.RecheckAfter = time.Time{}
			folder[uid] = record
		}
	}
	before := f.EnvelopeReads.Load()
	f.MaxEnvelopeBatch.Store(0)
	if pages := collect(); pages != 3 {
		t.Fatalf("12 unavailable UIDs expected three bounded pages, got %d", pages)
	}
	if f.MaxEnvelopeBatch.Load() > maxUnavailableChecks || f.EnvelopeReads.Load()-before != 12 {
		t.Fatal("availability checks were unbounded or repeated")
	}
	if f.DateReads.Load() != reads {
		t.Fatal("recheck fetched historical Date headers")
	}
}
