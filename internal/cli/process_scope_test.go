package cli

import (
	"bytes"
	"context"
	"errors"
	"github.com/osauer/canary/v3/internal/procinfo"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

func TestAppArgvRetainsBoundariesAndRejectsWrappers(t *testing.T) {
	exe := installWithSpaces(t)
	args := []string{exe, "app", "--state-dir", "/tmp/app  state", "--public-url", ""}
	got, exact, ok := appArgvMatch(args, executablePathVariants(exe))
	if !ok || !exact || !slices.Equal(got, args[1:]) {
		t.Fatalf("args=%q exact=%v ok=%v", got, exact, ok)
	}
	for _, prefix := range [][]string{{"/bin/sh", "-c"}, {"/usr/bin/env"}, {"echo"}} {
		if _, _, ok := appArgvMatch(append(prefix, args...), nil); ok {
			t.Fatalf("accepted wrapper %q", prefix)
		}
	}
}

func TestScopeUsesEffectivePathsWithoutCallerFallback(t *testing.T) {
	base := []string{"HOME=/synthetic/home", "XDG_RUNTIME_DIR=/synthetic/runtime-a"}
	a, err := scopeFromEnvironment(base)
	if err != nil {
		t.Fatal(err)
	}
	equivalent, err := scopeFromEnvironment(append(slices.Clone(base), "CANARY_SOCKET=/synthetic/runtime-a/ibkr/ibkr.sock", "XDG_CONFIG_HOME=/synthetic/home/.config"))
	if err != nil || a != equivalent {
		t.Fatal("equivalent default scope differs", err)
	}
	for _, entry := range []string{"XDG_RUNTIME_DIR=/synthetic/runtime-b", "HOME=/synthetic/other", "CANARY_CONFIG=/synthetic/other.toml", "XDG_STATE_HOME=/synthetic/state-b"} {
		b, err := scopeFromEnvironment(append(slices.Clone(base), entry))
		if err != nil || a == b {
			t.Fatalf("missed scope difference %s: %v", entry, err)
		}
	}
	for _, env := range [][]string{{}, {"HOME=relative"}, {"HOME=/synthetic/home", "CANARY_SOCKET=relative.sock"}} {
		if _, err := scopeFromEnvironment(env); err == nil {
			t.Fatalf("accepted unattributable scope %q", env)
		}
	}
}

func TestRestartScopeFailurePrecedesStopIntentAndProcessMutation(t *testing.T) {
	t.Setenv("CANARY_SOCKET", "")
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_RUNTIME_DIR", filepath.Join(t.TempDir(), "runtime"))
	var out bytes.Buffer
	opts := &restartOptions{out: &out, err: &out}
	exit := runRestartAllCore(context.Background(), opts, restartDeps{}, appRestartDeps{preflight: func(context.Context) error { return errors.New("different profile") }})
	if exit != 1 {
		t.Fatalf("exit=%d", exit)
	}
	// WithStartupLock would create the runtime directory, so even the startup
	// reservation and stop-intent clearing must not happen before this refusal.
	if _, err := os.Stat(os.Getenv("XDG_RUNTIME_DIR")); !os.IsNotExist(err) {
		t.Fatalf("scope preflight mutated runtime directory: %v", err)
	}
}

func TestStopScopeFailurePrecedesInhibitAndDaemonHealth(t *testing.T) {
	t.Setenv("CANARY_SOCKET", "")
	var out bytes.Buffer
	opts := &stopOptions{app: true, daemon: true, out: &out, err: &out}
	deps := stopDeps{app: appRestartDeps{preflight: func(context.Context) error { return errors.New("different profile") }}, inhibit: func(string) error { t.Fatal("inhibited before scope proof"); return nil }}
	if exit := runStopCore(context.Background(), opts, deps); exit != 1 {
		t.Fatalf("exit=%d", exit)
	}
}

func TestScopeStableWhenStateIsCreatedUnderSymlink(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "target")
	link := filepath.Join(root, "link")
	if err := os.Mkdir(target, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	env := []string{"HOME=" + link}
	before, err := scopeFromEnvironment(env)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(link, ".local/state/ibkr/app"), 0700); err != nil {
		t.Fatal(err)
	}
	after, err := scopeFromEnvironment(env)
	if err != nil || before != after {
		t.Fatalf("scope changed after initialization: %v", err)
	}
}

func TestAppPathGuardsAndRecoveryArguments(t *testing.T) {
	for _, args := range [][]string{{"app", "-state-dir", "relative"}, {"app", "-state-dir=relative"}, {"app", "--log=relative"}} {
		if err := verifyAppPathArgs(args); err == nil {
			t.Fatalf("accepted relative path %q", args)
		}
	}
	for _, args := range [][]string{{"app", "--state-dir", "/tmp/app state"}, {"app", "-log=stderr"}} {
		if err := verifyAppPathArgs(args); err != nil {
			t.Fatal(err)
		}
	}
	args := []string{"app", "--log", "/tmp/app's log", "--preview-read-grant", "--public-url", ""}
	want := "canary app '--log' '/tmp/app'\\''s log' '--preview-read-grant' '--public-url' ''"
	if got := appRecoveryCommand(args); got != want {
		t.Fatalf("recovery=%q want %q", got, want)
	}
}

func TestQuiesceRecordRejectsChangedProfileAndDefinition(t *testing.T) {
	root := testRestartStateHome(t)
	exe := testCurrentExecutable(t)
	plist := writeTestAppLaunchAgent(t, root, exe, "app", "--remote")
	path := writeTestQuiesceMarker(t, plist, exe, time.Now(), "app", "--remote")
	marker, exists, err := readRestartQuiesceMarker(path)
	if err != nil || !exists {
		t.Fatal(err)
	}
	if _, err := validateRestartQuiesceMarker(marker, executablePathVariants(exe), false); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CANARY_APP_ADDR", "127.0.0.1:18899")
	if _, err := validateRestartQuiesceMarker(marker, executablePathVariants(exe), false); err == nil {
		t.Fatal("accepted changed app defaults")
	}
	t.Setenv("CANARY_APP_ADDR", "")
	data, err := os.ReadFile(plist)
	if err != nil {
		t.Fatal(err)
	}
	data = bytes.Replace(data, []byte("<key>KeepAlive</key>"), []byte("<key>EnvironmentVariables</key><dict><key>HOME</key><string>/other/profile</string></dict><key>KeepAlive</key>"), 1)
	if err := os.WriteFile(plist, data, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := validateRestartQuiesceMarker(marker, executablePathVariants(exe), false); err == nil {
		t.Fatal("accepted changed launch definition")
	}
	if err := verifySupervisorDefinition(appSupervisor{PlistPath: plist, Args: []string{"app"}}); err == nil {
		t.Fatal("accepted unattributable custom supervisor environment")
	}
}

func TestSignalRechecksActualSelectedProcessAfterPreflight(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	same := procinfo.Invocation{Args: []string{"/tmp/canary", "app"}, Environment: os.Environ()}
	if err := verifyAppProcessScope(42, func(int) (procinfo.Invocation, error) { return same, nil }); err != nil {
		t.Fatal(err)
	}
	// Discovery selected A during preflight; just before the signal the selected
	// PID/launch identity now belongs to B. No signal may use the old proof.
	changed := same
	changed.Environment = append(slices.Clone(same.Environment), "XDG_RUNTIME_DIR=/other/runtime")
	called := false
	err := signalVerifiedApp(42, time.Second, func(int) (procinfo.Invocation, error) { return changed, nil }, func(int, time.Duration) error { called = true; return nil })
	if err == nil || called {
		t.Fatal("signal reused stale preflight scope proof")
	}
	if err := signalVerifiedApp(42, time.Second, func(int) (procinfo.Invocation, error) { return same, nil }, func(int, time.Duration) error { called = true; return nil }); err != nil || !called {
		t.Fatal("same-profile control did not signal", err)
	}
}
