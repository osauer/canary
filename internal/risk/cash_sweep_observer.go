package risk

import (
	"math"
	"slices"
	"time"
)

// CashSweepDeliverable is exact, scoped exercise evidence, never inferred
// from an option's multiplier. This increment supports simple share delivery;
// adjusted multi-asset and cash-settled contracts remain explicitly unknown.
type CashSweepDeliverable struct {
	ConID, UnderlyingConID                     int
	Currency, Source                           string
	Right, Expiry                              string
	Strike                                     float64
	Multiplier                                 int
	AsOf, ValidUntil, EarliestSettlement       time.Time
	SharesPerContract, ExerciseCashPerContract float64
}

// CashSweepFundingPosition retains original quote clocks and exact identity.
// SettledAvailableShares excludes unsettled, pledged and already reserved stock.
type CashSweepFundingPosition struct {
	ConID                            int
	Symbol, Currency, SecType, Right string
	Expiry                           string
	Quantity, Strike                 float64
	Multiplier                       int
	Price                            *float64
	PriceAt                          time.Time
	SettledAvailableShares           *float64
	Deliverable                      *CashSweepDeliverable
}

// CashSweepOperationalInput is a frozen consumption of fenced broker reads.
// Source is live_partial or frozen_synthetic; no label grants trading authority.
type CashSweepOperationalInput struct {
	AsOf, AccountReceiptAt, PositionsReceiptAt time.Time
	Source, PolicyFingerprint, ScopeReason     string
	Positions                                  []CashSweepFundingPosition
	Currencies                                 []string
	TakeoverGapPct                             float64
}

// CashSweepFundingObligation describes gross principal before commitments.
// IndicativePrincipal is context only, never an admitted funding amount.
type CashSweepFundingObligation struct {
	ConID                 int       `json:"con_id"`
	Currency              string    `json:"currency"`
	Kind                  string    `json:"kind"`
	GrossPrincipal        *float64  `json:"gross_principal,omitempty"`
	IndicativePrincipal   *float64  `json:"indicative_principal,omitempty"`
	CoveredShares         *float64  `json:"covered_shares,omitempty"`
	EarliestSettlement    time.Time `json:"earliest_settlement,omitzero"`
	QuoteOriginalAt       time.Time `json:"quote_original_at,omitzero"`
	DeliverableOriginalAt time.Time `json:"deliverable_original_at,omitzero"`
	Gaps                  []string  `json:"gaps,omitempty"`
}

// CashSweepOperationalCurrency distinguishes known components from a complete
// gross obligation. Neither is net funding or permission to spend cash.
type CashSweepOperationalCurrency struct {
	Currency             string   `json:"currency"`
	KnownGrossComponents float64  `json:"known_gross_components"`
	GrossPrincipal       *float64 `json:"gross_principal,omitempty"`
	Gaps                 []string `json:"gaps,omitempty"`
}

// CashSweepOperationalObservation is partial operational evidence. It cannot
// create CashSweepFundingEvidence or NAVFloorPassed/MarginPassed.
type CashSweepOperationalObservation struct {
	AsOf               time.Time                      `json:"as_of"`
	AccountReceiptAt   time.Time                      `json:"account_receipt_at,omitzero"`
	PositionsReceiptAt time.Time                      `json:"positions_receipt_at,omitzero"`
	Source             string                         `json:"source"`
	State              string                         `json:"state"`
	PolicyFingerprint  string                         `json:"policy_fingerprint"`
	Gaps               []string                       `json:"gaps,omitempty"`
	Currencies         []CashSweepOperationalCurrency `json:"currencies"`
	Obligations        []CashSweepFundingObligation   `json:"obligations"`
}

func sweepFinite(v float64) bool      { return !math.IsNaN(v) && !math.IsInf(v, 0) }
func sweepNonnegative(v float64) bool { return sweepFinite(v) && v >= 0 }

// ObserveCashSweepOperationalFunding computes gross native obligations only.
// No exercise of a protective long option, sale proceeds or FX is credited.
func ObserveCashSweepOperationalFunding(in CashSweepOperationalInput) CashSweepOperationalObservation {
	out := CashSweepOperationalObservation{AsOf: in.AsOf, AccountReceiptAt: in.AccountReceiptAt, PositionsReceiptAt: in.PositionsReceiptAt,
		Source: in.Source, State: "partial", PolicyFingerprint: in.PolicyFingerprint,
		Gaps: []string{"finite_portfolio_stress_uncommissioned", "stressed_margin_unavailable", "exit_horizon_uncommissioned", "commitment_overlap_unreconciled"}}
	if in.ScopeReason != "" || in.AsOf.IsZero() || in.Source != "live_partial" && in.Source != "frozen_synthetic" {
		out.State = "unavailable"
		out.Gaps = append(out.Gaps, "operational_source_scope_unavailable")
		return out
	}
	ccys := map[string]*CashSweepOperationalCurrency{}
	ensure := func(ccy string) *CashSweepOperationalCurrency {
		if ccys[ccy] == nil {
			ccys[ccy] = &CashSweepOperationalCurrency{Currency: ccy}
		}
		return ccys[ccy]
	}
	for _, ccy := range in.Currencies {
		ensure(ccy)
	}
	available := map[int]float64{}
	stocks := map[int]CashSweepFundingPosition{}
	rows := slices.Clone(in.Positions)
	slices.SortStableFunc(rows, func(a, b CashSweepFundingPosition) int {
		if a.ConID < b.ConID {
			return -1
		}
		if a.ConID > b.ConID {
			return 1
		}
		return 0
	})
	seen := map[int]bool{}
	for _, p := range rows {
		if p.ConID <= 0 || seen[p.ConID] || p.Currency == "" || !sweepFinite(p.Quantity) {
			out.State = "unavailable"
			out.Gaps = append(out.Gaps, "position_identity_or_quantity_unavailable")
			return out
		}
		seen[p.ConID] = true
		ensure(p.Currency)
		if p.SecType == "STK" {
			stocks[p.ConID] = p
		}
		if p.SecType == "STK" && p.Quantity > 0 && p.SettledAvailableShares != nil && sweepNonnegative(*p.SettledAvailableShares) && *p.SettledAvailableShares <= p.Quantity {
			available[p.ConID] = *p.SettledAvailableShares
		}
	}
	for _, p := range rows {
		if p.Quantity == 0 {
			continue
		}
		c := ensure(p.Currency)
		var o CashSweepFundingObligation
		o.ConID, o.Currency, o.QuoteOriginalAt = p.ConID, p.Currency, p.PriceAt
		switch p.SecType {
		case "STK":
			if p.Quantity > 0 {
				continue
			}
			o.Kind = "short_stock_cover"
			if p.Price == nil || !sweepNonnegative(*p.Price) || *p.Price == 0 || p.PriceAt.IsZero() || p.PriceAt.After(in.AsOf) || !sweepNonnegative(in.TakeoverGapPct) {
				o.Gaps = append(o.Gaps, "cover_price_or_finite_gap_unavailable")
			} else {
				o.GrossPrincipal = new(-p.Quantity * *p.Price * (1 + in.TakeoverGapPct/100))
			}
			o.Gaps = append(o.Gaps, "borrow_recall_deadline_unavailable")
		case "OPT":
			if p.Quantity > 0 {
				o.Kind = "long_option_exercise"
				o.Gaps = append(o.Gaps, "automatic_exercise_policy_unavailable")
				break
			}
			o.Kind = "short_option_assignment"
			if p.Multiplier > 0 && sweepNonnegative(p.Strike) && p.Strike > 0 {
				o.IndicativePrincipal = new(-p.Quantity * p.Strike * float64(p.Multiplier))
			}
			d := p.Deliverable
			if d == nil || !sweepNonnegative(p.Strike) || p.Strike <= 0 || p.Multiplier <= 0 || d.ConID != p.ConID || d.Currency != p.Currency || d.Right != p.Right || d.Expiry == "" || d.Expiry != p.Expiry || d.Strike != p.Strike || d.Multiplier != p.Multiplier ||
				(d.Source != "native_authoritative" && d.Source != "frozen_synthetic") || in.Source == "live_partial" && d.Source == "frozen_synthetic" ||
				d.AsOf.IsZero() || d.AsOf.After(in.AsOf) || !d.ValidUntil.After(in.AsOf) || d.UnderlyingConID <= 0 || !sweepNonnegative(d.SharesPerContract) || d.SharesPerContract == 0 || math.Trunc(d.SharesPerContract) != d.SharesPerContract || !sweepNonnegative(d.ExerciseCashPerContract) || d.ExerciseCashPerContract <= 0 ||
				math.Abs(d.ExerciseCashPerContract-p.Strike*float64(p.Multiplier)) > 1e-6 || !sweepNonnegative(p.Strike*float64(p.Multiplier)) || math.Trunc(p.Quantity) != p.Quantity {
				o.Gaps = append(o.Gaps, "exact_option_deliverable_unavailable")
				break
			}
			o.DeliverableOriginalAt, o.EarliestSettlement = d.AsOf, d.EarliestSettlement
			if d.EarliestSettlement.IsZero() || d.EarliestSettlement.Before(in.AsOf) {
				o.Gaps = append(o.Gaps, "assignment_settlement_deadline_unavailable")
			}
			switch p.Right {
			case "P":
				o.GrossPrincipal = new(-p.Quantity * d.ExerciseCashPerContract)
			case "C":
				shares := -p.Quantity * d.SharesPerContract
				if !sweepNonnegative(shares) {
					o.Gaps = append(o.Gaps, "deliverable_share_arithmetic_invalid")
					break
				}
				stock, identified := stocks[d.UnderlyingConID]
				if identified && stock.Currency != p.Currency {
					o.Gaps = append(o.Gaps, "underlying_currency_conflict")
					break
				}
				if _, known := available[d.UnderlyingConID]; !known {
					o.Gaps = append(o.Gaps, "settled_covered_shares_unavailable")
				}
				covered := min(available[d.UnderlyingConID], shares)
				available[d.UnderlyingConID] -= covered
				o.CoveredShares = new(covered)
				if covered == shares {
					o.GrossPrincipal = new(0.)
				} else {
					if identified && stock.Price != nil && sweepNonnegative(*stock.Price) && *stock.Price > 0 && !stock.PriceAt.IsZero() && !stock.PriceAt.After(in.AsOf) && sweepNonnegative(in.TakeoverGapPct) {
						o.GrossPrincipal = new((shares - covered) * *stock.Price * (1 + in.TakeoverGapPct/100))
					} else {
						o.Gaps = append(o.Gaps, "uncovered_call_funding_unavailable")
					}
				}
			default:
				o.Gaps = append(o.Gaps, "option_right_unavailable")
			}
		case "BOND":
			if p.Quantity > 0 {
				continue
			}
			fallthrough
		default:
			o.Kind = "unsupported_funding_exposure"
			o.Gaps = append(o.Gaps, "instrument_funding_model_unavailable")
		}
		if o.GrossPrincipal != nil && !sweepNonnegative(*o.GrossPrincipal) {
			o.GrossPrincipal = nil
			o.Gaps = append(o.Gaps, "funding_arithmetic_invalid")
		}
		if o.IndicativePrincipal != nil && !sweepNonnegative(*o.IndicativePrincipal) {
			o.IndicativePrincipal = nil
		}
		if o.GrossPrincipal != nil {
			c.KnownGrossComponents += *o.GrossPrincipal
		}
		c.Gaps = append(c.Gaps, o.Gaps...)
		out.Obligations = append(out.Obligations, o)
	}
	for _, c := range ccys {
		if !sweepNonnegative(c.KnownGrossComponents) {
			c.Gaps = append(c.Gaps, "funding_arithmetic_invalid")
			c.KnownGrossComponents = 0
		}
		slices.Sort(c.Gaps)
		c.Gaps = slices.Compact(c.Gaps)
		if len(c.Gaps) == 0 {
			c.GrossPrincipal = new(c.KnownGrossComponents)
		}
		out.Currencies = append(out.Currencies, *c)
	}
	slices.SortFunc(out.Currencies, func(a, b CashSweepOperationalCurrency) int {
		if a.Currency < b.Currency {
			return -1
		}
		if a.Currency > b.Currency {
			return 1
		}
		return 0
	})
	return out
}

// CloneCashSweepOperationalObservation isolates mutable snapshot consumers.
func CloneCashSweepOperationalObservation(in *CashSweepOperationalObservation) *CashSweepOperationalObservation {
	if in == nil {
		return nil
	}
	out := *in
	out.Gaps = slices.Clone(in.Gaps)
	out.Currencies = slices.Clone(in.Currencies)
	out.Obligations = slices.Clone(in.Obligations)
	clone := func(p *float64) *float64 {
		if p == nil {
			return nil
		}
		return new(*p)
	}
	for i := range out.Currencies {
		c := &out.Currencies[i]
		c.GrossPrincipal = clone(c.GrossPrincipal)
		c.Gaps = slices.Clone(c.Gaps)
	}
	for i := range out.Obligations {
		o := &out.Obligations[i]
		o.GrossPrincipal, o.IndicativePrincipal, o.CoveredShares = clone(o.GrossPrincipal), clone(o.IndicativePrincipal), clone(o.CoveredShares)
		o.Gaps = slices.Clone(o.Gaps)
	}
	return &out
}
