package update

import (
	"archive/tar"
	"bufio"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	archivepath "path"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"

	"github.com/osauer/canary/v2/internal/productidentity"
	"github.com/osauer/canary/v2/internal/xdgcache"
)

// ErrInstallInProgress signals that another `canary update` already holds
var ErrInstallInProgress = errors.New("another Canary update is already running")

// defaultInstallSubdir is the user-default install root when no explicit
const defaultInstallSubdir = ".local/bin"

// ResolveInstallDir returns the directory where the updated `canary`
func ResolveInstallDir() (string, error) {
	// docgen:env CANARY_INSTALL_DIR | Override the install directory for `canary update`. Defaults to `$HOME/.local/bin`. The release pipeline uses this to sandbox dog-food installs to a temporary directory.
	if _, retired := os.LookupEnv("IBKR_INSTALL_DIR"); retired {
		return "", errors.New("IBKR_INSTALL_DIR was retired by the Canary rename; use CANARY_INSTALL_DIR")
	}
	if v := os.Getenv("CANARY_INSTALL_DIR"); v != "" {
		return v, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve $HOME: %w", err)
	}
	return filepath.Join(home, defaultInstallSubdir), nil
}

// CacheDir returns the update cache directory: where the tarball,
func CacheDir() (string, error) {
	return xdgcache.CacheDir("update")
}

// AcquireLock takes the install-time flock at <cacheDir>/update.lock.
// queue rather than race on publication — the loser exits immediately with the friendly
func AcquireLock(cacheDir string) (*xdgcache.Lock, error) {
	lock, err := xdgcache.OpenLock(filepath.Join(cacheDir, "update.lock"))
	if err != nil {
		if errors.Is(err, xdgcache.ErrLocked) {
			return nil, ErrInstallInProgress
		}
		return nil, fmt.Errorf("acquire install lock: %w", err)
	}
	return lock, nil
}

// VerifySignature verifies that sumsSigPath is a valid PGP detached
// failure reason) on any mismatch.
// MUST run BEFORE VerifyChecksum: without this, a same-release attacker
func VerifySignature(sumsPath, sumsSigPath string) error {
	signed, err := os.Open(sumsPath)
	if err != nil {
		return fmt.Errorf("open SHA256SUMS: %w", err)
	}
	defer signed.Close()
	sig, err := os.Open(sumsSigPath)
	if err != nil {
		return fmt.Errorf("open SHA256SUMS.asc: %w", err)
	}
	defer sig.Close()
	return VerifyDetachedSignature(signed, sig)
}

// VerifyChecksum reads SHA256SUMS (one `<sha>  <filename>` line per
func VerifyChecksum(tarballPath, sumsPath, assetName string) error {
	expected, err := lookupChecksum(sumsPath, assetName)
	if err != nil {
		return err
	}

	f, err := os.Open(tarballPath)
	if err != nil {
		return fmt.Errorf("open tarball: %w", err)
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return fmt.Errorf("hash tarball: %w", err)
	}
	got := hex.EncodeToString(h.Sum(nil))
	if !strings.EqualFold(got, expected) {
		return fmt.Errorf("checksum mismatch for %s: expected %s, got %s", assetName, expected, got)
	}
	return nil
}

// lookupChecksum scans the SHA256SUMS file for assetName and returns
func lookupChecksum(sumsPath, assetName string) (string, error) {
	f, err := os.Open(sumsPath)
	if err != nil {
		return "", fmt.Errorf("open SHA256SUMS: %w", err)
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		// "<hex>  <filename>" or "<hex> *<filename>" (binary mode).
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		name := strings.TrimPrefix(fields[1], "*")
		if name == assetName {
			return strings.ToLower(fields[0]), nil
		}
	}
	if err := sc.Err(); err != nil {
		return "", fmt.Errorf("scan SHA256SUMS: %w", err)
	}
	return "", fmt.Errorf("SHA256SUMS has no entry for %s", assetName)
}

// magicNumbers are the leading bytes a freshly-extracted Linux or
var magicNumbers = [][]byte{
	{0x7F, 0x45, 0x4C, 0x46}, // ELF (Linux)
	{0xFE, 0xED, 0xFA, 0xCE}, // Mach-O 32-bit LE
	{0xFE, 0xED, 0xFA, 0xCF}, // Mach-O 64-bit LE
	{0xCE, 0xFA, 0xED, 0xFE}, // Mach-O 32-bit BE (legacy)
	{0xCF, 0xFA, 0xED, 0xFE}, // Mach-O 64-bit BE
	{0xCA, 0xFE, 0xBA, 0xBE}, // Mach-O fat binary (multi-arch)
	{0xCA, 0xFE, 0xBA, 0xBF}, // Mach-O fat binary 64-bit
}

// hasExecutableMagic reports whether the first bytes of the file at
// path match a known ELF or Mach-O magic. False on read failure too —
func hasExecutableMagic(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	buf := make([]byte, 4)
	n, _ := io.ReadFull(f, buf)
	if n < 4 {
		return false
	}
	for _, m := range magicNumbers {
		if buf[0] == m[0] && buf[1] == m[1] && buf[2] == m[2] && buf[3] == m[3] {
			return true
		}
	}
	return false
}

// ExtractTarball untars+ungzips tarballPath into destDir, expecting a single
//
//	even after SHA verification because verification only proves
func ExtractTarball(tarballPath, destDir, archiveRoot string) (string, error) {
	executable := productidentity.Executable
	archiveRoot = archivepath.Clean(strings.TrimSpace(archiveRoot))
	if archiveRoot == "." {
		archiveRoot = ""
	}
	if archivepath.IsAbs(archiveRoot) || archiveRoot == ".." || strings.HasPrefix(archiveRoot, "../") || strings.Contains(archiveRoot, "/") {
		return "", fmt.Errorf("unsupported archive root %q", archiveRoot)
	}
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return "", fmt.Errorf("mkdir destDir: %w", err)
	}
	f, err := os.Open(tarballPath)
	if err != nil {
		return "", fmt.Errorf("open tarball: %w", err)
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return "", fmt.Errorf("gunzip: %w", err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)

	var binPath string
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", fmt.Errorf("read tar header: %w", err)
		}
		// Tar headers always use slash-separated paths, independent of the
		name := archivepath.Clean(hdr.Name)
		if archivepath.IsAbs(name) || name == ".." || strings.HasPrefix(name, "../") {
			return "", fmt.Errorf("tar entry %q escapes archive root", hdr.Name)
		}
		if hdr.Typeflag != tar.TypeReg {
			// Skip safe directories, symlinks, etc. — only the expected
			continue
		}
		rootEntry := executable
		if archiveRoot != "" {
			rootEntry = archiveRoot + "/" + executable
		}
		if name != executable && name != rootEntry {
			continue
		}
		if binPath != "" {
			return "", fmt.Errorf("tarball contains multiple %q binary entries", executable)
		}
		if hdr.Size < 0 || hdr.Size > 200<<20 {
			return "", fmt.Errorf("tar entry %q size %d exceeds the 200 MiB limit", hdr.Name, hdr.Size)
		}
		out := filepath.Join(destDir, executable)
		if !strings.HasPrefix(out, filepath.Clean(destDir)+string(os.PathSeparator)) {
			return "", fmt.Errorf("tar entry %q escapes destDir", hdr.Name)
		}
		w, err := os.OpenFile(out, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
		if err != nil {
			return "", fmt.Errorf("create %s: %w", out, err)
		}
		if _, err := io.Copy(w, io.LimitReader(tr, 200<<20)); err != nil {
			_ = w.Close()
			return "", fmt.Errorf("write %s: %w", out, err)
		}
		if err := w.Close(); err != nil {
			return "", fmt.Errorf("close %s: %w", out, err)
		}
		// Force the mode — archive/tar would have set it from the
		if err := os.Chmod(out, 0o755); err != nil {
			return "", fmt.Errorf("chmod %s: %w", out, err)
		}
		binPath = out
		// Do not break: scanning the rest lets us reject a duplicate entry
	}
	if binPath == "" {
		return "", fmt.Errorf("tarball did not contain a %q binary entry", executable)
	}
	if !hasExecutableMagic(binPath) {
		return "", fmt.Errorf("extracted file %s is not a valid ELF/Mach-O binary", binPath)
	}
	return binPath, nil
}

// StripQuarantine removes the com.apple.quarantine extended attribute
// Critically this MUST run on the staging binary BEFORE the os.Rename
// with no rollback signal if the strip fails. Strip-before-rename
// gives us a single point of failure with the prior binary intact.
func StripQuarantine(path string) error {
	if runtime.GOOS != "darwin" {
		return nil
	}
	cmd := exec.Command("xattr", "-d", "com.apple.quarantine", path)
	out, err := cmd.CombinedOutput()
	if err == nil {
		return nil
	}
	// xattr's stderr language is "No such xattr" on macOS 10.13+ —
	combined := strings.ToLower(string(out))
	if strings.Contains(combined, "no such xattr") {
		return nil
	}
	if _, ok := errors.AsType[*exec.Error](err); ok {
		// `xattr` binary not on PATH. Not fatal; nothing to strip.
		return nil
	}
	return fmt.Errorf("strip quarantine from %s: %w (output: %s)", path, err, strings.TrimSpace(string(out)))
}

// InstallCanonical installs srcBinary only as the canonical canary executable.
// hidden paths and restored only if canonical publication fails. After success,
// is retained because daemon state migrations are forward-only.
func InstallCanonical(srcBinary, installDir string) error {
	if err := os.MkdirAll(installDir, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", installDir, err)
	}
	canonical := productidentity.CanonicalPath(installDir)
	preUpgrade := filepath.Join(installDir, productidentity.PreUpgradeExecutable)
	exists := map[string]bool{}
	for _, path := range []string{canonical, preUpgrade} {
		if info, err := os.Lstat(path); err == nil {
			if !info.Mode().IsRegular() {
				return fmt.Errorf("refusing to replace executable path %s: expected a regular file, got %s", path, info.Mode().Type())
			}
			exists[path] = true
		} else if err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("inspect executable path %s: %w", path, err)
		}
	}
	for _, residue := range []string{canonical + ".bak", preUpgrade + ".bak"} {
		if err := os.Remove(residue); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove retired executable residue %s: %w", residue, err)
		}
	}

	staged, err := copyBinaryIntoDir(srcBinary, installDir)
	if err != nil {
		return err
	}
	canonicalInstalled := false
	defer func() {
		if !canonicalInstalled {
			_ = os.Remove(staged)
		}
	}()

	canonicalStage := ""
	preUpgradeStage := ""
	published := false
	defer func() {
		if published {
			_ = discardRetiredExecutable(canonicalStage)
			_ = discardRetiredExecutable(preUpgradeStage)
			return
		}
		if canonicalStage != "" {
			_ = os.Rename(canonicalStage, canonical)
		}
		if preUpgradeStage != "" {
			_ = os.Rename(preUpgradeStage, preUpgrade)
		}
	}()

	if exists[canonical] {
		canonicalStage, err = stageExistingExecutable(canonical, installDir, ".canary-pre-install-*")
		if err != nil {
			return err
		}
	}
	if exists[preUpgrade] {
		preUpgradeStage, err = stageExistingExecutable(preUpgrade, installDir, ".ibkr-pre-upgrade-*")
		if err != nil {
			return err
		}
	}
	if err := os.Rename(staged, canonical); err != nil {
		return fmt.Errorf("install %s -> %s: %w", staged, canonical, err)
	}
	canonicalInstalled = true
	published = true
	if err := discardRetiredExecutable(canonicalStage); err != nil {
		return fmt.Errorf("installed %s but could not remove transaction staging: %w", canonical, err)
	}
	canonicalStage = ""
	if preUpgradeStage != "" {
		if err := discardRetiredExecutable(preUpgradeStage); err != nil {
			return fmt.Errorf("installed %s but could not remove transaction staging: %w", canonical, err)
		}
		preUpgradeStage = ""
	}
	return nil
}

func stageExistingExecutable(path, installDir, pattern string) (string, error) {
	f, err := os.CreateTemp(installDir, pattern)
	if err != nil {
		return "", fmt.Errorf("create transaction staging path: %w", err)
	}
	staged := f.Name()
	if err := f.Close(); err != nil {
		_ = os.Remove(staged)
		return "", fmt.Errorf("close transaction staging path: %w", err)
	}
	if err := os.Remove(staged); err != nil {
		return "", fmt.Errorf("prepare transaction staging path: %w", err)
	}
	if err := os.Rename(path, staged); err != nil {
		return "", fmt.Errorf("stage existing executable %s: %w", path, err)
	}
	return staged, nil
}

func discardRetiredExecutable(path string) error {
	if path == "" {
		return nil
	}
	if err := os.Chmod(path, 0o600); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("make retired executable non-runnable %s: %w", path, err)
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove retired executable %s: %w", path, err)
	}
	return nil
}

func copyBinaryIntoDir(srcBinary, destDir string) (string, error) {
	in, err := os.Open(srcBinary)
	if err != nil {
		return "", fmt.Errorf("open extracted binary: %w", err)
	}
	defer in.Close()

	out, err := os.CreateTemp(destDir, ".canary-update-*")
	if err != nil {
		return "", fmt.Errorf("create staging binary in %s: %w", destDir, err)
	}
	staged := out.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(staged)
		}
	}()
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return "", fmt.Errorf("copy extracted binary to staging path: %w", err)
	}
	if err := out.Chmod(0o755); err != nil {
		_ = out.Close()
		return "", fmt.Errorf("chmod staging binary: %w", err)
	}
	if err := out.Close(); err != nil {
		return "", fmt.Errorf("close staging binary: %w", err)
	}
	cleanup = false
	return staged, nil
}

// CleanupOnSignal installs a SIGTERM/SIGINT handler that removes the
func CleanupOnSignal(paths ...string) (cancel func()) {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGINT, syscall.SIGTERM)
	done := make(chan struct{})
	go func() {
		select {
		case <-ch:
			for _, p := range paths {
				_ = os.Remove(p)
			}
			// Exit non-zero so callers wrapping `canary update` see
			os.Exit(130)
		case <-done:
			return
		}
	}()
	return func() {
		signal.Stop(ch)
		close(done)
		for _, p := range paths {
			_ = os.Remove(p)
		}
	}
}

// Plan is the full sequence of artefacts an install touches. Exposed
type Plan struct {
	CacheDir    string // ~/.cache/ibkr/update/ (durable namespace compatibility pin)
	TarballPath string // CacheDir/<asset>.tar.gz
	SumsPath    string // CacheDir/SHA256SUMS
	SumsSigPath string // CacheDir/SHA256SUMS.asc (PGP detached signature)
	ExtractDir  string // CacheDir/extract/
	InstallDir  string // $CANARY_INSTALL_DIR or ~/.local/bin
	DestPath    string // InstallDir/canary
	// ArchiveRoot is the exact top-level directory derived from AssetName
	ArchiveRoot string
	AssetName   string // <asset>.tar.gz (used for SHA lookup)
	AssetURL    string // GitHub asset URL
	SumsURL     string // SHA256SUMS asset URL
	SumsSigURL  string // SHA256SUMS.asc asset URL
}

// PlanFor builds a Plan for the given release on the current host. The
func PlanFor(rel *Release) (*Plan, error) {
	assetName, assetURL, ok := rel.AssetForHost()
	if !ok {
		return nil, fmt.Errorf("no release asset for %s/%s", runtime.GOOS, runtime.GOARCH)
	}
	sumsName, sumsURL, ok := rel.SHA256SUMSAsset()
	if !ok {
		return nil, errors.New("release is missing SHA256SUMS asset")
	}
	_ = sumsName
	sigName, sigURL, ok := rel.SHA256SUMSSigAsset()
	if !ok {
		return nil, fmt.Errorf("%w (release predates signing or signing step failed in the release pipeline)", ErrSignatureMissing)
	}
	_ = sigName
	cacheDir, err := CacheDir()
	if err != nil {
		return nil, err
	}
	installDir, err := ResolveInstallDir()
	if err != nil {
		return nil, err
	}
	return &Plan{
		CacheDir:    cacheDir,
		TarballPath: filepath.Join(cacheDir, assetName),
		SumsPath:    filepath.Join(cacheDir, "SHA256SUMS"),
		SumsSigPath: filepath.Join(cacheDir, "SHA256SUMS.asc"),
		ExtractDir:  filepath.Join(cacheDir, "extract"),
		InstallDir:  installDir,
		DestPath:    productidentity.CanonicalPath(installDir),
		ArchiveRoot: strings.TrimSuffix(assetName, ".tar.gz"),
		AssetName:   assetName,
		AssetURL:    assetURL,
		SumsURL:     sumsURL,
		SumsSigURL:  sigURL,
	}, nil
}

// RunInstall executes the install flow end-to-end against a planned
func RunInstall(ctx context.Context, plan *Plan) error {
	if err := os.MkdirAll(plan.CacheDir, 0o755); err != nil {
		return fmt.Errorf("mkdir cache: %w", err)
	}
	lock, err := AcquireLock(plan.CacheDir)
	if err != nil {
		return err
	}
	defer lock.Release()

	// Signal-handler cleanup. Tempfiles get removed on Ctrl-C, on
	cleanup := CleanupOnSignal(plan.TarballPath, plan.SumsPath, plan.SumsSigPath, plan.ExtractDir)
	defer cleanup()

	if err := DownloadAsset(ctx, plan.SumsURL, plan.SumsPath); err != nil {
		return fmt.Errorf("download SHA256SUMS: %w", err)
	}
	if err := DownloadAsset(ctx, plan.SumsSigURL, plan.SumsSigPath); err != nil {
		return fmt.Errorf("download SHA256SUMS.asc: %w", err)
	}
	// Signature MUST verify before we trust SHA256SUMS — without this
	if err := VerifySignature(plan.SumsPath, plan.SumsSigPath); err != nil {
		return err
	}
	if err := DownloadAsset(ctx, plan.AssetURL, plan.TarballPath); err != nil {
		return fmt.Errorf("download tarball: %w", err)
	}
	if err := VerifyChecksum(plan.TarballPath, plan.SumsPath, plan.AssetName); err != nil {
		return err
	}
	// Fresh extract dir per install — prior runs may have left
	if err := os.RemoveAll(plan.ExtractDir); err != nil {
		return fmt.Errorf("clear extract dir: %w", err)
	}
	binPath, err := ExtractTarball(plan.TarballPath, plan.ExtractDir, plan.ArchiveRoot)
	if err != nil {
		return err
	}
	// MUST strip quarantine BEFORE rename: strip-after-rename leaves
	// fails. See StripQuarantine doc.
	if err := StripQuarantine(binPath); err != nil {
		return err
	}
	if err := InstallCanonical(binPath, plan.InstallDir); err != nil {
		return err
	}
	return nil
}
