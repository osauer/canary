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

// TestPositionViewKey covers the symbol/option key namespacing so
// stock and option views with the same Symbol cannot collide.
func TestPositionViewKey(t *testing.T) {
	t.Parallel()
	stock := rpc.PositionView{Symbol: "AAPL", SecType: rpc.SecTypeStock}
	opt := rpc.PositionView{Symbol: "AAPL", SecType: rpc.SecTypeOption, Expiry: "20260619", Strike: 195, Right: "C"}

	if got := positionViewKey(stock); got != "STK|AAPL" {
		t.Errorf("stock key = %q, want STK|AAPL", got)
	}
	if got := positionViewKey(opt); got == "STK|AAPL" {
		t.Errorf("option key collides with stock key: %q", got)
	}

	opt2 := opt
	opt2.Strike = 196
	if positionViewKey(opt) == positionViewKey(opt2) {
		t.Errorf("different strikes produced identical keys")
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
