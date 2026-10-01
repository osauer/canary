package daemon

import (
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/rpc"
	ibkrlib "github.com/osauer/canary/v2/pkg/ibkr"
)

// A held terminal stock kept drawing broker "no security definition" answers
// from a lingering quote re-probe and after every reconnect, although
// reviewed evidence proves it defunct. Only the exact contract match seeds
// the connector's reviewed terminal set; the same ticker on another ConID or
// another ticker on the dead ConID never does. The last set carries to a new
// connector, and a read that no longer matches lifts it.
func TestReviewedTerminalHoldingsSeedConnectorFromExactMatchOnly(t *testing.T) {
	const deadConID = 900201
	record := earningsTerminalRecord{Contract: earningsTerminalContract{ConID: deadConID, Symbol: "SYNTHDEAD", SecType: "STK"}, Classification: earningsTerminalClassEquityCancelled,
		EffectiveDate: "2026-08-01", VerifiedAt: terminalImportBase.Add(-time.Hour), RevalidateAfter: terminalImportBase.Add(24 * time.Hour)}
	s := &Server{earningsTerminal: &earningsTerminalStore{revision: 2, reviewedAt: terminalImportBase, byConID: map[int]earningsTerminalStored{deadConID: {record: record, fingerprint: earningsTerminalRecordFingerprint(record)}}}}
	newConnector := func() *ibkrlib.Connector {
		c := ibkrlib.NewConnector(&ibkrlib.ConnectorConfig{})
		t.Cleanup(func() { _ = c.Stop() })
		return c
	}
	routed := func(conID int) string {
		return ibkrlib.MarketDataKeyForContract(ibkrlib.Contract{Symbol: "SYNTHDEAD", SecType: "STK", ConID: conID, Exchange: "SMART", Currency: "USD"})
	}

	// Symbol match on a different ConID, and the dead ConID under another
	// ticker, seed nothing.
	mismatched := []rpc.PositionView{
		{Symbol: "SYNTHDEAD", ConID: deadConID + 1, SecType: "STK"},
		{Symbol: "SYNTHOTHER", ConID: deadConID, SecType: "STK"},
	}
	held := s.markReviewedTerminalStocks(mismatched, terminalImportBase)
	if len(held) != 0 {
		t.Fatalf("non-exact rows seeded the reviewed set: %+v", held)
	}
	c := newConnector()
	s.publishReviewedTerminalHoldings(c, held)
	if c.IsSymbolInactive("SYNTHDEAD") || c.IsSymbolInactive("SYNTHOTHER") {
		t.Fatal("non-exact rows suppressed broker requests")
	}

	exact := append([]rpc.PositionView{{Symbol: "SYNTHDEAD", ConID: deadConID, SecType: "STK"}}, mismatched...)
	held = s.markReviewedTerminalStocks(exact, terminalImportBase)
	if len(held) != 1 || held["SYNTHDEAD"].ConID != deadConID {
		t.Fatalf("exact row not seeded alone: %+v", held)
	}
	s.publishReviewedTerminalHoldings(c, held)
	if !c.IsSymbolInactive("SYNTHDEAD") || !c.IsSymbolInactive(routed(deadConID)) {
		t.Fatal("exact reviewed terminal holding still reaches the broker")
	}
	if c.IsSymbolInactive(routed(deadConID+1)) || c.IsSymbolInactive("SYNTHOTHER") {
		t.Fatal("the same ticker on another ConID was suppressed")
	}

	// A reconnect's new connector is seeded before any positions read.
	successor := newConnector()
	s.seedReviewedTerminalHoldings(successor)
	if !successor.IsSymbolInactive("SYNTHDEAD") {
		t.Fatal("new connector was not seeded with the last reviewed set")
	}
	(&Server{}).seedReviewedTerminalHoldings(newConnector())

	// An expired review no longer matches; the next full read lifts it.
	s.publishReviewedTerminalHoldings(successor, s.markReviewedTerminalStocks(exact, record.RevalidateAfter))
	if successor.IsSymbolInactive("SYNTHDEAD") {
		t.Fatal("expired review still suppresses broker requests")
	}
	fresh := newConnector()
	s.seedReviewedTerminalHoldings(fresh)
	if fresh.IsSymbolInactive("SYNTHDEAD") {
		t.Fatal("a lifted set was carried to a new connector")
	}
}
