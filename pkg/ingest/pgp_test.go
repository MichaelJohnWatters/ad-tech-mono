package ingest

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/ProtonMail/go-crypto/openpgp"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/ingestjobs"
	localpgp "github.com/MichaelJohnWatters/ad-tech-mono/pkg/pgp"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/pipeline"
)

// encryptCSV armors a CSV to the given armored public key, as a provider would.
func encryptCSV(t *testing.T, csv, armoredPub string) []byte {
	t.Helper()
	pub, err := openpgp.ReadArmoredKeyRing(strings.NewReader(armoredPub))
	if err != nil {
		t.Fatalf("read public: %v", err)
	}
	var buf bytes.Buffer
	w, err := openpgp.Encrypt(&buf, pub, nil, nil, nil)
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	if _, err := io.WriteString(w, csv); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	return buf.Bytes()
}

func procWithKeyring(t *testing.T) (*Processor, string) {
	t.Helper()
	priv, pub, _, err := localpgp.Generate()
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	kr, err := localpgp.ParsePrivate(priv)
	if err != nil {
		t.Fatalf("parse private: %v", err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	return &Processor{Pipeline: pipeline.New(log), Log: log, PGPKeyring: kr}, pub
}

// A PGP-encrypted CSV that decrypts to a valid id file passes ValidateSample.
func TestValidateSampleDecryptsPGP(t *testing.T) {
	p, pub := procWithKeyring(t)
	ciphertext := encryptCSV(t, "user_id\nu1\nu2\n", pub)
	if err := p.ValidateSample(context.Background(), "audience.csv.pgp", ciphertext, ingestjobs.SegmentSpec{}, 10); err != nil {
		t.Fatalf("expected decrypt+accept, got: %v", err)
	}
}

// Plaintext still validates exactly as before (ADR 0007 unchanged).
func TestValidateSamplePlaintextUnchanged(t *testing.T) {
	p, _ := procWithKeyring(t)
	if err := p.ValidateSample(context.Background(), "audience.csv", []byte("user_id\nu1\n"), ingestjobs.SegmentSpec{}, 10); err != nil {
		t.Fatalf("expected plaintext accept, got: %v", err)
	}
}

// A PGP file with no keyring configured is rejected (content reject, not infra).
func TestValidateSamplePGPNoKeyRejects(t *testing.T) {
	// Build ciphertext with one key, then validate with a processor that has NO
	// keyring.
	_, pub := procWithKeyring(t)
	ciphertext := encryptCSV(t, "user_id\nu1\n", pub)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	noKey := &Processor{Pipeline: pipeline.New(log), Log: log}
	err := noKey.ValidateSample(context.Background(), "audience.csv.pgp", ciphertext, ingestjobs.SegmentSpec{}, 10)
	if err == nil {
		t.Fatal("expected reject when PGP file has no key configured")
	}
	if !IsReject(err) {
		t.Fatalf("expected rejectErr, got %T: %v", err, err)
	}
}

// A PGP file encrypted to a DIFFERENT key is rejected (wrong-key content reject).
func TestValidateSamplePGPWrongKeyRejects(t *testing.T) {
	p, _ := procWithKeyring(t) // p holds key A
	_, pubB, _, err := localpgp.Generate()
	if err != nil {
		t.Fatalf("generate B: %v", err)
	}
	ciphertext := encryptCSV(t, "user_id\nu1\n", pubB) // encrypted to B
	err = p.ValidateSample(context.Background(), "audience.csv.pgp", ciphertext, ingestjobs.SegmentSpec{}, 10)
	if err == nil {
		t.Fatal("expected reject decrypting to a key we don't hold")
	}
	if !IsReject(err) {
		t.Fatalf("expected rejectErr, got %T: %v", err, err)
	}
}
