package config

import (
	"github.com/osauer/canary/v2/internal/marketcal"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

func TestGatewayLoggingConfig(t *testing.T) {
	d := Daemon{}
	if !slices.Contains(d.GatewayLogMarkets(), marketcal.MarketJPTSE) || !slices.Contains(d.GatewayLogMarkets(), marketcal.MarketHKHKEX) {
		t.Fatal("default omitted Asia")
	}
	before, after := d.GatewayLogPadding()
	if before != 6*time.Hour || after != 4*time.Hour {
		t.Fatal("missing duty padding")
	}
	for _, body := range []string{`log_markets=[]`, `log_markets=["SMART"]`, `log_markets=[""]`, `log_markets=["us_equity","us_equity"]`, `log_markets=["always","jp_tse"]`, `log_before_open_minutes=-1`, `log_after_close_minutes=721`, `log_calendar_mode="guess"`} {
		p := filepath.Join(t.TempDir(), "config.toml")
		_ = os.WriteFile(p, []byte("[daemon]\n"+body), 0600)
		c, err := Load(p)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = c.Resolve(); err == nil {
			t.Fatalf("accepted %s", body)
		}
	}
	p := filepath.Join(t.TempDir(), "config.toml")
	_ = os.WriteFile(p, []byte("[daemon]\nlog_calendar_mode=\"scheduled\"\nlog_markets=[\"jp_tse\",\"hk_hkex\"]\nlog_before_open_minutes=0\nlog_after_close_minutes=60\n"), 0600)
	c, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	r, err := c.Resolve()
	if err != nil {
		t.Fatal(err)
	}
	before, after = r.Daemon.GatewayLogPadding()
	if before != 0 || after != time.Hour || len(r.Daemon.GatewayLogMarkets()) != 2 {
		t.Fatal("configured scope/padding lost")
	}
	if len((Daemon{LogMarkets: []string{"always"}}).GatewayLogMarkets()) != 0 {
		t.Fatal("always must not infer closed")
	}
}
