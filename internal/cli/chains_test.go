package cli

import (
	"time"

	"github.com/osauer/canary/v3/internal/rpc"
)

// goldenChainsAt is a synthetic session: 2026-10-14, close 16:00 New York.
var goldenChainsAt = time.Date(2026, 10, 14, 20, 0, 0, 0, time.UTC)

func goldenChainRecorderState(enabled bool) rpc.ChainRecorderState {
	// Research names a session added (a collection setting), and one chain a
	// synthetic registered strategy declares.
	st := rpc.ChainRecorderState{Enabled: enabled, Symbols: []string{"SPX", "SPY", "QQQ"}, MaxSymbols: 12,
		Declared: []rpc.ChainRecorderDeclaration{{Symbol: "SYNO", Source: "strategy:s9"}},
		Expiries: "two nearest standard monthlies and the nearest weekly", StrikeWindowPct: 0.2, MaxStrikesPerExpiry: 40,
		LeadMinutes: 30, MaxLines: 4, LineReserve: 20}
	if enabled {
		st.NextRun = new(goldenChainsAt.Add(23*time.Hour + 30*time.Minute))
	}
	return st
}

func goldenChainsRecorded() rpc.ChainsRecordedResult {
	close := goldenChainsAt
	expiries := []rpc.ChainRecorderExpiry{
		{Date: "2026-10-16", TradingClass: "SPY", Slot: "monthly_1", InWindow: 241, Selected: 40, Contracts: 80},
		{Date: "2026-10-23", TradingClass: "SPY", Slot: "weekly", InWindow: 241, Selected: 40, Contracts: 80},
		{Date: "2026-11-20", TradingClass: "SPY", Slot: "monthly_2", InWindow: 241, Selected: 40, Contracts: 80},
	}
	full := rpc.ChainRecorderCoverage{Planned: 240, Attempted: 240, Quoted: 236, TwoSided: 231, WithIV: 238, WithGreeks: 238, WithOI: 229,
		ByStatus: map[string]int{"quoted": 236, "model_only": 2, "no_quote": 2}}
	return rpc.ChainsRecordedResult{Version: 1, AsOf: goldenChainsAt, Recorder: goldenChainRecorderState(true),
		Store: rpc.ChainRecorderStore{Present: true, Runs: 2, Rows: 642, Bytes: 141 << 10},
		Runs: []rpc.ChainRecorderRun{
			{RunID: 2, SessionDate: "2026-10-14", SessionClose: close, ScheduledAt: close.Add(-30 * time.Minute), StartedAt: close.Add(-30 * time.Minute),
				Deadline: close, FinishedAt: close.Add(-22 * time.Minute), Status: "partial", StopReason: "pacing",
				Coverage: rpc.ChainRecorderCoverage{Planned: 480, Attempted: 402, Quoted: 389, TwoSided: 380, WithIV: 395, WithGreeks: 395, WithOI: 371, NotAttempted: 78,
					ByStatus: map[string]int{"quoted": 389, "no_quote": 8, "pacing": 5, "not_attempted": 78}},
				Symbols: []rpc.ChainRecorderSymbolResult{
					{Symbol: "SPY", Source: "explicit", Status: "planned", Spot: 662.41, SpotSource: "last", SpotDataType: "live", Expiries: expiries, Coverage: full},
					{Symbol: "SYNA", Source: "held", Status: "planned", Spot: 101.5, SpotSource: "mid", SpotDataType: "live", Expiries: expiries,
						Coverage: rpc.ChainRecorderCoverage{Planned: 240, Attempted: 162, Quoted: 153, NotAttempted: 78}},
					{Symbol: "SYNB", Source: "held", Status: "chain_unavailable", Detail: "security definition farm unavailable"},
				}},
			{RunID: 1, SessionDate: "2026-10-13", SessionClose: close.Add(-24 * time.Hour), ScheduledAt: close.Add(-24*time.Hour - 30*time.Minute),
				StartedAt: close.Add(-24*time.Hour - 30*time.Minute), Deadline: close.Add(-24 * time.Hour), Status: "interrupted",
				Coverage: rpc.ChainRecorderCoverage{Planned: 240, Attempted: 162, Quoted: 160, TwoSided: 158, WithIV: 161, WithGreeks: 161, WithOI: 150, NotAttempted: 78,
					ByStatus: map[string]int{"quoted": 160, "no_quote": 2}},
				Symbols: []rpc.ChainRecorderSymbolResult{{Symbol: "SPY", Source: "explicit", Status: "planned", Spot: 659.02, Expiries: expiries,
					Coverage: rpc.ChainRecorderCoverage{Planned: 240, Attempted: 162, Quoted: 160, NotAttempted: 78}}}},
		}}
}

func goldenChainsOff() rpc.ChainsRecordedResult {
	return rpc.ChainsRecordedResult{Version: 1, AsOf: goldenChainsAt, Recorder: goldenChainRecorderState(false), Runs: []rpc.ChainRecorderRun{}}
}

func goldenChainsCSV() goldenConn {
	res := goldenChainsRecorded()
	res.Runs = res.Runs[:1]
	at := goldenChainsAt.Add(-25 * time.Minute)
	return goldenConn{
		rpc.MethodChainsRecorded: res,
		rpc.MethodChainsRecordedRows: rpc.ChainsRecordedRowsResult{Rows: []rpc.ChainRecordedRow{
			{RunID: 2, SessionDate: "2026-10-14", Symbol: "SPY", TradingClass: "SPY", Expiry: "2026-11-20", ExpirySlot: "monthly_2", Strike: 660, Right: "C",
				Status: "quoted", ConID: 900001, Bid: new(14.2), Ask: new(14.27), Last: new(14.25), BidSize: new(41.0), AskSize: new(18.0),
				Volume: new(int64(3120)), OpenInterest: new(int64(25410)), IV: new(0.1564), Delta: new(0.5312), Gamma: new(0.0091), Vega: new(0.8811),
				Theta: new(-0.1622), UnderlyingPrice: new(662.38), ModelSource: "live", Feed: "live", RequestedAt: at, ObservedAt: at.Add(900 * time.Millisecond),
				QuoteReceivedAt: at.Add(450 * time.Millisecond), LastTradeAt: at.Add(-2 * time.Minute)},
			{RunID: 2, SessionDate: "2026-10-14", Symbol: "SPY", TradingClass: "SPY", Expiry: "2026-11-20", ExpirySlot: "monthly_2", Strike: 530, Right: "P",
				Status: "model_only", IV: new(0.3102), Delta: new(-0.0211), Gamma: new(0.0004), Vega: new(0.0912), Theta: new(-0.0187), ModelSource: "live",
				Feed: "live", RequestedAt: at, ObservedAt: at.Add(3 * time.Second)},
			{RunID: 2, SessionDate: "2026-10-14", Symbol: "SPY", TradingClass: "SPY", Expiry: "2026-10-23", ExpirySlot: "weekly", Strike: 700, Right: "C",
				Status: "not_attempted", Detail: "pacing"},
		}},
	}
}
