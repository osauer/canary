package main

import (
	"path/filepath"
	"strings"
	"testing"
)

// The isolation guard refuses everything that could reach the operator's
// daemon or a real TWS or IB Gateway, before anything starts.
func TestCheckIsolationRefusesALiveLookingStack(t *testing.T) {
	dir := t.TempDir()
	ok := newStack(dir)
	operator := operatorSockets(envMap{"HOME": "/Users/someone", "XDG_RUNTIME_DIR": "/run/user/501"}.get)
	if err := checkIsolation(ok, operator, "127.0.0.1", 50123); err != nil {
		t.Fatalf("an isolated stack was refused: %v", err)
	}
	for _, port := range []int{4001, 4002, 7496, 7497, 0} {
		if err := checkIsolation(ok, operator, "127.0.0.1", port); err == nil || !strings.Contains(err.Error(), "port") {
			t.Errorf("port %d accepted: %v", port, err)
		}
	}
	if err := checkIsolation(ok, operator, "192.0.2.7", 50123); err == nil {
		t.Error("a non-loopback broker host was accepted")
	}

	// A socket the operator's environment resolves to: the home cache socket.
	live := ok
	live.dir = "/Users/someone/.cache"
	live.socket = "/Users/someone/.cache/ibkr/ibkr.sock"
	live.env = append([]string(nil), ok.env...)
	live.env[0] = "CANARY_SOCKET=" + live.socket
	if err := checkIsolation(live, operator, "127.0.0.1", 50123); err == nil || !strings.Contains(err.Error(), "operator's daemon socket") {
		t.Fatalf("the operator's socket was accepted: %v", err)
	}

	// Any root outside the stack's directory.
	escaped := newStack(dir)
	escaped.env = append([]string(nil), escaped.env...)
	escaped.env[3] = "HOME=" + filepath.Dir(dir)
	if err := checkIsolation(escaped, operator, "127.0.0.1", 50123); err == nil || !strings.Contains(err.Error(), "HOME=") {
		t.Fatalf("a home outside the stack was accepted: %v", err)
	}
}

type envMap map[string]string

func (m envMap) get(k string) string { return m[k] }
