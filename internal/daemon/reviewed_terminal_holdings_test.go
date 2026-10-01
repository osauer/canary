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
	base := time.Now().UTC()
	const deadConID = 900201
	record := earningsTerminalRecord{Contract: earningsTerminalContract{ConID: deadConID, Symbol: "SYNTHDEAD", SecType: "STK"}, Classification: earningsTerminalClassEquityCancelled,
		EffectiveDate: "2026-08-01", VerifiedAt: base.Add(-time.Hour), RevalidateAfter: base.Add(24 * time.Hour)}
	s := &Server{earningsTerminal: &earningsTerminalStore{revision: 2, reviewedAt: base, byConID: map[int]earningsTerminalStored{deadConID: {record: record, fingerprint: earningsTerminalRecordFingerprint(record)}}}}
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
	held := s.markReviewedTerminalStocks(mismatched, base)
	if len(held) != 0 {
		t.Fatalf("non-exact rows seeded the reviewed set: %+v", held)
	}
	c := newConnector()
	s.publishReviewedTerminalHoldings(c, held)
	if c.IsSymbolInactive("SYNTHDEAD") || c.IsSymbolInactive("SYNTHOTHER") {
		t.Fatal("non-exact rows suppressed broker requests")
	}

	exact := append([]rpc.PositionView{{Symbol: "SYNTHDEAD", ConID: deadConID, SecType: "STK"}}, mismatched...)
	held = s.markReviewedTerminalStocks(exact, base)
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

func TestReviewedTerminalSeedRevalidatesExpiryAndRevocation(t *testing.T) {
	for _, mode := range []string{"expired", "revoked", "changed_revision"} {
		t.Run(mode, func(t *testing.T) {
			now := time.Now().UTC()
			const id = 900901
			record := earningsTerminalRecord{Contract: earningsTerminalContract{ConID: id, Symbol: "SYNTHDEAD", SecType: "STK"}, Classification: earningsTerminalClassEquityCancelled, EffectiveDate: "2026-08-01", VerifiedAt: now.Add(-3 * time.Hour), RevalidateAfter: now.Add(time.Hour)}
			if mode == "expired" {
				record.RevalidateAfter = now.Add(-time.Hour)
			}
			s := &Server{earningsTerminal: &earningsTerminalStore{revision: 1, reviewedAt: now, byConID: map[int]earningsTerminalStored{id: {record: record, fingerprint: earningsTerminalRecordFingerprint(record)}}}}
			held := s.markReviewedTerminalStocks([]rpc.PositionView{{Symbol: "SYNTHDEAD", ConID: id, SecType: "STK"}}, now.Add(-2*time.Hour))
			if len(held) != 1 {
				t.Fatal("fixture did not originally match")
			}
			s.publishReviewedTerminalHoldings(nil, held)
			if mode == "revoked" {
				delete(s.earningsTerminal.byConID, id)
			}
			if mode == "changed_revision" {
				s.earningsTerminal.revision++
			}
			c := ibkrlib.NewConnector(&ibkrlib.ConnectorConfig{})
			defer c.Stop()
			s.seedReviewedTerminalHoldings(c)
			if c.IsSymbolInactive("SYNTHDEAD") {
				t.Fatal("stale evidence seeded suppression")
			}
		})
	}
}
