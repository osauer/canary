package update

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/pem"
	"strings"
	"testing"
)

func TestEmbeddedEd25519PublicKeyFingerprint(t *testing.T) {
	publicKey, err := embeddedEd25519PublicKey()
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKIXPublicKey(publicKey)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(der)
	if got := hex.EncodeToString(digest[:]); got != ReleaseEd25519PublicKeySHA256 {
		t.Fatalf("public key fingerprint = %s, want %s", got, ReleaseEd25519PublicKeySHA256)
	}
}

func TestVerifyEd25519SignatureRFC8032Vector(t *testing.T) {
	publicKey, err := hex.DecodeString("d75a980182b10ab7d54bfed3c964073a0ee172f3daa62325af021a68f707511a")
	if err != nil {
		t.Fatal(err)
	}
	signature, err := hex.DecodeString("e5564300c360ac729086e2cc806e828a84877f1eb8e5d974d873e065224901555fb8821590a33bacc61e39701cf9b46bd25bf5f0595bbe24655141438e7a100b")
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKIXPublicKey(ed25519.PublicKey(publicKey))
	if err != nil {
		t.Fatal(err)
	}
	original := embeddedEd25519PublicKeyPEM
	embeddedEd25519PublicKeyPEM = pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der})
	t.Cleanup(func() { embeddedEd25519PublicKeyPEM = original })

	encoded := base64.RawStdEncoding.EncodeToString(signature)
	if err := VerifyEd25519Signature(strings.NewReader(""), strings.NewReader(encoded)); err != nil {
		t.Fatalf("RFC 8032 test vector did not verify: %v", err)
	}
}

func TestVerifyEd25519SignatureRejectsOversizedInputs(t *testing.T) {
	if err := VerifyEd25519Signature(bytes.NewReader(make([]byte, (1<<20)+1)), bytes.NewReader(nil)); err == nil {
		t.Fatal("accepted oversized SHA256SUMS")
	}
	if err := VerifyEd25519Signature(bytes.NewReader(nil), bytes.NewReader(make([]byte, 1025))); err == nil {
		t.Fatal("accepted oversized compact signature")
	}
}
