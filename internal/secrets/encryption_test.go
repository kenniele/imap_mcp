package secrets

import (
	"bytes"
	"testing"
)

func TestEncryption(t *testing.T) {
	key := bytes.Repeat([]byte{7}, 32)
	c, e := New(key)
	if e != nil {
		t.Fatal(e)
	}
	encrypted, e := c.Encrypt([]byte("private application password"), "account:one")
	if e != nil {
		t.Fatal(e)
	}
	again, _ := c.Encrypt([]byte("private application password"), "account:one")
	if bytes.Equal(again, encrypted) || bytes.Contains(encrypted, []byte("private application password")) {
		t.Fatal("nonce reuse or plaintext leak")
	}
	out, e := c.Decrypt(encrypted, "account:one")
	if e != nil || string(out) != "private application password" {
		t.Fatalf("roundtrip: %v", e)
	}
	if _, e = c.Decrypt(encrypted, "account:two"); e == nil {
		t.Fatal("ciphertext must be bound to account")
	}
	encrypted[len(encrypted)-1] ^= 1
	if _, e = c.Decrypt(encrypted, "account:one"); e == nil {
		t.Fatal("tampering accepted")
	}
	if _, e = New([]byte("short")); e == nil {
		t.Fatal("short key accepted")
	}
	for _, b := range [][]byte{nil, {1, 2, 3}} {
		if _, e = c.Decrypt(b, "account:one"); e == nil {
			t.Fatal("short ciphertext accepted")
		}
	}
}
