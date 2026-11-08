package mcp

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"

	"github.com/osauer/canary/v2/internal/dial"
	"github.com/osauer/canary/v2/internal/rpc"
	"github.com/osauer/canary/v2/internal/rpc/api"
	"github.com/osauer/canary/v2/internal/update"
)

type clientActivation struct {
	Version            string `json:"version"`
	ActivationRequired bool   `json:"activation_required"`
	RestartCommand     string `json:"restart_command,omitempty"`
	Guidance           string `json:"guidance,omitempty"`
}

// Status keeps the daemon's observations intact and adds this MCP process's
// version and explicit activation guidance. Reading never restarts a process.
func (s *Server) statusWithActivation(ctx context.Context, conn *dial.Conn, raw json.RawMessage) json.RawMessage {
	var fields map[string]json.RawMessage
	var health rpc.HealthResult
	if json.Unmarshal(raw, &fields) != nil || json.Unmarshal(raw, &health) != nil {
		return raw
	}
	info := clientActivation{Version: s.version, ActivationRequired: s.version != "" && health.DaemonVersion != "" && s.version != health.DaemonVersion}
	if info.ActivationRequired {
		var settings rpc.PlatformSettings
		if update.IsDevelopmentBuild(s.version) || !update.EvaluateAvailability(health.DaemonVersion, s.version).Available {
			info.Guidance = "MCP and daemon versions differ, but replacing the daemon with this MCP build is not a verified upgrade. Update the older installation to the intended release and recheck canary_status; do not downgrade the daemon."
		} else if err := rpc.Call(ctx, conn, api.SettingsGet, rpc.NoParams{}, &settings); err != nil || settings.Build.TradingWritesAvailable.Source != rpc.SettingsSourceBuild {
			info.Guidance = "MCP and daemon versions differ. The daemon build could not be verified. Use the updated installation matching the daemon's build to restart it, then recheck canary_status."
		} else if settings.Build.TradingWritesAvailable.Value != mcpTradingBuild {
			info.Guidance = "MCP and daemon versions differ and their trading capabilities differ. Update and restart the daemon from its matching build; do not replace a trading daemon with the standard MCPB binary. Then recheck canary_status."
		} else if exe, err := os.Executable(); err == nil {
			info.RestartCommand = activationCommand(exe, os.Getenv)
			if info.RestartCommand != "" {
				info.Guidance = "Run restart_command in your terminal to activate this installed version for the shared daemon, then recheck canary_status. This also refreshes an app already running in the selected scope."
			} else {
				info.Guidance = "The MCP home directory is unavailable, so a safe scoped restart command cannot be generated. Restart from the matching installation in its original environment and recheck canary_status."
			}
		} else {
			info.Guidance = "MCP and daemon versions differ. The embedded executable path is unavailable; restart from the updated matching installation and recheck canary_status."
		}
	}
	fields["mcp_client"], _ = json.Marshal(info)
	out, err := json.Marshal(fields)
	if err != nil {
		return raw
	}
	return out
}

func activationCommand(exe string, getenv func(string) string) string {
	quote := func(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'" }
	parts := []string{"env"}
	scopeKeys := []string{"CANARY_SOCKET", "CANARY_CONFIG", "CANARY_LOG", "CANARY_APP_STATE_DIR", "XDG_CONFIG_HOME", "XDG_STATE_HOME", "XDG_CACHE_HOME", "XDG_RUNTIME_DIR"}
	for _, key := range scopeKeys {
		parts = append(parts, "-u", key)
	}
	if home := getenv("HOME"); home != "" {
		if abs, err := filepath.Abs(home); err == nil {
			home = abs
		}
		parts = append(parts, quote("HOME="+home))
	} else {
		return ""
	}
	// Only path scope is copied. Never expose provider keys or other credentials.
	for _, key := range scopeKeys {
		if value := getenv(key); value != "" {
			if abs, err := filepath.Abs(value); err == nil {
				value = abs
			}
			parts = append(parts, quote(key+"="+value))
		}
	}
	parts = append(parts, quote(exe), "restart")
	return strings.Join(parts, " ")
}
