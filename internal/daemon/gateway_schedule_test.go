package daemon

import (
	"bytes"
	"context"
	"github.com/osauer/canary/v2/internal/config"
	"github.com/osauer/canary/v2/internal/marketcal"
	ibkrlib "github.com/osauer/canary/v2/pkg/ibkr"
	"strings"
	"testing"
	"time"
)

func scheduleTime(t *testing.T, s string) time.Time {
	t.Helper()
	v, e := time.Parse(time.RFC3339, s)
	if e != nil {
		t.Fatal(e)
	}
	return v
}

func TestGatewayScheduleCalendarEdges(t *testing.T) {
	for _, tc := range []struct {
		name, at      string
		market        marketcal.Market
		before, after time.Duration
		want          bool
	}{
		{"Tokyo lunch", "2026-09-14T03:00:00Z", marketcal.MarketJPTSE, 0, 0, true},
		{"HK morning", "2026-09-14T02:00:00Z", marketcal.MarketHKHKEX, 0, 0, true},
		{"HK holiday", "2026-10-01T02:00:00Z", marketcal.MarketHKHKEX, 0, 0, false},
		{"weekend", "2026-09-19T12:00:00Z", marketcal.MarketUSEquity, 0, 0, false},
		{"before DST US open", "2026-03-06T14:00:00Z", marketcal.MarketUSEquity, 0, 0, false},
		{"after DST US open", "2026-03-09T14:00:00Z", marketcal.MarketUSEquity, 0, 0, true},
		{"early close", "2026-11-27T18:30:00Z", marketcal.MarketUSEquity, 0, 0, false},
		{"early close postwork", "2026-11-27T18:30:00Z", marketcal.MarketUSEquity, 0, time.Hour, true},
		{"preparation", "2026-09-14T12:30:00Z", marketcal.MarketUSEquity, time.Hour, 0, true},
		{"exact close", "2026-09-14T20:00:00Z", marketcal.MarketUSEquity, 0, 0, false},
		{"calendar expired", "2027-02-01T22:00:00Z", marketcal.MarketHKHKEX, 0, 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			now := scheduleTime(t, tc.at)
			v := compileGatewaySchedule(now, []marketcal.Market{tc.market}, tc.before, tc.after, now.Add(time.Hour), false)
			if got := v.required(now, now); got != tc.want {
				t.Fatalf("required=%t want=%t", got, tc.want)
			}
		})
	}
}

func TestGatewayScheduleUnknownExpiryAndCrossing(t *testing.T) {
	now := scheduleTime(t, "2026-09-14T13:00:00Z")
	v := compileGatewaySchedule(now, []marketcal.Market{marketcal.MarketUSEquity}, 0, 0, now.Add(time.Hour), false)
	if v.required(now, now) {
		t.Fatal("before duty")
	}
	if !v.required(now, now.Add(30*time.Minute)) {
		t.Fatal("crossing opening hidden")
	}
	if !v.required(now, now.Add(time.Hour)) {
		t.Fatal("expired evidence hidden")
	}
	if !v.required(now.Add(-13*time.Hour), now) {
		t.Fatal("old recovery interval treated known")
	}
	if !v.required(now, now.Add(-time.Second)) {
		t.Fatal("clock rollback hidden")
	}
	v.unknown = true
	if !v.required(now, now) {
		t.Fatal("unknown quiet")
	}
	v = nil
	if !v.required(now, now) {
		t.Fatal("nil quiet")
	}
}

func TestGatewayScheduleQuietEpisodePromotesAndRecovers(t *testing.T) {
	now := scheduleTime(t, "2026-09-14T13:29:00Z")
	var out bytes.Buffer
	s := &Server{cfg: &config.Resolved{Daemon: config.Daemon{LogCalendarMode: "scheduled"}}, logger: NewLogger(&out, "info"), now: func() time.Time { return now }}
	s.gatewaySchedule.view.Store(compileGatewaySchedule(now, []marketcal.Market{marketcal.MarketUSEquity}, 0, 0, now.Add(time.Hour), false))
	s.logGatewayUnavailable("synthetic unavailable")
	if strings.Contains(out.String(), "level=WARN") {
		t.Fatal("quiet incident warned")
	}
	now = now.Add(time.Minute)
	s.logGatewayUnavailable("synthetic unavailable")
	if strings.Count(out.String(), "level=WARN") != 1 {
		t.Fatal("opening did not promote immediately")
	}
	s.logGatewayRecovered()
	if strings.Count(out.String(), "level=WARN") != 2 {
		t.Fatal("recovery of required incident hidden")
	}
}

func TestGatewayScopeDoesNotGuessFromSymbolOrCurrency(t *testing.T) {
	for _, c := range []ibkrlib.Contract{{SecType: "STK", Exchange: "SMART", Currency: "USD"}, {SecType: "OPT", Exchange: "CBOE"}, {SecType: "FUT", Exchange: "CME"}, {SecType: "STK", PrimaryExch: "UNKNOWN", Exchange: "NYSE"}} {
		if _, ok := loggingPositionMarket(c); ok {
			t.Fatal("guessed unsupported hours")
		}
	}
	for _, v := range []string{"SEHK", "TSEJ"} {
		if _, ok := loggingPositionMarket(ibkrlib.Contract{SecType: "STK", PrimaryExch: v}); !ok {
			t.Fatal("Asian identity ignored")
		}
	}
}

func TestGatewayScheduleWorkerShutdownAndConservativeDefault(t *testing.T) {
	for _, mode := range []string{"", "scheduled"} {
		s := &Server{cfg: &config.Resolved{Daemon: config.Daemon{LogCalendarMode: mode}}, now: time.Now}
		ctx, cancel := context.WithCancel(context.Background())
		s.startGatewaySchedule(ctx)
		cancel()
		s.gatewaySchedule.wg.Wait()
		now := time.Now()
		if !s.gatewayLogRequired(now, now) {
			t.Fatal("missing inventory quieted")
		}
	}
}

func BenchmarkGatewayScheduleDecision(b *testing.B) {
	now := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
	s := &Server{cfg: &config.Resolved{Daemon: config.Daemon{LogCalendarMode: "scheduled"}}}
	s.gatewaySchedule.view.Store(compileGatewaySchedule(now, marketcal.AllMarkets(), 6*time.Hour, 4*time.Hour, now.Add(time.Hour), false))
	b.ReportAllocs()
	for b.Loop() {
		s.gatewayLogRequired(now, now)
	}
}

func TestGatewayScheduleRejectsSuccessorAndObsoletePublication(t *testing.T) {
	now := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
	first, second := &ibkrlib.Connector{}, &ibkrlib.Connector{}
	evidence := gatewayScopeEvidence{connector: first, until: now.Add(time.Hour)}
	evidence.observeConnector(nil)
	if evidence.until.IsZero() {
		t.Fatal("same-scope outage discarded retained scope")
	}
	evidence.observeConnector(second)
	if !evidence.until.IsZero() {
		t.Fatal("successor inherited quiet permission")
	}
	s := &Server{cfg: &config.Resolved{Daemon: config.Daemon{LogCalendarMode: "scheduled"}}, connector: second, connectorEpoch: 2}
	v := compileGatewaySchedule(now, marketcal.AllMarkets(), 0, 0, now.Add(time.Hour), false)
	if s.publishGatewaySchedule(first, 1, v) || s.publishGatewaySchedule(second, 1, v) {
		t.Fatal("obsolete publication accepted")
	}
	if !s.gatewayLogRequired(now, now) {
		t.Fatal("obsolete evidence enabled quieting")
	}
	if !s.publishGatewaySchedule(second, 2, v) {
		t.Fatal("current publication rejected")
	}
}
