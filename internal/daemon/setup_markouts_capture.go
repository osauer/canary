package daemon

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"math"
	"slices"
	"time"

	"github.com/osauer/canary/v2/internal/rpc"
)

const (
	// setupMarkoutQuoteBudget is the read budget. The read starts this long
	// before the target so the accepted quote is at or before the target.
	setupMarkoutQuoteBudget = 5 * time.Second
	// setupMarkoutMaxQuoteAge is the oldest quote, measured at the target, a
	// capture may use.
	setupMarkoutMaxQuoteAge = 60 * time.Second
	// setupMarkoutMaxReads bounds concurrent market-data lines; a target that
	// cannot get a line before its clock is recorded missing.
	setupMarkoutMaxReads = 4
)

var errSetupMarkoutBrokerUnavailable = errors.New("broker session unavailable")

// setupMarkoutQuote is one exact-contract read. AsOf is the older of the two
// side receipts on the request-owned subscription and is zero when the feed
// does not establish source freshness (frozen, delayed or cached sides).
type setupMarkoutQuote struct {
	Bid, Ask         *float64
	BidSize, AskSize *float64
	AsOf             time.Time
	DataType         string
	ReadStartedAt    time.Time
	ReadAt           time.Time
}

// applySetupMarkoutRule resolves one pending target against one read. The
// quote must be live, two-sided, finite, positive and non-crossed, at or
// before the target and no older than 60 seconds there, with displayed size
// on the marking side covering the fill quantity. Otherwise the target is
// missing with the first failing condition and carries no prices.
func applySetupMarkoutRule(t rpc.SetupMarkoutTarget, q setupMarkoutQuote, now time.Time) rpc.SetupMarkoutTarget {
	t.ReadStartedAt, t.ReadAt, t.DataType = q.ReadStartedAt.UTC(), q.ReadAt.UTC(), q.DataType
	if !q.AsOf.IsZero() {
		t.QuoteAsOf = q.AsOf.UTC()
	}
	markSize := q.BidSize
	if t.Side == rpc.OrderActionSell {
		markSize = q.AskSize
	}
	reason := ""
	switch {
	case q.Bid == nil || q.Ask == nil:
		reason = "quote_not_two_sided"
	case !positiveFinite(*q.Bid) || !positiveFinite(*q.Ask):
		reason = "quote_not_positive_finite"
	case *q.Ask < *q.Bid:
		reason = "quote_crossed"
	case q.DataType != rpc.MarketDataLive:
		reason = "quote_not_live"
	case q.AsOf.IsZero():
		reason = "quote_time_unavailable"
	case q.AsOf.After(t.TargetAt):
		reason = "quote_after_target"
	case t.TargetAt.Sub(q.AsOf) > setupMarkoutMaxQuoteAge:
		reason = "quote_too_old"
	case markSize == nil || !positiveFinite(*markSize):
		reason = "displayed_size_unavailable"
	case *markSize < t.FillQuantity:
		reason = "displayed_size_insufficient"
	case t.Side != rpc.OrderActionBuy && t.Side != rpc.OrderActionSell:
		reason = "fill_side_unknown"
	case t.Contract.Multiplier <= 0:
		reason = "multiplier_unknown"
	}
	if reason != "" {
		return setupMarkoutMissing(t, reason, now)
	}
	mark, signed := *q.Bid, t.FillQuantity
	if t.Side == rpc.OrderActionSell {
		mark, signed = *q.Ask, -t.FillQuantity
	}
	markout := math.Round((mark-t.FillPrice)*signed*float64(t.Contract.Multiplier)*1e6) / 1e6
	t.Status, t.Reason, t.ResolvedAt = rpc.SetupMarkoutCaptured, "", now.UTC()
	t.Bid, t.Ask = new(*q.Bid), new(*q.Ask)
	t.BidSize, t.AskSize = cloneFloat64Ptr(q.BidSize), cloneFloat64Ptr(q.AskSize)
	t.MarkPrice, t.Markout = new(mark), new(markout)
	return t
}

// setupMarkoutBrokerAuthority pins the current connector session without the
// reconnect side effect of order preview: a markout read never steers the
// gateway lifecycle.
func (s *Server) setupMarkoutBrokerAuthority() (*orderPreviewBrokerAuthority, error) {
	s.mu.Lock()
	connector, epoch := s.connector, s.connectorEpoch
	s.mu.Unlock()
	if connector == nil || !connector.IsReady() {
		return nil, errSetupMarkoutBrokerUnavailable
	}
	session, ok := connector.CaptureSession()
	if !ok {
		return nil, errSetupMarkoutBrokerUnavailable
	}
	authority := &orderPreviewBrokerAuthority{connector: connector, connectorEpoch: epoch, session: session}
	if !s.orderPreviewBrokerAuthorityCurrent(authority) {
		return nil, errSetupMarkoutBrokerUnavailable
	}
	return authority, nil
}

// readSetupMarkoutQuote reads the exact contract through the same
// request-owned exact-session read used for option-exit evidence, waiting for
// both sides and their displayed sizes within budget.
func (s *Server) readSetupMarkoutQuote(ctx context.Context, contract rpc.ContractParams, budget time.Duration) (setupMarkoutQuote, error) {
	if rt := s.setupMarkouts; rt != nil && rt.quote != nil {
		return rt.quote(ctx, contract, budget)
	}
	authority, err := s.setupMarkoutBrokerAuthority()
	if err != nil {
		return setupMarkoutQuote{}, err
	}
	snap, sizes, err := s.exactSessionContractQuote(ctx, authority, contract, budget, true, true)
	if err != nil {
		if !s.orderPreviewBrokerAuthorityCurrent(authority) {
			return setupMarkoutQuote{}, errSetupMarkoutBrokerUnavailable
		}
		return setupMarkoutQuote{}, err
	}
	out := setupMarkoutQuote{Bid: snap.Bid, Ask: snap.Ask, DataType: snap.DataType, AsOf: snap.PriceAt}
	if sizes.Bid != nil {
		out.BidSize = new(float64(*sizes.Bid))
	}
	if sizes.Ask != nil {
		out.AskSize = new(float64(*sizes.Ask))
	}
	return out, nil
}

// captureDueSetupMarkouts starts one bounded read per exact contract and
// target clock whose capture window [target-budget, target] is open. Reads run
// off the worker loop; a target without a free line stays pending and is
// recorded missing once its clock passes.
func (s *Server) captureDueSetupMarkouts(ctx context.Context, now time.Time) {
	rt := s.setupMarkouts
	if rt == nil {
		return
	}
	groups := map[string][]rpc.SetupMarkoutTarget{}
	for _, t := range rt.store.pending() {
		if now.Before(t.TargetAt.Add(-setupMarkoutQuoteBudget)) || now.After(t.TargetAt) || rt.isInFlight(t) {
			continue
		}
		key := fmt.Sprintf("%d\x00%d", t.Contract.ConID, t.TargetAt.UnixNano())
		groups[key] = append(groups[key], t)
	}
	keys := make([]string, 0, len(groups))
	for key := range groups {
		keys = append(keys, key)
	}
	slices.SortFunc(keys, func(a, b string) int { return cmp.Compare(a, b) })
	for _, key := range keys {
		group := groups[key]
		select {
		case rt.reads <- struct{}{}:
		default:
			return
		}
		rt.setInFlight(group, true)
		rt.wg.Go(func() {
			defer func() { <-rt.reads }()
			defer rt.setInFlight(group, false)
			s.captureSetupMarkoutGroup(ctx, group)
		})
	}
}

func (s *Server) captureSetupMarkoutGroup(ctx context.Context, group []rpc.SetupMarkoutTarget) {
	if len(group) == 0 {
		return
	}
	started := s.setupMarkoutNow()
	readCtx, cancel := context.WithTimeout(ctx, setupMarkoutQuoteBudget)
	q, err := s.readSetupMarkoutQuote(readCtx, group[0].Contract, setupMarkoutQuoteBudget)
	cancel()
	now := s.setupMarkoutNow()
	q.ReadStartedAt, q.ReadAt = started, now
	resolved := make([]rpc.SetupMarkoutTarget, 0, len(group))
	for _, t := range group {
		if err != nil {
			reason := "quote_unavailable"
			if errors.Is(err, errSetupMarkoutBrokerUnavailable) {
				reason = "broker_unavailable"
			}
			t.ReadStartedAt, t.ReadAt = started.UTC(), now.UTC()
			resolved = append(resolved, setupMarkoutMissing(t, reason, now))
			continue
		}
		resolved = append(resolved, applySetupMarkoutRule(t, q, now))
	}
	if err := s.setupMarkouts.store.resolve(ctx, resolved); err != nil {
		s.warnf("setup markout capture: %v", err)
	}
}

func (rt *setupMarkoutRuntime) setInFlight(group []rpc.SetupMarkoutTarget, on bool) {
	rt.inFlightMu.Lock()
	defer rt.inFlightMu.Unlock()
	for _, t := range group {
		if on {
			rt.inFlight[setupMarkoutKey(t)] = true
		} else {
			delete(rt.inFlight, setupMarkoutKey(t))
		}
	}
}
