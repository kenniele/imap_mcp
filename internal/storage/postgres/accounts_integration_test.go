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
	if _, e = store.DB.Exec(ctx, migrations.AccountsSQL); e != nil {
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
	accounts, e = store.List(ctx)
	if e != nil || len(accounts) != 0 {
		t.Fatal("delete failed")
	}
}
