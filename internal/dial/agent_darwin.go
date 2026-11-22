//go:build darwin

package dial

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/osauer/canary/v3/internal/productidentity"
)

// macOS ends every process in an app's coalition when the app quits and may
// not leave background processes behind. A daemon started by fork and exec is
// in its starter's coalition whatever session it is given, so a daemon
// started from an app, or from a terminal inside one, died with it and left
// no line in its log (2026-11-22). Only launchd starts a process outside the
// starter's coalition. So the default daemon runs as a per-user launchd
// agent: `canary restart` installs it, and every start goes through it while
// it names this binary and this environment. Anything else keeps the direct
// start. Design: internal-docs/design/daemon-lifetime.md.

// daemonAgentEnv marks a daemon launchd started; its value is "launchd".
// docgen:env CANARY_DAEMON_SUPERVISOR | Set to `launchd` by the daemon's macOS launchd agent, never by hand. It tells the daemon that launchd started it, so it ends with status 0 when it refuses to start and honours a stop before it starts.
const daemonAgentEnv = "CANARY_DAEMON_SUPERVISOR"

// agentLaunchctl runs launchctl; tests replace it.
var agentLaunchctl = func(ctx context.Context, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, "/bin/launchctl", args...).CombinedOutput()
}

// RunningUnderDaemonAgent reports whether launchd started this process as
// the daemon agent.
func RunningUnderDaemonAgent() bool {
	return os.Getenv(daemonAgentEnv) == "launchd"
}

// agentHome is the account's home directory when this process may use the
// agent at all: the default socket, and a HOME that is the account's own. A
// test or a second instance with its own HOME, socket or state must never
// install a job into the user's launchd domain.
var agentHome = accountHome

func accountHome() (string, bool) {
	if SocketPathOverridden() {
		return "", false
	}
	u, err := user.Current()
	if err != nil || u.HomeDir == "" || u.Uid == "0" {
		return "", false
	}
	if home, err := os.UserHomeDir(); err != nil || filepath.Clean(home) != filepath.Clean(u.HomeDir) {
		return "", false
	}
	return u.HomeDir, true
}

func agentPlistPath(home string) string {
	return filepath.Join(home, "Library", "LaunchAgents", productidentity.DaemonLaunchAgentLabel+".plist")
}

func agentDomain() string { return "gui/" + strconv.Itoa(os.Getuid()) }

func agentTarget() string { return agentDomain() + "/" + productidentity.DaemonLaunchAgentLabel }

// agentScopeKeys are the variables that decide which config, state, log and
// socket a daemon uses. They are the agent's scope: the agent runs one
// daemon, for the scope that installed it, and a starter in another scope
// gets the direct start and leaves the agent alone.
var agentScopeKeys = []string{"XDG_CONFIG_HOME", "XDG_STATE_HOME", "XDG_CACHE_HOME", "XDG_RUNTIME_DIR", "CANARY_CONFIG", "CANARY_LOG", "CANARY_SPX_MEMBERS_AUTO_REFRESH"}

// agentPassKeys are carried into the agent from the shell that installs it
// and are not part of the scope. launchd gives a job a bare environment, so
// without them a daemon behind a proxy would lose its feeds.
var agentPassKeys = []string{"HTTPS_PROXY", "HTTP_PROXY", "ALL_PROXY", "NO_PROXY", "https_proxy", "http_proxy", "all_proxy", "no_proxy", "TZ", "IBKR_PACKET_LOG_TEMPLATE"}

// agentScopeEnv and agentSocketEnv are written into the agent's environment
// as its identity: the scope it serves and the socket its daemon owns. The
// daemon does not read them.
const (
	agentScopeEnv  = "CANARY_DAEMON_AGENT_SCOPE"
	agentSocketEnv = "CANARY_DAEMON_AGENT_SOCKET"
)

// agentScope is this process's scope. A variable set to its own default is
// the same scope as the variable unset: a shell that exports
// XDG_CONFIG_HOME=~/.config and an app that exports nothing mean one daemon.
func agentScope(home string) map[string]string {
	defaults := map[string]string{
		"XDG_CONFIG_HOME": filepath.Join(home, ".config"),
		"XDG_STATE_HOME":  filepath.Join(home, ".local", "state"),
		"XDG_CACHE_HOME":  filepath.Join(home, ".cache"),
	}
	scope := map[string]string{}
	for _, key := range agentScopeKeys {
		value := os.Getenv(key)
		if value == "" || defaults[key] != "" && filepath.Clean(value) == defaults[key] {
			continue
		}
		scope[key] = value
	}
	return scope
}

func agentScopeID(scope map[string]string) string {
	keys := make([]string, 0, len(scope))
	for key := range scope {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	h := sha256.New()
	for _, key := range keys {
		fmt.Fprintf(h, "%s=%s\x00", key, scope[key])
	}
	return hex.EncodeToString(h.Sum(nil))[:32]
}

func plistEscape(s string) string {
	var b bytes.Buffer
	_ = xml.EscapeText(&b, []byte(s))
	return b.String()
}

// renderDaemonAgent is the agent's plist for bin in a scope.
//
// ProcessType Interactive: launchd applies no resource limits. As a
// Background job the daemon took 2m15s to start serving where it takes half
// a minute, and it watches orders.
//
// KeepAlive with SuccessfulExit false: launchd starts the job when it is
// loaded and again after it ended by a signal or a non-zero status, and
// leaves it down after an exit with status 0. The daemon therefore ends
// every exit it chooses with status 0 under the agent.
//
// ExitTimeOut is how long launchd waits after SIGTERM before it kills; it
// must outlast the daemon's own bounded shutdown.
func renderDaemonAgent(bin, logPath string, scope, pass map[string]string) []byte {
	env := map[string]string{}
	maps.Copy(env, pass)
	maps.Copy(env, scope)
	env[daemonAgentEnv], env[agentScopeEnv], env[agentSocketEnv] = "launchd", agentScopeID(scope), DefaultSocketPath()
	keys := make([]string, 0, len(env))
	for key := range env {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>` + plistEscape(productidentity.DaemonLaunchAgentLabel) + `</string>
	<key>ProgramArguments</key>
	<array>
		<string>` + plistEscape(bin) + `</string>
		<string>daemon</string>
	</array>
	<key>EnvironmentVariables</key>
	<dict>
`)
	for _, key := range keys {
		b.WriteString("\t\t<key>" + plistEscape(key) + "</key>\n\t\t<string>" + plistEscape(env[key]) + "</string>\n")
	}
	b.WriteString(`	</dict>
	<key>KeepAlive</key>
	<dict>
		<key>SuccessfulExit</key>
		<false/>
	</dict>
	<key>ProcessType</key>
	<string>Interactive</string>
	<key>ExitTimeOut</key>
	<integer>20</integer>
	<key>StandardOutPath</key>
	<string>` + plistEscape(logPath) + `</string>
	<key>StandardErrorPath</key>
	<string>` + plistEscape(logPath) + `</string>
</dict>
</plist>
`)
	return []byte(b.String())
}

// agentFile is what an installed agent says about itself.
type agentFile struct {
	present  bool
	scopeID  string // "" when the file names no scope
	socket   string
	program  string
	isDaemon bool
}

// agentReadPlist prints a plist as JSON. plutil reads the XML Canary writes
// and whatever a macOS tool rewrote it to, binary included, so "is this my
// agent" never depends on how the file is spelled.
var agentReadPlist = func(ctx context.Context, path string) ([]byte, error) {
	return exec.CommandContext(ctx, "/usr/bin/plutil", "-convert", "json", "-o", "-", path).Output()
}

// installedAgent reads the installed agent. A file that cannot be read as a
// plist is present and says nothing, which a restart replaces.
func installedAgent(ctx context.Context, home string) agentFile {
	path := agentPlistPath(home)
	if _, err := os.Stat(path); err != nil {
		return agentFile{}
	}
	out := agentFile{present: true}
	data, err := agentReadPlist(ctx, path)
	if err != nil {
		return out
	}
	var plist struct {
		ProgramArguments     []string
		EnvironmentVariables map[string]string
	}
	if json.Unmarshal(data, &plist) != nil {
		return out
	}
	out.scopeID, out.socket = plist.EnvironmentVariables[agentScopeEnv], plist.EnvironmentVariables[agentSocketEnv]
	if len(plist.ProgramArguments) == 2 {
		out.program, out.isDaemon = plist.ProgramArguments[0], plist.ProgramArguments[1] == "daemon"
	}
	return out
}

// agentLoaded reports whether launchd has the agent loaded. An error that is
// the caller's own cancelled context is not an answer.
func agentLoaded(ctx context.Context) (bool, error) {
	_, err := agentLaunchctl(ctx, "print", agentTarget())
	if err != nil && ctx.Err() != nil {
		return false, ctx.Err()
	}
	return err == nil, nil
}

// agentPID is the pid of the agent's running process, 0 when it is not
// running or not loaded.
func agentPID(ctx context.Context) int {
	out, err := agentLaunchctl(ctx, "print", agentTarget())
	if err != nil {
		return 0
	}
	for line := range strings.SplitSeq(string(out), "\n") {
		if rest, ok := strings.CutPrefix(strings.TrimSpace(line), "pid = "); ok {
			pid, _ := strconv.Atoi(strings.TrimSpace(rest))
			return pid
		}
	}
	return 0
}

// agentUnloadWait bounds the wait for launchd to finish unloading the agent:
// bootout returns before the job's process has exited.
var agentUnloadWait = 30 * time.Second

// unloadDaemonAgent unloads the agent and waits until launchd no longer has
// it, which is when its daemon has exited. A daemon launchd restarted inside
// a restart's gap ends here, before the restart starts its own.
func unloadDaemonAgent(ctx context.Context) error {
	loaded, err := agentLoaded(ctx)
	if err != nil || !loaded {
		return err
	}
	out, bootErr := agentLaunchctl(ctx, "bootout", agentTarget())
	deadline := time.Now().Add(agentUnloadWait)
	for {
		loaded, err := agentLoaded(ctx)
		if err != nil {
			return err
		}
		if !loaded {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("launchd still has the daemon agent loaded %s after bootout (%v: %s)", agentUnloadWait, bootErr, strings.TrimSpace(string(out)))
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}

func prepareAgentLog(logPath string) error {
	if err := os.MkdirAll(filepath.Dir(logPath), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Chmod(0o600)
}

// installDaemonAgent writes the agent for bin in this scope and loads it,
// replacing a loaded one. Loading starts the daemon.
func installDaemonAgent(ctx context.Context, home, bin string) error {
	logPath := DefaultLogPath()
	if err := prepareAgentLog(logPath); err != nil {
		return fmt.Errorf("prepare daemon log: %w", err)
	}
	if err := unloadDaemonAgent(ctx); err != nil {
		return err
	}
	pass := map[string]string{}
	for _, key := range agentPassKeys {
		if value := os.Getenv(key); value != "" {
			pass[key] = value
		}
	}
	path := agentPlistPath(home)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	// Owner-only: a proxy variable copied into the agent can carry a
	// password. launchd reads a 0600 agent of its own user.
	if err := os.WriteFile(tmp, renderDaemonAgent(bin, logPath, agentScope(home), pass), 0o600); err != nil {
		return err
	}
	if err := os.Chmod(tmp, 0o600); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if out, err := agentLaunchctl(ctx, "bootstrap", agentDomain(), path); err != nil {
		return fmt.Errorf("launchctl bootstrap: %v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// RemoveDaemonAgent unloads the daemon agent, which stops the daemon it
// runs, and deletes its file. It reports whether an agent was there.
func RemoveDaemonAgent(ctx context.Context) (bool, error) {
	home, ok := agentHome()
	if !ok {
		return false, nil
	}
	path := agentPlistPath(home)
	_, statErr := os.Stat(path)
	loaded, _ := agentLoaded(ctx)
	unloadErr := unloadDaemonAgent(ctx)
	// The file goes even when the unload failed: an agent file left behind
	// unloaded would be loaded again at the next login.
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return true, err
	}
	return loaded || statErr == nil, unloadErr
}

// sameExecutable reports whether two paths name one binary, also when one of
// them reaches it through a symlink.
func sameExecutable(a, b string) bool {
	if a == b {
		return true
	}
	ra, errA := filepath.EvalSymlinks(a)
	rb, errB := filepath.EvalSymlinks(b)
	return errA == nil && errB == nil && ra == rb
}

// agentWarn tells the person at the terminal that the daemon is not under
// the agent although it should be. Silence here would leave the daemon in
// its starter's coalition with nobody knowing.
var agentWarn = func(format string, args ...any) {
	fmt.Fprintf(os.Stderr, productidentity.Executable+": "+format+"\n", args...)
}

// QuiesceDaemonAgent ends a daemon the agent is running, or about to run
// again, and unloads the agent. `canary restart` calls it after it stopped
// the daemon and before it starts the new one: launchd restarts a daemon
// that ended by a kill or a non-zero status at once, and that daemon must
// not hold the instance lock when the restart starts its own. That holds for
// any agent whose daemon owns this socket, whatever scope installed it, and
// for a file that does not say; an agent on another socket is left alone,
// and so is everything when socketPath is not the default daemon's.
// The wait has its own bound, outside the caller's start budget: unloading a
// daemon that is busy shutting down can take as long as its shutdown.
func QuiesceDaemonAgent(ctx context.Context, socketPath string) error {
	home, ok := agentHome()
	if !ok || RunningUnderDaemonAgent() || socketPath != DefaultSocketPath() {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), agentUnloadWait+5*time.Second)
	defer cancel()
	if file := installedAgent(ctx, home); !file.present || file.socket != "" && file.socket != DefaultSocketPath() {
		return nil
	}
	return unloadDaemonAgent(ctx)
}

// startDaemonViaAgent starts bin's daemon through the launchd agent and
// returns its pid. handled is false when the agent is not this starter's to
// use, and the caller starts the daemon directly: another socket or HOME, no
// GUI launchd domain (an SSH login), an agent installed for another scope,
// or, without install, an agent that is missing or names another binary.
// install is the restart path: it writes the agent for bin and loads it.
// An own agent that is installed and not loaded, as a failed restart leaves
// it, is loaded again by the next start.
func startDaemonViaAgent(ctx context.Context, bin, socketPath string, install bool) (pid int, handled bool, err error) {
	home, ok := agentHome()
	// The agent is for the daemon on the default socket. A caller that
	// names another socket, as every test and harness does, is starting
	// another daemon, whatever its environment says.
	if !ok || RunningUnderDaemonAgent() || socketPath != DefaultSocketPath() {
		return 0, false, nil
	}
	if _, err := agentLaunchctl(ctx, "print", agentDomain()); err != nil {
		return 0, false, nil
	}
	file := installedAgent(ctx, home)
	// A caller whose time ran out while the file was being read knows
	// nothing about the agent. It must not replace it, remove it or start a
	// daemon beside it.
	if err := ctx.Err(); err != nil {
		return 0, true, err
	}
	mine := file.scopeID == agentScopeID(agentScope(home))
	// A file under Canary's label that names no scope is not another
	// scope's agent: a restart replaces it, any other start goes direct.
	unmarked := file.present && file.scopeID == ""
	switch {
	case unmarked && !install:
		return 0, false, nil
	case file.present && !mine && !unmarked:
		// One agent per user. It belongs to the scope that installed it.
		if install {
			agentWarn("the daemon's launchd agent serves another configuration (other XDG_* or CANARY_* directories); this daemon starts directly and stops when the app that started it quits. `canary daemon --remove-agent` in that configuration frees the agent.")
		}
		return 0, false, nil
	case install:
		if err := installDaemonAgent(ctx, home, bin); err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return 0, true, ctxErr
			}
			_, _ = RemoveDaemonAgent(ctx)
			agentWarn("could not install the daemon's launchd agent (%v); the daemon starts directly and stops when the app that started it quits", err)
			return 0, false, nil
		}
	default:
		if !file.present || !file.isDaemon || !sameExecutable(file.program, bin) {
			return 0, false, nil
		}
		loaded, err := agentLoaded(ctx)
		if err != nil {
			return 0, true, err
		}
		if !loaded {
			// Loading starts the daemon.
			if out, err := agentLaunchctl(ctx, "bootstrap", agentDomain(), agentPlistPath(home)); err != nil {
				agentWarn("could not load the daemon's launchd agent (%v: %s); the daemon starts directly and stops when the app that started it quits", err, strings.TrimSpace(string(out)))
				return 0, false, nil
			}
		} else if out, err := agentLaunchctl(ctx, "kickstart", agentTarget()); err != nil {
			return 0, true, fmt.Errorf("launchctl kickstart: %v: %s", err, strings.TrimSpace(string(out)))
		}
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		// For a moment after launchd spawns the job its process cannot be
		// signalled (EPERM), which the liveness probe reads as gone. Hand
		// the pid back only once it answers.
		if pid := agentPID(ctx); pid > 0 && IsProcessAlive(pid) {
			return pid, true, nil
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			// The usual reason is a daemon that chose to exit at once: a
			// stop intent, or a config it refuses. Its log says which.
			msg := "the daemon launchd started did not stay up; see `launchctl print " + agentTarget() + "`"
			if tail := TailLastLine(DefaultLogPath(), 0); tail != "" {
				msg += "\n  last daemon log: " + tail
			}
			return 0, true, errors.New(msg)
		}
		time.Sleep(50 * time.Millisecond)
	}
}
