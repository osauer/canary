package cli

import (
	"bytes"
	"context"
	"errors"
	"io"
	"path/filepath"
	"strings"
	"testing"

	"github.com/osauer/canary/v2/internal/update"
)

// fakeFetch returns a static Release and a nil error. The Release
// always carries v9.9.9 plus assets for every supported host so tests
// don't have to skip per-platform.
func fakeFetch(tag string) fetchFunc {
	return func(ctx context.Context, _ string) (*update.Release, error) {
		return &update.Release{
			TagName: tag,
			Assets: []update.Asset{
				{Name: "SHA256SUMS", URL: "https://example/SHA256SUMS"},
				{Name: "SHA256SUMS.asc", URL: "https://example/SHA256SUMS.asc"},
				{Name: "canary-" + tag + "-darwin-arm64.tar.gz", URL: "https://example/darwin-arm64"},
				{Name: "canary-" + tag + "-darwin-amd64.tar.gz", URL: "https://example/darwin-amd64"},
				{Name: "canary-" + tag + "-linux-amd64.tar.gz", URL: "https://example/linux-amd64"},
				{Name: "canary-" + tag + "-linux-arm64.tar.gz", URL: "https://example/linux-arm64"},
			},
		}, nil
	}
}

func fakeFetchErr(err error) fetchFunc {
	return func(ctx context.Context, _ string) (*update.Release, error) { return nil, err }
}

func recordingInstall(installed *bool) installFunc {
	return func(ctx context.Context, plan *update.Plan) error {
		*installed = true
		return nil
	}
}

func recordingRestart(called *bool) stackRestartFunc {
	return func(context.Context, string, io.Writer, io.Writer) int {
		*called = true
		return 0
	}
}

// newOpts returns a baseline updateOptions for tests with stdin empty
// and isTTY=true (the default for in-tty tests).
func newOpts(installed string) (*updateOptions, *bytes.Buffer, *bytes.Buffer) {
	out, errBuf := &bytes.Buffer{}, &bytes.Buffer{}
	opts := &updateOptions{
		installedVersion: installed,
		in:               &bytes.Buffer{},
		out:              out,
		err:              errBuf,
		isTTY:            true,
	}
	return opts, out, errBuf
}

func TestRunUpdateCore_AlreadyLatest(t *testing.T) {
	t.Parallel()
	opts, out, _ := newOpts("v9.9.9")
	exit := runUpdateCore(context.Background(), opts, fakeFetch("v9.9.9"), nil, nil)
	if exit != 0 {
		t.Fatalf("exit = %d, want 0", exit)
	}
	if !strings.Contains(out.String(), "already on") {
		t.Fatalf("output = %q, want 'already on'", out.String())
	}
}

func TestRunUpdateCore_Behind_Installs(t *testing.T) {
	t.Parallel()
	opts, out, _ := newOpts("v0.32.0")
	opts.restart = true // suppress prompt
	installed := false
	restarted := false
	exit := runUpdateCore(context.Background(), opts, fakeFetch("v9.9.9"),
		recordingInstall(&installed), recordingRestart(&restarted))
	if exit != 0 {
		t.Fatalf("exit = %d, want 0\nout:%s", exit, out.String())
	}
	if !installed {
		t.Fatalf("installFunc not called")
	}
	if !restarted {
		t.Fatalf("shared stack restart was not called")
	}
	if !strings.Contains(out.String(), "installing v9.9.9") {
		t.Fatalf("output = %q, want 'installing v9.9.9'", out.String())
	}
}

func TestRunUpdateCore_Ahead_NoInstall(t *testing.T) {
	t.Parallel()
	opts, out, _ := newOpts("v99.99.99")
	installed := false
	exit := runUpdateCore(context.Background(), opts, fakeFetch("v9.9.9"),
		recordingInstall(&installed), nil)
	if exit != 0 {
		t.Fatalf("exit = %d, want 0\nout:%s", exit, out.String())
	}
	if installed {
		t.Fatal("installFunc called despite installed > latest")
	}
}

func TestRunUpdateCore_Force_InstallsRegardless(t *testing.T) {
	t.Parallel()
	opts, _, _ := newOpts("v9.9.9")
	opts.force = true
	opts.restart = true
	installed := false
	exit := runUpdateCore(context.Background(), opts, fakeFetch("v9.9.9"),
		recordingInstall(&installed), recordingRestart(new(bool)))
	if exit != 0 {
		t.Fatalf("exit = %d, want 0", exit)
	}
	if !installed {
		t.Fatal("--force did not trigger install on equal versions")
	}
}

func TestRunUpdateCore_Dev_InstallsAnyTag(t *testing.T) {
	t.Parallel()
	opts, out, _ := newOpts("dev")
	opts.restart = true
	installed := false
	exit := runUpdateCore(context.Background(), opts, fakeFetch("v9.9.9"),
		recordingInstall(&installed), recordingRestart(new(bool)))
	if exit != 0 {
		t.Fatalf("exit = %d, want 0", exit)
	}
	if !installed {
		t.Fatalf("dev build did not install tagged release\nout:%s", out.String())
	}
}

func TestRunUpdateCore_Check_AlreadyLatest_Exit0(t *testing.T) {
	t.Parallel()
	opts, out, _ := newOpts("v9.9.9")
	opts.check = true
	exit := runUpdateCore(context.Background(), opts, fakeFetch("v9.9.9"), nil, nil)
	if exit != 0 {
		t.Fatalf("exit = %d, want 0 (open-decision: --check is informational)", exit)
	}
	if !strings.Contains(out.String(), "already on latest") {
		t.Fatalf("output = %q, want 'already on latest'", out.String())
	}
}

func TestRunUpdateCore_Check_UpdateAvailable_Exit0(t *testing.T) {
	t.Parallel()
	opts, out, _ := newOpts("v0.32.0")
	opts.check = true
	installed := false
	exit := runUpdateCore(context.Background(), opts, fakeFetch("v9.9.9"),
		recordingInstall(&installed), nil)
	if exit != 0 {
		t.Fatalf("exit = %d, want 0 (open-decision: --check is informational)", exit)
	}
	if installed {
		t.Fatal("--check triggered an install")
	}
	if !strings.Contains(out.String(), "would install") {
		t.Fatalf("output = %q, want 'would install'", out.String())
	}
}

func TestRunUpdateCore_Check_FetchFailure_Exit1(t *testing.T) {
	t.Parallel()
	opts, _, errBuf := newOpts("v0.32.0")
	opts.check = true
	exit := runUpdateCore(context.Background(), opts, fakeFetchErr(errors.New("dns")), nil, nil)
	if exit != 1 {
		t.Fatalf("exit = %d, want 1 (fetch failures non-zero per design)", exit)
	}
	if !strings.Contains(errBuf.String(), "could not reach GitHub") {
		t.Fatalf("stderr = %q, want 'could not reach GitHub'", errBuf.String())
	}
}

func TestRunUpdateCore_NonTTY_NoRestartFlag_Refuses(t *testing.T) {
	t.Parallel()
	opts, _, errBuf := newOpts("v0.32.0")
	opts.isTTY = false
	installed := false
	exit := runUpdateCore(context.Background(), opts, fakeFetch("v9.9.9"),
		recordingInstall(&installed), nil)
	if exit != 2 {
		t.Fatalf("exit = %d, want 2 (ambiguous mode)", exit)
	}
	if installed {
		t.Fatal("install ran in ambiguous mode")
	}
	if !strings.Contains(errBuf.String(), "ambiguous in non-interactive mode") {
		t.Fatalf("stderr = %q, want 'ambiguous in non-interactive mode'", errBuf.String())
	}
}

func TestRunUpdateCore_NonTTY_RestartFlag_OK(t *testing.T) {
	t.Parallel()
	opts, _, _ := newOpts("v0.32.0")
	opts.isTTY = false
	opts.restart = true
	installed := false
	exit := runUpdateCore(context.Background(), opts, fakeFetch("v9.9.9"),
		recordingInstall(&installed), recordingRestart(new(bool)))
	if exit != 0 {
		t.Fatalf("exit = %d, want 0", exit)
	}
	if !installed {
		t.Fatal("install did not run")
	}
}

func TestRunUpdateCore_NonTTY_NoRestartFlag_OK(t *testing.T) {
	t.Parallel()
	opts, _, _ := newOpts("v0.32.0")
	opts.isTTY = false
	opts.noRestart = true
	installed := false
	exit := runUpdateCore(context.Background(), opts, fakeFetch("v9.9.9"),
		recordingInstall(&installed), recordingRestart(new(bool)))
	if exit != 0 {
		t.Fatalf("exit = %d, want 0", exit)
	}
	if !installed {
		t.Fatal("install did not run")
	}
}

func TestRunUpdateCore_NonTTY_Check_DoesNotRequireFlag(t *testing.T) {
	t.Parallel()
	// --check is a query, not an install — should NOT require
	// --restart/--no-restart in non-TTY mode.
	opts, out, _ := newOpts("v0.32.0")
	opts.isTTY = false
	opts.check = true
	exit := runUpdateCore(context.Background(), opts, fakeFetch("v9.9.9"), nil, nil)
	if exit != 0 {
		t.Fatalf("exit = %d, want 0", exit)
	}
	if !strings.Contains(out.String(), "would install") {
		t.Fatalf("output = %q, want 'would install'", out.String())
	}
}

func TestRunUpdateCore_TTYPrompt_YesInstalls(t *testing.T) {
	t.Parallel()
	opts, _, _ := newOpts("v0.32.0")
	opts.in = strings.NewReader("y\n")
	opts.isTTY = true
	installed := false
	restarted := false
	exit := runUpdateCore(context.Background(), opts, fakeFetch("v9.9.9"),
		recordingInstall(&installed), recordingRestart(&restarted))
	if exit != 0 {
		t.Fatalf("exit = %d, want 0", exit)
	}
	if !installed {
		t.Fatal("install did not run")
	}
	if !restarted {
		t.Fatal("shared stack restart did not run after a yes answer")
	}
}

func TestRunUpdateCore_TTYPrompt_NoSkipsRestart(t *testing.T) {
	t.Parallel()
	opts, out, _ := newOpts("v0.32.0")
	opts.in = strings.NewReader("n\n")
	opts.isTTY = true
	installed := false
	restarted := false
	exit := runUpdateCore(context.Background(), opts, fakeFetch("v9.9.9"),
		recordingInstall(&installed), recordingRestart(&restarted))
	if exit != 0 {
		t.Fatalf("exit = %d, want 0\nout:%s", exit, out.String())
	}
	if !installed {
		t.Fatal("install did not run")
	}
	if restarted {
		t.Fatal("daemon restart fired despite 'n' prompt answer")
	}
}

func TestRunUpdateCore_NoAssetForHost(t *testing.T) {
	t.Parallel()
	opts, _, errBuf := newOpts("v0.32.0")
	opts.restart = true
	// Construct a release that has SHA256SUMS but only plan9 assets
	// — AssetForHost will fail on every supported host.
	fetch := func(ctx context.Context, _ string) (*update.Release, error) {
		return &update.Release{
			TagName: "v9.9.9",
			Assets: []update.Asset{
				{Name: "SHA256SUMS", URL: "https://example/SHA256SUMS"},
				{Name: "canary-v9.9.9-plan9-mips.tar.gz", URL: "https://example/plan9"},
			},
		}, nil
	}
	installed := false
	exit := runUpdateCore(context.Background(), opts, fetch, recordingInstall(&installed), nil)
	if exit != 1 {
		t.Fatalf("exit = %d, want 1", exit)
	}
	if installed {
		t.Fatal("install ran despite missing platform asset")
	}
	if !strings.Contains(errBuf.String(), "no release asset") {
		t.Fatalf("stderr = %q, want 'no release asset'", errBuf.String())
	}
}

func TestRunUpdateCore_InstallInProgress(t *testing.T) {
	t.Parallel()
	opts, _, errBuf := newOpts("v0.32.0")
	opts.restart = true
	fail := func(ctx context.Context, plan *update.Plan) error {
		return update.ErrInstallInProgress
	}
	exit := runUpdateCore(context.Background(), opts, fakeFetch("v9.9.9"), fail, nil)
	if exit != 1 {
		t.Fatalf("exit = %d, want 1", exit)
	}
	if !strings.Contains(errBuf.String(), "another Canary update is already running") {
		t.Fatalf("stderr = %q, want 'another Canary update is already running'", errBuf.String())
	}
}

func TestRunUpdateCore_RestartRunsAfterInstallAndPropagatesFailure(t *testing.T) {
	installDir := t.TempDir()
	t.Setenv("CANARY_INSTALL_DIR", installDir)
	opts, _, errBuf := newOpts("v0.32.0")
	opts.restart = true
	var events []string
	install := func(context.Context, *update.Plan) error {
		events = append(events, "install")
		return nil
	}
	var restartedExecutable string
	restart := func(_ context.Context, executable string, _ io.Writer, _ io.Writer) int {
		events = append(events, "stack-restart")
		restartedExecutable = executable
		return 1
	}
	exit := runUpdateCore(context.Background(), opts, fakeFetch("v9.9.9"), install, restart)
	if exit != 1 {
		t.Fatalf("exit = %d, want propagated restart failure", exit)
	}
	if got := strings.Join(events, ","); got != "install,stack-restart" {
		t.Fatalf("events = %q, want install,stack-restart", got)
	}
	if want := filepath.Join(installDir, "canary"); restartedExecutable != want {
		t.Fatalf("restart executable = %q, want installed path %q", restartedExecutable, want)
	}
	if !strings.Contains(errBuf.String(), "ordered but non-atomic") {
		t.Fatalf("stderr missing non-atomic guidance: %s", errBuf.String())
	}
}

func TestRestartInstalledStackAdapterPinsBothProcessesToInstalledExecutable(t *testing.T) {
	t.Setenv("CANARY_SOCKET", "")
	installedExecutable := filepath.Join(t.TempDir(), "installed", "canary")
	var daemonExecutable, appExecutable string

	daemonFactory := func(executable string) restartDeps {
		daemonExecutable = executable
		return restartDeps{
			find: func(context.Context, string) (update.DaemonProcess, error) {
				return update.DaemonProcess{}, update.ErrDaemonNotRunning
			},
		}
	}
	appFactory := func(executable string) appRestartDeps {
		appExecutable = executable
		return appRestartDeps{
			find: func(context.Context) (appProcess, error) {
				return appProcess{}, errAppNotRunning
			},
		}
	}

	var out, errBuf bytes.Buffer
	exit := restartInstalledStackAdapterUsing(context.Background(), installedExecutable, &out, &errBuf, daemonFactory, appFactory)
	if exit != 0 {
		t.Fatalf("exit = %d, stderr=%s", exit, errBuf.String())
	}
	if daemonExecutable != installedExecutable {
		t.Fatalf("daemon executable = %q, want installed path %q", daemonExecutable, installedExecutable)
	}
	if appExecutable != installedExecutable {
		t.Fatalf("app executable = %q, want installed path %q", appExecutable, installedExecutable)
	}
}

func TestParseUpdateFlags_RestartAndNoRestart_Rejected(t *testing.T) {
	t.Parallel()
	opts := &updateOptions{}
	out, errBuf := &bytes.Buffer{}, &bytes.Buffer{}
	exit, ok := parseUpdateFlags([]string{"--restart", "--no-restart"}, opts, out, errBuf)
	if ok {
		t.Fatal("ok=true for mutually-exclusive flags")
	}
	if exit != 2 {
		t.Fatalf("exit = %d, want 2", exit)
	}
}

func TestVersionNewer(t *testing.T) {
	t.Parallel()
	tcs := []struct {
		latest, installed string
		want              bool
	}{
		{"v9.9.9", "v0.32.0", true},
		{"v0.32.0", "v9.9.9", false},
		{"v9.9.9", "v9.9.9", false},
		{"v9.9.9", "dev", true},
		{"v9.9.9", "", true},
		{"garbage", "v0.32.0", false},
		{"v0.32.0", "garbage", false},
	}
	for _, tc := range tcs {
		got := versionNewer(tc.latest, tc.installed)
		if got != tc.want {
			t.Errorf("versionNewer(%q, %q) = %v, want %v", tc.latest, tc.installed, got, tc.want)
		}
	}
}

func TestNormalizeVersion(t *testing.T) {
	t.Parallel()
	tcs := map[string]string{
		"":         "",
		"dev":      "dev",
		"0.32.0":   "v0.32.0",
		"v0.32.0":  "v0.32.0",
		"  v1.0.0": "v1.0.0",
	}
	for in, want := range tcs {
		if got := normalizeVersion(in); got != want {
			t.Errorf("normalizeVersion(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestPromptRestart(t *testing.T) {
	t.Parallel()
	tcs := []struct {
		input string
		want  bool
	}{
		{"\n", true}, // enter → default Y
		{"y\n", true},
		{"Y\n", true},
		{"yes\n", true},
		{"n\n", false},
		{"no\n", false},
		{"", true}, // EOF before any input → default Y
	}
	for _, tc := range tcs {
		got := promptRestart(strings.NewReader(tc.input), &bytes.Buffer{})
		if got != tc.want {
			t.Errorf("promptRestart(%q) = %v, want %v", tc.input, got, tc.want)
		}
	}
}
