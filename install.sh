#!/bin/sh
# Canary installer — one-shot binary install for Darwin and Linux.
#
#   curl -fsSL https://raw.githubusercontent.com/osauer/canary/main/install.sh | sh
#
# Detects your OS/arch, downloads the matching pre-built tarball from the
# latest GitHub release, verifies the SHA-256 checksum, installs the binary

set -eu

REPO="osauer/canary"
INSTALL_DIR="${CANARY_INSTALL_DIR:-$HOME/.local/bin}"

# --- pretty printing ---------------------------------------------------------
if [ -t 1 ] && [ -z "${NO_COLOR:-}" ]; then
	BOLD=$(printf '\033[1m')
	GREEN=$(printf '\033[32m')
	YELLOW=$(printf '\033[33m')
	RED=$(printf '\033[31m')
	DIM=$(printf '\033[2m')
	RESET=$(printf '\033[0m')
else
	BOLD=""; GREEN=""; YELLOW=""; RED=""; DIM=""; RESET=""
fi

info()  { printf '%s==>%s %s\n' "$GREEN" "$RESET" "$*"; }
warn()  { printf '%s!!%s %s\n' "$YELLOW" "$RESET" "$*" >&2; }
fail()  { printf '%sERROR:%s %s\n' "$RED" "$RESET" "$*" >&2; exit 1; }
step()  { printf '%s%s%s\n' "$DIM" "$*" "$RESET"; }

if [ "${IBKR_INSTALL_DIR+x}" = "x" ]; then
	fail "IBKR_INSTALL_DIR was retired by the Canary rename; use CANARY_INSTALL_DIR"
fi

# --- prereqs -----------------------------------------------------------------
command -v curl >/dev/null 2>&1 || fail "curl is required but not on PATH"
command -v tar  >/dev/null 2>&1 || fail "tar is required but not on PATH"

# Pick a checksum verifier — macOS has shasum, most Linux distros have
# sha256sum. We need one or the other.
if command -v shasum >/dev/null 2>&1; then
	SHA256_CMD="shasum -a 256"
elif command -v sha256sum >/dev/null 2>&1; then
	SHA256_CMD="sha256sum"
else
	fail "need shasum (macOS) or sha256sum (linux) to verify the download"
fi

# --- platform detection ------------------------------------------------------
os=$(uname -s)
arch=$(uname -m)

case "$os" in
	Darwin) os=darwin ;;
	Linux)  os=linux ;;
	MINGW*|MSYS*|CYGWIN*)
		fail "Windows is not supported — the daemon uses Unix-only primitives. Try WSL." ;;
	*)
		fail "unsupported OS: $os (need Darwin or Linux)" ;;
esac

case "$arch" in
	arm64|aarch64) arch=arm64 ;;
	x86_64|amd64)  arch=amd64 ;;
	*)
		fail "unsupported architecture: $arch (need arm64 or amd64)" ;;
esac

PLATFORM="${os}-${arch}"
info "Platform detected: $BOLD$PLATFORM$RESET"

# --- resolve latest release tag ---------------------------------------------
# Trick: curl -I against /releases/latest follows the redirect; the final URL
# ends in the version tag (e.g. .../tag/v0.6.2). No API call, no rate limit.
step "Looking up latest release..."
final_url=$(curl -fsSLI -o /dev/null -w '%{url_effective}' "https://github.com/${REPO}/releases/latest")
VERSION=$(printf '%s' "$final_url" | sed 's|.*/||')

case "$VERSION" in
	v[0-9]*) : ;;
	*) fail "could not resolve latest release tag (got '$VERSION' from $final_url)" ;;
esac
info "Latest version:    $BOLD$VERSION$RESET"

# --- download tarball + checksums into a scratch dir ------------------------
# first Canary-named release exists. The latest release at that boundary is
# exactly v2.3.1 and publishes only the pre-rename archive shape. Accept that
# is deliberately not a general legacy-asset fallback.
archive_product="canary"
archive_binary="canary"
if [ "$VERSION" = "v2.3.1" ]; then
	archive_product="ibkr"
	archive_binary="ibkr"
	warn "Using the version-bounded v2.3.1 bootstrap archive; rerun this installer after the first Canary release"
fi
TARBALL="${archive_product}-${VERSION}-${PLATFORM}.tar.gz"
TARBALL_URL="https://github.com/${REPO}/releases/download/${VERSION}/${TARBALL}"
SUMS_URL="https://github.com/${REPO}/releases/download/${VERSION}/SHA256SUMS"

tmp=$(mktemp -d -t canary-install.XXXXXX)
canonical_stage=""
canonical_retired=""
pre_upgrade_retired=""
install_started=0
install_committed=0
install_rolled_back=0
canonical_published=0

copy_executable() {
	source_path="$1"
	dest_path="$2"
	if command -v install >/dev/null 2>&1; then
		install -m 0755 "$source_path" "$dest_path"
	else
		cp "$source_path" "$dest_path"
		chmod 0755 "$dest_path"
	fi
}

rollback_install() {
	[ "$install_started" = "1" ] || return 0
	[ "$install_committed" = "0" ] || return 0
	[ "$install_rolled_back" = "0" ] || return 0
	install_rolled_back=1

	if [ "$canonical_published" = "1" ]; then
		rm -f "$canonical"
	fi
	if [ -n "$canonical_retired" ] && [ -e "$canonical_retired" ]; then
		if ! chmod 0755 "$canonical_retired" || ! mv -f "$canonical_retired" "$canonical"; then
			warn "automatic rollback could not restore canonical executable $canonical"
			return 1
		fi
		canonical_retired=""
	fi
	if [ -n "$pre_upgrade_retired" ] && [ -e "$pre_upgrade_retired" ]; then
		if ! chmod 0755 "$pre_upgrade_retired" || ! mv -f "$pre_upgrade_retired" "$pre_upgrade"; then
			warn "automatic rollback could not restore pre-upgrade executable $pre_upgrade"
			return 1
		fi
		pre_upgrade_retired=""
	fi
	return 0
}

cleanup() {
	if [ "$install_started" = "1" ] && [ "$install_committed" = "0" ]; then
		rollback_install || true
	fi
	rm -rf "$tmp"
	[ -z "$canonical_stage" ] || rm -f "$canonical_stage"
	if [ -n "$canonical_retired" ]; then
		chmod 0600 "$canonical_retired" 2>/dev/null || true
		rm -f "$canonical_retired"
	fi
	if [ -n "$pre_upgrade_retired" ]; then
		chmod 0600 "$pre_upgrade_retired" 2>/dev/null || true
		rm -f "$pre_upgrade_retired"
	fi
}
trap cleanup EXIT
trap 'rollback_install || true; exit 130' HUP INT TERM

SIG_URL="https://github.com/${REPO}/releases/download/${VERSION}/SHA256SUMS.asc"
KEY_URL="https://raw.githubusercontent.com/${REPO}/${VERSION}/internal/update/release-signing-key.asc"
EXPECTED_FP="D98426D48FED85EFA33904694D922A4F922B7D7D"
release_major=$(printf '%s' "$VERSION" | sed -E 's/^v([0-9]+).*/\1/')
require_sig=0
if [ "$release_major" -ge 1 ] 2>/dev/null; then
	require_sig=1
fi

step "Downloading $TARBALL..."
curl -fSL --progress-bar -o "$tmp/$TARBALL" "$TARBALL_URL"
curl -fsSL -o "$tmp/SHA256SUMS" "$SUMS_URL"
# .asc is required for v1.0.0+ bootstrap installs. Older pre-v1 releases did
# not publish it, so they keep the historical checksum-only path.
if ! curl -fsSL -o "$tmp/SHA256SUMS.asc" "$SIG_URL" 2>/dev/null; then
	if [ "$require_sig" = "1" ]; then
		fail "release $VERSION does not publish SHA256SUMS.asc — aborting instead of downgrading integrity verification"
	fi
	warn "Release predates SHA256SUMS.asc (pre-v1.0.0) — skipping PGP verification"
fi

# --- verify PGP signature ----------------------------------------------------
if [ -s "$tmp/SHA256SUMS.asc" ]; then
	if ! command -v gpg >/dev/null 2>&1; then
		if [ "$require_sig" = "1" ]; then
			fail "gpg is required to verify $VERSION (install gpg or download and verify manually)"
		fi
		warn "gpg missing; skipping PGP verification for pre-v1 release"
	else
		step "Verifying PGP signature on SHA256SUMS..."
		# Fetch the release-signing public key from the tagged source tree, then
		mkdir -p "$tmp/gnupg" && chmod 700 "$tmp/gnupg"
		if curl -fsSL "$KEY_URL" | GNUPGHOME="$tmp/gnupg" gpg --batch --quiet --import 2>/dev/null; then
			got_fp=$(GNUPGHOME="$tmp/gnupg" gpg --batch --with-colons --fingerprint 2>/dev/null \
				| awk -F: '/^fpr:/{print $10; exit}')
			if [ "$got_fp" != "$EXPECTED_FP" ]; then
				fail "release-signing key fingerprint $got_fp != expected $EXPECTED_FP — aborting (SECURITY.md has the canonical fingerprint)"
			fi
			if GNUPGHOME="$tmp/gnupg" gpg --batch --verify "$tmp/SHA256SUMS.asc" "$tmp/SHA256SUMS" 2>/dev/null; then
				info "PGP signature OK (maintainer key $EXPECTED_FP)"
			else
				fail "PGP signature on SHA256SUMS did not verify — aborting (tarball may be tampered)"
			fi
		else
			if [ "$require_sig" = "1" ]; then
				fail "could not fetch/import release-signing key for $VERSION — aborting"
			fi
			warn "Couldn't fetch maintainer key; skipping PGP verification for pre-v1 release"
		fi
	fi
fi

# --- verify checksum ---------------------------------------------------------
step "Verifying SHA-256 checksum..."
expected_sum=$(awk -v asset="$TARBALL" '$2 == asset { print $1 }' "$tmp/SHA256SUMS")
case "$expected_sum" in
	""|*[!0-9A-Fa-f]*) fail "SHA256SUMS does not contain a valid exact entry for $TARBALL" ;;
	*) ;;
esac
[ "$(printf '%s' "$expected_sum" | wc -c | tr -d ' ')" = "64" ] || \
	fail "SHA256SUMS contains a malformed digest for $TARBALL"
actual_sum=$($SHA256_CMD "$tmp/$TARBALL" | awk '{print $1}')
[ "$actual_sum" = "$expected_sum" ] || \
	fail "checksum verification failed for $TARBALL — aborting (the download may be corrupted or tampered with)"
info "Checksum OK"

# --- extract + install -------------------------------------------------------
step "Extracting..."
tar -tzf "$tmp/$TARBALL" > "$tmp/tar.entries" || fail "could not list $TARBALL"
archive_prefix="${archive_product}-${VERSION}-${PLATFORM}"
while IFS= read -r entry; do
	case "$entry" in
		"$archive_prefix/"|"$archive_prefix/$archive_binary"|"$archive_prefix/LICENSE"|"$archive_prefix/README.md") ;;
		""|/*|*"/../"*|"../"*|*"/.."|*\\*)
			fail "unsafe archive entry: $entry" ;;
		*)
			fail "unexpected archive entry: $entry" ;;
	esac
done < "$tmp/tar.entries"
tar -xzf "$tmp/$TARBALL" -C "$tmp"
extracted_dir="$tmp/$archive_prefix"
[ ! -L "$extracted_dir/$archive_binary" ] || fail "extracted executable is a symlink — aborting"
[ -f "$extracted_dir/$archive_binary" ] && [ -x "$extracted_dir/$archive_binary" ] || \
	fail "extracted tree missing the expected executable (tried $extracted_dir/$archive_binary)"

step "Installing to $INSTALL_DIR/canary..."
mkdir -p "$INSTALL_DIR"

canonical="$INSTALL_DIR/canary"
pre_upgrade="$INSTALL_DIR/ibkr"
# The pre-rename installer allowed an arbitrary IBKR_INSTALL_DIR. If that old
# leave two forward-incompatible binaries able to address the same daemon
legacy_on_path=$(command -v ibkr 2>/dev/null || true)
case "$legacy_on_path" in
	"")
		;;
	/*)
		legacy_dir=${legacy_on_path%/*}
		[ -n "$legacy_dir" ] || legacy_dir=/
		legacy_name=${legacy_on_path##*/}
		;;
	*/*)
		legacy_dir=${legacy_on_path%/*}
		legacy_name=${legacy_on_path##*/}
		;;
	*)
		legacy_dir=.
		legacy_name=$legacy_on_path
		;;
esac
if [ -n "$legacy_on_path" ]; then
	install_dir_physical=$(cd "$INSTALL_DIR" && pwd -P) || \
		fail "could not resolve install directory $INSTALL_DIR"
	legacy_dir_physical=$(cd "$legacy_dir" 2>/dev/null && pwd -P || true)
	[ -n "$legacy_dir_physical" ] || \
		fail "found the pre-upgrade executable at $legacy_on_path but could not resolve its directory safely"
	if [ "$legacy_dir_physical/$legacy_name" != "$install_dir_physical/ibkr" ]; then
		fail "found the pre-upgrade executable at $legacy_on_path; rerun with CANARY_INSTALL_DIR=$legacy_dir_physical so the installer can retire it safely"
	fi
fi
upgrading_existing=0
upgrading_legacy=0
for path in "$canonical" "$pre_upgrade"; do
	if [ -e "$path" ] || [ -L "$path" ]; then
		[ -f "$path" ] && [ ! -L "$path" ] || \
			fail "refusing executable path $path because it is not a regular file"
		upgrading_existing=1
	fi
done
if [ -e "$pre_upgrade" ]; then
	upgrading_legacy=1
fi
rm -f "$INSTALL_DIR/canary.bak" "$INSTALL_DIR/ibkr.bak"

# Stage the candidate in the destination directory. Existing public paths move
# to transaction-only hidden names and are restored only if canonical
# publication fails. They are made non-executable and deleted after success.
canonical_stage="$INSTALL_DIR/.canary-install.$$"
canonical_retired="$INSTALL_DIR/.canary-pre-install.$$"
pre_upgrade_retired="$INSTALL_DIR/.ibkr-pre-upgrade.$$"
rm -f "$canonical_stage" "$canonical_retired" "$pre_upgrade_retired"
copy_executable "$extracted_dir/$archive_binary" "$canonical_stage"
install_started=1
if [ -e "$canonical" ]; then
	mv -f "$canonical" "$canonical_retired"
else
	canonical_retired=""
fi
if [ -e "$pre_upgrade" ]; then
	mv -f "$pre_upgrade" "$pre_upgrade_retired"
else
	pre_upgrade_retired=""
fi
canonical_published=1
if ! mv -f "$canonical_stage" "$canonical"; then
	rollback_install || fail "canonical publish failed and automatic rollback was incomplete"
	fail "could not publish the canonical executable; restored the prior installation"
fi
canonical_stage=""
# Once both prior paths are non-executable there is no runnable rollback
# authority. Removal is best-effort only after that invariant is established.
for retired in "$canonical_retired" "$pre_upgrade_retired"; do
	[ -z "$retired" ] || chmod 0600 "$retired" || {
		rollback_install || fail "transaction staging cleanup failed and automatic rollback was incomplete"
		fail "could not retire the prior executable; restored the prior installation"
	}
done
install_committed=1
if [ -n "$canonical_retired" ]; then
	rm -f "$canonical_retired" || warn "could not remove non-executable transaction residue $canonical_retired"
	canonical_retired=""
fi
if [ -n "$pre_upgrade_retired" ]; then
	rm -f "$pre_upgrade_retired" || warn "could not remove non-executable transaction residue $pre_upgrade_retired"
	pre_upgrade_retired=""
fi

# macOS Gatekeeper marks downloads with com.apple.quarantine; clearing it
# avoids "cannot verify developer" prompts on first run. Silent on linux.
xattr -d com.apple.quarantine "$canonical" 2>/dev/null || true

# --- PATH handling -----------------------------------------------------------
# Auto-edit shell rc files ONLY when installing to the default location.
DEFAULT_INSTALL_DIR="$HOME/.local/bin"

if [ "$INSTALL_DIR" = "$DEFAULT_INSTALL_DIR" ]; then
	# Already on PATH? Nothing to do.
	case ":${PATH}:" in
		*":${INSTALL_DIR}:"*) need_path_update=0 ;;
		*) need_path_update=1 ;;
	esac

	if [ "$need_path_update" = "1" ]; then
		# Pick the rc file and the export syntax from $SHELL.
		case "${SHELL:-}" in
			*/fish)
				rc="$HOME/.config/fish/config.fish"
				line="set -gx PATH \$HOME/.local/bin \$PATH"
				;;
			*/zsh)
				rc="$HOME/.zshrc"
				line='export PATH="$HOME/.local/bin:$PATH"'
				;;
			*/bash)
				rc="$HOME/.bashrc"
				line='export PATH="$HOME/.local/bin:$PATH"'
				;;
			*)
				rc="$HOME/.profile"
				line='export PATH="$HOME/.local/bin:$PATH"'
				;;
		esac

		# Idempotent: only append if our line (or a moral equivalent) isn't already there.
		if [ -f "$rc" ] && grep -Fq '$HOME/.local/bin' "$rc" 2>/dev/null; then
			info "$INSTALL_DIR already referenced in $rc — leaving it alone"
		else
			printf '\n# Added by Canary installer\n%s\n' "$line" >> "$rc"
			info "Added $INSTALL_DIR to PATH in $rc"
			warn "Open a new terminal (or run: source $rc) for canary to be on PATH in this shell"
		fi
	fi
else
	# Custom install dir: don't touch rc files; just tell the user.
	case ":${PATH}:" in
		*":${INSTALL_DIR}:"*) ;;
		*) warn "$INSTALL_DIR is not on \$PATH; add it manually or invoke canary by absolute path" ;;
	esac
fi

# --- verify install ----------------------------------------------------------
step "Verifying..."
installed_version=$("$canonical" version 2>/dev/null | head -n1 || true)
case "$installed_version" in
	"Canary CLI  $VERSION"*|"canary $VERSION"*|"canary  $VERSION"*)
		info "Installed: $BOLD$installed_version$RESET"
		;;
	"ibkr $VERSION"*|"ibkr  $VERSION"*)
		if [ "$VERSION" = "v2.3.1" ]; then
			info "Installed the version-bounded $VERSION bootstrap at $canonical"
		else
			warn "Installed binary reports retired product identity: $installed_version"
		fi
		;;
	*)                warn "Installed binary reports unexpected version: $installed_version" ;;
esac
[ ! -e "$pre_upgrade" ] && [ ! -L "$pre_upgrade" ] || \
	fail "pre-upgrade executable path still exists after canonical install: $pre_upgrade"

# --- next steps --------------------------------------------------------------
printf '\n'
printf '%sNext steps%s\n' "$BOLD" "$RESET"
if [ "$upgrading_existing" = "1" ]; then
	printf '  %s•%s Complete the upgrade: %scanary restart%s\n' "$GREEN" "$RESET" "$BOLD" "$RESET"
	if [ "$upgrading_legacy" = "1" ]; then
		printf '    This starts the Canary-named daemon and migrates supported existing state before broker connection.\n'
	fi
else
	printf '  %s•%s Try the CLI:           %scanary account%s   (needs IB Gateway running)\n' "$GREEN" "$RESET" "$BOLD" "$RESET"
fi
printf '  %s•%s Wire Claude Desktop:   %scanary setup claude-desktop%s\n' "$GREEN" "$RESET" "$BOLD" "$RESET"
printf '  %s•%s Full docs:             https://github.com/%s\n' "$GREEN" "$RESET" "$REPO"
printf '\n'
