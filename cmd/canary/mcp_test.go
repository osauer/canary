package main

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/mcp"
)

func TestWatchMCPParentCancelsWhenParentChanges(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var parent atomic.Int32
	parent.Store(42)
	getppid := func() int { return int(parent.Load()) }
	go watchMCPParent(ctx, cancel, 42, getppid, 5*time.Millisecond)

	parent.Store(43)
	select {
	case <-ctx.Done():
	case <-time.After(200 * time.Millisecond):
		t.Fatal("watchMCPParent did not cancel after parent PID changed")
	}
}

func TestMCPServeOptionsDoNotIdleTimeout(t *testing.T) {
	t.Parallel()

	opts := mcpServeOptions()
	if opts.IdleTimeout != 0 {
		t.Fatalf("MCP stdio server must not idle-exit while the host keeps stdin open; IdleTimeout=%s", opts.IdleTimeout)
	}
}

func TestPrintMCPUsageUsesCanonicalIdentity(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	printMCPUsage(&out)
	got := out.String()
	for _, want := range []string{`"canary"`, `"canary-monitor"`, "canary_*", "canary://quote/{symbol}", "canary_stress", "canary_status"} {
		if !strings.Contains(got, want) {
			t.Errorf("usage missing %q:\n%s", want, got)
		}
	}
	for _, retired := range []string{`"ibkr"`, `"ibkr-monitor"`, "ibkr_*", "ibkr://"} {
		if strings.Contains(got, retired) {
			t.Errorf("usage retains retired identity %q:\n%s", retired, got)
		}
	}
}

func TestParseMCPArgsProfiles(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		args    []string
		want    mcp.Profile
		wantErr bool
	}{
		{name: "default", want: mcp.ProfileFull},
		{name: "full", args: []string{"--profile", "full"}, want: mcp.ProfileFull},
		{name: "monitor", args: []string{"--profile", "monitor"}, want: mcp.ProfileMonitor},
		{name: "monitor equals", args: []string{"--profile=monitor"}, want: mcp.ProfileMonitor},
		{name: "unknown", args: []string{"--profile", "diagnostic"}, wantErr: true},
		{name: "positional", args: []string{"monitor"}, wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			stdout := &bytes.Buffer{}
			stderr := &bytes.Buffer{}
			got, code := parseMCPArgs(tc.args, stdout, stderr)
			if tc.wantErr {
				if code == 0 {
					t.Fatalf("parseMCPArgs(%v) code = 0, want error; stdout=%q stderr=%q", tc.args, stdout.String(), stderr.String())
				}
				return
			}
			if code != 0 {
				t.Fatalf("parseMCPArgs(%v) code = %d, want 0; stderr=%q", tc.args, code, stderr.String())
			}
			if got != tc.want {
				t.Fatalf("parseMCPArgs(%v) profile = %q, want %q", tc.args, got, tc.want)
			}
		})
	}
}

func TestDialMCPDaemonHonorsCanceledContextBeforeAutospawn(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := dialMCPDaemon(ctx, filepath.Join(t.TempDir(), "missing.sock"))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("dialMCPDaemon with canceled ctx = %v, want context.Canceled", err)
	}
}
