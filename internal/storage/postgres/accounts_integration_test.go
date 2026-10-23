//go:build integration

package postgres

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/url"
	"os"
	"testing"
	"time"

	"mail-mcp/internal/domain"
	"mail-mcp/internal/secrets"
	"mail-mcp/migrations"

	"github.com/jackc/pgx/v5"
)

func TestPostgresEncryptedAccountLifecycle(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set TEST_DATABASE_URL for a temporary PostgreSQL instance")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	admin, e := Open(ctx, dsn)
	if e != nil {
		t.Fatal(e)
	}
	defer admin.Close()
	suffix := make([]byte, 8)
	if _, e := rand.Read(suffix); e != nil {
		t.Fatal(e)
	}
	schema := "mailmcp_test_" + hex.EncodeToString(suffix)
	identifier := pgx.Identifier{schema}.Sanitize()
	if _, e = admin.DB.Exec(ctx, "CREATE SCHEMA "+identifier); e != nil {
		t.Fatal(e)
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		_, _ = admin.DB.Exec(cleanup, "DROP SCHEMA "+identifier+" CASCADE")
	}()
	u, e := url.Parse(dsn)
	if e != nil {
		t.Fatal(e)
	}
	q := u.Query()
	q.Set("search_path", schema)
	u.RawQuery = q.Encode()
	store, e := Open(ctx, u.String())
	if e != nil {
		t.Fatal(e)
	}
	defer store.Close()
	if _, e = store.DB.Exec(ctx, migrations.AccountsSQL+"\n"+migrations.MailDatesSQL+"\n"+migrations.DateAvailabilitySQL+"\n"+migrations.DateAvailabilitySQL); e != nil {
		t.Fatal(e)
	}
	cipher, _ := secrets.New(bytes.Repeat([]byte{42}, 32))
	id := "00000000-0000-4000-8000-000000000001"
	encrypted, _ := cipher.Encrypt([]byte("test application password"), "account:"+id)
	a := domain.Account{ID: id, Alias: "university", Email: "student@example.com", Provider: "mailru", IMAPHost: "imap.mail.ru", IMAPPort: 993, IMAPTLS: true, Username: "student@example.com", Secret: encrypted, Enabled: true}
	if e = store.Add(ctx, a); e != nil {
		t.Fatal(e)
	}
	if e = store.Add(ctx, a); e == nil {
		t.Fatal("duplicate alias accepted")
	}
	accounts, e := store.List(ctx)
	if e != nil || len(accounts) != 1 {
		t.Fatalf("list=%v err=%v", accounts, e)
	}
	if bytes.Contains(accounts[0].Secret, []byte("test application password")) {
		t.Fatal("plaintext persisted")
	}
	raw, _ := json.Marshal(accounts)
	if bytes.Contains(raw, []byte("test application password")) || bytes.Contains(raw, []byte("Secret")) {
		t.Fatal("credentials in account JSON")
	}
	plain, e := cipher.Decrypt(accounts[0].Secret, "account:"+id)
	if e != nil || string(plain) != "test application password" {
		t.Fatal("stored encrypted credential does not decrypt")
	}
	key := domain.DateIndexKey{AccountID: id, MailboxKey: "connection-one", Folder: "Учёба/Задания", UIDValidity: 4294967295}
	date := time.Date(2026, 10, 23, 9, 0, 0, 0, time.FixedZone("MSK", 3*60*60))
	if e = store.SaveDates(ctx, key, map[uint32]domain.IndexedDate{4294967295: {Date: date}}); e != nil {
		t.Fatal(e)
	}
	if e = store.SaveDates(ctx, key, map[uint32]domain.IndexedDate{42: {Date: date, Unavailable: true}}); e != nil {
		t.Fatal(e)
	}
	unavailable, e := store.LoadDates(ctx, key, []uint32{42})
	if e != nil || !unavailable[42].Unavailable {
		t.Fatal("unavailable state was not persisted")
	}
	repairedDate := date.Add(time.Hour)
	if e = store.SaveDates(ctx, key, map[uint32]domain.IndexedDate{42: {Date: repairedDate}}); e != nil {
		t.Fatal(e)
	}
	repaired, e := store.LoadDates(ctx, key, []uint32{42})
	if e != nil || repaired[42].Unavailable || !repaired[42].Date.Equal(repairedDate) {
		t.Fatal("repaired provider date did not replace unavailable state")
	}
	// Reopening the store models an application restart; no in-memory cache is shared.
	restarted, e := Open(ctx, u.String())
	if e != nil {
		t.Fatal(e)
	}
	dates, e := restarted.LoadDates(ctx, key, []uint32{1, 4294967295})
	restarted.Close()
	if e != nil || len(dates) != 1 || !dates[4294967295].Date.Equal(date) {
		t.Fatalf("date index did not persist: %v, %v", dates, e)
	}
	for _, changed := range []domain.DateIndexKey{
		{AccountID: id, MailboxKey: key.MailboxKey, Folder: key.Folder, UIDValidity: 1},
		{AccountID: id, MailboxKey: key.MailboxKey, Folder: "INBOX", UIDValidity: key.UIDValidity},
		{AccountID: id, MailboxKey: "connection-two", Folder: key.Folder, UIDValidity: key.UIDValidity},
		{AccountID: "00000000-0000-4000-8000-000000000002", MailboxKey: key.MailboxKey, Folder: key.Folder, UIDValidity: key.UIDValidity},
	} {
		dates, e := store.LoadDates(ctx, changed, []uint32{4294967295})
		if e != nil || len(dates) != 0 {
			t.Fatal("date index leaked across mailbox identity")
		}
	}
	// Only live UIDs are looked up; an externally deleted UID is excluded.
	if dates, e := store.LoadDates(ctx, key, []uint32{1}); e != nil || len(dates) != 0 {
		t.Fatal("date index ignored live UID membership")
	}
	if e = store.Disable(ctx, "university"); e != nil {
		t.Fatal(e)
	}
	accounts, e = store.List(ctx)
	if e != nil || accounts[0].Enabled {
		t.Fatal("disable failed")
	}
	if e = store.Delete(ctx, "university"); e != nil {
		t.Fatal(e)
	}
	var cached int
	if e := store.DB.QueryRow(ctx, "SELECT count(*) FROM mail_message_dates").Scan(&cached); e != nil || cached != 0 {
		t.Fatal("account deletion retained cached dates")
	}
	accounts, e = store.List(ctx)
	if e != nil || len(accounts) != 0 {
		t.Fatal("delete failed")
	}
}
