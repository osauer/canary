package mcp

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/osauer/canary/v3/internal/dial"
	"github.com/osauer/canary/v3/internal/rpc"
)

func activationSettingsConn(t *testing.T, build rpc.SettingsBool) *dial.Conn {
	t.Helper()
	path := filepath.Join("/tmp", fmt.Sprintf("canary-activation-%d.sock", time.Now().UnixNano()))
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close(); _ = os.Remove(path) })
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		var req rpc.Request
		if json.NewDecoder(c).Decode(&req) != nil {
			return
		}
		if req.Method != rpc.MethodSettingsGet {
			return
		}
		result := rpc.PlatformSettings{Build: rpc.PlatformBuildSettings{TradingWritesAvailable: build}}
		raw, _ := json.Marshal(result)
		_ = json.NewEncoder(c).Encode(rpc.Response{ID: req.ID, Ok: true, Result: raw})
	}()
	conn, err := dial.Connect(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

func decodeActivation(t *testing.T, raw json.RawMessage) clientActivation {
	t.Helper()
	var result struct {
		Client  clientActivation `json:"mcp_client"`
		Version string           `json:"daemon_version"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		t.Fatal(err)
	}
	if result.Version == "" {
		t.Fatal("daemon evidence was lost")
	}
	return result.Client
}

func TestMCPActivationRequiresVerifiedUpgradeAndMatchingBuild(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	for _, tc := range []struct {
		name, client, daemon string
		build                rpc.SettingsBool
		command              bool
	}{
		{"upgrade", "v3.23.1", "v3.23.0", rpc.SettingsBool{Value: mcpTradingBuild, Source: rpc.SettingsSourceBuild}, true},
		{"capability mismatch", "v3.23.1", "v3.23.0", rpc.SettingsBool{Value: !mcpTradingBuild, Source: rpc.SettingsSourceBuild}, false},
		{"unverified build", "v3.23.1", "v3.23.0", rpc.SettingsBool{}, false},
		{"newer daemon", "v3.23.1", "v3.24.0", rpc.SettingsBool{}, false},
		{"daemon development", "v3.23.1", "v3.23.0-4-gabcdef", rpc.SettingsBool{}, false},
		{"client development", "v3.23.1-dirty", "v3.23.0", rpc.SettingsBool{}, false},
		{"unknown version", "dev", "v3.23.0", rpc.SettingsBool{}, false},
		{"different major", "v4.0.0", "v3.23.0", rpc.SettingsBool{}, false},
		{"current", "v3.23.1", "v3.23.1", rpc.SettingsBool{}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			conn := activationSettingsConn(t, tc.build)
			raw, _ := json.Marshal(rpc.HealthResult{DaemonVersion: tc.daemon})
			got := decodeActivation(t, (&Server{version: tc.client}).statusWithActivation(t.Context(), conn, raw))
			if (got.RestartCommand != "") != tc.command {
				t.Fatalf("unexpected activation=%+v", got)
			}
		})
	}
}

func TestMCPActivationCommandPinsScopeAndQuotesPaths(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, "bundle's binary")
	script := "#!/bin/sh\nprintf '%s\\n' \"$1\" \"$HOME\" \"${CANARY_SOCKET-unset}\" \"$CANARY_CONFIG\"\n"
	if err := os.WriteFile(exe, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	config := filepath.Join(dir, "config ' $(touch SHOULD_NOT_EXIST).toml")
	values := map[string]string{"HOME": dir, "CANARY_CONFIG": config, "SYNTHETIC_SECRET": "must-not-appear"}
	command := activationCommand(exe, func(key string) string { return values[key] })
	if strings.Contains(command, "must-not-appear") {
		t.Fatal("credential escaped allowlist")
	}
	cmd := exec.Command("sh", "-c", command)
	cmd.Env = append(os.Environ(), "HOME=/wrong/home", "CANARY_SOCKET=/wrong/daemon.sock", "CANARY_CONFIG=/wrong/config")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("command failed: %v %s", err, out)
	}
	want := "restart\n" + dir + "\nunset\n" + config + "\n"
	if string(out) != want {
		t.Fatalf("scope changed: got %q want %q", out, want)
	}
	if activationCommand(exe, func(string) string { return "" }) != "" {
		t.Fatal("missing home must withhold command")
	}
}
