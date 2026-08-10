#!/bin/sh
# sign-testdata.sh — produce internal/update/testdata/sample-sha256sums.asc.
#
# Signs the checked-in `sample-sha256sums` fixture with the maintainer's
# current release-signing key (fingerprint pinned in keyring.go) and
# writes the detached armored signature next to it.
#
# Run once at v1.0 setup, and again only when the signing key rotates.

set -eu

cd "$(git rev-parse --show-toplevel)"

FP=$(awk -F\" '/ReleaseSigningKeyFingerprint =/{print $2; exit}' \
    internal/update/keyring.go)
if [ -z "$FP" ]; then
    echo "sign-testdata: could not extract fingerprint from internal/update/keyring.go" >&2
    exit 1
fi
echo "==> signing fixture with maintainer key $FP"

if ! gpg --list-secret-keys --with-colons "$FP" >/dev/null 2>&1; then
    echo "sign-testdata: secret key $FP not in local gpg keyring — see SECURITY.md for key generation" >&2
    exit 1
fi

IN=internal/update/testdata/sample-sha256sums
OUT=internal/update/testdata/sample-sha256sums.asc

if [ ! -f "$IN" ]; then
    echo "sign-testdata: $IN missing — fixture should be checked in" >&2
    exit 1
fi

gpg --batch --yes \
    --local-user "$FP" \
    --armor --detach-sign \
    --output "$OUT" \
    "$IN"

echo "==> self-verify with gpg"
gpg --verify "$OUT" "$IN"

echo "==> running Go-side verifier (the real test the fixture exists for)"
go test -run TestEmbeddedKeyAcceptsRealGPGSignature -v ./internal/update/

echo
echo "Signature fixture refreshed. Stage and commit:"
echo "  git add $OUT && git commit -m 'test(update): refresh real-GPG signed SHA256SUMS fixture'"
