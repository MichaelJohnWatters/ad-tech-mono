// Package pgp is the platform's audience-file decrypt-on-ingest helper (ADR
// 0008). Data providers PGP-encrypt files to the platform PUBLIC key; we
// decrypt on ingest with the PRIVATE key. There is one platform-wide keypair;
// the armored private key lives in the secrets store (purpose=pgp_private) and
// rotation-aware code loads every non-revoked private key into a keyring so an
// in-flight file encrypted to a rotating predecessor still decrypts.
//
// Library: github.com/ProtonMail/go-crypto/openpgp (maintained; the stdlib-
// adjacent golang.org/x/crypto/openpgp is frozen).
package pgp

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/ProtonMail/go-crypto/openpgp/armor"
	"github.com/ProtonMail/go-crypto/openpgp/packet"
)

// armorMessageHeader is the ASCII-armor preamble on an armored OpenPGP message.
const armorMessageHeader = "-----BEGIN PGP MESSAGE-----"

// Generate makes a fresh platform keypair and returns both halves ASCII-armored
// plus the primary-key fingerprint (uppercase hex). RSA 2048 — widely
// interoperable with providers' gpg toolchains. Seeded locally by cmd/seed;
// prod supplies the private key via SOPS/K8s Secret.
func Generate() (armoredPrivate, armoredPublic, fingerprint string, err error) {
	entity, err := openpgp.NewEntity("adtech platform ingest", "audience decrypt-on-ingest", "ingest@adtech.local", &packet.Config{RSABits: 2048})
	if err != nil {
		return "", "", "", fmt.Errorf("generate entity: %w", err)
	}
	priv, err := armorEntity(entity, openpgp.PrivateKeyType, func(w io.Writer) error {
		return entity.SerializePrivateWithoutSigning(w, nil)
	})
	if err != nil {
		return "", "", "", fmt.Errorf("armor private: %w", err)
	}
	pub, err := armorEntity(entity, openpgp.PublicKeyType, entity.Serialize)
	if err != nil {
		return "", "", "", fmt.Errorf("armor public: %w", err)
	}
	return priv, pub, fingerprintHex(entity), nil
}

// ParsePrivate reads one or more armored private keys into a keyring. Used to
// build the ingest keyring from the active + rotating pgp_private secrets.
// ParsePublic reads an armored PUBLIC key into a keyring — the encryption
// recipient set. Used by Encrypt (and provider tooling parity).
func ParsePublic(armored string) (openpgp.EntityList, error) {
	block, err := armor.Decode(strings.NewReader(armored))
	if err != nil {
		return nil, fmt.Errorf("decode armored public key: %w", err)
	}
	el, err := openpgp.ReadKeyRing(block.Body)
	if err != nil {
		return nil, fmt.Errorf("read public key: %w", err)
	}
	return el, nil
}

// Encrypt returns plaintext ASCII-armor-encrypted to the given armored public
// key — the operation a provider performs before upload. Exposed so tests (and
// any provider tooling we ship) produce exactly what MaybeDecrypt consumes.
func Encrypt(plaintext []byte, armoredPublic string) ([]byte, error) {
	recipients, err := ParsePublic(armoredPublic)
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	aw, err := armor.Encode(&buf, "PGP MESSAGE", nil)
	if err != nil {
		return nil, fmt.Errorf("armor encode: %w", err)
	}
	w, err := openpgp.Encrypt(aw, recipients, nil, nil, nil)
	if err != nil {
		return nil, fmt.Errorf("encrypt: %w", err)
	}
	if _, err := w.Write(plaintext); err != nil {
		return nil, err
	}
	if err := w.Close(); err != nil {
		return nil, err
	}
	if err := aw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func ParsePrivate(armored string) (openpgp.EntityList, error) {
	list, err := openpgp.ReadArmoredKeyRing(strings.NewReader(armored))
	if err != nil {
		return nil, fmt.Errorf("read private key: %w", err)
	}
	return list, nil
}

// PublicArmorFromPrivate derives the armored PUBLIC key + fingerprint from an
// armored PRIVATE key, so the platform never stores the public half separately —
// the public-key endpoint derives it on demand from the private secret.
func PublicArmorFromPrivate(armored string) (armoredPublic, fingerprint string, err error) {
	list, err := ParsePrivate(armored)
	if err != nil {
		return "", "", err
	}
	if len(list) == 0 {
		return "", "", errors.New("no key in armored private material")
	}
	entity := list[0]
	pub, err := armorEntity(entity, openpgp.PublicKeyType, entity.Serialize)
	if err != nil {
		return "", "", fmt.Errorf("armor public: %w", err)
	}
	return pub, fingerprintHex(entity), nil
}

// IsEncrypted reports whether body looks like OpenPGP data: ASCII armor
// (-----BEGIN PGP MESSAGE-----) OR a binary OpenPGP packet header (first byte
// has the high bit set, 0x80..0xFF — every OpenPGP packet tag byte does).
func IsEncrypted(body []byte) bool {
	if len(body) == 0 {
		return false
	}
	trimmed := bytes.TrimLeft(body, " \t\r\n")
	if bytes.HasPrefix(trimmed, []byte(armorMessageHeader)) {
		return true
	}
	return body[0]&0x80 != 0
}

// MaybeDecrypt is the decrypt seam in front of ADR 0007's decode. It detects
// whether body is OpenPGP and, if so, decrypts it with keyring:
//
//   - not OpenPGP        → (body, false, nil)  — plaintext passes through unchanged.
//   - OpenPGP + decrypts → (plaintext, true, nil).
//   - OpenPGP + can't    → (nil, true, err)  — encrypted to a key we don't hold,
//     no key configured (nil/empty keyring), or corrupt ciphertext. The caller
//     maps wasEncrypted+err to a content rejection.
func MaybeDecrypt(body []byte, keyring openpgp.EntityList) (plaintext []byte, wasEncrypted bool, err error) {
	if !IsEncrypted(body) {
		return body, false, nil
	}
	if len(keyring) == 0 {
		return nil, true, errors.New("no PGP private key configured")
	}

	reader := io.Reader(bytes.NewReader(body))
	if bytes.HasPrefix(bytes.TrimLeft(body, " \t\r\n"), []byte(armorMessageHeader)) {
		block, aerr := armor.Decode(bytes.NewReader(body))
		if aerr != nil {
			return nil, true, fmt.Errorf("decode armor: %w", aerr)
		}
		reader = block.Body
	}

	md, rerr := openpgp.ReadMessage(reader, keyring, nil, nil)
	if rerr != nil {
		return nil, true, fmt.Errorf("read pgp message: %w", rerr)
	}
	out, rerr := io.ReadAll(md.UnverifiedBody)
	if rerr != nil {
		return nil, true, fmt.Errorf("read pgp body: %w", rerr)
	}
	return out, true, nil
}

// armorEntity serializes an entity via serialize into an ASCII-armor block of
// blockType.
func armorEntity(_ *openpgp.Entity, blockType string, serialize func(io.Writer) error) (string, error) {
	var buf bytes.Buffer
	w, err := armor.Encode(&buf, blockType, nil)
	if err != nil {
		return "", err
	}
	if err := serialize(w); err != nil {
		_ = w.Close()
		return "", err
	}
	if err := w.Close(); err != nil {
		return "", err
	}
	return buf.String(), nil
}

// fingerprintHex renders the primary key fingerprint as uppercase hex.
func fingerprintHex(entity *openpgp.Entity) string {
	if entity == nil || entity.PrimaryKey == nil {
		return ""
	}
	return strings.ToUpper(fmt.Sprintf("%x", entity.PrimaryKey.Fingerprint))
}
