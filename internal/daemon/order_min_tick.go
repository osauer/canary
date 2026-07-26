package daemon

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/osauer/canary/v2/internal/rpc"
)

// previewMinTickTimeout bounds the one-off broker contract-details fetch a
// preview pays on a cold cache. Trail rounding falls back to the static grid
// when it expires; broker WhatIf stays the fail-closed backstop.
const previewMinTickTimeout = 2 * time.Second

// resolveContractMinTick returns the venue minimum price increment for a
// contract: the cached value when known, otherwise one broker
// contract-details fetch per conID, cached for the daemon lifetime (venue
// ticks are static for practical purposes). Returns 0 when unresolved.
//
// Generation and preview must round trail prices on the same grid — the
// proposal-vs-preview drift gate compares them exactly — which is why both
// paths resolve through this single cache.
func (s *Server) resolveContractMinTick(ctx context.Context, contract rpc.ContractParams, timeout time.Duration) float64 {
	if s == nil {
		return 0
	}
	if tick := s.cachedContractMinTick(contract.ConID); tick > 0 {
		return tick
	}
	c := s.gatewayConnector()
	if c == nil || contract.Symbol == "" {
		return 0
	}
	detail, err := c.ContractDetailsFirst(ctx, *previewIBKRContract(contract), timeout)
	if err != nil || detail == nil || detail.MinTick <= 0 {
		return 0
	}
	tick := detail.MinTick
	// IBKR's contract-details MinTick is the minimum across *all* valid
	// venues (midpoint/dark pools report 0.0001 for EUR names) — finer than
	// the lit-venue trading grid. Clamp stocks to the cent floor so
	// resolution can only coarsen rounding, never emit sub-cent prices the
	// venue rejects; broker WhatIf remains the fail-closed arbiter.
	switch strings.ToUpper(strings.TrimSpace(contract.SecType)) {
	case "STK", "ETF":
		tick = max(tick, 0.01)
	}
	conID := contract.ConID
	if conID == 0 {
		conID = detail.ConID
	}
	s.storeContractMinTick(conID, tick)
	return tick
}

func (s *Server) cachedContractMinTick(conID int) float64 {
	if s == nil || conID == 0 {
		return 0
	}
	s.minTickMu.Lock()
	defer s.minTickMu.Unlock()
	return s.minTickByConID[conID]
}

// trailRedemptionGuard re-checks a trailing-stop draft against the live
// market at token redemption: a preview token lives ten minutes and the
// market can move through the initial stop in that window — redeeming then
// places a stop on the trigger side that fires immediately at the moved
// price. With no live two-sided quote (off-hours placement of a resting
// protective stop) the guard stands aside: broker WhatIf already vetted the
// draft and the exchange holds the order until the open.
func (s *Server) trailRedemptionGuard(ctx context.Context, draft rpc.OrderDraft) string {
	if draft.Trail == nil || draft.Trail.InitialStopPrice <= 0 {
		return ""
	}
	quote, err := s.fetchPreviewQuote(ctx, draft.Contract, previewMinTickTimeout)
	if err != nil {
		return ""
	}
	stop := draft.Trail.InitialStopPrice
	if strings.EqualFold(draft.Action, rpc.OrderActionSell) {
		if quote.Bid != nil && *quote.Bid > 0 && stop >= *quote.Bid {
			return fmt.Sprintf("stale trail reference: initial stop %.4f is at/above the current bid %.4f and would trigger immediately; preview again", stop, *quote.Bid)
		}
		return ""
	}
	if quote.Ask != nil && *quote.Ask > 0 && stop <= *quote.Ask {
		return fmt.Sprintf("stale trail reference: initial stop %.4f is at/below the current ask %.4f and would trigger immediately; preview again", stop, *quote.Ask)
	}
	return ""
}

func (s *Server) storeContractMinTick(conID int, tick float64) {
	if s == nil || conID == 0 || tick <= 0 {
		return
	}
	s.minTickMu.Lock()
	defer s.minTickMu.Unlock()
	if s.minTickByConID == nil {
		s.minTickByConID = map[int]float64{}
	}
	s.minTickByConID[conID] = tick
}
