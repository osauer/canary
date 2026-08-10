// Package app assembles and runs the independently authenticated Canary HTTP
// and PWA host. It owns the app state directory, its single-process lock,
// device authentication, live view cache, alert delivery integration, and
// optional relay connector. Broker connectivity, trading decisions, and risk
// policy remain behind the typed daemon client and are not reimplemented here.
package app

import (
	"github.com/osauer/canary/v2/internal/dial"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Default app-host settings cover the LAN-capable listen address, pairing
// lifetime, and local snapshot refresh intervals.
const (
	DefaultAddr        = "0.0.0.0:8765"
	DefaultPairingTTL  = 5 * time.Minute
	DefaultPollEvery   = 5 * time.Second
	DefaultStressEvery = time.Minute
)

// Options configures one Canary app-host process. Durations control app-local
// pairing expiry and refresh cadence; SocketPath addresses the daemon's typed
// RPC socket. StateDir contains private grants and app delivery state and is
// protected by an exclusive process lock for the App lifetime.
type Options struct {
	Addr             string
	PublicURL        string
	PublicURLFromEnv bool
	Remote           bool
	RemoteURL        string
	StateDir         string
	SocketPath       string
	Version          string
	PairingTTL       time.Duration
	PollEvery        time.Duration
	StressEvery      time.Duration
}

// DefaultOptions returns environment-aware defaults for an app host serving
// version. It reads the documented CANARY_APP_* variables, chooses the daemon
// socket and state directory defaults, and performs no network or filesystem
// writes.
func DefaultOptions(version string) Options {
	// docgen:env CANARY_APP_ADDR | HTTP listen address for `canary app`. Defaults to `0.0.0.0:8765` so a paired phone on the LAN can reach the app.
	addr := strings.TrimSpace(os.Getenv("CANARY_APP_ADDR"))
	if addr == "" {
		addr = DefaultAddr
	}
	stateDir := DefaultStateDir()
	// docgen:env CANARY_APP_PUBLIC_URL | Public trusted HTTPS base URL for the `canary app` PWA/relay origin. Defaults to a LAN URL for wildcard listen addresses, falling back to loopback when no LAN address is available.
	publicURL := strings.TrimRight(strings.TrimSpace(os.Getenv("CANARY_APP_PUBLIC_URL")), "/")
	publicURLFromEnv := publicURL != ""
	if publicURL == "" {
		publicURL = PublicURLForAddr(addr)
	}
	// docgen:env CANARY_APP_REMOTE | Enable the outbound Cloudflare Worker relay for `canary app`, making pairing URLs reachable through the public relay origin.
	remote := parseBoolEnv(os.Getenv("CANARY_APP_REMOTE"))
	// docgen:env CANARY_APP_REMOTE_URL | Cloudflare Worker relay base URL for `canary app --remote`. Defaults to `https://remote.osauer.dev`.
	remoteURL := strings.TrimRight(strings.TrimSpace(os.Getenv("CANARY_APP_REMOTE_URL")), "/")
	if remoteURL == "" {
		remoteURL = relayDefaultURL()
	}
	return Options{
		Addr:             addr,
		PublicURL:        publicURL,
		PublicURLFromEnv: publicURLFromEnv,
		Remote:           remote,
		RemoteURL:        remoteURL,
		StateDir:         stateDir,
		SocketPath:       dial.DefaultSocketPath(),
		Version:          version,
		PairingTTL:       DefaultPairingTTL,
		PollEvery:        DefaultPollEvery,
		StressEvery:      DefaultStressEvery,
	}
}

func parseBoolEnv(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

func relayDefaultURL() string {
	return "https://remote.osauer.dev"
}

// DefaultStateDir returns the app-owned state directory selected from
// CANARY_APP_STATE_DIR, XDG_STATE_HOME, or the user's home directory, in that
// order. It does not create the directory.
func DefaultStateDir() string {
	// docgen:env CANARY_APP_STATE_DIR | Directory for `canary app` paired devices, alert settings, VAPID keys, and private alert-delivery evidence. Defaults to `$XDG_STATE_HOME/ibkr/app` or `$HOME/.local/state/ibkr/app`.
	if v := strings.TrimSpace(os.Getenv("CANARY_APP_STATE_DIR")); v != "" {
		return v
	}
	if v := os.Getenv("XDG_STATE_HOME"); v != "" {
		return filepath.Join(v, "ibkr", "app")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".local", "state", "ibkr", "app")
}
