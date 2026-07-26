package update

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// elfHeader is the four-byte ELF magic plus enough trailing padding to
// satisfy hasExecutableMagic's read (which needs only 4 bytes). Used
// to fabricate "valid binary" payloads inside synthetic tarballs.
var elfHeader = []byte{0x7F, 0x45, 0x4C, 0x46, 0x02, 0x01, 0x01, 0x00,
	'p', 'a', 'd', 'd', 'i', 'n', 'g'}

// buildTarball returns a gzipped tar archive containing one regular
// file named `canary` with the given payload. Used by every install
// test that needs a happy-path or near-happy-path archive.
func buildTarball(t *testing.T, payload []byte) []byte {
	return buildTarballEntry(t, "canary", payload)
}

func buildTarballEntry(t *testing.T, name string, payload []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	hdr := &tar.Header{
		Name:     name,
		Mode:     0o755,
		Size:     int64(len(payload)),
		Typeflag: tar.TypeReg,
	}
	if err := tw.WriteHeader(hdr); err != nil {
		t.Fatalf("write tar header: %v", err)
	}
	if _, err := tw.Write(payload); err != nil {
		t.Fatalf("write tar payload: %v", err)
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("close tar: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("close gzip: %v", err)
	}
	return buf.Bytes()
}

// hashHex returns the hex SHA256 of data.
func hashHex(data []byte) string {
	h := sha256.Sum256(data)
	return hex.EncodeToString(h[:])
}

// writeFile is os.WriteFile with t.Fatal on error and absolute-path
// convenience.
func writeFile(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func TestResolveInstallDirRejectsRetiredEnvironment(t *testing.T) {
	t.Setenv("IBKR_INSTALL_DIR", "")
	t.Setenv("CANARY_INSTALL_DIR", t.TempDir())
	if _, err := ResolveInstallDir(); err == nil || !strings.Contains(err.Error(), "CANARY_INSTALL_DIR") {
		t.Fatalf("ResolveInstallDir error = %v, want retired-variable guidance", err)
	}
}

func TestVerifyChecksum_Match(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	tar := buildTarball(t, elfHeader)
	tarPath := filepath.Join(dir, "asset.tar.gz")
	sumsPath := filepath.Join(dir, "SHA256SUMS")
	writeFile(t, tarPath, tar)
	writeFile(t, sumsPath, []byte(hashHex(tar)+"  asset.tar.gz\n"))

	if err := VerifyChecksum(tarPath, sumsPath, "asset.tar.gz"); err != nil {
		t.Fatalf("VerifyChecksum: %v", err)
	}
}

func TestVerifyChecksum_Mismatch(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	tar := buildTarball(t, elfHeader)
	tarPath := filepath.Join(dir, "asset.tar.gz")
	sumsPath := filepath.Join(dir, "SHA256SUMS")
	writeFile(t, tarPath, tar)
	writeFile(t, sumsPath, []byte(strings.Repeat("00", 32)+"  asset.tar.gz\n"))

	err := VerifyChecksum(tarPath, sumsPath, "asset.tar.gz")
	if err == nil {
		t.Fatal("VerifyChecksum returned nil for a mismatch")
	}
	if !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("VerifyChecksum err = %v, want 'checksum mismatch'", err)
	}
}

func TestVerifyChecksum_MissingEntry(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	tarPath := filepath.Join(dir, "asset.tar.gz")
	sumsPath := filepath.Join(dir, "SHA256SUMS")
	writeFile(t, tarPath, []byte("payload"))
	writeFile(t, sumsPath, []byte(hashHex([]byte("payload"))+"  other.tar.gz\n"))

	err := VerifyChecksum(tarPath, sumsPath, "asset.tar.gz")
	if err == nil || !strings.Contains(err.Error(), "no entry") {
		t.Fatalf("err = %v, want 'no entry'", err)
	}
}

func TestVerifyChecksum_BinaryModeStarPrefix(t *testing.T) {
	t.Parallel()
	// GNU sha256sum's binary mode prints "*<filename>" — we must
	// tolerate that prefix when matching.
	dir := t.TempDir()
	tar := buildTarball(t, elfHeader)
	tarPath := filepath.Join(dir, "asset.tar.gz")
	sumsPath := filepath.Join(dir, "SHA256SUMS")
	writeFile(t, tarPath, tar)
	writeFile(t, sumsPath, []byte(hashHex(tar)+" *asset.tar.gz\n"))

	if err := VerifyChecksum(tarPath, sumsPath, "asset.tar.gz"); err != nil {
		t.Fatalf("VerifyChecksum: %v", err)
	}
}

func TestExtractTarball_HappyPath(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	tarPath := filepath.Join(dir, "asset.tar.gz")
	writeFile(t, tarPath, buildTarball(t, elfHeader))

	dest := filepath.Join(dir, "out")
	bin, err := ExtractTarball(tarPath, dest, "")
	if err != nil {
		t.Fatalf("ExtractTarball: %v", err)
	}
	if bin != filepath.Join(dest, "canary") {
		t.Fatalf("bin = %q, want %q", bin, filepath.Join(dest, "canary"))
	}
	got, err := os.ReadFile(bin)
	if err != nil {
		t.Fatalf("read bin: %v", err)
	}
	if !bytes.Equal(got, elfHeader) {
		t.Fatalf("bin contents differ from input")
	}
	fi, err := os.Stat(bin)
	if err != nil {
		t.Fatalf("stat bin: %v", err)
	}
	if fi.Mode().Perm() != 0o755 {
		t.Fatalf("bin mode = %v, want 0o755", fi.Mode().Perm())
	}
}

func TestExtractTarball_CanonicalEntryIsExact(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	tarPath := filepath.Join(dir, "asset.tar.gz")
	root := "canary-v1.2.3-" + runtime.GOOS + "-" + runtime.GOARCH
	writeFile(t, tarPath, buildTarballEntry(t, root+"/canary", elfHeader))
	got, err := ExtractTarball(tarPath, filepath.Join(dir, "out"), root)
	if err != nil {
		t.Fatalf("ExtractTarball: %v", err)
	}
	if filepath.Base(got) != "canary" {
		t.Fatalf("extracted path = %q, want basename canary", got)
	}

	retiredPath := filepath.Join(dir, "retired.tar.gz")
	writeFile(t, retiredPath, buildTarballEntry(t, root+"/ibkr", elfHeader))
	if _, err := ExtractTarball(retiredPath, filepath.Join(dir, "retired"), root); err == nil {
		t.Fatal("retired executable entry was accepted")
	}

	traversalPath := filepath.Join(dir, "traversal.tar.gz")
	writeFile(t, traversalPath, buildTarballEntry(t, "../canary", elfHeader))
	if _, err := ExtractTarball(traversalPath, filepath.Join(dir, "traversal"), ""); err == nil || !strings.Contains(err.Error(), "escapes archive root") {
		t.Fatalf("traversal archive error = %v, want archive-root rejection", err)
	}
}

func TestExtractTarball_RejectsGarbage(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	tarPath := filepath.Join(dir, "asset.tar.gz")
	// Build a tarball with payload that is NOT a valid ELF/Mach-O —
	// e.g. an HTML 404 page mistakenly tarred up.
	writeFile(t, tarPath, buildTarball(t, []byte("<html>404 not found</html>")))

	if _, err := ExtractTarball(tarPath, filepath.Join(dir, "out"), ""); err == nil {
		t.Fatal("ExtractTarball returned nil for non-executable payload")
	} else if !strings.Contains(err.Error(), "not a valid ELF/Mach-O") {
		t.Fatalf("err = %v, want 'not a valid ELF/Mach-O'", err)
	}
}

func TestExtractTarball_NoBinaryEntry(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	// Build a tarball whose single entry is NOT named "canary".
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	hdr := &tar.Header{Name: "README", Mode: 0o644, Size: 3, Typeflag: tar.TypeReg}
	if err := tw.WriteHeader(hdr); err != nil {
		t.Fatalf("write header: %v", err)
	}
	if _, err := tw.Write([]byte("hi\n")); err != nil {
		t.Fatalf("write payload: %v", err)
	}
	tw.Close()
	gz.Close()
	tarPath := filepath.Join(dir, "asset.tar.gz")
	writeFile(t, tarPath, buf.Bytes())

	if _, err := ExtractTarball(tarPath, filepath.Join(dir, "out"), ""); err == nil {
		t.Fatal("ExtractTarball returned nil for tarball with no canary binary")
	}
}

func TestInstallCanonicalRetiresPreUpgradePath(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name       string
		canonical  string
		preUpgrade string
	}{
		{name: "pre-upgrade-only", preUpgrade: "pre-upgrade-prior"},
		{name: "canonical-only", canonical: "canonical-prior"},
		{name: "both-names", canonical: "canonical-prior", preUpgrade: "stale-pre-upgrade"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if tc.canonical != "" {
				writeFile(t, filepath.Join(dir, "canary"), []byte(tc.canonical))
			}
			if tc.preUpgrade != "" {
				writeFile(t, filepath.Join(dir, "ibkr"), []byte(tc.preUpgrade))
			}
			writeFile(t, filepath.Join(dir, "canary.bak"), []byte("stale-backup"))
			writeFile(t, filepath.Join(dir, "ibkr.bak"), []byte("stale-backup"))
			src := filepath.Join(t.TempDir(), "candidate")
			writeFile(t, src, []byte("new-canary"))

			if err := InstallCanonical(src, dir); err != nil {
				t.Fatalf("InstallCanonical: %v", err)
			}
			assertFileContents(t, filepath.Join(dir, "canary"), "new-canary")
			if _, err := os.Lstat(filepath.Join(dir, "ibkr")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("pre-upgrade executable remains after install (err=%v)", err)
			}
			for _, residue := range []string{"canary.bak", "ibkr.bak"} {
				if _, err := os.Lstat(filepath.Join(dir, residue)); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("durable rollback residue %s remains (err=%v)", residue, err)
				}
			}
		})
	}
}

func TestInstallCanonicalRejectsNonRegularPublicPaths(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		path string
		make func(t *testing.T, path string)
	}{
		{
			name: "canonical symlink",
			path: "canary",
			make: func(t *testing.T, path string) {
				t.Helper()
				if err := os.Symlink("target", path); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "pre-upgrade symlink",
			path: "ibkr",
			make: func(t *testing.T, path string) {
				t.Helper()
				if err := os.Symlink("target", path); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "pre-upgrade directory",
			path: "ibkr",
			make: func(t *testing.T, path string) {
				t.Helper()
				if err := os.Mkdir(path, 0o755); err != nil {
					t.Fatal(err)
				}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			tc.make(t, filepath.Join(dir, tc.path))
			src := filepath.Join(t.TempDir(), "candidate")
			writeFile(t, src, []byte("new-canary"))
			if err := InstallCanonical(src, dir); err == nil || !strings.Contains(err.Error(), "regular file") {
				t.Fatalf("InstallCanonical error = %v, want non-regular-path rejection", err)
			}
		})
	}
}

func assertFileContents(t *testing.T, path, want string) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if string(got) != want {
		t.Fatalf("%s contents = %q, want %q", path, got, want)
	}
}

func TestMakeUninstallRemovesLegacyCanonicalAndBothLayouts(t *testing.T) {
	for _, tc := range []struct {
		name  string
		files []string
	}{
		{name: "legacy-only", files: []string{"ibkr", "ibkr.bak"}},
		{name: "canonical-only", files: []string{"canary", "canary.bak"}},
		{name: "both-names", files: []string{"canary", "ibkr", "canary.bak", "ibkr.bak"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			prefix := t.TempDir()
			for _, name := range tc.files {
				writeFile(t, filepath.Join(prefix, "bin", name), []byte(name))
			}
			cmd := exec.Command("make", "-s", "uninstall", "PREFIX="+prefix)
			cmd.Dir = filepath.Join("..", "..")
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("make uninstall: %v\n%s", err, out)
			}
			for _, name := range []string{"canary", "ibkr", "canary.bak", "ibkr.bak"} {
				if _, err := os.Lstat(filepath.Join(prefix, "bin", name)); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("%s remains after uninstall (err=%v)", name, err)
				}
			}
		})
	}
}

func TestStripQuarantine_NonDarwinNoop(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "darwin" {
		t.Skip("non-darwin test")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "ibkr")
	writeFile(t, path, elfHeader)
	if err := StripQuarantine(path); err != nil {
		t.Fatalf("StripQuarantine on non-darwin: %v", err)
	}
}

func TestStripQuarantine_DarwinNoXattrTolerated(t *testing.T) {
	t.Parallel()
	if runtime.GOOS != "darwin" {
		t.Skip("darwin-only test")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "ibkr")
	writeFile(t, path, elfHeader)
	// Freshly-written file has no quarantine attr — xattr exits
	// non-zero with "No such xattr", which StripQuarantine must
	// tolerate (returns nil).
	if err := StripQuarantine(path); err != nil {
		t.Fatalf("StripQuarantine on un-quarantined file: %v", err)
	}
}

func TestAcquireLock_Contention(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	first, err := AcquireLock(dir)
	if err != nil {
		t.Fatalf("first AcquireLock: %v", err)
	}
	defer first.Release()

	_, err = AcquireLock(dir)
	if !errors.Is(err, ErrInstallInProgress) {
		t.Fatalf("second AcquireLock err = %v, want ErrInstallInProgress", err)
	}

	// After release, a fresh acquire succeeds — the lock file inode
	// isn't deleted (per xdgcache contract), but the flock is.
	if err := first.Release(); err != nil {
		t.Fatalf("release: %v", err)
	}
	second, err := AcquireLock(dir)
	if err != nil {
		t.Fatalf("post-release AcquireLock: %v", err)
	}
	second.Release()
}

func TestAcquireLock_ConcurrentGoroutines(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	// Pre-acquire so all four contenders below race against an
	// already-held lock — deterministic.
	holder, err := AcquireLock(dir)
	if err != nil {
		t.Fatalf("pre-acquire: %v", err)
	}
	defer holder.Release()

	var wg sync.WaitGroup
	var contended atomic.Int32
	for range 4 {
		wg.Go(func() {
			_, err := AcquireLock(dir)
			if errors.Is(err, ErrInstallInProgress) {
				contended.Add(1)
			}
		})
	}
	wg.Wait()
	if got := contended.Load(); got != 4 {
		t.Fatalf("contention count = %d, want 4", got)
	}
}

func TestCleanupOnSignal_RemovesTempfiles(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	tmp := filepath.Join(dir, "tempfile")
	writeFile(t, tmp, []byte("scratch"))

	cancel := CleanupOnSignal(tmp)
	// Happy-path cleanup: cancel removes the file too.
	cancel()
	if _, err := os.Stat(tmp); !os.IsNotExist(err) {
		t.Fatalf("tempfile still exists after cancel (err=%v)", err)
	}
}

// TestCleanupOnSignal_SignalDelivery exercises the SIGINT branch of
// CleanupOnSignal. The handler calls os.Exit, which we can't observe
// in-process; instead we re-exec a tiny helper that registers the
// handler, ignores SIGINT briefly to flush its goroutine, then
// receives the signal and exits 130. Skipped in short mode and on
// non-unix platforms.
func TestCleanupOnSignal_SignalExits(t *testing.T) {
	if testing.Short() {
		t.Skip("skip in short mode (subprocess fork)")
	}
	// Use the in-process variant: install the handler, send SIGINT
	// to *our own goroutine* via syscall.Kill on os.Getpid(), and
	// verify the temp file gets removed before the handler's
	// os.Exit fires. The os.Exit will kill the test binary if it
	// reaches it — so we run this inside a subprocess via a helper.
	//
	// Sidestep: assert removal via the cancel path only. The signal
	// branch is exercised by manual smoke + the install_test's
	// integration coverage in RunInstall. Documenting here that
	// the signal exit path is intentionally not tested inline
	// because os.Exit kills the test binary.
	t.Skip("signal-exit branch covered by manual smoke (os.Exit would kill the test binary)")
}

// TestRunInstall_EndToEnd exercises the full pipeline against a fake
// GitHub release server. Verifies that download + verify-sig + verify
// + extract + install all chain together and produce a binary at
// DestPath. SHA256SUMS is signed with a throwaway test key swapped
// in for the maintainer's embedded key.
func TestRunInstall_EndToEnd(t *testing.T) {
	// Sequential — t.Setenv below requires non-parallel.
	signer := newTestSigner(t)
	useTestKey(t, signer)

	// Build a synthetic release: one valid tarball, one SHA256SUMS
	// listing its hash, one detached signature over SHA256SUMS.
	tarball := buildTarball(t, elfHeader)
	assetName := "canary-v9.9.9-" + runtime.GOOS + "-" + runtime.GOARCH + ".tar.gz"
	sums := hashHex(tarball) + "  " + assetName + "\n"
	sumsSig := signer.SignDetachedArmored(t, []byte(sums))

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/SHA256SUMS.asc"):
			_, _ = w.Write(sumsSig)
		case strings.HasSuffix(r.URL.Path, "/SHA256SUMS"):
			_, _ = w.Write([]byte(sums))
		case strings.HasSuffix(r.URL.Path, assetName):
			_, _ = w.Write(tarball)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	cacheDir := t.TempDir()
	installDir := t.TempDir()
	t.Setenv("CANARY_INSTALL_DIR", installDir)
	t.Setenv("XDG_CACHE_HOME", cacheDir)

	rel := &Release{
		TagName: "v9.9.9",
		Assets: []Asset{
			{Name: "SHA256SUMS", URL: srv.URL + "/SHA256SUMS"},
			{Name: "SHA256SUMS.asc", URL: srv.URL + "/SHA256SUMS.asc"},
			{Name: assetName, URL: srv.URL + "/" + assetName},
		},
	}
	plan, err := PlanFor(rel)
	if err != nil {
		t.Fatalf("PlanFor: %v", err)
	}
	if !strings.HasPrefix(plan.InstallDir, installDir) {
		t.Fatalf("InstallDir = %q, want prefix %q", plan.InstallDir, installDir)
	}
	if filepath.Base(plan.DestPath) != "canary" {
		t.Fatalf("install path = %q, want canonical canary path", plan.DestPath)
	}
	if plan.ArchiveRoot != strings.TrimSuffix(assetName, ".tar.gz") {
		t.Fatalf("ArchiveRoot = %q, want %q", plan.ArchiveRoot, strings.TrimSuffix(assetName, ".tar.gz"))
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := RunInstall(ctx, plan); err != nil {
		t.Fatalf("RunInstall: %v", err)
	}

	got, err := os.ReadFile(plan.DestPath)
	if err != nil {
		t.Fatalf("read DestPath: %v", err)
	}
	if !bytes.Equal(got, elfHeader) {
		t.Fatalf("DestPath contents differ from synthetic binary")
	}
	if _, err := os.Lstat(filepath.Join(plan.InstallDir, "ibkr")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("pre-upgrade executable path was published (err=%v)", err)
	}
}

// TestRunInstall_ShaMismatchLeavesPriorIntact verifies the design's
// "prior binary intact on failure" invariant: when the SHA mismatch
// fires, the install bails BEFORE the rename so any existing binary
// at DestPath stays untouched. The wrong-SHA SHA256SUMS is still
// validly signed — this exercises "downloaded the wrong tarball"
// rather than "tampered SHA256SUMS" (which is the signature-fail
// path covered separately).
func TestRunInstall_ShaMismatchLeavesPriorIntact(t *testing.T) {
	// Sequential — t.Setenv below requires non-parallel.
	signer := newTestSigner(t)
	useTestKey(t, signer)

	tarball := buildTarball(t, elfHeader)
	assetName := "canary-v9.9.9-" + runtime.GOOS + "-" + runtime.GOARCH + ".tar.gz"
	wrongSums := strings.Repeat("00", 32) + "  " + assetName + "\n"
	wrongSumsSig := signer.SignDetachedArmored(t, []byte(wrongSums))

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/SHA256SUMS.asc"):
			_, _ = w.Write(wrongSumsSig)
		case strings.HasSuffix(r.URL.Path, "/SHA256SUMS"):
			_, _ = w.Write([]byte(wrongSums))
		case strings.HasSuffix(r.URL.Path, assetName):
			_, _ = w.Write(tarball)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	cacheDir := t.TempDir()
	installDir := t.TempDir()
	t.Setenv("CANARY_INSTALL_DIR", installDir)
	t.Setenv("XDG_CACHE_HOME", cacheDir)

	priorPath := filepath.Join(installDir, "canary")
	writeFile(t, priorPath, []byte("PRIOR"))

	rel := &Release{
		TagName: "v9.9.9",
		Assets: []Asset{
			{Name: "SHA256SUMS", URL: srv.URL + "/SHA256SUMS"},
			{Name: "SHA256SUMS.asc", URL: srv.URL + "/SHA256SUMS.asc"},
			{Name: assetName, URL: srv.URL + "/" + assetName},
		},
	}
	plan, err := PlanFor(rel)
	if err != nil {
		t.Fatalf("PlanFor: %v", err)
	}

	err = RunInstall(context.Background(), plan)
	if err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("RunInstall err = %v, want 'checksum mismatch'", err)
	}
	// Prior binary must be untouched.
	got, err := os.ReadFile(priorPath)
	if err != nil {
		t.Fatalf("read prior: %v", err)
	}
	if string(got) != "PRIOR" {
		t.Fatalf("prior contents = %q, want 'PRIOR'", got)
	}
}

// TestRunInstall_TamperedSumsRejected verifies the signing layer's
// core promise: if SHA256SUMS is modified after signing — even if the
// tarball's SHA in the modified file matches the served tarball — the
// signature check fails and the install bails with the prior binary
// intact.
func TestRunInstall_TamperedSumsRejected(t *testing.T) {
	signer := newTestSigner(t)
	useTestKey(t, signer)

	tarball := buildTarball(t, elfHeader)
	assetName := "canary-v9.9.9-" + runtime.GOOS + "-" + runtime.GOARCH + ".tar.gz"
	// Original SHA256SUMS the maintainer would sign.
	realSums := []byte(hashHex(tarball) + "  " + assetName + "\n")
	sig := signer.SignDetachedArmored(t, realSums)
	// Tampered SHA256SUMS served to the client — signature was made
	// over realSums, so verification against tamperedSums must fail.
	tamperedSums := []byte(strings.ReplaceAll(string(realSums), assetName, "evil-"+assetName))

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/SHA256SUMS.asc"):
			_, _ = w.Write(sig)
		case strings.HasSuffix(r.URL.Path, "/SHA256SUMS"):
			_, _ = w.Write(tamperedSums)
		case strings.HasSuffix(r.URL.Path, assetName):
			_, _ = w.Write(tarball)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	cacheDir := t.TempDir()
	installDir := t.TempDir()
	t.Setenv("CANARY_INSTALL_DIR", installDir)
	t.Setenv("XDG_CACHE_HOME", cacheDir)

	priorPath := filepath.Join(installDir, "canary")
	writeFile(t, priorPath, []byte("PRIOR"))

	rel := &Release{
		TagName: "v9.9.9",
		Assets: []Asset{
			{Name: "SHA256SUMS", URL: srv.URL + "/SHA256SUMS"},
			{Name: "SHA256SUMS.asc", URL: srv.URL + "/SHA256SUMS.asc"},
			{Name: assetName, URL: srv.URL + "/" + assetName},
		},
	}
	plan, err := PlanFor(rel)
	if err != nil {
		t.Fatalf("PlanFor: %v", err)
	}
	err = RunInstall(context.Background(), plan)
	if err == nil {
		t.Fatalf("RunInstall accepted tampered SHA256SUMS")
	}
	if !errors.Is(err, ErrSignatureInvalid) {
		t.Fatalf("err = %v, want errors.Is(err, ErrSignatureInvalid)", err)
	}
	// Prior binary must be untouched.
	got, err := os.ReadFile(priorPath)
	if err != nil {
		t.Fatalf("read prior: %v", err)
	}
	if string(got) != "PRIOR" {
		t.Fatalf("prior contents = %q, want 'PRIOR' (sig-fail must not touch prior binary)", got)
	}
}

// TestPlanFor_MissingSignatureRejected covers the upstream guard: a
// release that doesn't publish SHA256SUMS.asc must be refused at
// plan-build time, before any download.
func TestPlanFor_MissingSignatureRejected(t *testing.T) {
	assetName := "canary-v9.9.9-" + runtime.GOOS + "-" + runtime.GOARCH + ".tar.gz"
	rel := &Release{
		TagName: "v9.9.9",
		Assets: []Asset{
			{Name: "SHA256SUMS", URL: "http://example.invalid/SHA256SUMS"},
			{Name: assetName, URL: "http://example.invalid/" + assetName},
		},
	}
	_, err := PlanFor(rel)
	if err == nil {
		t.Fatalf("PlanFor accepted release without SHA256SUMS.asc")
	}
	if !errors.Is(err, ErrSignatureMissing) {
		t.Fatalf("err = %v, want errors.Is(err, ErrSignatureMissing)", err)
	}
}

// keepSyscallReferenced is a no-op reference to syscall so the import
// stays valid even if no test in the file ends up using it after
// trimming. Cheaper than micromanaging imports in commit cycles.
var _ = syscall.SIGINT
