package secrets

import (
	"crypto/rand"
	"encoding/base64"
	"strings"
	"testing"
)

func newKey(t *testing.T) string {
	t.Helper()
	k := make([]byte, 32)
	if _, err := rand.Read(k); err != nil {
		t.Fatal(err)
	}
	return base64.StdEncoding.EncodeToString(k)
}

func TestRoundTrip(t *testing.T) {
	c, err := New(newKey(t))
	if err != nil {
		t.Fatal(err)
	}
	const plain = "EVOLUTION-API-KEY-super-secreta-123"
	enc, err := c.Encrypt(plain)
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	if strings.Count(enc, ":") != 2 {
		t.Fatalf("formato esperado iv:tag:ciphertext, got %q", enc)
	}
	got, err := c.Decrypt(enc)
	if err != nil {
		t.Fatalf("decrypt: %v", err)
	}
	if got != plain {
		t.Fatalf("round-trip = %q, want %q", got, plain)
	}
}

func TestDecryptRejectsTamper(t *testing.T) {
	c, _ := New(newKey(t))
	enc, _ := c.Encrypt("x")
	parts := strings.Split(enc, ":")
	// Flip the ciphertext → GCM auth must fail.
	parts[2] = base64.StdEncoding.EncodeToString([]byte("tampered-ciphertext"))
	if _, err := c.Decrypt(strings.Join(parts, ":")); err == nil {
		t.Fatal("esperava falha de autenticação ao adulterar o ciphertext")
	}
}

func TestKeyValidation(t *testing.T) {
	if _, err := New("not-base64-or-wrong-size"); err == nil {
		t.Fatal("esperava erro para chave inválida")
	}
}
