package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCashLedgerConfigurationSurvivesStrictAndDaemonLoad(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte("[cash_ledger]\nurl=\"https://localhost:5001/v1/api\"\nca_cert_file=\"/synthetic/gateway.pem\"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	strict, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	daemon, issues, err := LoadForDaemon(path)
	if err != nil || len(issues) != 0 {
		t.Fatalf("daemon load: %v %#v", err, issues)
	}
	for _, cfg := range []*Config{strict, daemon} {
		r, err := cfg.Resolve()
		if err != nil || r.CashLedger.URL != "https://localhost:5001/v1/api" || r.CashLedger.CACertFile != "/synthetic/gateway.pem" {
			t.Fatalf("source config lost: %v %#v", err, r)
		}
	}
}
