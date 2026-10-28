//go:build trading

package daemon

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/rpc"
	ibkrlib "github.com/osauer/canary/v2/pkg/ibkr"
)

// hedgeSendRig records every broker send of a hedge order server.
type hedgeSendRig struct {
	mu   sync.Mutex
	sent []*ibkrlib.RawOrder
}

func newHedgeSendRig(t *testing.T, s *Server) *hedgeSendRig {
	t.Helper()
	r := &hedgeSendRig{}
	s.openOrderInventoryForTest = func(context.Context, bool) (ibkrlib.OpenOrderSnapshot, brokerStateScope, error) {
		return ibkrlib.OpenOrderSnapshot{Complete: true, AsOf: s.orderNow()}, brokerStateScope{Account: "DU1234567", Mode: rpc.AccountModePaper}, nil
	}
	s.reduceFillSnapshotForTest = func(ctx context.Context, _ time.Duration) ([]*ibkrlib.RawPosition, error) {
		pos, err := s.reducePositionsForTest(ctx)
		if err != nil {
			return nil, err
		}
		var rows []*ibkrlib.RawPosition
		for _, positions := range [][]rpc.PositionView{pos.Stocks, pos.Options} {
			for _, row := range positions {
				rows = append(rows, &ibkrlib.RawPosition{Account: s.currentBrokerStateScope().Account, Contract: ibkrlib.Contract{ConID: row.ConID, Symbol: row.Symbol, SecType: row.SecType, Currency: row.Currency, Multiplier: row.Multiplier}, Position: row.Quantity})
			}
		}
		return rows, nil
	}
	next := 3000
	s.orderReserveBrokerID = func(context.Context) (int, error) { next++; return next, nil }
	s.orderPlaceBroker = func(_ context.Context, _ *ibkrlib.Contract, order *ibkrlib.RawOrder) error {
		r.mu.Lock()
		defer r.mu.Unlock()
		copied := *order
		r.sent = append(r.sent, &copied)
		return nil
	}
	return r
}

func (r *hedgeSendRig) sends() []*ibkrlib.RawOrder {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]*ibkrlib.RawOrder(nil), r.sent...)
}

// The hedge leg's token goes through the existing basket submit and places
// exactly the reviewed terms: SELL 5 at the bid, a plain limit, good for
// the day, in regular hours, with no stop beside it.
func TestIndexHedgeTokenPlacesTheReviewedSale(t *testing.T) {
	s := hedgeOrderServer(t, 25, mandateReductionStock("SPY", 9101, 3000, 50))
	r := newHedgeSendRig(t, s)
	preview := hedgePlan(t, s, rpc.TradeProposalReducePortfolioParams{})
	if len(preview.Legs) != 1 || preview.Legs[0].PreviewToken == "" {
		t.Fatalf("preview = %+v, want a hedge token", preview.Legs)
	}
	res, err := s.reducePortfolioSubmit(t.Context(), rpc.TradeProposalReducePortfolioParams{Mode: rpc.ReduceModeHedge, PreviewTokens: []string{preview.Legs[0].PreviewToken}, RequestRef: "hedge-1"})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Blockers) > 0 || len(res.Legs) != 1 || !res.Legs[0].Placed || len(res.Legs[0].Blockers) > 0 {
		t.Fatalf("submit = %+v legs %+v, want the hedge placed", res.Blockers, res.Legs)
	}
	sent := r.sends()
	if len(sent) != 1 {
		t.Fatalf("sends = %d, want one", len(sent))
	}
	o := sent[0]
	if o.Action != rpc.OrderActionSell || o.TotalQty != 5 || o.OrderType != rpc.OrderTypeLMT || o.LmtPrice != 5999.75 || o.TIF != rpc.OrderTIFDay || o.OutsideRth || o.AdaptivePriority != "" || o.OcaGroup != "" {
		t.Fatalf("broker order = %+v, want SELL 5 LMT 5999.75 DAY in regular hours", o)
	}
}

// The hours and the bid are read again at submit: a session that closed
// since the review, or a bid that fell below the reviewed limit, sends
// nothing.
func TestIndexHedgeTokenRefusedAtSubmit(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(*Server)
		code string
	}{
		{"session closed", hedgeClosedSession, "outside_regular_hours"},
		{"bid below the limit", func(s *Server) {
			s.orderPreviewQuote = func(_ context.Context, c rpc.ContractParams, _ time.Duration) (rpc.OrderQuoteSnapshot, error) {
				bid, ask := 5990.0, 5990.5
				return rpc.OrderQuoteSnapshot{Symbol: c.Symbol, Bid: &bid, Ask: &ask, DataType: rpc.MarketDataLive, QuoteQuality: "firm"}, nil
			}
		}, "beyond_worst_price"},
	} {
		s := hedgeOrderServer(t, 25, mandateReductionStock("SPY", 9101, 3000, 50))
		r := newHedgeSendRig(t, s)
		preview := hedgePlan(t, s, rpc.TradeProposalReducePortfolioParams{})
		tc.edit(s)
		res, err := s.reducePortfolioSubmit(t.Context(), rpc.TradeProposalReducePortfolioParams{Mode: rpc.ReduceModeHedge, PreviewTokens: []string{preview.Legs[0].PreviewToken}})
		if err != nil {
			t.Fatal(err)
		}
		if len(res.Legs) != 1 || res.Legs[0].Placed || firstBlockerCodeOf(res.Legs[0].Blockers) != tc.code || len(r.sends()) != 0 {
			t.Fatalf("%s: legs %+v sends %d, want %s and nothing sent", tc.name, res.Legs, len(r.sends()), tc.code)
		}
	}
}
