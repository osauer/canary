package integration

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// Owner decision 2026-09-26, end to end through the real daemon binary in a
// fresh HOME: the first start writes every policy file from Canary's
// defaults; a file deleted between restarts comes back; a broken file is left
// alone and never stops the daemon.
func TestLifecycle_FreshInstallWritesPolicyFilesAndRestartsKeepThem(t *testing.T) {
	t.Parallel()
	env, socketPath, _ := lifecycleEnv(t)
	// An isolated stack keeps its policy files under its own XDG config root; the
	// home directory is the operator default, for a daemon that sets no XDG root.
	policies := filepath.Join(filepath.Dir(socketPath), "config", "ibkr", "policies")
	names := []string{"rulebook-policy.toml", "protection-policy.toml", "opportunity-policy.toml", "risk-policy.toml"}

	out, code := runCLI(t, env, 30*time.Second, "status", "--json")
	requireOfflineDaemonStatus(t, out, code)
	for _, name := range names {
		data, err := os.ReadFile(filepath.Join(policies, name))
		if err != nil || !strings.HasPrefix(string(data), "# Canary defaults, not yet reviewed.\n") {
			t.Fatalf("fresh install did not write %s: %v\n%s", name, err, data)
		}
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(socketPath), "home", ".config", "ibkr", "policies")); !os.IsNotExist(err) {
		t.Fatalf("the stack wrote policies under its home directory: %v", err)
	}
	stopLifecycleDaemon(t, socketPath)

	if err := os.Remove(filepath.Join(policies, "rulebook-policy.toml")); err != nil {
		t.Fatal(err)
	}
	broken := "kind = \"ibkr.protection_policy\"\npolicy_version = [oops\n"
	if err := os.WriteFile(filepath.Join(policies, "protection-policy.toml"), []byte(broken), 0o600); err != nil {
		t.Fatal(err)
	}
	out, code = runCLI(t, env, 30*time.Second, "status", "--json")
	requireOfflineDaemonStatus(t, out, code)
	if data, err := os.ReadFile(filepath.Join(policies, "rulebook-policy.toml")); err != nil || !strings.HasPrefix(string(data), "# Canary defaults, not yet reviewed.\n") {
		t.Fatalf("deleted rulebook file was not written again: %v", err)
	}
	if data, _ := os.ReadFile(filepath.Join(policies, "protection-policy.toml")); string(data) != broken {
		t.Fatal("the daemon rewrote a broken protection file")
	}
	out, code = runCLI(t, env, 30*time.Second, "policy", "show", "--json")
	if code != 0 || !strings.Contains(out, `"policy": "protection"`) || !strings.Contains(out, `"review": "unreviewed"`) {
		t.Fatalf("policy show: exit %d\n%s", code, out)
	}
	stopLifecycleDaemon(t, socketPath)
}

func stopLifecycleDaemon(t *testing.T, socketPath string) {
	t.Helper()
	pid := daemonPID(socketPath)
	if pid == 0 {
		t.Fatal("no daemon to stop")
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		t.Fatal(err)
	}
	if err := proc.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	if !waitForDaemonExit(socketPath, 10*time.Second) {
		t.Fatalf("daemon %d did not exit", pid)
	}
}
