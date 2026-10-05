package cli

import (
	"bytes"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/config"
	"github.com/osauer/canary/v2/internal/rpc"
)

func TestCommandDisplayPreservesEvidenceAtNarrowWidths(t *testing.T) {
	at := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	for _, width := range []string{"40", "80"} {
		t.Run(width, func(t *testing.T) {
			t.Setenv("COLUMNS", width)
			for _, color := range []bool{false, true} {
				cases := []struct {
					name   string
					render func(*Env)
					want   []string
				}{
					{"technical", func(e *Env) {
						renderTechnicalText(e, &rpc.TechnicalResult{Benchmark: "SPY", Currency: "USD", LookbackDays: 300, Rows: []rpc.TechnicalRow{{Symbol: "SYNTH", Price: new(100.0), TrendState: "uptrend", DataQuality: "partial", RS63D: new(.012), MissingReasons: []string{"atr_unavailable"}, Error: "quote unavailable\x1b[2J"}}})
					}, []string{"error", "quote unavailable", "+1.2 pp", "—", "atr unavailable"}},
					{"coverage", func(e *Env) {
						renderSetupCoverage(e, rpc.SetupCoverageResult{Truncated: true, Contracts: []rpc.SetupCoverageContract{{Symbol: "SYNTH", SlotsScheduled: 12, SlotsCovered: 3, States: map[string]int{"unavailable:history_missing": 2}}}})
					}, []string{"3/12 scheduled slots covered", "unavailable:history missing", "truncated"}},
					{"options", func(e *Env) {
						renderSetupOptions(e, rpc.SetupOptionsResult{Underlying: rpc.ContractParams{Symbol: "SYNTH", Currency: "USD"}, Quote: &rpc.SetupOptionQuote{Status: rpc.SetupQuoteMissing, DataType: "delayed", AsOf: at}})
					}, []string{"missing", "delayed", "— / — USD", "2026-10-05", "exact order preview"}},
					{"markouts", func(e *Env) {
						renderSetupMarkouts(e, rpc.SetupMarkoutsResult{Targets: []rpc.SetupMarkoutTarget{{Contract: rpc.ContractParams{Symbol: "SYNTH"}, Status: rpc.SetupMarkoutMissing, Reason: "quote too old", Currency: "USD"}}})
					}, []string{"Not realized profit", "exclude commission", "missing", "quote too old", "— USD"}},
					{"proposals", func(e *Env) {
						renderProposalsSummary(e, &rpc.TradeProposalSnapshot{Proposals: []rpc.TradeProposal{{Key: "synthetic-proposal", Symbol: "SYNTH", Shadow: true, Blockers: []rpc.TradingBlocker{{Code: "quote_missing", Message: "No current quote", Action: "Refresh evidence"}}}}})
					}, []string{"observation only", "preview and submit refuse", "quote_missing", "Refresh evidence", "--details"}},
					{"proposal details", func(e *Env) {
						renderProposalsDetailsDisplay(e, &rpc.TradeProposalSnapshot{Proposals: []rpc.TradeProposal{{Key: "synthetic-proposal", Symbol: "SYNTH", Details: []string{strings.Repeat("evidence ", 30) + "last fact"}, Blockers: []rpc.TradingBlocker{{Message: "Missing evidence", Action: "Refresh"}}}}})
					}, []string{"last fact", "Missing evidence", "Refresh"}},
				}
				for _, tc := range cases {
					var out bytes.Buffer
					env := &Env{Stdout: &out, Color: color}
					tc.render(env)
					got := strings.Join(strings.Fields(stripDisplayANSI(out.String())), " ")
					for _, want := range tc.want {
						if !strings.Contains(got, want) {
							t.Fatalf("%s missing %q: %s", tc.name, want, got)
						}
					}
					for line := range strings.SplitSeq(out.String(), "\n") {
						if visibleLen(line) > outputColumns(env.Stdout) {
							t.Fatalf("%s exceeds %s: %q", tc.name, width, line)
						}
					}
					if strings.Contains(out.String(), "\x1b[2J") {
						t.Fatal("external terminal control escaped")
					}
				}
			}
		})
	}
}

func stripDisplayANSI(s string) string {
	for _, code := range []string{ansiGreen, ansiYellow, ansiRed, ansiDim, ansiBold, ansiReset} {
		s = strings.ReplaceAll(s, code, "")
	}
	return s
}

func TestStatusColorsSeparateDeliveryAccessAndBlockers(t *testing.T) {
	env := &Env{Color: true}
	if got := formatTradingMode(env, rpc.TradingStatus{Mode: config.TradingModeLive}); strings.Contains(got, ansiYellow) {
		t.Fatal("ready live mode warns", got)
	}
	if got := formatTradingMode(env, rpc.TradingStatus{Mode: config.TradingModeLive, Blocked: true}); !strings.Contains(got, ansiYellow) {
		t.Fatal("real blocker lost warning", got)
	}
	st := rpc.PlatformSettings{}
	st.Features.StockProtection.Enabled.Value = true
	st.MarketData.Quality.Status = "delayed"
	if got := settingsVerdict(st); got.Level != statusConcernNotice {
		t.Fatal("delivery mode mistaken for degradation", got)
	}
	st.MarketData.Quality.Status = "degraded"
	if got := settingsVerdict(st); got.Level != statusConcernWarn {
		t.Fatal("degradation lost warning", got)
	}
	for _, v := range []bool{false, true} {
		if got := formatSettingsBool(env, rpc.SettingsBool{Value: v, Access: rpc.SettingsAccessWrite}); strings.Contains(got, ansiGreen) {
			t.Fatal("writability mistaken for health", got)
		}
	}
}

func TestMacroSummaryKeepsCoverageAheadOfHeadlinesAndDetailsComplete(t *testing.T) {
	t.Setenv("COLUMNS", "40")
	r := rpc.MacroSnapshotResult{CoverageStatus: "partial", Sources: []rpc.MacroSource{{ID: "official", Availability: "available", Stale: true, Detail: "cached calendar"}}, EventsTruncated: true, PublicationsTruncated: true}
	for i := range 12 {
		r.Events = append(r.Events, rpc.MacroEvent{Title: "Event " + strings.Repeat("x", i), Date: "2026-10-05"})
	}
	for range 6 {
		r.Publications = append(r.Publications, rpc.MacroPublication{Title: "Publication", SourceURL: "https://example.com/official"})
	}
	original := r
	var out bytes.Buffer
	env := &Env{Stdout: &out, Color: true}
	renderMacro(env, r, false)
	got := strings.Join(strings.Fields(stripDisplayANSI(out.String())), " ")
	for _, want := range []string{"stale", "cached calendar", "events and publications incomplete", "+4 more events", "+3 more publications", "time unspecified"} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q: %s", want, got)
		}
	}
	if strings.Index(got, "stale") > strings.Index(got, "Scheduled events") {
		t.Fatal("coverage buried below headlines")
	}
	out.Reset()
	renderMacro(env, r, true)
	got = out.String()
	if strings.Count(got, "https://example.com/official") != 6 || strings.Contains(got, "more events") {
		t.Fatal("details omitted evidence", got)
	}
	if !reflect.DeepEqual(r, original) {
		t.Fatal("render mutated authority")
	}
	for line := range strings.SplitSeq(got, "\n") {
		if visibleLen(line) > 40 {
			t.Fatal("macro line too wide", line)
		}
	}
}

func TestSetupDefaultTextAndJSONRemainSeparate(t *testing.T) {
	for _, args := range [][]string{{"coverage", "--symbol", "SYNTH"}, {"options", "--symbol", "SYNTH", "--con-id", "17"}, {"markouts"}} {
		c := &setupCLIConn{}
		var out, errOut bytes.Buffer
		env := &Env{Conn: c, Stdout: &out, Stderr: &errOut}
		if code := Run(t.Context(), env, "setups", args); code != 0 {
			t.Fatal(errOut.String())
		}
		if strings.HasPrefix(strings.TrimSpace(out.String()), "{") {
			t.Fatal("default output remains JSON")
		}
	}
}

func TestProposalSummaryKeepsExactContractAndQueuedAuthority(t *testing.T) {
	var out bytes.Buffer
	renderProposalsSummary(&Env{Stdout: &out}, &rpc.TradeProposalSnapshot{Proposals: []rpc.TradeProposal{
		{Symbol: "SYNTH", Action: "SELL", Quantity: 2, Contract: rpc.ContractParams{Symbol: "SYNTH", SecType: "OPT", Expiry: "20351219", Right: "C", Strike: 100}, Queued: &rpc.TradeProposalQueued{State: "waiting"}},
		{Symbol: "SYNTHB", Action: "BUY", Quantity: 3, Bucket: rpc.TradeProposalBucketCashSweep, CashSweep: &rpc.TradeProposalCashSweep{QuantityUnit: rpc.BondQuantityUnitFace1000}},
	}})
	got := strings.Join(strings.Fields(out.String()), " ")
	for _, want := range []string{"SYNTH 20351219 100 C", "SELL 2 contracts", "queued · executor only", "Queue: waiting", "BUY 3 face_1000"} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q: %s", want, got)
		}
	}
}
