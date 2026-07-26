package productidentity

import (
	"path/filepath"
	"testing"
)

func TestExecutableTransitionContract(t *testing.T) {
	t.Parallel()
	if ProductName != "Canary" || Executable != "canary" || PreUpgradeExecutable != "ibkr" {
		t.Fatalf("unexpected executable identity: %q %q %q", ProductName, Executable, PreUpgradeExecutable)
	}
	for _, name := range []string{Executable, PreUpgradeExecutable} {
		if !IsManagedProcessExecutableBase(name) {
			t.Fatalf("managed process executable %q was not recognized", name)
		}
	}
	if IsManagedProcessExecutableBase("ibkr-trading") || IsManagedProcessExecutableBase("canaryd") {
		t.Fatal("an unrelated executable was recognized")
	}
	dir := filepath.Join("tmp", "bin")
	if got := CanonicalPath(dir); got != filepath.Join(dir, "canary") {
		t.Fatalf("CanonicalPath = %q", got)
	}
}

func TestDurableIdentifiersRemainSafetyContinuityPins(t *testing.T) {
	t.Parallel()
	if PersistentNamespace != "ibkr" {
		t.Fatalf("persistent namespace changed to %q; this would fork durable authority", PersistentNamespace)
	}
	if DaemonSocketName != "ibkr.sock" || DaemonLockName != "ibkr.lock" {
		t.Fatalf("daemon IPC identity changed: socket=%q lock=%q", DaemonSocketName, DaemonLockName)
	}
	if AppLaunchAgentLabel != "com.osauer.ibkr-app" {
		t.Fatalf("LaunchAgent identity changed to %q", AppLaunchAgentLabel)
	}
}
