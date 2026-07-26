package daemon

import (
	"context"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/config"
	"github.com/osauer/canary/v2/internal/rpc"
	ibkrlib "github.com/osauer/canary/v2/pkg/ibkr"
)

func TestPurgeRestorePreviewAndExecuteUnavailableBeforeEvidenceStageOrWire(t *testing.T) {
	t.Parallel()
	srv := newPurgeRestoreTestServer(t, config.Trading{Mode: config.TradingModePaper, MaxNotional: 100_000})
	seedPurgeLedgerFill(t, srv.purgeLedger, "purge-test", "leg-aapl", purgeLedgerTestStockContract(), rpc.OrderActionSell, 1, 100)
	portfolioCalls, quoteCalls, whatIfCalls, reserveCalls, brokerCalls := 0, 0, 0, 0, 0
	srv.purgeRefreshPositions = func() ([]*ibkrlib.RawPosition, error) {
		portfolioCalls++
		return nil, nil
	}
	srv.orderPreviewQuote = func(context.Context, rpc.ContractParams, time.Duration) (rpc.OrderQuoteSnapshot, error) {
		quoteCalls++
		return rpc.OrderQuoteSnapshot{}, nil
	}
	srv.orderPreviewWhatIf = func(context.Context, rpc.OrderDraft) (rpc.OrderWhatIfResult, error) {
		whatIfCalls++
		return rpc.OrderWhatIfResult{Status: rpc.OrderWhatIfStatusAccepted, Available: true}, nil
	}
	srv.orderReserveBrokerID = func(context.Context) (int, error) {
		reserveCalls++
		return 1001, nil
	}
	srv.orderPlaceBroker = func(context.Context, *ibkrlib.Contract, *ibkrlib.RawOrder) error {
		brokerCalls++
		return nil
	}
	before, err := srv.orderJournal.LoadEvents(0)
	if err != nil {
		t.Fatalf("load journal before restore: %v", err)
	}

	preview, err := srv.previewPurgeRestore(context.Background(), rpc.PurgeRestoreParams{All: true, Scale: 1})
	if err != nil {
		t.Fatalf("previewPurgeRestore: %v", err)
	}
	assertPurgeSubmissionUnavailable(t, preview.Status, preview.Blockers)
	executed, err := srv.executePurgeRestore(context.Background(), rpc.PurgeRestoreParams{
		All: true, Scale: 1, WaitMs: 1, Origin: rpc.OrderOriginHumanTTY,
	})
	if err != nil {
		t.Fatalf("executePurgeRestore: %v", err)
	}
	assertPurgeSubmissionUnavailable(t, executed.Status, executed.Blockers)
	if portfolioCalls != 0 || quoteCalls != 0 || whatIfCalls != 0 || reserveCalls != 0 || brokerCalls != 0 {
		t.Fatalf("portfolio=%d quote=%d whatif=%d reserve=%d broker=%d, want all zero", portfolioCalls, quoteCalls, whatIfCalls, reserveCalls, brokerCalls)
	}
	after, err := srv.orderJournal.LoadEvents(0)
	if err != nil {
		t.Fatalf("load journal after restore: %v", err)
	}
	if len(after) != len(before) {
		t.Fatalf("journal events changed from %d to %d before typed refusal", len(before), len(after))
	}
}

func newPurgeRestoreTestServer(t *testing.T, trading config.Trading) *Server {
	t.Helper()
	srv := newPurgeExecuteTestServer(t)
	trading = trading.WithDefaults()
	srv.cfg.Trading = trading
	srv.orderWritesEnabled = func() bool { return true }
	return srv
}

func seedPurgeLedgerFill(t *testing.T, store *purgeLedgerStore, purgeID, legID string, contract rpc.ContractParams, action string, quantity float64, avgFillPrice float64) {
	t.Helper()
	ev := purgeLedgerFillEvent(purgeExecuteSource, "purge-"+legID, purgeID, legID, contract, action, quantity, quantity, avgFillPrice)
	ev.Endpoint = "127.0.0.1:4002"
	ev.ClientID = 31
	ev.Account = "DU1234567"
	ev.Mode = rpc.AccountModePaper
	if err := commitTestPurgeLedgerEvent(store, ev); err != nil {
		t.Fatalf("seed purge ledger %s: %v", legID, err)
	}
}

func TestRestoreScaledQuantitySnapsFloatArtifacts(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name      string
		remaining float64
		scale     float64
		wantQty   int
		wantOK    bool
	}{
		{name: "float artifact 100*0.07 snaps to 7", remaining: 100, scale: 0.07, wantQty: 7, wantOK: true},
		{name: "thirds snap to integer", remaining: 3, scale: 1.0 / 3.0, wantQty: 1, wantOK: true},
		{name: "exact half scale", remaining: 100, scale: 0.5, wantQty: 50, wantOK: true},
		{name: "genuinely fractional remains rejected", remaining: 10.5, scale: 1, wantQty: 0, wantOK: false},
		{name: "sub-share product rejected", remaining: 1, scale: 0.4, wantQty: 0, wantOK: false},
		{name: "zero rejected", remaining: 0, scale: 1, wantQty: 0, wantOK: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			qty, ok := restoreScaledQuantity(tc.remaining, tc.scale)
			if qty != tc.wantQty || ok != tc.wantOK {
				t.Fatalf("restoreScaledQuantity(%v, %v) = (%d, %v), want (%d, %v)",
					tc.remaining, tc.scale, qty, ok, tc.wantQty, tc.wantOK)
			}
		})
	}
}

func purgeLedgerTestOptionContract() rpc.ContractParams {
	return rpc.ContractParams{
		ConID:        777001,
		Symbol:       "SPY",
		SecType:      "OPT",
		Exchange:     "SMART",
		Currency:     "USD",
		LocalSymbol:  "SPY  260619C00520000",
		TradingClass: "SPY",
		Expiry:       "20260619",
		Strike:       520,
		Right:        "C",
		Multiplier:   100,
	}
}
