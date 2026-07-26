package daemon

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/config"
	"github.com/osauer/canary/v2/internal/rpc"
	ibkrlib "github.com/osauer/canary/v2/pkg/ibkr"
)

func TestPurgeExecuteSubmissionUnavailableBeforePortfolioStageOrWire(t *testing.T) {
	t.Parallel()
	srv := newPurgeExecuteTestServer(t)
	portfolioCalls, quoteCalls, reserveCalls, brokerCalls := 0, 0, 0, 0
	srv.purgeRefreshPositions = func() ([]*ibkrlib.RawPosition, error) {
		portfolioCalls++
		return []*ibkrlib.RawPosition{{
			Account:  "DU1234567",
			Contract: ibkrlib.Contract{ConID: 265598, Symbol: "AAPL", SecType: "STK", Exchange: "SMART", Currency: "USD"},
			Position: 1,
		}}, nil
	}
	srv.orderPreviewQuote = func(context.Context, rpc.ContractParams, time.Duration) (rpc.OrderQuoteSnapshot, error) {
		quoteCalls++
		return rpc.OrderQuoteSnapshot{}, nil
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
		t.Fatalf("load journal before purge: %v", err)
	}

	res, err := srv.executePurge(context.Background(), rpc.PurgeExecuteParams{
		PurgeID:       "purge-test",
		All:           true,
		WaitMs:        1,
		BypassPreview: func() *bool { value := false; return &value }(),
		Origin:        rpc.OrderOriginHumanTTY,
	})
	if err != nil {
		t.Fatalf("executePurge: %v", err)
	}
	assertPurgeSubmissionUnavailable(t, res.Status, res.Blockers)
	if portfolioCalls != 0 || quoteCalls != 0 || reserveCalls != 0 || brokerCalls != 0 {
		t.Fatalf("portfolio=%d quote=%d reserve=%d broker=%d, want all zero", portfolioCalls, quoteCalls, reserveCalls, brokerCalls)
	}
	after, err := srv.orderJournal.LoadEvents(0)
	if err != nil {
		t.Fatalf("load journal after purge: %v", err)
	}
	if len(after) != len(before) {
		t.Fatalf("journal events changed from %d to %d before typed refusal", len(before), len(after))
	}
}

func TestPurgeExecuteBlockersUseBrokerWriteAuthorization(t *testing.T) {
	t.Parallel()
	srv := newPurgeExecuteTestServer(t)
	blockers := srv.purgeExecuteBlockers(rpc.TradingStatus{Mode: config.TradingModeLive,
		CanPreview: true,
		CanWrite:   false,
		Account:    "U1234567",
		Endpoint:   "127.0.0.1:4001",
		ClientID:   31,
		Blockers: []rpc.TradingBlocker{{
			Code:    "gateway_port_unpinned",
			Message: "order submission requires a pinned gateway port",
			Action:  "Set [gateway].port.",
		}},
	})
	if !hasBlocker(blockers, "gateway_port_unpinned") || !hasBlocker(blockers, purgeSubmissionUnavailableBlocker.Code) {
		t.Fatalf("blockers = %+v, want gateway and typed purge-submission blockers", blockers)
	}
	if hasBlocker(blockers, "paper_writes_only") {
		t.Fatalf("blockers = %+v, did not expect a paper-only gate", blockers)
	}
}

func assertPurgeSubmissionUnavailable(t *testing.T, status string, blockers []rpc.TradingBlocker) {
	t.Helper()
	if status != purgeExecuteStatusBlocked && status != purgeRestoreStatusBlocked {
		t.Fatalf("status=%q, want blocked", status)
	}
	if !hasBlocker(blockers, purgeSubmissionUnavailableBlocker.Code) {
		t.Fatalf("blockers=%+v, want %q", blockers, purgeSubmissionUnavailableBlocker.Code)
	}
}

func TestPurgeAggressiveLimitAlignsToObservedStockQuoteGrid(t *testing.T) {
	t.Parallel()

	bid, ask := 159.56, 159.58
	sell, err := purgeAggressiveLimit(rpc.OrderActionSell, rpc.ContractParams{Symbol: "SAP", SecType: "STK"}, rpc.OrderQuoteSnapshot{
		Bid:      &bid,
		Ask:      &ask,
		DataType: rpc.MarketDataLive,
	})
	if err != nil {
		t.Fatalf("purgeAggressiveLimit SAP sell: %v", err)
	}
	if sell != 159.52 {
		t.Fatalf("SAP sell limit = %.4f, want 159.5200", sell)
	}

	bid, ask = 47.62, 47.625
	buy, err := purgeAggressiveLimit(rpc.OrderActionBuy, rpc.ContractParams{Symbol: "MBG", SecType: "STK"}, rpc.OrderQuoteSnapshot{
		Bid:      &bid,
		Ask:      &ask,
		DataType: rpc.MarketDataLive,
	})
	if err != nil {
		t.Fatalf("purgeAggressiveLimit MBG buy: %v", err)
	}
	if buy != 47.635 {
		t.Fatalf("MBG buy limit = %.4f, want 47.6350", buy)
	}
}

func TestPurgeOptionFallbackIdentityRequiresMultiplier(t *testing.T) {
	t.Parallel()
	want := purgeLedgerTestOptionContract()
	want.ConID = 0
	mini := want
	mini.Multiplier = 10
	pos := ibkrlib.RawPosition{
		Contract: ibkrlib.Contract{
			Symbol:       mini.Symbol,
			SecType:      mini.SecType,
			Expiry:       mini.Expiry,
			Strike:       mini.Strike,
			Right:        mini.Right,
			Exchange:     mini.Exchange,
			Currency:     mini.Currency,
			LocalSymbol:  mini.LocalSymbol,
			TradingClass: mini.TradingClass,
			Multiplier:   mini.Multiplier,
		},
		Position: 1,
	}
	leg := rpc.PurgeExecuteLeg{Symbol: want.Symbol, SecType: want.SecType, Contract: want, OriginalSide: purgeOriginalSideLong}
	if purgePositionMatchesLeg(pos, leg) {
		t.Fatal("standard option leg matched mini-option position without ConID")
	}
	if qty := positionQuantityForContract([]*ibkrlib.RawPosition{&pos}, want); qty != 0 {
		t.Fatalf("positionQuantityForContract mini mismatch = %.2f, want 0", qty)
	}
	pos.Contract.Multiplier = want.Multiplier
	if !purgePositionMatchesLeg(pos, leg) {
		t.Fatal("matching option multiplier did not match without ConID")
	}
	if qty := positionQuantityForContract([]*ibkrlib.RawPosition{&pos}, want); qty != 1 {
		t.Fatalf("positionQuantityForContract matching multiplier = %.2f, want 1", qty)
	}
}

func TestPurgeLegIDIncludesOptionMultiplier(t *testing.T) {
	t.Parallel()
	standard := purgeLedgerTestOptionContract()
	standard.ConID = 0
	mini := standard
	mini.Multiplier = 10
	if purgeLegIDForContract(standard) == purgeLegIDForContract(mini) {
		t.Fatal("option leg id should include multiplier when ConID is unavailable")
	}
}

func newPurgeExecuteTestServer(t *testing.T) *Server {
	t.Helper()
	srv := newOrderPreviewTestServer(t, config.Trading{Mode: config.TradingModePaper})
	srv.orderWritesEnabled = func() bool { return true }
	authority, err := srv.orderJournal.coreStore()
	if err != nil {
		t.Fatalf("order authority: %v", err)
	}
	srv.purgeLedger = newPurgeLedgerStore(filepath.Join(t.TempDir(), "purge-ledger.json"), srv.now)
	if err := srv.purgeLedger.UseCoreStore(authority); err != nil {
		t.Fatalf("attach purge authority: %v", err)
	}
	nextID := 1000
	srv.orderReserveBrokerID = func(context.Context) (int, error) {
		nextID++
		return nextID, nil
	}
	return srv
}
