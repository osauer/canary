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
	env, socketPath, logPath := lifecycleEnv(t)
	policies := filepath.Join(filepath.Dir(socketPath), "home", ".config", "ibkr", "policies")
	names := []string{"rulebook-policy.toml", "protection-policy.toml", "opportunity-policy.toml", "risk-policy.toml"}

	if out, code := runCLI(t, env, 30*time.Second, "status", "--json"); code != 0 {
		t.Fatalf("first start: exit %d\n%s", code, out)
	}
	for _, name := range names {
		data, err := os.ReadFile(filepath.Join(policies, name))
		if err != nil || !strings.HasPrefix(string(data), "# Canary defaults, not yet reviewed.\n") {
			t.Fatalf("fresh install did not write %s: %v\n%s", name, err, data)
		}
	}
	stopLifecycleDaemon(t, socketPath)

	if err := os.Remove(filepath.Join(policies, "rulebook-policy.toml")); err != nil {
		t.Fatal(err)
	}
	broken := "kind = \"ibkr.protection_policy\"\npolicy_version = [oops\n"
	if err := os.WriteFile(filepath.Join(policies, "protection-policy.toml"), []byte(broken), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, code := runCLI(t, env, 30*time.Second, "status", "--json"); code != 0 {
		log, _ := os.ReadFile(logPath)
		t.Fatalf("a broken policy file stopped the daemon: exit %d\n%s\n%s", code, out, log)
	}
	if data, err := os.ReadFile(filepath.Join(policies, "rulebook-policy.toml")); err != nil || !strings.HasPrefix(string(data), "# Canary defaults, not yet reviewed.\n") {
		t.Fatalf("deleted rulebook file was not written again: %v", err)
	}
	if data, _ := os.ReadFile(filepath.Join(policies, "protection-policy.toml")); string(data) != broken {
		t.Fatal("the daemon rewrote a broken protection file")
	}
	out, code := runCLI(t, env, 30*time.Second, "policy", "show", "--json")
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
