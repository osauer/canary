package cli

import (
	"bytes"

	"github.com/osauer/canary/v2/internal/rpc"
	"strings"

	"testing"
	"time"
)

func TestStatusAccountIDPrefersPinOverManagedAccountsAggregate(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name      string
		pinned    string
		connected string
		want      string
	}{
		{name: "single account login", pinned: "DU2222222", connected: "DU2222222", want: "DU2222222"},
		{name: "connected before the pin resolves", connected: "DU2222222", want: "DU2222222"},
		{name: "multi-account login shows the pin", pinned: "DU2222222", connected: "DU1111111,DU2222222", want: "DU2222222"},
		{name: "unpinned multi-account login names no sibling", connected: "DU1111111,DU2222222", want: "auto-detect"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			got := statusAccountID(rpc.HealthResult{Account: test.pinned, ConnectedAccount: test.connected})
			if got != test.want {
				t.Fatalf("statusAccountID = %q, want %q", got, test.want)
			}
		})
	}
}

func TestRenderStatus_DataQualityKeepsGatewayReady(t *testing.T) {
	t.Parallel()
	var stdout bytes.Buffer
	env := &Env{Stdout: &stdout, Stderr: &bytes.Buffer{}}
	res := &rpc.HealthResult{
		DaemonVersion: "v1.0.0",
		UptimeSeconds: 1842,
		GatewayHost:   "127.0.0.1",
		GatewayPort:   7496,
		ClientID:      15,
		Connected:     true,
		ServerVersion: 203,
		DataQuality: []rpc.DataQualityHealth{
			{Surface: "gamma", Status: "degraded", Summary: "degraded: SPX excluded", DegradedClusters: []string{"gamma"}},
			{Surface: "regime", Status: "stale", Summary: "stale: vol, credit", StaleClusters: []string{"vol", "credit"}},
		},
	}
	renderStatusText(env, res, nil)
	got := stdout.String()
	for _, want := range []string{
		"IBKR Gateway  READY",
		"Data quality   gamma degraded",
		"SPX excluded",
		"regime stale",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("status missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "Next concern") {
		t.Fatalf("data-quality-only status should not duplicate Next concern:\n%s", got)
	}
}

func TestStatusVerdict(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		in   rpc.HealthResult
		cli  string
		want string
	}{
		{
			name: "ready",
			in:   rpc.HealthResult{Connected: true},
			want: "READY",
		},
		{
			name: "background is still ready",
			in: rpc.HealthResult{
				Connected:       true,
				BackgroundTasks: []rpc.BackgroundTaskStatus{{Name: "gamma-zero"}},
			},
			want: "READY",
		},
		{
			name: "version drift",
			in:   rpc.HealthResult{DaemonVersion: "v1.2.3", Connected: true},
			cli:  "v1.2.4",
			want: "ATTENTION",
		},
		{
			name: "market data warning",
			in:   rpc.HealthResult{Connected: true, DataType: rpc.MarketDataDelayed},
			want: "ATTENTION",
		},
		{
			name: "trading blocked",
			in: rpc.HealthResult{
				Connected: true,
				Trading:   rpc.TradingStatus{Mode: "paper", Blocked: true},
			},
			want: "ATTENTION",
		},
		{
			name: "data farm warning",
			in: rpc.HealthResult{
				Connected: true,
				DataFarms: []rpc.DataFarmHealth{{
					Name:   "usopt",
					Type:   "market",
					Status: "disconnected",
				}},
			},
			want: "ATTENTION",
		},
		{
			name: "subsystem warning",
			in: rpc.HealthResult{
				Connected: true,
				Subsystems: []rpc.SubsystemHealth{{
					Name:   "history",
					Status: "degraded",
				}},
			},
			want: "ATTENTION",
		},
		{
			name: "starting",
			in:   rpc.HealthResult{},
			want: "STARTING",
		},
		{
			name: "offline",
			in:   rpc.HealthResult{LastError: "dial timeout"},
			want: "OFFLINE",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := statusVerdict(tc.in, tc.cli)
			if got.Text != tc.want {
				t.Fatalf("statusVerdict(%+v) = %q, want %q", tc.in, got.Text, tc.want)
			}
		})
	}
}

func TestRenderStatusAlertCoverageRow(t *testing.T) {
	t.Parallel()
	base := func() *rpc.HealthResult {
		return &rpc.HealthResult{DaemonVersion: "v1.0.0", Account: "DU0000000", AccountMode: rpc.AccountModePaper,
			GatewayHost: "127.0.0.1", GatewayPort: 4002, PortOrigin: "discovered", ClientID: 17, Connected: true, ServerVersion: 178}
	}
	partial := &rpc.AlertCandidateSnapshot{Coverage: rpc.AlertCoverage{
		State: rpc.AlertCoveragePartial, Freshness: rpc.AlertCoverageCurrent,
		ExpectedSources: []rpc.AlertSource{rpc.AlertSourceRulebook, rpc.AlertSourceRegime, rpc.AlertSourceProtection},
		CoveredSources:  []rpc.AlertSource{rpc.AlertSourceRulebook, rpc.AlertSourceProtection},
	}}
	var stdout bytes.Buffer
	renderStatusText(&Env{Stdout: &stdout, Stderr: &bytes.Buffer{}}, base(), partial)
	if !strings.Contains(stdout.String(), "Alerts         2/3 sources covered — missing: regime") {
		t.Fatalf("partial alert coverage row missing:\n%s", stdout.String())
	}

	complete := &rpc.AlertCandidateSnapshot{Coverage: rpc.AlertCoverage{
		State: rpc.AlertCoverageComplete, Freshness: rpc.AlertCoverageCurrent,
		ExpectedSources: []rpc.AlertSource{rpc.AlertSourceRulebook},
		CoveredSources:  []rpc.AlertSource{rpc.AlertSourceRulebook},
	}}
	stdout.Reset()
	renderStatusText(&Env{Stdout: &stdout, Stderr: &bytes.Buffer{}}, base(), complete)
	if !strings.Contains(stdout.String(), "Alerts         1/1 sources covered") {
		t.Fatalf("complete alert coverage row missing:\n%s", stdout.String())
	}

	stdout.Reset()
	renderStatusText(&Env{Stdout: &stdout, Stderr: &bytes.Buffer{}}, base(), nil)
	if strings.Contains(stdout.String(), "Alerts ") {
		t.Fatalf("alerts row rendered without a snapshot:\n%s", stdout.String())
	}
}

func TestRenderBriefTwoMovementsAndDegradation(t *testing.T) {
	var stdout bytes.Buffer
	env := &Env{Stdout: &stdout, Stderr: &bytes.Buffer{}}
	res := rpc.BriefResult{
		AsOf: time.Date(2026, 7, 18, 8, 0, 0, 0, time.Local), BriefFingerprint: "sha256:abcdef",
		Review: rpc.BriefReviewSection{
			SessionPnL:    rpc.BriefAccountRow{BriefRowState: rpc.BriefRowState{Status: "unavailable", Detail: "account down"}},
			LastSession:   rpc.BriefLastSessionRow{BriefRowState: rpc.BriefRowState{Status: "unavailable", Detail: "not captured for 2026-07-17"}, SessionDate: "2026-07-17"},
			Attribution:   rpc.BriefMoversRow{BriefRowState: rpc.BriefRowState{Status: "unavailable", Detail: "positions down"}},
			Rules:         rpc.BriefRulesRow{BriefRowState: rpc.BriefRowState{Status: "degraded", Detail: "current policy has unknown checks"}, Pass: 8, Unknown: 2},
			Proposals:     rpc.BriefProposalsRow{BriefRowState: rpc.BriefRowState{Status: "ok", Detail: "no proposals"}, Offered: 2, Acted: 1},
			Overrides:     rpc.BriefOverridesRow{BriefRowState: rpc.BriefRowState{Status: "ok", Detail: "none"}},
			CapitalEvents: rpc.BriefCapitalEventsRow{BriefRowState: rpc.BriefRowState{Status: "ok", Detail: "no capital events"}},
			Reconcile:     rpc.BriefReconcileRow{BriefRowState: rpc.BriefRowState{Status: "degraded", Detail: "never"}},
			AutoExtend:    rpc.BriefAutoExtendRow{BriefRowState: rpc.BriefRowState{Status: "ok", Detail: "none"}},
			WorkingOrders: rpc.BriefCountRow{BriefRowState: rpc.BriefRowState{Status: "ok", Detail: "journal"}},
		},
		Ready: rpc.BriefReadySection{
			Regime:        rpc.BriefRegimeRow{BriefRowState: rpc.BriefRowState{Status: "degraded", Detail: "gateway unavailable"}},
			Breadth:       rpc.BriefBreadthRow{BriefRowState: rpc.BriefRowState{Status: "unavailable", Detail: "cold"}},
			Gamma:         rpc.BriefGammaRow{BriefRowState: rpc.BriefRowState{Status: "unavailable", Detail: "cold"}},
			Stress:        rpc.BriefStressRow{BriefRowState: rpc.BriefRowState{Status: "degraded", Detail: "partial"}},
			Session:       rpc.BriefSessionRow{BriefRowState: rpc.BriefRowState{Status: "ok", Detail: "official"}},
			Capital:       rpc.BriefCapitalRow{BriefRowState: rpc.BriefRowState{Status: "attention", Detail: "block tier breached"}, Tier: "block", Enforcement: "shadow"},
			Latch:         rpc.BriefLatchRow{BriefRowState: rpc.BriefRowState{Status: "ok", Detail: "open"}},
			PremiumAtRisk: rpc.BriefMoneyCoverageRow{BriefRowState: rpc.BriefRowState{Status: "degraded", Detail: "nil values excluded"}},
			HedgeCost:     rpc.BriefMoneyCoverageRow{BriefRowState: rpc.BriefRowState{Status: "degraded", Detail: "nil greeks excluded"}},
			PolicyDrift:   rpc.BriefPolicyDriftRow{BriefRowState: rpc.BriefRowState{Status: "ok", Detail: "match"}},
		},
	}
	renderBrief(env, res)
	got := stdout.String()
	for _, want := range []string{"Review  (since the last close)", "Ready  (today)", "session P&L", "by underlying", "proposals", "capital events", "policy adherence", "gateway unavailable", "nil greeks excluded", "current policy has unknown checks", "attention", "tier block · enforcement shadow", "2 offered · 1 acted", "last session close", "2026-07-17 · not captured"} {
		if !strings.Contains(got, want) {
			t.Fatalf("brief render missing %q:\n%s", want, got)
		}
	}
	var regimeLine string
	for line := range strings.SplitSeq(got, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "regime ") {
			regimeLine = line
			break
		}
	}
	if regimeLine == "" || !strings.HasSuffix(regimeLine, " —") || strings.Contains(regimeLine, "·") {
		t.Fatalf("empty regime stage and verdict must render an em dash, got %q:\n%s", regimeLine, got)
	}
}

func TestRenderTradingStatusTextWriteBlockers(t *testing.T) {
	t.Parallel()
	var stdout bytes.Buffer
	env := &Env{Stdout: &stdout, Stderr: &bytes.Buffer{}}
	renderTradingStatusText(env, &rpc.TradingStatus{
		Mode:           "paper",
		Endpoint:       "127.0.0.1:7497",
		Account:        "DU1234567",
		AccountOrigin:  "pinned",
		ClientID:       15,
		ClientIDOrigin: "pinned",
		MCPTrading:     rpc.TradingMCPDisabled,
		CanPreview:     true,
		CanWrite:       false,
		WriteBlockers: []rpc.TradingBlocker{{
			Code:    "order_writes_unavailable",
			Message: "order writes are unavailable in this build",
			Action:  "Rebuild the daemon with the trading write capability.",
		}},
	})
	got := stdout.String()
	for _, want := range []string{
		"Canary Trading  READY",
		"Capabilities   preview=true write=false",
		"Write blockers:",
		"order_writes_unavailable: order writes are unavailable in this build",
		"action: Rebuild the daemon with the trading write capability.",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("trading status missing %q:\n%s", want, got)
		}
	}
}
