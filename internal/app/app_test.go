package app

import (
	"strings"
	"testing"
)

func TestNewRejectsSecondAppForSameStateDir(t *testing.T) {
	t.Parallel()

	opts := DefaultOptions("test")
	opts.StateDir = t.TempDir()
	opts.Addr = "127.0.0.1:0"
	opts.PublicURL = "http://127.0.0.1:0"

	first, err := New(opts)
	if err != nil {
		t.Fatalf("first New: %v", err)
	}
	defer func() { _ = first.Close() }()

	second, err := New(opts)
	if err == nil {
		_ = second.Close()
		t.Fatalf("second New unexpectedly succeeded")
	}
	if !strings.Contains(err.Error(), "another Canary app process is already running") {
		t.Fatalf("second New error = %v", err)
	}
}

func TestDefaultOptionsMarksPublicURLFromEnv(t *testing.T) {
	t.Setenv("CANARY_APP_PUBLIC_URL", " http://example.test:8765/ ")

	opts := DefaultOptions("test")
	if !opts.PublicURLFromEnv {
		t.Fatalf("PublicURLFromEnv = false, want true")
	}
	if opts.PublicURL != "http://example.test:8765" {
		t.Fatalf("PublicURL = %q, want trimmed env value", opts.PublicURL)
	}
}

func TestDefaultOptionsRemoteEnv(t *testing.T) {
	t.Setenv("CANARY_APP_REMOTE", "yes")
	t.Setenv("CANARY_APP_REMOTE_URL", " https://remote.example.test/ ")

	opts := DefaultOptions("test")
	if !opts.Remote {
		t.Fatalf("Remote = false, want true")
	}
	if opts.RemoteURL != "https://remote.example.test" {
		t.Fatalf("RemoteURL = %q, want trimmed remote URL", opts.RemoteURL)
	}
}
