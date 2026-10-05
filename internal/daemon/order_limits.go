package daemon

import (
	"context"
	"fmt"
	"math"
	"strings"
	"sync"
	"time"

	"github.com/osauer/canary/v2/internal/risk"
	"github.com/osauer/canary/v2/internal/rpc"
)

// Order limits in force (owner decision 2026-10-05 19:56 CEST). The per-order
// gates are read from the risk constitution's [order_limits] table only; the
// notional cap scales with the book as min(ceiling, max(floor, pct × NLV)).
// A missing key refuses every order preview with a blocker naming it. An NLV
// that cannot be read currently binds the floor and the summary says so.

// orderLimitsNLVMaxAge bounds how old the account reading the cap scales
// with may be; an older one is not current and the floor binds.
const orderLimitsNLVMaxAge = 15 * time.Minute

// orderLimitsNLVRefreshAge is the age past which a preview reads the account
// again before it sizes the cap.
const orderLimitsNLVRefreshAge = 5 * time.Minute

// orderLimitsNLVRefreshBudget bounds that account read.
const orderLimitsNLVRefreshBudget = 4 * time.Second

// orderLimitsNLVReading is the latest current account reading of net
// liquidation value, kept for the order cap.
type orderLimitsNLVReading struct {
	scope brokerStateScope
	base  string
	value float64
	asOf  time.Time
}

// orderLimitsNLVState guards the reading; the zero value is empty.
type orderLimitsNLVState struct {
	mu      sync.Mutex
	reading orderLimitsNLVReading
}

// recordOrderLimitsNLV keeps a successful, current, same-account account read
// as the NLV the order cap scales with.
func (s *Server) recordOrderLimitsNLV(res *rpc.AccountResult) {
	if s == nil || res == nil || !currentPortfolioAuthority(res.Authority) || res.AccountID != res.Authority.Scope.AccountID ||
		res.Authority.Fields == nil || !res.Authority.Fields.NetLiquidation || !res.Authority.Fields.BaseCurrency ||
		!positiveFinite(res.NetLiquidation) || normCcy(res.BaseCurrency) == "" {
		return
	}
	scope := s.currentBrokerStateScope()
	if !strings.EqualFold(strings.TrimSpace(scope.Account), strings.TrimSpace(res.AccountID)) {
		return
	}
	asOf := res.AsOf
	if asOf.IsZero() {
		asOf = s.nowUTC()
	}
	s.orderLimitsNLV.mu.Lock()
	defer s.orderLimitsNLV.mu.Unlock()
	if prev := s.orderLimitsNLV.reading; sameBrokerScope(prev.scope, scope) && prev.asOf.After(asOf) {
		return
	}
	s.orderLimitsNLV.reading = orderLimitsNLVReading{scope: scope, base: normCcy(res.BaseCurrency), value: res.NetLiquidation, asOf: asOf.UTC()}
}

// currentOrderLimitsNLV returns the reading when it is current for the
// selected account and base currency, else the reason it is not.
func (s *Server) currentOrderLimitsNLV(base string, now time.Time) risk.OrderLimitsNLV {
	if s == nil {
		return risk.OrderLimitsNLV{Unavailable: "net liquidation value cannot be read"}
	}
	s.orderLimitsNLV.mu.Lock()
	r := s.orderLimitsNLV.reading
	s.orderLimitsNLV.mu.Unlock()
	switch {
	case r.asOf.IsZero() || !positiveFinite(r.value):
		return risk.OrderLimitsNLV{Unavailable: "no current account reading of net liquidation value"}
	case !sameBrokerScope(r.scope, s.currentBrokerStateScope()):
		return risk.OrderLimitsNLV{Unavailable: "the last net liquidation value read belongs to another account session"}
	case base != "" && !strings.EqualFold(r.base, base):
		return risk.OrderLimitsNLV{Unavailable: fmt.Sprintf("net liquidation value was read in %s, not the base currency %s", r.base, base)}
	case now.Sub(r.asOf) > orderLimitsNLVMaxAge:
		return risk.OrderLimitsNLV{Unavailable: fmt.Sprintf("net liquidation value was last read %d minutes ago, beyond the %d minutes it counts as current",
			int(math.Round(now.Sub(r.asOf).Minutes())), int(orderLimitsNLVMaxAge.Minutes()))}
	}
	return risk.OrderLimitsNLV{Base: r.value, AsOf: r.asOf}
}

// orderLimitsFloorOverride returns the active one-shot override of the floor
// for the selected account, the latest-expiring one when several are active.
func (s *Server) orderLimitsFloorOverride(now time.Time) *risk.OrderLimitsOverride {
	if s == nil || s.riskCapital == nil {
		return nil
	}
	var best *risk.OrderLimitsOverride
	for _, o := range s.riskCapital.OverridesSnapshotForScope(s.currentBrokerStateScope()) {
		if !o.Active || o.Control != risk.OrderLimitFloorOverrideControl || !now.Before(o.ExpiresAt) {
			continue
		}
		if best == nil || o.ExpiresAt.After(best.ExpiresAt) {
			best = &risk.OrderLimitsOverride{ID: o.ID, ExpiresAt: o.ExpiresAt}
		}
	}
	return best
}

// orderLimitsInForce computes the limits one order is judged by, without any
// broker read. baseCurrency is the account base currency of the order's
// notional; empty uses the constitution's own.
func (s *Server) orderLimitsInForce(baseCurrency string) risk.OrderLimitsInForce {
	now := time.Now().UTC()
	if s != nil {
		now = s.nowUTC()
	}
	var mgr riskPolicySnapshot
	if s != nil {
		mgr = s.riskPolicies.snapshot()
	} else {
		mgr = (*riskPolicyManager)(nil).snapshot()
	}
	base := normCcy(baseCurrency)
	if mgr.policy == nil {
		why := "no risk policy is loaded"
		if msg := strings.TrimSpace(mgr.message); msg != "" {
			why += " (" + msg + ")"
		}
		return risk.EvaluateOrderLimits(nil, base, risk.OrderLimitsNLV{}, nil, why)
	}
	policyBase := normCcy(mgr.policy.Capital.BaseCurrency)
	if base == "" {
		base = policyBase
	}
	limits := risk.EvaluateOrderLimits(mgr.policy.OrderLimits, base, s.currentOrderLimitsNLV(base, now), s.orderLimitsFloorOverride(now), "")
	if limits.Complete && policyBase != "" && base != "" && policyBase != base {
		limits.Complete = false
		limits.Unavailable = fmt.Sprintf("risk-policy.toml states its amounts in %s (capital.base_currency) but the account base currency is %s", policyBase, base)
		limits.Summary = limits.Unavailable + "; every order preview is refused until they match"
	}
	return limits
}

// orderLimitsInForceForPreview reads the account again when the NLV reading
// is old, so a preview sizes the cap from a current book, then computes the
// limits. A failed read leaves the floor to bind.
func (s *Server) orderLimitsInForceForPreview(ctx context.Context, baseCurrency string) risk.OrderLimitsInForce {
	if s != nil && !s.hasOrderPreviewBrokerTestSeam() && s.orderLimitsNLVNeedsRefresh(s.nowUTC()) {
		if ctx == nil {
			ctx = context.Background()
		}
		readCtx, cancel := context.WithTimeout(ctx, orderLimitsNLVRefreshBudget)
		_, _ = s.buildAccountSummary(readCtx, true)
		cancel()
	}
	return s.orderLimitsInForce(baseCurrency)
}

func (s *Server) orderLimitsNLVNeedsRefresh(now time.Time) bool {
	s.orderLimitsNLV.mu.Lock()
	defer s.orderLimitsNLV.mu.Unlock()
	r := s.orderLimitsNLV.reading
	return r.asOf.IsZero() || now.Sub(r.asOf) > orderLimitsNLVRefreshAge || !sameBrokerScope(r.scope, s.currentBrokerStateScope())
}

// orderLimitsIncompleteError is the refusal while the limits cannot judge an
// order: a key is missing, no policy is loaded, or its currency differs.
func orderLimitsIncompleteError(l risk.OrderLimitsInForce) error {
	return fmt.Errorf("order limits are not in force: %s", strings.TrimSpace(l.Summary))
}

// orderRiskLimitBlocker types a preview the order limits refused. The code
// stays order_risk_limit; the message names the key or the cap in force.
func orderRiskLimitBlocker(l risk.OrderLimitsInForce, err error) rpc.TradingBlocker {
	b := rpc.TradingBlocker{Code: previewRiskLimitCode, Message: err.Error()}
	if !l.Complete {
		b.Action = "Write the named keys in risk-policy.toml [order_limits] with a higher policy_version (canary policy ensure --dry-run shows the migration); until then no order preview passes."
	}
	return b
}
