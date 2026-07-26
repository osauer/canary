package update

import (
	"bytes"
	"crypto"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/ProtonMail/go-crypto/openpgp/armor"
	"github.com/ProtonMail/go-crypto/openpgp/packet"
)

// testSigner is a throwaway PGP keypair generated per-test. Carries the
// helpers tests need: the armored public key (to swap into
// embeddedPublicKey), the fingerprint (to swap into the pinned
// constant), and a Sign method that emits a detached armored signature
// over arbitrary bytes — the same shape `gpg --detach-sign --armor`
// emits at release time.
type testSigner struct {
	entity      *openpgp.Entity
	armoredPub  []byte
	fingerprint string
}

// newTestSigner generates an ed25519 PGP entity, exports the public
// key in ASCII-armored form, and returns helpers a test can drop in
// alongside embeddedPublicKey / ReleaseSigningKeyFingerprint.
func newTestSigner(t *testing.T) *testSigner {
	t.Helper()
	// EdDSA algorithm matches the maintainer's ed25519 release key, so
	// signatures produced here exercise the same verification path the
	// release pipeline produces. Default curve is Ed25519.
	cfg := &packet.Config{Algorithm: packet.PubKeyAlgoEdDSA}
	entity, err := openpgp.NewEntity("ibkr test signer", "throwaway", "test@example.invalid", cfg)
	if err != nil {
		t.Fatalf("newTestSigner: NewEntity: %v", err)
	}

	buf := &bytes.Buffer{}
	w, err := armor.Encode(buf, openpgp.PublicKeyType, nil)
	if err != nil {
		t.Fatalf("newTestSigner: armor.Encode: %v", err)
	}
	if err := entity.Serialize(w); err != nil {
		t.Fatalf("newTestSigner: serialize public key: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("newTestSigner: close armor: %v", err)
	}

	fp := strings.ToUpper(fmt.Sprintf("%x", entity.PrimaryKey.Fingerprint))
	return &testSigner{
		entity:      entity,
		armoredPub:  buf.Bytes(),
		fingerprint: fp,
	}
}

// SignDetachedArmored produces an ASCII-armored detached PGP signature
// over msg using this signer's private key. Output matches what
// `gpg --detach-sign --armor` would write to SHA256SUMS.asc.
func (s *testSigner) SignDetachedArmored(t *testing.T, msg []byte) []byte {
	t.Helper()
	buf := &bytes.Buffer{}
	// openpgp.ArmoredDetachSign defaults to SHA-256 — matches the GPG
	// release-signing default and is what our verifier consumes.
	if err := openpgp.ArmoredDetachSign(buf, s.entity, bytes.NewReader(msg), &packet.Config{
		DefaultHash: crypto.SHA256,
	}); err != nil {
		t.Fatalf("SignDetachedArmored: %v", err)
	}
	return buf.Bytes()
}

// useTestKey swaps embeddedPublicKey + ReleaseSigningKeyFingerprint to
// the test signer's values for the duration of the test, restoring them
// on test cleanup. Tests that need to exercise verification against a
// non-maintainer key call this once at the top.
func useTestKey(t *testing.T, s *testSigner) {
	t.Helper()
	origKey := embeddedPublicKey
	origFp := ReleaseSigningKeyFingerprint
	embeddedPublicKey = s.armoredPub
	ReleaseSigningKeyFingerprint = s.fingerprint
	t.Cleanup(func() {
		embeddedPublicKey = origKey
		ReleaseSigningKeyFingerprint = origFp
	})
}

// TestEmbeddedKeyMatchesFingerprint is the build-time-swap canary: if
// someone replaces release-signing-key.asc without updating the pinned
// fingerprint (or vice versa), this fails loud. Run by `make check`.
func TestEmbeddedKeyMatchesFingerprint(t *testing.T) {
	keys, err := EmbeddedKeyring()
	if err != nil {
		t.Fatalf("EmbeddedKeyring(): %v", err)
	}
	if len(keys) != 1 {
		t.Fatalf("EmbeddedKeyring(): got %d entities, want 1", len(keys))
	}
	gotFp := strings.ToUpper(fmt.Sprintf("%x", keys[0].PrimaryKey.Fingerprint))
	if gotFp != ReleaseSigningKeyFingerprint {
		t.Fatalf("embedded key fingerprint %s != pinned %s — release-signing-key.asc and ReleaseSigningKeyFingerprint must rotate together", gotFp, ReleaseSigningKeyFingerprint)
	}
}

// TestVerifyDetachedSignature_Roundtrip is the happy path: a signature
// produced by the embedded key verifies against the embedded key.
func TestVerifyDetachedSignature_Roundtrip(t *testing.T) {
	signer := newTestSigner(t)
	useTestKey(t, signer)

	msg := []byte("abc123  canary-v1.0.0-darwin-arm64.tar.gz\ndef456  SHA256SUMS\n")
	sig := signer.SignDetachedArmored(t, msg)

	if err := VerifyDetachedSignature(bytes.NewReader(msg), bytes.NewReader(sig)); err != nil {
		t.Fatalf("VerifyDetachedSignature: unexpected error: %v", err)
	}
}

// TestVerifyDetachedSignature_Tampered modifies the signed content
// after signing; verification must reject with ErrSignatureInvalid.
func TestVerifyDetachedSignature_Tampered(t *testing.T) {
	signer := newTestSigner(t)
	useTestKey(t, signer)

	msg := []byte("abc123  canary-v1.0.0-darwin-arm64.tar.gz\n")
	sig := signer.SignDetachedArmored(t, msg)

	tampered := []byte("BADBAD  canary-v1.0.0-darwin-arm64.tar.gz\n")
	err := VerifyDetachedSignature(bytes.NewReader(tampered), bytes.NewReader(sig))
	if err == nil {
		t.Fatalf("VerifyDetachedSignature accepted tampered payload")
	}
	if !errors.Is(err, ErrSignatureInvalid) {
		t.Fatalf("err = %v, want errors.Is(err, ErrSignatureInvalid)", err)
	}
}

// TestVerifyDetachedSignature_WrongKey signs with one key, verifies
// against another (the embedded key path) — must fail.
func TestVerifyDetachedSignature_WrongKey(t *testing.T) {
	wrongSigner := newTestSigner(t) // generates a key
	rightSigner := newTestSigner(t) // generates a different key
	useTestKey(t, rightSigner)      // embedded key is rightSigner's

	msg := []byte("abc  canary-v1.0.0-linux-amd64.tar.gz\n")
	sig := wrongSigner.SignDetachedArmored(t, msg) // signed by wrongSigner

	err := VerifyDetachedSignature(bytes.NewReader(msg), bytes.NewReader(sig))
	if err == nil {
		t.Fatalf("VerifyDetachedSignature accepted signature from non-embedded key")
	}
	if !errors.Is(err, ErrSignatureInvalid) {
		t.Fatalf("err = %v, want errors.Is(err, ErrSignatureInvalid)", err)
	}
}

// TestVerifyDetachedSignature_GarbageSignature feeds non-PGP bytes as
// the signature; must fail (not crash) with ErrSignatureInvalid.
func TestVerifyDetachedSignature_GarbageSignature(t *testing.T) {
	signer := newTestSigner(t)
	useTestKey(t, signer)

	msg := []byte("abc  canary-v1.0.0-linux-amd64.tar.gz\n")
	garbage := []byte("this is not a PGP signature, just random bytes\n")

	err := VerifyDetachedSignature(bytes.NewReader(msg), bytes.NewReader(garbage))
	if err == nil {
		t.Fatalf("VerifyDetachedSignature accepted garbage signature")
	}
	if !errors.Is(err, ErrSignatureInvalid) {
		t.Fatalf("err = %v, want errors.Is(err, ErrSignatureInvalid)", err)
	}
}

// TestEmbeddedKeyring_FingerprintMismatchRejected covers the
// defence-in-depth pinning: if the constant is overridden to something
// the .asc file doesn't match, the keyring loader refuses.
func TestEmbeddedKeyring_FingerprintMismatchRejected(t *testing.T) {
	signer := newTestSigner(t)
	origKey := embeddedPublicKey
	origFp := ReleaseSigningKeyFingerprint
	embeddedPublicKey = signer.armoredPub
	// Wrong fingerprint — does not match the parsed key.
	ReleaseSigningKeyFingerprint = "DEADBEEFDEADBEEFDEADBEEFDEADBEEFDEADBEEF"
	t.Cleanup(func() {
		embeddedPublicKey = origKey
		ReleaseSigningKeyFingerprint = origFp
	})

	_, err := EmbeddedKeyring()
	if err == nil {
		t.Fatalf("EmbeddedKeyring accepted fingerprint mismatch")
	}
	if !strings.Contains(err.Error(), "fingerprint") {
		t.Fatalf("err = %v, want fingerprint-mismatch error", err)
	}
}

// TestEmbeddedKeyring_EmptyKeyRejected covers the zero-byte .asc case
// (e.g. a build that forgot to populate the embedded asset).
func TestEmbeddedKeyring_EmptyKeyRejected(t *testing.T) {
	origKey := embeddedPublicKey
	embeddedPublicKey = nil
	t.Cleanup(func() { embeddedPublicKey = origKey })

	_, err := EmbeddedKeyring()
	if err == nil {
		t.Fatalf("EmbeddedKeyring accepted empty key")
	}
}

// _ keeps io imported when the test list trims; harmless.
var _ = io.Discard
