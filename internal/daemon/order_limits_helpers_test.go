package daemon

import (
	"time"

	"github.com/osauer/canary/v2/internal/config"
	"github.com/osauer/canary/v2/internal/risk"
	"github.com/osauer/canary/v2/internal/rpc"
)

// testOrderLimitsTable is a complete [order_limits] table: the given floor,
// 5% of NLV, a ceiling of at least 100,000, five option contracts, and no
// shorts or sell-to-open.
func testOrderLimitsTable(floor float64) *risk.ConstitutionOrderLimits {
	return &risk.ConstitutionOrderLimits{
		MaxOrderFloorBase: new(floor), MaxOrderPctNLV: new(5.0), MaxOrderCeilingBase: new(max(floor, 100000.0)),
		MaxOptionContracts: new(5), AllowStockShort: new(false), AllowOptionSellToOpen: new(false),
	}
}

// testOrderLimitsFromTrading translates a test's legacy [trading] gates into
// the [order_limits] table it now installs; an absent gate takes the value
// the compiled default used to supply.
func testOrderLimitsFromTrading(tr config.Trading) *risk.ConstitutionOrderLimits {
	floor := 10000.0
	if tr.MaxNotional != nil {
		floor = *tr.MaxNotional
	}
	t := testOrderLimitsTable(floor)
	if tr.MaxOptionContracts != nil {
		t.MaxOptionContracts = new(*tr.MaxOptionContracts)
	}
	if tr.AllowStockShort != nil {
		t.AllowStockShort = new(*tr.AllowStockShort)
	}
	if tr.AllowOptionSellToOpen != nil {
		t.AllowOptionSellToOpen = new(*tr.AllowOptionSellToOpen)
	}
	return t
}

// testOrderLimits evaluates a complete table without NLV, so the floor binds.
func testOrderLimits(floor float64, edit ...func(*risk.ConstitutionOrderLimits)) risk.OrderLimitsInForce {
	t := testOrderLimitsTable(floor)
	for _, e := range edit {
		e(t)
	}
	return risk.EvaluateOrderLimits(t, "", risk.OrderLimitsNLV{Unavailable: "test without NLV"}, nil, "")
}

// installTestOrderLimits makes s's risk-policy manager hold an active
// constitution with the given [order_limits] (nil: no table).
func installTestOrderLimits(s *Server, table *risk.ConstitutionOrderLimits) {
	m := newRiskPolicyManager("", time.Minute, nil)
	m.active = &risk.Constitution{Kind: risk.ConstitutionKind, SchemaVersion: 2, PolicyID: "test", PolicyVersion: 1, OrderLimits: table}
	m.status, m.source = rpc.RiskPolicyStatusActive, "file"
	s.riskPolicies = m
}

// setTestOrderLimits edits the order limits a test server holds.
func setTestOrderLimits(s *Server, edit func(*risk.ConstitutionOrderLimits)) {
	current := testOrderLimitsTable(10000)
	if snap := s.riskPolicies.snapshot(); snap.policy != nil && snap.policy.OrderLimits != nil {
		copied := *snap.policy.OrderLimits
		current = &copied
	}
	edit(current)
	if current.MaxOrderFloorBase != nil && current.MaxOrderCeilingBase != nil && *current.MaxOrderFloorBase > *current.MaxOrderCeilingBase {
		current.MaxOrderCeilingBase = new(*current.MaxOrderFloorBase)
	}
	installTestOrderLimits(s, current)
}

// withTestOrderLimits installs complete order limits (10,000 floor) on s,
// so proposal rows read ready as before the limits moved to the policy.
func withTestOrderLimits(s *Server) *Server {
	installTestOrderLimits(s, testOrderLimitsTable(10000))
	return s
}

// testOrderLimitsTOML is a complete [order_limits] table for a test policy
// file; the protective SELL stops of the trading rigs need allow_stock_short.
const testOrderLimitsTOML = `
[order_limits]
max_order_floor_base = 10000.0
max_order_pct_nlv = 5.0
max_order_ceiling_base = 100000.0
max_option_contracts = 5
allow_stock_short = true
allow_option_sell_to_open = false
`
