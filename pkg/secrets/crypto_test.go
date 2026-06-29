package secrets

import (
	"strings"
	"testing"
)

func testKey() []byte {
	k := make([]byte, 32)
	for i := range k {
		k[i] = byte(i)
	}
	return k
}

func TestCipher_RoundTrip(t *testing.T) {
	c, err := NewCipher(testKey())
	if err != nil {
		t.Fatalf("NewCipher: %v", err)
	}
	plain := "dev-ops-key-abc123"
	enc, err := c.Encrypt(plain)
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	if !strings.HasPrefix(enc, encPrefix) {
		t.Fatalf("ciphertext missing %q prefix: %q", encPrefix, enc)
	}
	if strings.Contains(enc, plain) {
		t.Fatalf("plaintext leaked into ciphertext: %q", enc)
	}
	got, err := c.Decrypt(enc)
	if err != nil {
		t.Fatalf("Decrypt: %v", err)
	}
	if got != plain {
		t.Fatalf("round-trip = %q, want %q", got, plain)
	}
}

func TestCipher_NonceIsRandom(t *testing.T) {
	c, _ := NewCipher(testKey())
	a, _ := c.Encrypt("same")
	b, _ := c.Encrypt("same")
	if a == b {
		t.Fatalf("identical ciphertext for repeated plaintext — nonce not random")
	}
}

func TestCipher_DisabledIsPassthrough(t *testing.T) {
	var c *Cipher = &Cipher{} // no key
	enc, err := c.Encrypt("hello")
	if err != nil || enc != "hello" {
		t.Fatalf("disabled Encrypt = %q, %v; want passthrough", enc, err)
	}
	dec, err := c.Decrypt("hello")
	if err != nil || dec != "hello" {
		t.Fatalf("disabled Decrypt = %q, %v; want passthrough", dec, err)
	}
	if c.Enabled() {
		t.Fatalf("Enabled() = true for keyless cipher")
	}
}

func TestCipher_NilReceiverIsPassthrough(t *testing.T) {
	var c *Cipher
	if enc, err := c.Encrypt("x"); err != nil || enc != "x" {
		t.Fatalf("nil Encrypt = %q, %v", enc, err)
	}
	if dec, err := c.Decrypt("x"); err != nil || dec != "x" {
		t.Fatalf("nil Decrypt = %q, %v", dec, err)
	}
}

func TestCipher_LegacyPlaintextReadsThrough(t *testing.T) {
	// A row written before encryption was enabled has no prefix; an
	// enabled cipher must still return it verbatim.
	c, _ := NewCipher(testKey())
	got, err := c.Decrypt("legacy-plaintext-value")
	if err != nil {
		t.Fatalf("Decrypt legacy: %v", err)
	}
	if got != "legacy-plaintext-value" {
		t.Fatalf("legacy = %q, want verbatim", got)
	}
}

func TestCipher_EncryptedValueWithoutKeyErrors(t *testing.T) {
	c, _ := NewCipher(testKey())
	enc, _ := c.Encrypt("secret")

	var disabled *Cipher
	if _, err := disabled.Decrypt(enc); err == nil {
		t.Fatalf("expected error decrypting ciphertext with no key, got nil")
	}
}

func TestCipher_WrongKeyFails(t *testing.T) {
	c1, _ := NewCipher(testKey())
	enc, _ := c1.Encrypt("secret")

	other := make([]byte, 32)
	for i := range other {
		other[i] = byte(255 - i)
	}
	c2, _ := NewCipher(other)
	if _, err := c2.Decrypt(enc); err == nil {
		t.Fatalf("expected auth failure decrypting with wrong key, got nil")
	}
}

func TestCipher_TamperDetected(t *testing.T) {
	c, _ := NewCipher(testKey())
	enc, _ := c.Encrypt("secret")
	// Flip a character in the base64 body.
	body := []byte(enc)
	last := len(body) - 1
	if body[last] == 'A' {
		body[last] = 'B'
	} else {
		body[last] = 'A'
	}
	if _, err := c.Decrypt(string(body)); err == nil {
		t.Fatalf("expected error on tampered ciphertext, got nil")
	}
}

func TestNewCipher_RejectsShortKey(t *testing.T) {
	if _, err := NewCipher([]byte("too-short")); err == nil {
		t.Fatalf("expected error for short key")
	}
}

func TestDecodeKey_HexAndBase64(t *testing.T) {
	// 32 bytes as 64 hex chars.
	hexKey := strings.Repeat("ab", 32)
	if b, err := decodeKey(hexKey); err != nil || len(b) != 32 {
		t.Fatalf("hex decode = %d bytes, %v", len(b), err)
	}
	// 32 bytes as base64.
	c, _ := NewCipher(testKey())
	_ = c
}
