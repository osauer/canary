package update

import (
	"bytes"
	"crypto/ed25519"
	"crypto/x509"
	_ "embed"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
)

//go:embed release-signing-key.ed25519.pem
var embeddedEd25519PublicKeyPEM []byte

// ReleaseEd25519PublicKeySHA256 identifies the compact release-signing public
// key as the SHA-256 of its PKIX DER encoding.
const ReleaseEd25519PublicKeySHA256 = "c9a8685a83c8d8584c1469f6f03973943e439f4aa2485468ffcda5a5db8c5578"

// ErrEd25519SignatureInvalid means SHA256SUMS.ed25519 did not verify against
// the compact release-signing key pinned in this binary.
var ErrEd25519SignatureInvalid = errors.New("SHA256SUMS Ed25519 signature did not verify against the embedded release-signing key")

// ErrEd25519SignatureMissing means a release cannot be consumed by current
// Canary updaters. Older binaries use the separately published PGP signature;
// current binaries deliberately do not carry that compatibility verifier.
var ErrEd25519SignatureMissing = errors.New("release is missing SHA256SUMS.ed25519")

func embeddedEd25519PublicKey() (ed25519.PublicKey, error) {
	if len(embeddedEd25519PublicKeyPEM) == 0 {
		return nil, errors.New("embedded Ed25519 release-signing key is not provisioned")
	}
	block, rest := pem.Decode(embeddedEd25519PublicKeyPEM)
	if block == nil || block.Type != "PUBLIC KEY" || len(bytes.TrimSpace(rest)) != 0 {
		return nil, errors.New("embedded Ed25519 release-signing key must contain exactly one PKIX PUBLIC KEY PEM block")
	}
	parsed, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse embedded Ed25519 release-signing key: %w", err)
	}
	publicKey, ok := parsed.(ed25519.PublicKey)
	if !ok {
		return nil, errors.New("embedded compact release-signing key is not Ed25519")
	}
	return publicKey, nil
}

// VerifyEd25519Signature verifies the base64-encoded detached signature over
// signed. Both inputs are bounded because they arrive from a release endpoint.
func VerifyEd25519Signature(signed, signature io.Reader) error {
	publicKey, err := embeddedEd25519PublicKey()
	if err != nil {
		return err
	}
	message, err := readEd25519Input(signed, 1<<20, "SHA256SUMS")
	if err != nil {
		return err
	}
	encoded, err := readEd25519Input(signature, 1024, "SHA256SUMS.ed25519")
	if err != nil {
		return err
	}
	raw, err := base64.RawStdEncoding.DecodeString(string(bytes.TrimSpace(encoded)))
	if err != nil || len(raw) != ed25519.SignatureSize {
		return ErrEd25519SignatureInvalid
	}
	if !ed25519.Verify(publicKey, message, raw) {
		return ErrEd25519SignatureInvalid
	}
	return nil
}

func readEd25519Input(r io.Reader, limit int64, name string) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", name, err)
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("read %s: input exceeds %d bytes", name, limit)
	}
	return data, nil
}
