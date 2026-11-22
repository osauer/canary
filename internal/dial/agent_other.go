//go:build !darwin

package dial

import "context"

// The launchd agent is macOS only. Elsewhere a daemon in its own session
// survives the terminal that started it; see
// internal-docs/design/daemon-lifetime.md for what is not covered on Linux.

// RunningUnderDaemonAgent reports whether a supervisor started this process
// as the daemon agent. Only macOS has one.
func RunningUnderDaemonAgent() bool { return false }

// RemoveDaemonAgent removes the daemon's supervisor agent. There is none
// outside macOS, so it reports that nothing was there.
func RemoveDaemonAgent(context.Context) (bool, error) { return false, nil }

func startDaemonViaAgent(context.Context, string, string, bool) (int, bool, error) {
	return 0, false, nil
}

// QuiesceDaemonAgent unloads the daemon's supervisor agent before a restart
// starts its own daemon. There is none outside macOS.
func QuiesceDaemonAgent(context.Context, string) error { return nil }
