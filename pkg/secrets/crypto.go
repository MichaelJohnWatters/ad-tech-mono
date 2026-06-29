package secrets

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strings"
)

// EncryptionKeyEnv names the env var holding the AES-256 key used to
// encrypt secret values at rest. The value is 32 bytes, encoded as 64
// hex chars or standard base64.
//
// Unset = encryption disabled: values are stored and read as plaintext.
// That's fine for local dev (one less thing to configure) but the prod /
// staging overlays MUST set it. A nil/disabled Cipher is a deliberate
// passthrough so the same code path works in both modes.
const EncryptionKeyEnv = "SECRETS_ENCRYPTION_KEY"

// encPrefix marks a stored value as ciphertext. Values WITHOUT this
// prefix are treated as legacy plaintext on read, so turning encryption
// on doesn't strand rows written before the key existed — they keep
// reading fine and get encrypted the next time they're rewritten. The
// "v1" leaves room to rotate the scheme (e.g. enc:v2: for a KMS-wrapped
// data key) without ambiguity.
const encPrefix = "enc:v1:"

// Cipher seals/opens secret values with AES-256-GCM. A nil *Cipher, or
// one built with no key, is a valid passthrough (plaintext) cipher — all
// methods are safe to call on it. This keeps callers free of nil checks
// and makes "encryption off" a first-class, explicit mode rather than a
// branch every call site has to remember.
type Cipher struct {
	aead cipher.AEAD // nil => disabled (passthrough)
}

// NewCipherFromEnv builds a Cipher from EncryptionKeyEnv. An unset env
// var yields a disabled (passthrough) cipher and no error — that's the
// supported local-dev mode. A SET-but-malformed key is an error: a
// misconfigured prod overlay should fail loudly, not silently fall back
// to plaintext.
func NewCipherFromEnv() (*Cipher, error) {
	raw := strings.TrimSpace(os.Getenv(EncryptionKeyEnv))
	if raw == "" {
		return &Cipher{}, nil
	}
	key, err := decodeKey(raw)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", EncryptionKeyEnv, err)
	}
	return NewCipher(key)
}

// NewCipher builds a Cipher from a raw 32-byte key. Exposed for tests and
// for callers that source the key from somewhere other than the env
// (e.g. a KMS-decrypted data key).
func NewCipher(key []byte) (*Cipher, error) {
	if len(key) != 32 {
		return nil, fmt.Errorf("key must be 32 bytes, got %d", len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("aes: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("gcm: %w", err)
	}
	return &Cipher{aead: aead}, nil
}

// Enabled reports whether the cipher will actually encrypt. False means
// passthrough (no key configured). Callers can use this to warn at boot
// that secrets are stored in the clear.
func (c *Cipher) Enabled() bool { return c != nil && c.aead != nil }

// Encrypt returns the at-rest representation of plaintext. With no key
// configured it returns plaintext unchanged (passthrough). With a key it
// returns "enc:v1:" + base64(nonce || ciphertext).
func (c *Cipher) Encrypt(plaintext string) (string, error) {
	if !c.Enabled() {
		return plaintext, nil
	}
	nonce := make([]byte, c.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", fmt.Errorf("nonce: %w", err)
	}
	sealed := c.aead.Seal(nonce, nonce, []byte(plaintext), nil)
	return encPrefix + base64.StdEncoding.EncodeToString(sealed), nil
}

// Decrypt reverses Encrypt. A value without the enc:v1: prefix is assumed
// to be legacy plaintext and returned as-is (so enabling encryption
// doesn't break pre-existing rows). An encrypted value with no key
// configured is an error rather than a silent leak.
func (c *Cipher) Decrypt(stored string) (string, error) {
	if !strings.HasPrefix(stored, encPrefix) {
		return stored, nil
	}
	if !c.Enabled() {
		return "", errors.New("value is encrypted but " + EncryptionKeyEnv + " is not set")
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(stored, encPrefix))
	if err != nil {
		return "", fmt.Errorf("base64: %w", err)
	}
	ns := c.aead.NonceSize()
	if len(raw) < ns {
		return "", errors.New("ciphertext too short")
	}
	nonce, ct := raw[:ns], raw[ns:]
	plain, err := c.aead.Open(nil, nonce, ct, nil)
	if err != nil {
		return "", fmt.Errorf("open: %w", err)
	}
	return string(plain), nil
}

// decodeKey accepts a 64-char hex string or a base64 (std/raw) string and
// returns the raw key bytes.
func decodeKey(s string) ([]byte, error) {
	if len(s) == 64 {
		if b, err := hex.DecodeString(s); err == nil {
			return b, nil
		}
	}
	if b, err := base64.StdEncoding.DecodeString(s); err == nil {
		return b, nil
	}
	if b, err := base64.RawStdEncoding.DecodeString(s); err == nil {
		return b, nil
	}
	return nil, errors.New("not valid hex or base64")
}
