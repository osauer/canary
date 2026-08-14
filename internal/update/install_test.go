package update

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/ProtonMail/go-crypto/openpgp/armor"
	"github.com/ProtonMail/go-crypto/openpgp/packet"
	"io"
	"net/http"
	"net/http/httptest"
	"os"

	"path/filepath"
	"runtime"
	"strings"

	"syscall"
	"testing"
	"time"
)

var elfHeader = []byte{0x7F, 0x45, 0x4C, 0x46, 0x02, 0x01, 0x01, 0x00,
	'p', 'a', 'd', 'd', 'i', 'n', 'g'}

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

func hashHex(data []byte) string {
	h := sha256.Sum256(data)
	return hex.EncodeToString(h[:])
}

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

func TestLatestReleaseForInstalledMajor(t *testing.T) {
	releases := []Release{{TagName: "v4.0.0", Draft: true}, {TagName: "v3.1.0", Prerelease: true}, {TagName: "v3.0.1"}, {TagName: "v3.0.0"}, {TagName: "v2.8.6", Draft: true}, {TagName: "v2.8.5"}, {TagName: "v2.8.4"}, {TagName: "not-semver"}}
	for installed, want := range map[string]string{"v2.8.4": "v2.8.5", "2.7.0": "v2.8.5", "v3.0.0": "v3.0.1", "dev": "v3.0.1", "": "v3.0.1"} {
		t.Run(installed, func(t *testing.T) {
			got, err := latestReleaseForInstalledMajor(releases, installed)
			if err != nil || got.TagName != want {
				t.Fatalf("release = %v, err = %v; want %s", got, err, want)
			}
		})
	}
	if _, err := latestReleaseForInstalledMajor(releases, "v4.0.0"); err == nil || !strings.Contains(err.Error(), "v4") {
		t.Fatalf("missing maintained v4 line error = %v", err)
	}
}

func TestEvaluateAvailabilityFailsClosedForDevelopmentBuildsAndDowngrades(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name      string
		installed string
		latest    string
		state     AvailabilityState
		available bool
	}{
		{name: "new stable", installed: "v3.0.1", latest: "v3.0.2", state: AvailabilityAvailable, available: true},
		{name: "equal", installed: "v3.0.2", latest: "v3.0.2", state: AvailabilityCurrent},
		{name: "newer installed", installed: "v3.1.0", latest: "v3.0.2", state: AvailabilityNewerInstalled},
		{name: "git describe ahead", installed: "v3.0.1-39-g7b62a7d4", latest: "v3.0.1", state: AvailabilityDevelopmentBuild},
		{name: "git describe dirty", installed: "v3.0.1-39-g7b62a7d4-dirty", latest: "v3.0.2", state: AvailabilityDevelopmentBuild},
		{name: "tag dirty", installed: "v3.0.1-dirty", latest: "v3.0.2", state: AvailabilityDevelopmentBuild},
		{name: "build metadata dirty", installed: "v2.8.5-0.20260813190449-dcc9af72fc6b+dirty", latest: "v2.8.5", state: AvailabilityDevelopmentBuild},
		{name: "dev", installed: "dev", latest: "v3.0.2", state: AvailabilityDevelopmentBuild},
		{name: "commit only", installed: "7b62a7d4", latest: "v3.0.2", state: AvailabilityInvalidVersion},
		{name: "different major", installed: "v2.8.5", latest: "v3.0.2", state: AvailabilityDifferentMajor},
		{name: "release candidate", installed: "v3.0.2-rc.1", latest: "v3.0.2", state: AvailabilityAvailable, available: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := EvaluateAvailability(tc.installed, tc.latest)
			if got.State != tc.state || got.Available != tc.available {
				t.Fatalf("EvaluateAvailability(%q, %q) = %+v, want state=%q available=%t", tc.installed, tc.latest, got, tc.state, tc.available)
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

func TestRunInstall_EndToEnd(t *testing.T) {

	signer := newTestSigner(t)
	useTestKey(t, signer)

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

var _ = syscall.SIGINT

type testSigner struct {
	entity      *openpgp.Entity
	armoredPub  []byte
	fingerprint string
}

func newTestSigner(t *testing.T) *testSigner {
	t.Helper()

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

func (s *testSigner) SignDetachedArmored(t *testing.T, msg []byte) []byte {
	t.Helper()
	buf := &bytes.Buffer{}

	if err := openpgp.ArmoredDetachSign(buf, s.entity, bytes.NewReader(msg), &packet.Config{
		DefaultHash: crypto.SHA256,
	}); err != nil {
		t.Fatalf("SignDetachedArmored: %v", err)
	}
	return buf.Bytes()
}

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

func TestVerifyDetachedSignature_Roundtrip(t *testing.T) {
	signer := newTestSigner(t)
	useTestKey(t, signer)

	msg := []byte("abc123  canary-v1.0.0-darwin-arm64.tar.gz\ndef456  SHA256SUMS\n")
	sig := signer.SignDetachedArmored(t, msg)

	if err := VerifyDetachedSignature(bytes.NewReader(msg), bytes.NewReader(sig)); err != nil {
		t.Fatalf("VerifyDetachedSignature: unexpected error: %v", err)
	}
}

var _ = io.Discard
