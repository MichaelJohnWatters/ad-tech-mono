package pgp

import (
	"bytes"
	"io"
	"strings"
	"testing"

	"github.com/ProtonMail/go-crypto/openpgp"
)

// encryptTo armors a plaintext to the given public keyring, for the round-trip
// tests. Mirrors what a provider's `gpg --encrypt --armor` produces.
func encryptTo(t *testing.T, plaintext []byte, pub openpgp.EntityList) []byte {
	t.Helper()
	var buf bytes.Buffer
	w, err := openpgp.Encrypt(&buf, pub, nil, nil, nil)
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	if _, err := w.Write(plaintext); err != nil {
		t.Fatalf("write plaintext: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	return buf.Bytes()
}

func keyringFromPrivate(t *testing.T, armored string) openpgp.EntityList {
	t.Helper()
	kr, err := ParsePrivate(armored)
	if err != nil {
		t.Fatalf("parse private: %v", err)
	}
	return kr
}

func publicKeyring(t *testing.T, armoredPub string) openpgp.EntityList {
	t.Helper()
	kr, err := openpgp.ReadArmoredKeyRing(strings.NewReader(armoredPub))
	if err != nil {
		t.Fatalf("read public: %v", err)
	}
	return kr
}

func TestGenerateAndDerivePublic(t *testing.T) {
	priv, pub, fp, err := Generate()
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if !strings.Contains(priv, "PGP PRIVATE KEY BLOCK") {
		t.Fatalf("private key not armored: %q", priv[:40])
	}
	if !strings.Contains(pub, "PGP PUBLIC KEY BLOCK") {
		t.Fatalf("public key not armored: %q", pub[:40])
	}
	if fp == "" {
		t.Fatal("empty fingerprint")
	}
	// Public derived from the private must match fingerprint.
	derivedPub, derivedFP, err := PublicArmorFromPrivate(priv)
	if err != nil {
		t.Fatalf("derive public: %v", err)
	}
	if derivedFP != fp {
		t.Fatalf("fingerprint mismatch: %s vs %s", derivedFP, fp)
	}
	if !strings.Contains(derivedPub, "PGP PUBLIC KEY BLOCK") {
		t.Fatal("derived public not armored")
	}
}

func TestMaybeDecryptArmoredRoundTrip(t *testing.T) {
	priv, pub, _, err := Generate()
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	plaintext := []byte("user_id,id_type\nabc123,hashed_email\ndef456,hashed_email\n")
	ciphertext := encryptTo(t, plaintext, publicKeyring(t, pub))

	got, wasEnc, err := MaybeDecrypt(ciphertext, keyringFromPrivate(t, priv))
	if err != nil {
		t.Fatalf("MaybeDecrypt: %v", err)
	}
	if !wasEnc {
		t.Fatal("wasEncrypted should be true for a PGP message")
	}
	if !bytes.Equal(got, plaintext) {
		t.Fatalf("round-trip mismatch:\n got %q\nwant %q", got, plaintext)
	}
}

func TestMaybeDecryptBinaryRoundTrip(t *testing.T) {
	priv, pub, _, err := Generate()
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	plaintext := []byte("hello binary pgp")

	// Binary (non-armored) OpenPGP message.
	var buf bytes.Buffer
	w, err := openpgp.Encrypt(&buf, publicKeyring(t, pub), nil, nil, nil)
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	if _, err := io.Copy(w, bytes.NewReader(plaintext)); err != nil {
		t.Fatalf("copy: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	ciphertext := buf.Bytes()
	if ciphertext[0]&0x80 == 0 {
		t.Fatalf("expected binary packet header high bit set, got 0x%02x", ciphertext[0])
	}

	got, wasEnc, err := MaybeDecrypt(ciphertext, keyringFromPrivate(t, priv))
	if err != nil {
		t.Fatalf("MaybeDecrypt: %v", err)
	}
	if !wasEnc {
		t.Fatal("wasEncrypted should be true")
	}
	if !bytes.Equal(got, plaintext) {
		t.Fatalf("binary round-trip mismatch: got %q want %q", got, plaintext)
	}
}

func TestMaybeDecryptPlaintextPassthrough(t *testing.T) {
	priv, _, _, err := Generate()
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	plaintext := []byte("user_id,id_type\nabc,hashed_email\n")
	got, wasEnc, err := MaybeDecrypt(plaintext, keyringFromPrivate(t, priv))
	if err != nil {
		t.Fatalf("MaybeDecrypt: %v", err)
	}
	if wasEnc {
		t.Fatal("plaintext CSV should not be flagged encrypted")
	}
	if !bytes.Equal(got, plaintext) {
		t.Fatal("plaintext should pass through unchanged")
	}
}

func TestMaybeDecryptWrongKeyErrors(t *testing.T) {
	_, pubA, _, err := Generate()
	if err != nil {
		t.Fatalf("generate A: %v", err)
	}
	privB, _, _, err := Generate()
	if err != nil {
		t.Fatalf("generate B: %v", err)
	}
	// Encrypt to A's public key, try to decrypt with B's private key.
	ciphertext := encryptTo(t, []byte("secret"), publicKeyring(t, pubA))
	_, wasEnc, err := MaybeDecrypt(ciphertext, keyringFromPrivate(t, privB))
	if !wasEnc {
		t.Fatal("wasEncrypted should be true even when decrypt fails")
	}
	if err == nil {
		t.Fatal("expected error decrypting to a key we don't hold")
	}
}

func TestMaybeDecryptNoKeyConfigured(t *testing.T) {
	_, pub, _, err := Generate()
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	ciphertext := encryptTo(t, []byte("secret"), publicKeyring(t, pub))
	_, wasEnc, err := MaybeDecrypt(ciphertext, nil)
	if !wasEnc {
		t.Fatal("wasEncrypted should be true")
	}
	if err == nil {
		t.Fatal("expected error when no keyring is configured")
	}
}
