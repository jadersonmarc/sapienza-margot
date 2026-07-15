// Package secrets encrypts per-tenant secrets at rest with AES-256-GCM. The
// wire format is "iv:tag:ciphertext" (each base64), byte-for-byte compatible
// with spa-sapienza/lib/agent/crypto.ts, so a secret encrypted in the console
// (TypeScript) can be decrypted here (Go) and vice-versa when the same key is used.
package secrets

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
)

// Cipher encrypts/decrypts with a fixed 32-byte key.
type Cipher struct {
	key []byte
}

// New builds a Cipher from a base64-encoded 32-byte key (as MARGOT_ENC_KEY).
func New(keyB64 string) (*Cipher, error) {
	key, err := base64.StdEncoding.DecodeString(keyB64)
	if err != nil {
		return nil, fmt.Errorf("decode key: %w", err)
	}
	if len(key) != 32 {
		return nil, fmt.Errorf("key must be 32 bytes (base64); got %d", len(key))
	}
	return &Cipher{key: key}, nil
}

// Encrypt returns "iv:tag:ciphertext" (base64 parts). Uses a 12-byte GCM nonce
// and appends the 16-byte tag separately (matching the TS format).
func (c *Cipher) Encrypt(plain string) (string, error) {
	block, err := aes.NewCipher(c.key)
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	iv := make([]byte, gcm.NonceSize()) // 12 bytes
	if _, err := rand.Read(iv); err != nil {
		return "", err
	}
	sealed := gcm.Seal(nil, iv, []byte(plain), nil) // ciphertext || tag
	tagStart := len(sealed) - gcm.Overhead()
	ct, tag := sealed[:tagStart], sealed[tagStart:]
	enc := func(b []byte) string { return base64.StdEncoding.EncodeToString(b) }
	return enc(iv) + ":" + enc(tag) + ":" + enc(ct), nil
}

// Decrypt reverses Encrypt.
func (c *Cipher) Decrypt(enc string) (string, error) {
	parts := strings.Split(enc, ":")
	if len(parts) != 3 {
		return "", errors.New("segredo cifrado inválido (esperado iv:tag:ciphertext)")
	}
	iv, err := base64.StdEncoding.DecodeString(parts[0])
	if err != nil {
		return "", fmt.Errorf("decode iv: %w", err)
	}
	tag, err := base64.StdEncoding.DecodeString(parts[1])
	if err != nil {
		return "", fmt.Errorf("decode tag: %w", err)
	}
	ct, err := base64.StdEncoding.DecodeString(parts[2])
	if err != nil {
		return "", fmt.Errorf("decode ciphertext: %w", err)
	}
	block, err := aes.NewCipher(c.key)
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	plain, err := gcm.Open(nil, iv, append(ct, tag...), nil)
	if err != nil {
		return "", fmt.Errorf("gcm open: %w", err)
	}
	return string(plain), nil
}
