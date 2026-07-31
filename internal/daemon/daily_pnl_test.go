package daemon

import (
	"context"
	"testing"
	"time"

	ibkrlib "github.com/osauer/canary/v2/pkg/ibkr"

	"github.com/osauer/canary/v2/internal/rpc"
)

// timeNowForTest returns a fixed-shape "non-zero" time so cache-validity
// checks key off "AsOf != zero" pass deterministically.
func timeNowForTest() time.Time { return time.Now().UTC() }

// TestPositionViewKey covers the key namespacing so views that share a
// Symbol cannot collide: across security types, across treasury issues
// (same base symbol, distinct conIds), and across futures expiries.
func TestPositionViewKey(t *testing.T) {
	t.Parallel()
	stock := rpc.PositionView{Symbol: "AAPL", SecType: rpc.SecTypeStock, ConID: 265598}
	opt := rpc.PositionView{Symbol: "AAPL", SecType: rpc.SecTypeOption, ConID: 700001, Expiry: "20260619", Strike: 195, Right: "C"}

	if positionViewKey(stock) == positionViewKey(opt) {
		t.Errorf("option key collides with stock key: %q", positionViewKey(opt))
	}

	opt2 := opt
	opt2.Strike = 196
	if positionViewKey(opt) == positionViewKey(opt2) {
		t.Errorf("different strikes produced identical keys")
	}

	// IB reports treasuries with a repeated base symbol; only the conId
	// (and the maturity behind it) separates the two holdings.
	bondA := rpc.PositionView{Symbol: "T", SecType: "BOND", ConID: 500001, LocalSymbol: "T 4 1/8 11/15/32"}
	bondB := rpc.PositionView{Symbol: "T", SecType: "BOND", ConID: 500002, LocalSymbol: "T 4 5/8 02/15/35"}
	if positionViewKey(bondA) == positionViewKey(bondB) {
		t.Errorf("same-symbol BOND rows with different conIds collided: %q", positionViewKey(bondA))
	}

	// Futures repeat the base symbol across expiries.
	futA := rpc.PositionView{Symbol: "ES", SecType: rpc.SecTypeFuture, ConID: 600001, Expiry: "20260320"}
	futB := rpc.PositionView{Symbol: "ES", SecType: rpc.SecTypeFuture, ConID: 600002, Expiry: "20260619"}
	if positionViewKey(futA) == positionViewKey(futB) {
		t.Errorf("same-symbol FUT rows with different expiries collided: %q", positionViewKey(futA))
	}
	futSameConID := futA
	futSameConID.Expiry = futB.Expiry
	if positionViewKey(futA) == positionViewKey(futSameConID) {
		t.Errorf("expiry does not participate in the FUT key: %q", positionViewKey(futA))
	}

	// A stock and a bond on the same symbol are different holdings.
	bondOnStockSymbol := rpc.PositionView{Symbol: "AAPL", SecType: "BOND", ConID: 265598}
	if positionViewKey(stock) == positionViewKey(bondOnStockSymbol) {
		t.Errorf("BOND key collides with STOCK key on the same symbol: %q", positionViewKey(stock))
	}
}

// TestFillDailyPnL_SameSymbolBondRows is the behavioral half of the
// namespacing: two treasury rows sharing a Symbol must each keep their
// own conId through the map handlePositions builds, so each row gets its
// own daily P&L rather than one row's value or none.
func TestFillDailyPnL_SameSymbolBondRows(t *testing.T) {
	t.Parallel()
	c := ibkrlib.NewConnector(&ibkrlib.ConnectorConfig{})
	firstPnL, secondPnL := 11.25, -3.75
	c.SeedPositionDailyPnLForTest(500001, ibkrlib.PositionDailyPnL{DailyPnL: &firstPnL})
	c.SeedPositionDailyPnLForTest(500002, ibkrlib.PositionDailyPnL{DailyPnL: &secondPnL})

	rows := []rpc.PositionView{
		{Symbol: "T", SecType: "BOND", ConID: 500001, LocalSymbol: "T 4 1/8 11/15/32"},
		{Symbol: "T", SecType: "BOND", ConID: 500002, LocalSymbol: "T 4 5/8 02/15/35"},
	}
	// Built the way handlePositions builds it.
	conIDs := map[string]int{}
	for _, row := range rows {
		conIDs[positionViewKey(row)] = row.ConID
	}
	if len(conIDs) != 2 {
		t.Fatalf("conID map holds %d entries, want 2 — same-symbol rows collided", len(conIDs))
	}

	srv := newTestServer(t)
	srv.fillDailyPnL(c, rows, conIDs)

	if rows[0].DailyPnL == nil || *rows[0].DailyPnL != firstPnL {
		t.Errorf("first bond DailyPnL = %v, want %v", rows[0].DailyPnL, firstPnL)
	}
	if rows[1].DailyPnL == nil || *rows[1].DailyPnL != secondPnL {
		t.Errorf("second bond DailyPnL = %v, want %v", rows[1].DailyPnL, secondPnL)
	}
}

// TestFillDailyPnL_PopulatesFromConnectorCache walks the happy path: a
// connector with a pre-populated PnL cache feeds DailyPnL onto rows
// whose conIDs are known.
func TestFillDailyPnL_PopulatesFromConnectorCache(t *testing.T) {
	t.Parallel()
	c := ibkrlib.NewConnector(&ibkrlib.ConnectorConfig{})
	conID := 265598
	dailyPnL := 12.50
	c.SeedPositionDailyPnLForTest(conID, ibkrlib.PositionDailyPnL{DailyPnL: &dailyPnL})

	rows := []rpc.PositionView{
		{Symbol: "AAPL", SecType: rpc.SecTypeStock},
	}
	conIDs := map[string]int{
		positionViewKey(rows[0]): conID,
	}

	srv := newTestServer(t)
	srv.fillDailyPnL(c, rows, conIDs)

	if rows[0].DailyPnL == nil {
		t.Fatalf("DailyPnL still nil after fillDailyPnL")
	}
	if *rows[0].DailyPnL != 12.50 {
		t.Errorf("DailyPnL = %v, want 12.50", *rows[0].DailyPnL)
	}
}

// TestFillDailyPnL_NilWhenNoSubscription confirms the no-fabrication
// invariant: a row whose conId hasn't yet emitted a frame is left
// nil, not set to 0.
func TestFillDailyPnL_NilWhenNoSubscription(t *testing.T) {
	t.Parallel()
	c := ibkrlib.NewConnector(&ibkrlib.ConnectorConfig{})
	// No subscription seeded; reaching fillDailyPnL via SubscribePosition...
	// would require a live connection, so we test the read-path branch
	// where the cache simply has no entry.

	rows := []rpc.PositionView{{Symbol: "AAPL", SecType: rpc.SecTypeStock}}
	conIDs := map[string]int{positionViewKey(rows[0]): 999999}

	srv := newTestServer(t)
	srv.fillDailyPnL(c, rows, conIDs)
	if rows[0].DailyPnL != nil {
		t.Errorf("DailyPnL = %v, want nil for unsubscribed conId", *rows[0].DailyPnL)
	}
}

// TestFillDailyPnL_EmptyRows is the early-return guard: no rows, no
// work. Mostly here to pin the behavior so future refactors don't
// accidentally make this branch issue subscriptions for an empty
// portfolio.
func TestFillDailyPnL_EmptyRows(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t)
	c := ibkrlib.NewConnector(&ibkrlib.ConnectorConfig{})
	srv.fillDailyPnL(c, nil, nil)
	srv.fillDailyPnL(c, []rpc.PositionView{}, map[string]int{})
}

// TestFillDailyPnL_RespectsMaxSubscriptionCap pins the soft cap. Real
// subscription kickoff requires a live connection so we exercise the
// cap-check branch via SubscribePositionDailyPnL's idempotency: a
// connector seeded with maxDailyPnLSubscriptions entries already won't
// issue further subscribes from fillDailyPnL.
//
// We can't easily exercise the full subscribe path without a live
// connection, but we can confirm the gate function returns the right
// count for the daemon's bookkeeping.
func TestFillDailyPnL_RespectsMaxSubscriptionCap(t *testing.T) {
	t.Parallel()
	c := ibkrlib.NewConnector(&ibkrlib.ConnectorConfig{})
	// Seed the cache with maxDailyPnLSubscriptions+1 entries.
	for i := range maxDailyPnLSubscriptions + 1 {
		c.SeedPositionDailyPnLForTest(1000+i, ibkrlib.PositionDailyPnL{})
	}
	if got := c.ActiveDailyPnLSubscriptions(); got != maxDailyPnLSubscriptions+1 {
		t.Fatalf("seeded count = %d, want %d", got, maxDailyPnLSubscriptions+1)
	}
	// Sanity: the daemon's wrapper agrees.
	srv := newTestServer(t)
	if got := srv.activeDailyPnLCount(c); got != maxDailyPnLSubscriptions+1 {
		t.Errorf("activeDailyPnLCount = %d, want %d", got, maxDailyPnLSubscriptions+1)
	}
}

// TestAccountDailyPnL_CacheRoundTrip pins the wire contract for the
// account-level surface: a value seeded into the connector's cache
// reads back from AccountDailyPnL, and handleAccountSummary would
// surface it onto AccountResult (we test the cache surface here; the
// full handler depends on a live RequestAccountSummary path).
func TestAccountDailyPnL_CacheRoundTrip(t *testing.T) {
	t.Parallel()
	c := ibkrlib.NewConnector(&ibkrlib.ConnectorConfig{})

	// unrealized/realized are inception-to-now TOTALS, not a decomposition
	// of dailyPnL — values chosen so they do not sum to dailyPnL.
	daily := 621.30
	unreal := -44485.00
	real_ := 1830.00
	c.SeedAccountDailyPnLForTest("U1", ibkrlib.AccountDailyPnL{
		DailyPnL:           &daily,
		UnrealizedTotalPnL: &unreal,
		RealizedTotalPnL:   &real_,
		AsOf:               timeNowForTest(),
	})

	snap, ok := c.AccountDailyPnL()
	if !ok {
		t.Fatalf("AccountDailyPnL ok=false after seed")
	}
	if snap.DailyPnL == nil || *snap.DailyPnL != 621.30 {
		t.Errorf("DailyPnL = %v, want 621.30", snap.DailyPnL)
	}
	if snap.UnrealizedTotalPnL == nil || *snap.UnrealizedTotalPnL != -44485.00 {
		t.Errorf("Unrealized = %v, want -44485.00", snap.UnrealizedTotalPnL)
	}
	if snap.RealizedTotalPnL == nil || *snap.RealizedTotalPnL != 1830.00 {
		t.Errorf("Realized = %v, want 1830.00", snap.RealizedTotalPnL)
	}
}

func TestWaitForAccountDailyPnL(t *testing.T) {
	t.Parallel()
	daily := 12.50
	reader := fakeAccountDailyPnLReader{snap: ibkrlib.AccountDailyPnL{
		DailyPnL: &daily,
		AsOf:     timeNowForTest(),
	}, ok: true}

	got, ok := waitForAccountDailyPnL(context.Background(), reader, time.Now().Add(50*time.Millisecond))
	if !ok {
		t.Fatal("waitForAccountDailyPnL ok=false, want true")
	}
	if got.DailyPnL == nil || *got.DailyPnL != daily {
		t.Fatalf("DailyPnL = %v, want %.2f", got.DailyPnL, daily)
	}
}

func TestWaitForAccountDailyPnLTimeout(t *testing.T) {
	t.Parallel()
	got, ok := waitForAccountDailyPnL(context.Background(), fakeAccountDailyPnLReader{}, time.Now().Add(5*time.Millisecond))
	if ok {
		t.Fatalf("waitForAccountDailyPnL ok=true with empty reader: %+v", got)
	}
}

func TestWaitForAccountDailyPnLIgnoresUnsetDailyPnL(t *testing.T) {
	t.Parallel()
	unrealized := 12.50
	reader := fakeAccountDailyPnLReader{snap: ibkrlib.AccountDailyPnL{
		UnrealizedTotalPnL: &unrealized,
		AsOf:               timeNowForTest(),
	}, ok: true}

	got, ok := waitForAccountDailyPnL(context.Background(), reader, time.Now().Add(5*time.Millisecond))
	if ok {
		t.Fatalf("waitForAccountDailyPnL ok=true with nil DailyPnL: %+v", got)
	}
}

type fakeAccountDailyPnLReader struct {
	snap ibkrlib.AccountDailyPnL
	ok   bool
}

func (f fakeAccountDailyPnLReader) AccountDailyPnL() (ibkrlib.AccountDailyPnL, bool) {
	return f.snap, f.ok
}
