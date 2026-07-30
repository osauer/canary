package ibkr

import (
	"path/filepath"
	"testing"
	"time"
)

func TestNewConnectionOwnsConfig(t *testing.T) {
	packetLogTemplate := filepath.Join(t.TempDir(), "connection-%d.log")
	interceptor := &WireInterceptor{}
	config := &ConnectionConfig{
		Host:            "127.0.0.2",
		Port:            4002,
		ClientID:        17,
		Account:         "test-account",
		PacketLogPath:   packetLogTemplate,
		WireInterceptor: interceptor,
	}
	wantCallerConfig := *config

	conn := NewConnection(config)
	t.Cleanup(func() {
		if err := conn.Disconnect(); err != nil {
			t.Errorf("Disconnect: %v", err)
		}
	})

	if got := *config; got != wantCallerConfig {
		t.Fatalf("NewConnection mutated caller config:\n got: %#v\nwant: %#v", got, wantCallerConfig)
	}
	if conn.config == config {
		t.Fatal("NewConnection retained the caller's config pointer")
	}
	assertConnectionDefaults(t, conn.config)
	if got, want := conn.config.PacketLogPath, filepath.Join(filepath.Dir(packetLogTemplate), "connection-17.log"); got != want {
		t.Fatalf("effective PacketLogPath = %q, want %q", got, want)
	}

	wantEffectiveConfig := *conn.config
	*config = ConnectionConfig{
		Host:                  "mutated.example",
		Port:                  9999,
		ClientID:              99,
		Account:               "mutated-account",
		PacketLogPath:         "mutated.log",
		LogWireHex:            true,
		WireInterceptor:       &WireInterceptor{enabled: true},
		MaxClientIDRetries:    99,
		AutoReconnect:         true,
		MaxRetries:            99,
		InitialDelay:          time.Nanosecond,
		MaxDelay:              time.Nanosecond,
		BackoffMultiplier:     99,
		Jitter:                true,
		ConnectTimeout:        time.Nanosecond,
		HeartbeatInterval:     time.Nanosecond,
		UseTLS:                true,
		EnableTLSFallback:     true,
		TLSInsecureSkipVerify: true,
		TLSServerName:         "mutated.example",
	}
	if got := *conn.config; got != wantEffectiveConfig {
		t.Fatalf("caller mutation changed live connection config:\n got: %#v\nwant: %#v", got, wantEffectiveConfig)
	}
}

func TestNewConnectorOwnsConfig(t *testing.T) {
	packetLogPrefix := filepath.Join(t.TempDir(), "connector-packets")
	t.Setenv("IBKR_PACKET_LOG_TEMPLATE", packetLogPrefix)

	baseConfig := &ConnectionConfig{
		Host:            "127.0.0.2",
		Port:            4002,
		ClientID:        23,
		Account:         "test-account",
		WireInterceptor: &WireInterceptor{},
	}
	config := &ConnectorConfig{
		BaseConfig: baseConfig,
	}
	wantCallerConfig := *config
	wantCallerBaseConfig := *baseConfig

	connector := NewConnector(config)
	t.Cleanup(func() {
		if err := connector.conn.Disconnect(); err != nil {
			t.Errorf("Disconnect: %v", err)
		}
	})

	if got := *config; got != wantCallerConfig {
		t.Fatalf("NewConnector mutated caller config:\n got: %#v\nwant: %#v", got, wantCallerConfig)
	}
	if got := *baseConfig; got != wantCallerBaseConfig {
		t.Fatalf("NewConnector mutated caller base config:\n got: %#v\nwant: %#v", got, wantCallerBaseConfig)
	}
	if connector.config == config {
		t.Fatal("NewConnector retained the caller's config pointer")
	}
	if connector.config.BaseConfig == baseConfig {
		t.Fatal("NewConnector retained the caller's base config pointer")
	}
	if got, want := connector.config.PreferredClientID, baseConfig.ClientID; got != want {
		t.Fatalf("effective PreferredClientID = %d, want %d", got, want)
	}
	if got, want := connector.config.BaseConfig.PacketLogPath, packetLogPrefix+"_23.log"; got != want {
		t.Fatalf("effective PacketLogPath = %q, want %q", got, want)
	}
	assertConnectionDefaults(t, connector.conn.config)

	wantEffectiveConfig := *connector.config
	wantEffectiveBaseConfig := *connector.config.BaseConfig
	wantConnectionConfig := *connector.conn.config
	*config = ConnectorConfig{
		PreferredClientID: 99,
		BaseConfig:        &ConnectionConfig{ClientID: 99},
	}
	*baseConfig = ConnectionConfig{
		Host:          "mutated.example",
		Port:          9999,
		ClientID:      99,
		Account:       "mutated-account",
		PacketLogPath: "mutated.log",
	}

	if got := *connector.config; got != wantEffectiveConfig {
		t.Fatalf("caller mutation changed live connector config:\n got: %#v\nwant: %#v", got, wantEffectiveConfig)
	}
	if got := *connector.config.BaseConfig; got != wantEffectiveBaseConfig {
		t.Fatalf("caller mutation changed live connector base config:\n got: %#v\nwant: %#v", got, wantEffectiveBaseConfig)
	}
	if got := *connector.conn.config; got != wantConnectionConfig {
		t.Fatalf("caller mutation changed live connection config:\n got: %#v\nwant: %#v", got, wantConnectionConfig)
	}
}

func assertConnectionDefaults(t *testing.T, config *ConnectionConfig) {
	t.Helper()

	defaults := DefaultConfig()
	if config.ConnectTimeout != defaults.ConnectTimeout {
		t.Errorf("ConnectTimeout = %v, want default %v", config.ConnectTimeout, defaults.ConnectTimeout)
	}
	if config.HeartbeatInterval != defaults.HeartbeatInterval {
		t.Errorf("HeartbeatInterval = %v, want default %v", config.HeartbeatInterval, defaults.HeartbeatInterval)
	}
	if config.MaxClientIDRetries != defaults.MaxClientIDRetries {
		t.Errorf("MaxClientIDRetries = %d, want default %d", config.MaxClientIDRetries, defaults.MaxClientIDRetries)
	}
	if config.MaxRetries != defaults.MaxRetries {
		t.Errorf("MaxRetries = %d, want default %d", config.MaxRetries, defaults.MaxRetries)
	}
	if config.InitialDelay != defaults.InitialDelay {
		t.Errorf("InitialDelay = %v, want default %v", config.InitialDelay, defaults.InitialDelay)
	}
	if config.MaxDelay != defaults.MaxDelay {
		t.Errorf("MaxDelay = %v, want default %v", config.MaxDelay, defaults.MaxDelay)
	}
	if config.BackoffMultiplier != defaults.BackoffMultiplier {
		t.Errorf("BackoffMultiplier = %v, want default %v", config.BackoffMultiplier, defaults.BackoffMultiplier)
	}
}
