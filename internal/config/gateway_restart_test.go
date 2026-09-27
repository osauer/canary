package config

import (
	"strings"
	"testing"
)

func TestLoadGatewayRestartDeclarationPreservesPinsAndMaintenance(t *testing.T) {
	body := strings.Replace(pinnedConfig, "tls = false", `tls = false
restart_time = "23:45"
restart_timezone = "local"
restart_grace = "5m"`, 1)
	cfg, issues, err := LoadForDaemon(writeDaemonConfig(t, body))
	if err != nil || len(issues) != 0 {
		t.Fatalf("load: %v %+v", err, issues)
	}
	requirePins(t, cfg)
	if cfg.Gateway.RestartTime != "23:45" || cfg.Gateway.RestartTimezone != "local" || cfg.Gateway.RestartGrace != "5m" || cfg.Gateway.MaintenanceWindows != nil {
		t.Fatalf("declaration changed: %+v", cfg.Gateway)
	}
	body = strings.Replace(body, `restart_time = "23:45"`, `restart_time = 23`, 1)
	cfg, issues, err = LoadForDaemon(writeDaemonConfig(t, body))
	if err != nil || len(issues) != 1 || issues[0].Key != "gateway.restart_time" {
		t.Fatalf("malformed diagnostic blocked pins or lost issue: %v %+v", err, issues)
	}
	requirePins(t, cfg)
}
