package daemon

import (
	"strings"
	"testing"
	"time"

	"github.com/osauer/canary/v3/internal/core"
	"github.com/osauer/canary/v3/internal/rpc"
)

// Monday 2 November 2026: London opens 08:00 GMT (the UK fell back on 25
// October), the US 09:30 EST (14:30Z, having fallen back on 1 November), so
// the core's row is due 14:45Z, 15:45 CET.
var (
	coreDueMonday = "2026-11-02"
	coreDueAt     = utc(2026, 11, 2, 14, 45)
)

// coreDueRow is a core_rebalance row of a synthetic London line for a
// window session.
func coreDueRow(session string) rpc.TradeProposal {
	contract := rpc.ContractParams{ConID: 900001, Symbol: "SYNX", SecType: "STK", Currency: "EUR", Exchange: "SMART", PrimaryExch: "LSEETF", Market: "uk"}
	return rpc.TradeProposal{Key: "core_rebalance:syn", Revision: "r1", State: rpc.TradeProposalStateGenerated, Bucket: rpc.TradeProposalBucketCoreRebalance,
		Symbol: "SYNX", SecType: "STK", Action: rpc.OrderActionBuy, Quantity: 10, OrderType: rpc.OrderTypeLMT, TIF: rpc.OrderTIFDay, PositionEffect: rpc.OrderPositionEffectOpen, Contract: contract,
		Core: &rpc.TradeProposalCore{Decision: "2026-10-30", Kind: "buy", Session: session, ConID: 900001, Currency: "EUR", Sleeve: "core", Stage: "live",
			Terms: rpc.CoreRowTerms{Episode: "core:DU1234567:paper:2026-10", Window: []string{"2026-11-02", "2026-11-03", "2026-11-04", "2026-11-05", "2026-11-06"}}}}
}

// The due time is the US cash open of the row's session plus fifteen
// minutes; a row outside the core, without a session or on a day the US
// does not trade has none.
func TestCoreRowDueAt(t *testing.T) {
	now := utc(2026, 11, 2, 9, 0)
	if due, ok := coreRowDueAt(coreDueRow(coreDueMonday), now); !ok || !due.Equal(coreDueAt) {
		t.Fatalf("due %v %v, want %v", due, ok, coreDueAt)
	}
	// Before the US fell back, the same open is 13:30Z: Friday 30 October.
	if due, ok := coreRowDueAt(coreDueRow("2026-10-30"), now); !ok || !due.Equal(utc(2026, 10, 30, 13, 45)) {
		t.Fatalf("due before the US clock change %v %v", due, ok)
	}
	other := readinessRow(rpc.TradeProposalBucketBudgetReduction)
	noSession := coreDueRow("")
	sunday := coreDueRow("2026-11-01")
	for name, row := range map[string]rpc.TradeProposal{"another bucket": other, "no session": noSession, "a day the US does not trade": sunday} {
		if _, ok := coreRowDueAt(row, now); ok {
			t.Fatalf("%s has a due time", name)
		}
	}
	if b := coreDueBlockers(coreDueRow(coreDueMonday), now); len(b) != 1 || b[0].Code != coreBeforeDueCode || !strings.Contains(b[0].Message, "2026-11-02T14:45:00Z") {
		t.Fatalf("before the due time: %+v", b)
	}
	if b := coreDueBlockers(coreDueRow(coreDueMonday), coreDueAt); b != nil {
		t.Fatalf("at the due time: %+v", b)
	}
	if b := coreDueBlockers(other, now); b != nil {
		t.Fatalf("another bucket: %+v", b)
	}
}

// Readiness of the core's row: before the due time an open London reads as
// the opening window with the due time as the default send; a closed London
// dates its default send at the due time of the row's session, not the
// London open; from the due time the row reads ready; a row for a later
// session than today's reads the opening window until that session's due
// time. Before this rule the row read ready five minutes after the London
// open, hours before the US open the design's due time waits for.
func TestCoreRowReadinessWaitsForTheDueTime(t *testing.T) {
	for name, tc := range map[string]struct {
		at      time.Time
		session string
		code    string
		send    time.Time
		opens   time.Time
	}{
		"London open, before the US open":       {utc(2026, 11, 2, 9, 0), coreDueMonday, rpc.ReadinessOpeningWindow, coreDueAt, utc(2026, 11, 2, 8, 0)},
		"a minute before the due time":          {utc(2026, 11, 2, 14, 44), coreDueMonday, rpc.ReadinessOpeningWindow, coreDueAt, utc(2026, 11, 2, 8, 0)},
		"the due time":                          {coreDueAt, coreDueMonday, rpc.ReadinessReady, time.Time{}, time.Time{}},
		"the evening before":                    {utc(2026, 11, 1, 20, 0), coreDueMonday, rpc.ReadinessMarketClosed, coreDueAt, utc(2026, 11, 2, 8, 0)},
		"today's session, the row for the next": {utc(2026, 11, 2, 15, 0), "2026-11-03", rpc.ReadinessOpeningWindow, utc(2026, 11, 3, 14, 45), utc(2026, 11, 2, 8, 0)},
	} {
		t.Run(name, func(t *testing.T) {
			srv, _ := readinessPreviewServer(t, t.TempDir(), tc.at)
			engine := &proposalEngine{server: srv, now: srv.now, store: &proposalStore{}}
			row := coreDueRow(tc.session)
			r := engine.classifyReadiness(row, coreDueBlockers(row, tc.at), false, readinessSessions{})
			if r == nil || r.Code != tc.code || r.Market != "uk_lse" || r.Queueable {
				t.Fatalf("readiness %+v, want %s on uk_lse, not queueable", r, tc.code)
			}
			if tc.code == rpc.ReadinessReady {
				if r.DefaultSendAt != nil {
					t.Fatalf("ready with a default send: %+v", r)
				}
				return
			}
			if r.DefaultSendAt == nil || !r.DefaultSendAt.Equal(tc.send) || r.OpensAt == nil || !r.OpensAt.Equal(tc.opens) {
				t.Fatalf("default send %v opens %v, want %v and %v", r.DefaultSendAt, r.OpensAt, tc.send, tc.opens)
			}
			if tc.code == rpc.ReadinessOpeningWindow && !strings.Contains(r.Message, "US cash open") {
				t.Fatalf("the opening window does not say why: %+v", r)
			}
		})
	}
}

// A frozen month another writer kept first is not served from memory: the
// conflict drops the in-memory copy and the next read re-reads the store.
func TestFreezeCoreConflictRereadsTheStore(t *testing.T) {
	store := openMarketTestCoreStore(t)
	scope := "DU1234567|paper|900001"
	other := &Server{coreStore: store, now: func() time.Time { return mandateTestNow }}
	first := &coreFrozenTarget{Version: coreTargetDocVer, Scope: scope, Decision: "2026-09-30", Digest: "theirs", FrozenAt: mandateTestNow, Signal: core.Signal{Decision: "2026-09-30"}}
	srv := &Server{coreStore: store, now: func() time.Time { return mandateTestNow }}
	if got := srv.coreFrozen(t.Context(), scope); got != nil {
		t.Fatalf("a month before any write: %+v", got)
	}
	if err := other.freezeCore(t.Context(), first); err != nil {
		t.Fatal(err)
	}
	mine := &coreFrozenTarget{Version: coreTargetDocVer, Scope: scope, Decision: "2026-09-30", Digest: "mine", FrozenAt: mandateTestNow.Add(time.Minute), Signal: core.Signal{Decision: "2026-09-30"}}
	if err := srv.freezeCore(t.Context(), mine); err == nil {
		t.Fatal("a stale revision was accepted")
	}
	if got := srv.coreFrozen(t.Context(), scope); got == nil || got.Digest != "theirs" {
		t.Fatalf("after the conflict the month reads %+v, want the store's", got)
	}
}

// The London line's quote contract: the core's row names market "uk"
// (coreContractParams), which the quote normaliser refused as unsupported
// until 2026-10-18, so no preview of the row could run. It now routes
// SMART with the line's own venue and currency, and defaults a bare London
// request to LSE in GBP.
func TestNormaliseStockQuoteContractAcceptsTheLondonMarket(t *testing.T) {
	line := coreDueRow(coreDueMonday).Contract
	contract, echo, routed, err := normaliseStockQuoteContract(line)
	if err != nil || !routed || contract.Exchange != "SMART" || contract.PrimaryExch != "LSEETF" || contract.Currency != "EUR" || contract.ConID != line.ConID || echo.Market != "uk" {
		t.Fatalf("the core's London line: %+v echo %+v routed %v err %v", contract, echo, routed, err)
	}
	bare, _, _, err := normaliseStockQuoteContract(rpc.ContractParams{Symbol: "SYNX", Market: "london"})
	if err != nil || bare.Exchange != "SMART" || bare.PrimaryExch != "LSE" || bare.Currency != "GBP" {
		t.Fatalf("a bare London request: %+v %v", bare, err)
	}
	if _, _, _, err := normaliseStockQuoteContract(rpc.ContractParams{Symbol: "SYNX", Market: "mars"}); err == nil || !strings.Contains(err.Error(), "supported: us, de, uk") {
		t.Fatalf("an unknown market: %v", err)
	}
}
