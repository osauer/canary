//go:build darwin

package dial

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// fakeLaunchd stands in for launchctl: it records the calls and answers
// print for a loaded job with a pid. A bootout takes effect only after
// lingering further print calls, as launchd's does while the job exits.
type fakeLaunchd struct {
	calls     []string
	loaded    bool
	pid       int
	lingering int
	unloading int
	// refuseBootstrap makes every bootstrap fail, as a launchd that will not
	// take the job does.
	refuseBootstrap bool
}

func (f *fakeLaunchd) run(_ context.Context, args ...string) ([]byte, error) {
	f.calls = append(f.calls, strings.Join(args, " "))
	switch args[0] {
	case "print":
		if args[1] == agentDomain() {
			return nil, nil
		}
		if f.loaded && f.unloading > 0 {
			if f.unloading--; f.unloading == 0 {
				f.loaded = false
			}
		}
		if !f.loaded {
			return nil, errors.New("not loaded")
		}
		return []byte("\tstate = running\n\tpid = " + strconv.Itoa(f.pid) + "\n"), nil
	case "bootout":
		if f.lingering > 0 {
			f.unloading = f.lingering
		} else {
			f.loaded = false
		}
	case "bootstrap":
		if f.loaded || f.refuseBootstrap {
			return []byte("Bootstrap failed: 5: Input/output error"), errors.New("exit status 5")
		}
		f.loaded = true
	}
	return nil, nil
}

func (f *fakeLaunchd) called(verb string) bool {
	for _, call := range f.calls {
		if strings.HasPrefix(call, verb+" ") {
			return true
		}
	}
	return false
}

func agentFixture(t *testing.T) (*fakeLaunchd, string, *[]string) {
	t.Helper()
	home := t.TempDir()
	for _, key := range append(append([]string{"CANARY_SOCKET", daemonAgentEnv}, agentScopeKeys...), agentPassKeys...) {
		t.Setenv(key, "")
	}
	// The socket, lock and log live under the temp home; this is the
	// fixture's scope.
	t.Setenv("XDG_RUNTIME_DIR", filepath.Join(home, "run"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, "state"))
	if err := os.MkdirAll(filepath.Dir(LockPath(DefaultSocketPath())), 0o700); err != nil {
		t.Fatal(err)
	}
	// The pid launchd reports must be a live process other than this one.
	fake := &fakeLaunchd{pid: os.Getppid()}
	var warnings []string
	prevRun, prevHome, prevWarn, prevWait := agentLaunchctl, agentHome, agentWarn, agentUnloadWait
	agentLaunchctl, agentHome = fake.run, func() (string, bool) { return home, true }
	agentWarn = func(format string, _ ...any) { warnings = append(warnings, format) }
	t.Cleanup(func() { agentLaunchctl, agentHome, agentWarn, agentUnloadWait = prevRun, prevHome, prevWarn, prevWait })
	return fake, home, &warnings
}

// A start uses the agent only when it is installed for this binary and this
// scope. Anything else is somebody else's daemon and gets the direct start,
// with launchd left alone.
func TestDaemonAgentIsUsedOnlyForItsOwnBinaryAndScope(t *testing.T) {
	fake, home, warnings := agentFixture(t)
	ctx := t.Context()
	const bin = "/opt/canary & co/bin/canary"

	if _, handled, err := startDaemonViaAgent(ctx, bin, DefaultSocketPath(), false); handled || err != nil {
		t.Fatalf("no agent installed: handled=%v err=%v, want the direct start", handled, err)
	}

	// Restart installs it; loading it starts the daemon.
	t.Setenv("HTTPS_PROXY", "http://proxy.example:3128")
	pid, handled, err := startDaemonViaAgent(ctx, bin, DefaultSocketPath(), true)
	if !handled || err != nil || pid != fake.pid || !fake.called("bootstrap") {
		t.Fatalf("install: pid=%d handled=%v err=%v calls=%v", pid, handled, err, fake.calls)
	}
	plist, err := os.ReadFile(agentPlistPath(home))
	if err != nil {
		t.Fatal(err)
	}
	// The agent can hold a proxy password: nobody else may read it.
	if info, err := os.Stat(agentPlistPath(home)); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("agent file mode %v, want 0600 (err %v)", info.Mode().Perm(), err)
	}
	for _, want := range []string{"<string>/opt/canary &amp; co/bin/canary</string>", "<string>daemon</string>", "<key>SuccessfulExit</key>\n\t\t<false/>", "<key>XDG_STATE_HOME</key>", daemonAgentEnv, "<key>HTTPS_PROXY</key>"} {
		if !strings.Contains(string(plist), want) {
			t.Fatalf("agent plist lacks %q:\n%s", want, plist)
		}
	}

	// The same binary and scope start through launchd, whatever else the
	// starter's environment holds: an app has no proxy variable and other
	// CANARY_* settings of its own.
	t.Setenv("HTTPS_PROXY", "")
	t.Setenv("CANARY_COLOR", "always")
	fake.calls = nil
	if pid, handled, err := startDaemonViaAgent(ctx, bin, DefaultSocketPath(), false); !handled || err != nil || pid != fake.pid || !fake.called("kickstart") {
		t.Fatalf("own agent: pid=%d handled=%v err=%v calls=%v", pid, handled, err, fake.calls)
	}

	// Another binary is not this agent's daemon.
	fake.calls = nil
	if _, handled, err := startDaemonViaAgent(ctx, "/tmp/checkout/bin/canary", DefaultSocketPath(), false); handled || err != nil || fake.called("kickstart") {
		t.Fatalf("another binary: handled=%v err=%v calls=%v, want the direct start", handled, err, fake.calls)
	}

	// Another scope leaves the agent alone, also on a restart, and says so.
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "other"))
	for _, install := range []bool{false, true} {
		fake.calls = nil
		if _, handled, err := startDaemonViaAgent(ctx, bin, DefaultSocketPath(), install); handled || err != nil || fake.called("kickstart") || fake.called("bootout") || fake.called("bootstrap") {
			t.Fatalf("another scope (install=%v): handled=%v err=%v calls=%v, want the direct start", install, handled, err, fake.calls)
		}
	}
	if len(*warnings) != 1 {
		t.Fatalf("a restart that could not use the agent must say so once, got %d warnings", len(*warnings))
	}
	if after, _ := os.ReadFile(agentPlistPath(home)); string(after) != string(plist) {
		t.Fatal("a starter in another scope rewrote the agent")
	}
}

// A variable set to its own default names the same scope as the variable
// unset: a shell that exports XDG_CONFIG_HOME=~/.config and an app that
// exports nothing mean one daemon.
func TestDaemonAgentScopeIgnoresVariablesAtTheirDefault(t *testing.T) {
	_, home, _ := agentFixture(t)
	bare := agentScopeID(agentScope(home))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("XDG_CACHE_HOME", filepath.Join(home, ".cache")+"/")
	if got := agentScopeID(agentScope(home)); got != bare {
		t.Fatal("defaults spelled out changed the scope")
	}
	t.Setenv("CANARY_CONFIG", filepath.Join(home, "alt.toml"))
	if got := agentScopeID(agentScope(home)); got == bare {
		t.Fatal("another config file is the same scope")
	}
}

// A second instance must never reach the user's launchd domain: another
// socket, or a HOME that is not the account's own (every hermetic test).
func TestDaemonAgentIsOnlyForTheAccountsDefaultDaemon(t *testing.T) {
	t.Setenv("CANARY_SOCKET", filepath.Join(t.TempDir(), "x.sock"))
	if _, ok := accountHome(); ok {
		t.Fatal("an overridden socket may use the agent")
	}
	t.Setenv("CANARY_SOCKET", "")
	t.Setenv("HOME", t.TempDir())
	if _, ok := accountHome(); ok {
		t.Fatal("a HOME that is not the account's may use the agent")
	}
}

// launchd's bootout returns before the job has exited, and a bootstrap in
// that moment fails. A reinstall waits for the unload. Removing the agent
// deletes the file even when the unload does not finish.
func TestDaemonAgentReinstallWaitsForTheUnloadAndRemoveDeletesTheFile(t *testing.T) {
	fake, home, warnings := agentFixture(t)
	ctx := t.Context()
	if _, _, err := startDaemonViaAgent(ctx, "/opt/a/canary", DefaultSocketPath(), true); err != nil {
		t.Fatal(err)
	}
	fake.calls, fake.lingering = nil, 3
	if _, handled, err := startDaemonViaAgent(ctx, "/opt/b/canary", DefaultSocketPath(), true); !handled || err != nil || len(*warnings) != 0 {
		t.Fatalf("reinstall over a lingering job: handled=%v err=%v warnings=%v calls=%v", handled, err, *warnings, fake.calls)
	}
	if plist, _ := os.ReadFile(agentPlistPath(home)); !strings.Contains(string(plist), "/opt/b/canary") {
		t.Fatalf("the agent still names the old binary:\n%s", plist)
	}

	// An unload that never finishes: the file still goes.
	fake.lingering, agentUnloadWait = 1_000_000, 200*time.Millisecond
	removed, err := RemoveDaemonAgent(ctx)
	if !removed || err == nil {
		t.Fatalf("remove with a stuck unload: removed=%v err=%v, want the error reported", removed, err)
	}
	if _, err := os.Stat(agentPlistPath(home)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the agent file survived removal: %v", err)
	}
	fake.loaded, fake.unloading, fake.lingering = false, 0, 0
	if removed, err := RemoveDaemonAgent(ctx); err != nil || removed {
		t.Fatalf("removing nothing: removed=%v err=%v", removed, err)
	}
}

// Before a restart starts its daemon it unloads the agent, so a daemon
// launchd restarted after the stop is gone. That holds for every agent whose
// daemon owns this socket, whatever configuration installed it: it would
// hold the same instance lock. An agent on another socket is not touched.
func TestRestartQuiescesTheAgentOnItsSocket(t *testing.T) {
	fake, home, _ := agentFixture(t)
	ctx := t.Context()
	if _, _, err := startDaemonViaAgent(ctx, "/opt/a/canary", DefaultSocketPath(), true); err != nil {
		t.Fatal(err)
	}
	fake.calls, fake.lingering = nil, 2
	if err := QuiesceDaemonAgent(ctx, DefaultSocketPath()); err != nil || fake.loaded || !fake.called("bootout") {
		t.Fatalf("quiesce: err=%v loaded=%v calls=%v", err, fake.loaded, fake.calls)
	}
	// Another config file, the same socket.
	fake.loaded, fake.calls, fake.lingering = true, nil, 0
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "other"))
	if err := QuiesceDaemonAgent(ctx, DefaultSocketPath()); err != nil || fake.loaded {
		t.Fatalf("an agent of another configuration on this socket stayed loaded: err=%v calls=%v", err, fake.calls)
	}
	// Another socket.
	fake.loaded, fake.calls = true, nil
	t.Setenv("XDG_RUNTIME_DIR", filepath.Join(home, "run2"))
	if err := QuiesceDaemonAgent(ctx, DefaultSocketPath()); err != nil || !fake.loaded || fake.called("bootout") {
		t.Fatalf("an agent on another socket was unloaded: err=%v calls=%v", err, fake.calls)
	}
}

// A restart that fails after it unloaded the agent leaves the agent file in
// place and unloaded. The next start, from any client, loads it again: the
// daemon must not quietly end up in its starter's coalition.
func TestPlainStartLoadsAnInstalledAgentThatIsNotLoaded(t *testing.T) {
	fake, _, warnings := agentFixture(t)
	ctx := t.Context()
	const bin = "/opt/canary/bin/canary"
	if _, _, err := startDaemonViaAgent(ctx, bin, DefaultSocketPath(), true); err != nil {
		t.Fatal(err)
	}
	if err := QuiesceDaemonAgent(ctx, DefaultSocketPath()); err != nil || fake.loaded {
		t.Fatalf("quiesce: err=%v loaded=%v", err, fake.loaded)
	}
	fake.calls = nil
	pid, handled, err := startDaemonViaAgent(ctx, bin, DefaultSocketPath(), false)
	if !handled || err != nil || pid != fake.pid || !fake.loaded || !fake.called("bootstrap") {
		t.Fatalf("plain start on an unloaded own agent: pid=%d handled=%v err=%v calls=%v", pid, handled, err, fake.calls)
	}
	if len(*warnings) != 0 {
		t.Fatalf("unexpected warnings: %v", *warnings)
	}
}

// When launchd will not take the agent, the start says so and the daemon is
// started directly; nothing half-installed stays behind after a restart.
func TestAgentThatCannotBeLoadedIsSaidAndTheStartGoesDirect(t *testing.T) {
	fake, home, warnings := agentFixture(t)
	ctx := t.Context()
	fake.refuseBootstrap = true
	if _, handled, err := startDaemonViaAgent(ctx, "/opt/canary/bin/canary", DefaultSocketPath(), true); handled || err != nil {
		t.Fatalf("install refused by launchd: handled=%v err=%v, want the direct start", handled, err)
	}
	if len(*warnings) != 1 {
		t.Fatalf("a failed install must be said once, got %d warnings", len(*warnings))
	}
	if _, err := os.Stat(agentPlistPath(home)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a failed install left its file behind: %v", err)
	}
}

// The agent is recognised by what it says, not by how the file is spelled:
// macOS tools rewrite plists, to other escaping or to the binary format.
func TestInstalledAgentIsReadHoweverTheFileIsSpelled(t *testing.T) {
	_, home, _ := agentFixture(t)
	ctx := t.Context()
	const bin = "/opt/it's \"canary\" & co/bin/canary"
	if _, _, err := startDaemonViaAgent(ctx, bin, DefaultSocketPath(), true); err != nil {
		t.Fatal(err)
	}
	want := installedAgent(ctx, home)
	if want.program != bin || !want.isDaemon || want.scopeID == "" || want.socket != DefaultSocketPath() {
		t.Fatalf("own agent read back wrong: %+v", want)
	}
	if out, err := exec.Command("/usr/bin/plutil", "-convert", "binary1", agentPlistPath(home)).CombinedOutput(); err != nil {
		t.Skipf("plutil cannot rewrite the file here: %v: %s", err, out)
	}
	if got := installedAgent(ctx, home); got != want {
		t.Fatalf("after a rewrite to the binary format the agent reads %+v, want %+v", got, want)
	}
}

// A daemon launchd started does not wait for the startup reservation: the
// starter that asked for it holds it while it waits for the socket.
func TestDaemonUnderTheAgentDoesNotWaitForTheStartupReservation(t *testing.T) {
	agentFixture(t)
	socket := DefaultSocketPath()
	_, release, err := WithStartupLock(t.Context(), socket)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	t.Setenv(daemonAgentEnv, "launchd")
	done := make(chan error, 1)
	go func() {
		releaseDaemon, err := LockDaemonStartup(context.Background(), socket)
		if err == nil {
			releaseDaemon()
		}
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the launchd-started daemon waited for the reservation its own starter holds")
	}
}

// A file under Canary's label that names no scope, hand-written or from an
// early build, is not another configuration's agent. A plain start leaves it
// alone and goes direct; a restart replaces it.
func TestDaemonAgentWithoutAScopeIsReplacedByARestart(t *testing.T) {
	fake, home, warnings := agentFixture(t)
	ctx := t.Context()
	const bin = "/opt/canary/bin/canary"
	path := agentPlistPath(home)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	old := `<?xml version="1.0" encoding="UTF-8"?><plist version="1.0"><dict><key>ProgramArguments</key><array><string>` + bin + `</string><string>daemon</string></array></dict></plist>`
	if err := os.WriteFile(path, []byte(old), 0o644); err != nil {
		t.Fatal(err)
	}
	fake.loaded = true
	if _, handled, err := startDaemonViaAgent(ctx, bin, DefaultSocketPath(), false); handled || err != nil || fake.called("kickstart") {
		t.Fatalf("plain start on an unmarked agent: handled=%v err=%v calls=%v", handled, err, fake.calls)
	}
	if _, handled, err := startDaemonViaAgent(ctx, bin, DefaultSocketPath(), true); !handled || err != nil || len(*warnings) != 0 {
		t.Fatalf("restart over an unmarked agent: handled=%v err=%v warnings=%v", handled, err, *warnings)
	}
	if file := installedAgent(ctx, home); file.scopeID != agentScopeID(agentScope(home)) || file.program != bin || !file.isDaemon {
		t.Fatalf("the restart did not replace the unmarked agent with its own: %+v", file)
	}
}

// A caller that names another socket is starting another daemon. It never
// reaches the agent, although its environment is the default one: a test
// that started a daemon on a temp socket once unloaded the live agent.
func TestAnotherSocketNeverReachesTheAgent(t *testing.T) {
	fake, _, _ := agentFixture(t)
	ctx := t.Context()
	const bin = "/opt/canary/bin/canary"
	if _, _, err := startDaemonViaAgent(ctx, bin, DefaultSocketPath(), true); err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(t.TempDir(), "other.sock")
	fake.calls = nil
	for _, install := range []bool{false, true} {
		if _, handled, err := startDaemonViaAgent(ctx, bin, other, install); handled || err != nil {
			t.Fatalf("another socket (install=%v): handled=%v err=%v", install, handled, err)
		}
	}
	if err := QuiesceDaemonAgent(ctx, other); err != nil {
		t.Fatal(err)
	}
	if len(fake.calls) != 0 || !fake.loaded {
		t.Fatalf("a start on another socket reached launchd: %v", fake.calls)
	}
}

// A starter whose time ran out while it read the agent knows nothing about
// it: it neither replaces nor removes the file, and reports its own error
// instead of starting a daemon beside the agent.
func TestCancelledStartLeavesTheAgentAlone(t *testing.T) {
	fake, home, warnings := agentFixture(t)
	const bin = "/opt/canary/bin/canary"
	if _, _, err := startDaemonViaAgent(t.Context(), bin, DefaultSocketPath(), true); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(agentPlistPath(home))
	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	fake.calls = nil
	for _, install := range []bool{false, true} {
		if _, handled, err := startDaemonViaAgent(cancelled, bin, DefaultSocketPath(), install); !handled || err == nil {
			t.Fatalf("cancelled start (install=%v): handled=%v err=%v, want the caller's own error", install, handled, err)
		}
	}
	if after, err := os.ReadFile(agentPlistPath(home)); err != nil || string(after) != string(before) || !fake.loaded || fake.called("bootout") || len(*warnings) != 0 {
		t.Fatalf("a cancelled start touched the agent: err=%v loaded=%v calls=%v warnings=%v", err, fake.loaded, fake.calls, *warnings)
	}
}

// The agent's binary and the starter's are the same file also when one of
// them is reached through a symlink.
func TestAgentBinaryMatchesThroughASymlink(t *testing.T) {
	fake, _, _ := agentFixture(t)
	ctx := t.Context()
	dir := t.TempDir()
	real := filepath.Join(dir, "canary-real")
	link := filepath.Join(dir, "canary")
	if err := os.WriteFile(real, []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	if _, _, err := startDaemonViaAgent(ctx, link, DefaultSocketPath(), true); err != nil {
		t.Fatal(err)
	}
	fake.calls = nil
	if _, handled, err := startDaemonViaAgent(ctx, real, DefaultSocketPath(), false); !handled || err != nil || !fake.called("kickstart") {
		t.Fatalf("the resolved path did not match the agent installed through the symlink: handled=%v err=%v calls=%v", handled, err, fake.calls)
	}
}
