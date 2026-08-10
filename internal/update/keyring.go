package update

import (
	"bytes"
	_ "embed"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/ProtonMail/go-crypto/openpgp"
)

// release-signing-key.asc is the maintainer's PGP public key, ASCII-armored.
// touches the repository; rotation requires shipping a new ibkr binary with
//
//go:embed release-signing-key.asc
var embeddedPublicKey []byte

// ReleaseSigningKeyFingerprint is the SHA-1 fingerprint of the embedded
// PGP public key, as printed by `gpg --fingerprint` with spaces stripped
// and upper-cased. Cross-checked against the parsed key's fingerprint at
// startup so a swapped .asc file at build time fails loud rather than
// silently accepting whatever key happens to be embedded.
//
// Rotation: when the signing key changes, update both the .asc file and
// this fingerprint in the same commit; TestEmbeddedKeyMatchesFingerprint
// (run by `make check`) catches any mismatch.
//
// Declared `var` rather than `const` only so tests in this package can
// swap it alongside embeddedPublicKey when exercising the wrong-key path.
// Treat as immutable in production code.
var ReleaseSigningKeyFingerprint = "D98426D48FED85EFA33904694D922A4F922B7D7D"

// ErrSignatureInvalid means the detached PGP signature did not verify
// Exposed as a typed sentinel so the install flow can fail with a clear
var ErrSignatureInvalid = errors.New("SHA256SUMS signature did not verify against the embedded release-signing key")

// ErrSignatureMissing means the .asc file was not delivered alongside the
// SHA256SUMS asset. Distinct from ErrSignatureInvalid so the CLI can hint
// "this release was published before signing was required (pre-v1.0.0)"
// vs "this release was tampered with."
var ErrSignatureMissing = errors.New("release SHA256SUMS.asc was not present alongside SHA256SUMS")

// EmbeddedKeyring parses the //go:embedded public key into a usable
// the runtime check fails closed.
func EmbeddedKeyring() (openpgp.EntityList, error) {
	if len(embeddedPublicKey) == 0 {
		return nil, errors.New("embedded release-signing key is empty (build asset missing or zero-byte)")
	}
	keys, err := openpgp.ReadArmoredKeyRing(bytes.NewReader(embeddedPublicKey))
	if err != nil {
		return nil, fmt.Errorf("parse embedded release-signing key: %w", err)
	}
	if len(keys) == 0 {
		return nil, errors.New("embedded release-signing key parsed to zero entities")
	}
	// Cross-check the fingerprint. openpgp returns the 20-byte SHA-1 fp;
	gotFp := strings.ToUpper(fmt.Sprintf("%x", keys[0].PrimaryKey.Fingerprint))
	if gotFp != ReleaseSigningKeyFingerprint {
		return nil, fmt.Errorf("embedded release-signing key fingerprint %s does not match pinned %s — refuse to trust", gotFp, ReleaseSigningKeyFingerprint)
	}
	return keys, nil
}

// VerifyDetachedSignature reports whether sig is a valid PGP detached
// signature over signed, produced by the embedded release-signing key.
// Both readers are fully consumed.
//
// Returns ErrSignatureInvalid on any verification failure (bad sig, wrong
// key, corrupted SHA256SUMS) — the underlying openpgp error is wrapped
// for diagnostics but the typed sentinel is what callers should check.
//
// The verifier accepts only signatures from the embedded key. A signature
// from any other key — even a perfectly valid one — fails with
// ErrSignatureInvalid because that key isn't in the keyring we pass.
func VerifyDetachedSignature(signed, sig io.Reader) error {
	keyring, err := EmbeddedKeyring()
	if err != nil {
		return err
	}
	// CheckArmoredDetachedSignature expects the signed payload + an
	if _, err := openpgp.CheckArmoredDetachedSignature(keyring, signed, sig, nil); err != nil {
		return fmt.Errorf("%w: %v", ErrSignatureInvalid, err)
	}
	return nil
}
