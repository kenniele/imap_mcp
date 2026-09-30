package imap

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"mail-mcp/internal/domain"
	"mail-mcp/internal/testutil"

	imaplib "github.com/emersion/go-imap/v2"
)

func TestIMAPTLSReadOnly(t *testing.T) {
	f := testutil.NewIMAP(t)
	a, user := f.Account(t, "university", []string{testutil.Plain, testutil.HTML, testutil.Multipart})
	pool := NewPool(f.Cipher, f.Roots)
	t.Cleanup(pool.Close)
	p := &Provider{Pool: pool}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	folders, e := p.Folders(ctx, a)
	if e != nil || len(folders) != 2 {
		t.Fatalf("folders=%v err=%v", folders, e)
	}
	before, e := user.Status("INBOX", &imaplib.StatusOptions{NumUnseen: true})
	if e != nil {
		t.Fatal(e)
	}
	q := domain.SearchQuery{Folder: "INBOX", Limit: 20}
	page, e := p.Search(ctx, a, q, domain.Position{})
	if e != nil || len(page.Messages) != 3 {
		t.Fatalf("page=%v error=%v", page, e)
	}
	if page.Messages[2].Subject != "ВКР" {
		t.Fatalf("encoded subject: %q", page.Messages[2].Subject)
	}
	for uid, want := range map[uint32]string{1: "Добрый день", 2: "Привет & hello", 3: "hello world"} {
		m, e := p.Get(ctx, a, "INBOX", uid, page.Start.UIDValidity, true)
		if e != nil {
			t.Fatal(e)
		}
		if !strings.Contains(m.Text, want) {
			t.Fatalf("uid=%d text=%q", uid, m.Text)
		}
		if uid == 3 && (len(m.Attachments) != 1 || m.Attachments[0].Filename != "document.pdf" || m.Attachments[0].ID != "university:INBOX:3:2") {
			t.Fatalf("attachments=%v", m.Attachments)
		}
		if strings.Contains(m.Text, "binary-secret") {
			t.Fatal("attachment bytes leaked")
		}
	}
	after, _ := user.Status("INBOX", &imaplib.StatusOptions{NumUnseen: true})
	if *before.NumUnseen != *after.NumUnseen {
		t.Fatal("read changed seen flags")
	}
	q.Query = "ВКР"
	page, e = p.Search(ctx, a, q, domain.Position{})
	if e != nil || len(page.Messages) != 1 {
		t.Fatalf("Russian SEARCH: %v %v", page, e)
	}
	q.Query = "hello world"
	page, e = p.Search(ctx, a, q, domain.Position{})
	if e != nil || len(page.Messages) != 1 {
		t.Fatalf("body SEARCH: %v %v", page, e)
	}
	thread, cut, e := p.Thread(ctx, a, "INBOX", 1, 0, 30)
	if e != nil || cut || len(thread) != 2 {
		t.Fatalf("thread=%v cut=%v err=%v", thread, cut, e)
	}
	if _, e = p.Get(ctx, a, "INBOX", 999, 0, false); e == nil {
		t.Fatal("missing UID accepted")
	}
	if _, e = p.Get(ctx, a, "INBOX", 1, 999, false); e == nil {
		t.Fatal("UIDVALIDITY mismatch accepted")
	}
	broken := a
	broken.ID = "broken"
	broken.Secret, _ = f.Cipher.Encrypt([]byte("wrong password"), "account:broken")
	if e = p.Test(ctx, broken); e == nil || e.Error() != "imap authentication failed" {
		t.Fatalf("bad auth error=%v", e)
	}
	untrusted := NewPool(f.Cipher, nil)
	defer untrusted.Close()
	if e = (&Provider{Pool: untrusted}).Test(ctx, a); e == nil {
		t.Fatal("untrusted TLS certificate accepted")
	}
}
func TestPoolCapacityCancellationAndReuse(t *testing.T) {
	f := testutil.NewIMAP(t)
	a, _ := f.Account(t, "personal", nil)
	pool := NewPool(f.Cipher, f.Roots)
	defer pool.Close()
	ctx := context.Background()
	one, e := pool.Acquire(ctx, a)
	if e != nil {
		t.Fatal(e)
	}
	two, e := pool.Acquire(ctx, a)
	if e != nil {
		t.Fatal(e)
	}
	short, cancel := context.WithTimeout(ctx, 30*time.Millisecond)
	defer cancel()
	if _, e = pool.Acquire(short, a); !errors.Is(e, context.DeadlineExceeded) {
		t.Fatalf("third connection error=%v", e)
	}
	pool.Release(a, one)
	reused, e := pool.Acquire(ctx, a)
	if e != nil || reused != one {
		t.Fatal("idle connection not reused")
	}
	pool.Invalidate(a, reused)
	pool.Release(a, two)
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			c, e := pool.Acquire(ctx, a)
			if e != nil {
				t.Error(e)
				return
			}
			pool.Release(a, c)
		})
	}
	wg.Wait()
	pool.mu.Lock()
	count := len(pool.buckets[a.ID].all)
	pool.mu.Unlock()
	if count > 2 {
		t.Fatalf("pool has %d connections", count)
	}
	pool.Close()
	if _, e = pool.Acquire(ctx, a); e == nil {
		t.Fatal("closed pool acquired")
	}
}

func TestLoginCancellation(t *testing.T) {
	f := testutil.NewIMAP(t)
	a, _ := f.Account(t, "slow", nil)
	f.Delays.Store("slow", time.Second)
	pool := NewPool(f.Cipher, f.Roots)
	defer pool.Close()
	p := &Provider{Pool: pool}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
	defer cancel()
	start := time.Now()
	err := p.Test(ctx, a)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected cancellation, got %v", err)
	}
	if time.Since(start) > 500*time.Millisecond {
		t.Fatal("SDK LOGIN exceeded request deadline")
	}
}
