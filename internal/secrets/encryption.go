package secrets

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
)

type Cipher struct{ aead cipher.AEAD }

func New(key []byte) (*Cipher, error) {
	if len(key) != 32 {
		return nil, errors.New("secret key must be exactly 32 bytes")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	a, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &Cipher{a}, nil
}
func ParseKey(value string) ([]byte, error) {
	if len(value) == 32 {
		return []byte(value), nil
	}
	key, err := base64.StdEncoding.DecodeString(value)
	if err != nil || len(key) != 32 {
		return nil, errors.New("MCP_SECRET_KEY must be 32 raw bytes or base64 encoding of 32 bytes")
	}
	return key, nil
}

// The purpose binds ciphertext to its account or cursor format to prevent substitution.
func (c *Cipher) Encrypt(plain []byte, purpose string) ([]byte, error) {
	nonce := make([]byte, c.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return c.aead.Seal(nonce, nonce, plain, []byte(purpose)), nil
}
func (c *Cipher) Decrypt(blob []byte, purpose string) ([]byte, error) {
	n := c.aead.NonceSize()
	if len(blob) < n+c.aead.Overhead() {
		return nil, errors.New("invalid encrypted value")
	}
	out, err := c.aead.Open(nil, blob[:n], blob[n:], []byte(purpose))
	if err != nil {
		return nil, errors.New("encrypted value authentication failed")
	}
	return out, nil
}
