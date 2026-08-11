package app

import (
	"strings"
	"testing"
)

func TestNewRefusesPreviewReadGrantOffLoopback(t *testing.T) {
	t.Parallel()
	for _, addr := range []string{"0.0.0.0:8765", "192.168.1.5:8765", ":8765"} {
		opts := Options{Addr: addr, StateDir: t.TempDir(), Version: "test", PreviewReadGrant: true}
		if _, err := New(opts); err == nil || !strings.Contains(err.Error(), "loopback") {
			t.Fatalf("New(%q) err=%v, want loopback refusal", addr, err)
		}
	}
}
