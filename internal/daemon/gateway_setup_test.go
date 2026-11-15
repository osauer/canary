package daemon

import (
	"strings"
	"testing"
	"time"

	"github.com/osauer/canary/v3/internal/rpc"
	ibkrlib "github.com/osauer/canary/v3/pkg/ibkr"
)

func gatewaySetupFixture() ibkrlib.APIConfigDiagnostic {
	return ibkrlib.APIConfigDiagnostic{
		State: "observed", AsOf: time.Date(2026, 10, 20, 6, 36, 0, 0, time.UTC),
		Settings: &ibkrlib.APISettings{
			ReadOnly: new(false), DownloadOpenOrders: new(true), RejectAboveRate: new(false),
			LocalhostOnly: new(true), LoopbackTrusted: new(true), CreateAPILog: new(true),
			MarketDataInAPILog: new(false), LoggingLevel: new("warning"), SocketPort: new(4001),
			MasterClientID: new(15),
		},
		Precautions: &ibkrlib.APIPrecautions{},
		LockAndExit: &ibkrlib.LockAndExit{Type: "restart", Time: "01:45", Period: "am"},
	}
}

func TestGatewaySetupNotesNameEachDifferenceOnce(t *testing.T) {
	t.Parallel()
	if notes := gatewaySetupNotes(gatewaySetupFixture(), 15, "127.0.0.1", "live"); len(notes) != 0 {
		t.Fatalf("a matching setup must produce no note: %v", notes)
	}
	cases := []struct {
		name   string
		mutate func(*ibkrlib.APIConfigDiagnostic)
		client int
		host   string
		mode   string
		want   string
	}{
		{"read-only while live", func(d *ibkrlib.APIConfigDiagnostic) { d.Settings.ReadOnly = new(true) }, 15, "127.0.0.1", "live", "Read-Only API is on"},
		{"master client unset", func(d *ibkrlib.APIConfigDiagnostic) { d.Settings.MasterClientID = nil }, 15, "127.0.0.1", "live", "Master API client ID is not 15"},
		{"master client other", func(d *ibkrlib.APIConfigDiagnostic) { d.Settings.MasterClientID = new(0) }, 15, "127.0.0.1", "live", "Master API client ID is not 15"},
		{"no order download", func(d *ibkrlib.APIConfigDiagnostic) { d.Settings.DownloadOpenOrders = new(false) }, 15, "127.0.0.1", "live", "Open orders are not downloaded"},
		{"reject above rate", func(d *ibkrlib.APIConfigDiagnostic) { d.Settings.RejectAboveRate = new(true) }, 15, "127.0.0.1", "live", "rejected instead of slowed"},
		{"loopback untrusted", func(d *ibkrlib.APIConfigDiagnostic) {
			d.Settings.LocalhostOnly, d.Settings.LoopbackTrusted = new(false), new(false)
		}, 15, "127.0.0.1", "live", "127.0.0.1 is not a trusted IP"},
		{"detail level", func(d *ibkrlib.APIConfigDiagnostic) { d.Settings.LoggingLevel = new("detail") }, 15, "127.0.0.1", "live", "API log level is Detail"},
		{"market data logged", func(d *ibkrlib.APIConfigDiagnostic) { d.Settings.MarketDataInAPILog = new(true) }, 15, "127.0.0.1", "live", "Market data is written to the API log"},
		{"auto logoff", func(d *ibkrlib.APIConfigDiagnostic) { d.LockAndExit.Type = "logoff" }, 15, "127.0.0.1", "live", "Daily auto log-off at 01:45 am instead of auto restart"},
		{"no restart", func(d *ibkrlib.APIConfigDiagnostic) { d.LockAndExit = &ibkrlib.LockAndExit{Type: "never"} }, 15, "127.0.0.1", "live", "No daily auto restart is set"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			d := gatewaySetupFixture()
			c.mutate(&d)
			notes := gatewaySetupNotes(d, c.client, c.host, c.mode)
			if len(notes) != 1 || !strings.Contains(notes[0], c.want) {
				t.Fatalf("want one note containing %q, got %v", c.want, notes)
			}
			if strings.ContainsAny(notes[0], "_{}") {
				t.Fatalf("note leaks a code or a struct: %q", notes[0])
			}
		})
	}
}

func TestGatewaySetupNotesSkipWhatDoesNotApply(t *testing.T) {
	t.Parallel()
	d := gatewaySetupFixture()
	d.Settings.ReadOnly = new(true)
	if notes := gatewaySetupNotes(d, 15, "127.0.0.1", "disabled"); len(notes) != 0 {
		t.Fatalf("read-only is no difference while trading is disabled: %v", notes)
	}
	d = gatewaySetupFixture()
	d.Settings.LocalhostOnly, d.Settings.LoopbackTrusted = new(false), new(false)
	if notes := gatewaySetupNotes(d, 15, "192.168.1.9", "live"); len(notes) != 0 {
		t.Fatalf("loopback trust is no difference for a remote gateway: %v", notes)
	}
	d = gatewaySetupFixture()
	d.Settings.DownloadOpenOrders, d.Settings.LoggingLevel, d.LockAndExit = nil, nil, nil
	if notes := gatewaySetupNotes(d, 15, "127.0.0.1", "live"); len(notes) != 0 {
		t.Fatalf("a setting the broker did not report is no difference: %v", notes)
	}
}

func TestGatewaySetupStoreIsPerSession(t *testing.T) {
	t.Parallel()
	var st gatewaySetupStore
	if _, ok := st.get(1); ok {
		t.Fatal("empty store served a reading")
	}
	st.set(1, gatewaySetupFixture())
	if _, ok := st.get(1); !ok {
		t.Fatal("stored reading not served for its session")
	}
	if _, ok := st.get(2); ok {
		t.Fatal("a reading from the previous session served for the next")
	}
}

func TestGatewaySetupHealthStates(t *testing.T) {
	t.Parallel()
	s := &Server{}
	s.endpoint.Host = "127.0.0.1"
	s.connectorEpoch = 4
	res := &rpc.HealthResult{Connected: true, ClientID: 15, Trading: rpc.TradingStatus{Mode: "live"}}
	if got := s.gatewaySetupHealth(res); got == nil || got.State != rpc.GatewaySetupUnknown || got.Reason != "not_read_yet" {
		t.Fatalf("before the first read: %+v", got)
	}
	s.gatewaySetup.set(4, ibkrlib.APIConfigDiagnostic{State: "unavailable", Reason: "transport_failed"})
	if got := s.gatewaySetupHealth(res); got.State != rpc.GatewaySetupUnknown || got.Reason != "transport_failed" {
		t.Fatalf("failed read: %+v", got)
	}
	s.gatewaySetup.set(4, gatewaySetupFixture())
	if got := s.gatewaySetupHealth(res); got.State != rpc.GatewaySetupAsExpected || len(got.Notes) != 0 || got.AsOf.IsZero() {
		t.Fatalf("matching setup: %+v", got)
	}
	d := gatewaySetupFixture()
	d.Settings.MasterClientID = nil
	s.gatewaySetup.set(4, d)
	if got := s.gatewaySetupHealth(res); got.State != rpc.GatewaySetupAttention || len(got.Notes) != 1 {
		t.Fatalf("one difference: %+v", got)
	}
	if got := s.gatewaySetupHealth(&rpc.HealthResult{Connected: false}); got != nil {
		t.Fatalf("no row without a connection: %+v", got)
	}
	applied := gatewayAPIHealth()
	applyGatewayDiagnostic(applied, d)
	if applied.Settings == nil || applied.Settings.MasterClientID != nil || applied.Precautions == nil || len(applied.Precautions.Bypassed) != 0 || applied.LockAndExit == nil || applied.LockAndExit.Type != "restart" {
		t.Fatalf("diagnostic rows: %+v", applied)
	}
}
