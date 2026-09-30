package rpc

import (
	"slices"
	"time"
)

// The cash sweep (internal-docs/design/cash-sweep.md) puts idle cash to work
// in same-currency bills and never converts. It is the first bucket whose
// rows buy; every other bucket stays close-or-reduce only. Phase B resolves
// and quotes the bill a row names and orders it: an active row is an
// ordinary proposal, previewed and submitted as a BOND LMT DAY order under
// every existing gate and the owner's approval.
const (
	// TradeProposalBucketCashSweep invests free cash above keep_cash into a
	// vocabulary bill of the same currency, or redeems the nearest maturity
	// when cash falls below keep_cash.
	TradeProposalBucketCashSweep = "cash_sweep"

	// CashSweepModeShadow and CashSweepModeActive are the two sweep modes.
	// Shadow is the policy default.
	CashSweepModeShadow = "shadow"
	CashSweepModeActive = "active"

	// CashSweepSideInvest and CashSweepSideRedeem name what a row does.
	CashSweepSideInvest = "invest"
	CashSweepSideRedeem = "redeem"

	// CashSweepQuantityPosition says a redemption's quantity counts the held
	// position's own unit. An invest row's quantity counts its bill's order
	// unit, named by a BondQuantityUnit* value (face_1000 or face_1).
	CashSweepQuantityPosition = "position"
)

// Cash sweep states, one per currency. The first three are the band verdict;
// no_instrument is a currency without a declared instrument; the next four
// are the unknown posture, in the order they are checked, and generate
// nothing. The last two replace invest when no bill can be named:
// universe_unavailable (no candidate list: TreasuryDirect unreachable for
// USD, no owner-listed isins elsewhere) and instrument_unresolved (no
// candidate confirmed by contract details and a quote).
const (
	CashSweepStateInvest                  = "invest"
	CashSweepStateRedeem                  = "redeem"
	CashSweepStateHold                    = "hold"
	CashSweepStateNoInstrument            = "no_instrument"
	CashSweepStateCashUnavailable         = "cash_unavailable"
	CashSweepStateSettlementUnknown       = "settlement_unknown"
	CashSweepStateEquivalentsUnclassified = "equivalents_unclassified"
	CashSweepStateNeedsYourNumber         = "needs_your_number"
	CashSweepStateUniverseUnavailable     = "universe_unavailable"
	CashSweepStateInstrumentUnresolved    = "instrument_unresolved"
)

// CashSweepTaxUnreviewedDetail is the detail line every row carries while
// the policy has no tax_reviewed_at. It is advisory (owner decision
// 2026-09-30 12:35 CEST): the owner's approval of each order is the gate.
const CashSweepTaxUnreviewedDetail = "tax treatment not yet confirmed (tax_reviewed_at unset)"

// Settled-cash sources on a currency's status: the broker's $LEDGER
// SettledCash field, else Canary's order journal.
const (
	CashSweepSettledSourceBroker  = "broker"
	CashSweepSettledSourceJournal = "journal"
)

// Where a bill candidate came from.
const (
	CashSweepBillSourceTreasuryDirect = "treasurydirect"
	CashSweepBillSourcePolicyISINs    = "policy_isins"
)

// Cash sweep blocker codes a row can carry. Each stops an order that could
// not be priced or sized, not a decision: the owner's approval of each order
// is the gate.
const (
	// CashSweepBlockerFreshQuote is on an invest row whose bill quote is not
	// a live bid or ask, and on a redemption whose position mark is stale.
	CashSweepBlockerFreshQuote = "fresh_bill_quote_required"
	// CashSweepBlockerBillRules is on a redemption whose held bill's contract
	// details carry no minimum size or minimum tick.
	CashSweepBlockerBillRules = "bill_contract_rules_unavailable"
	// CashSweepBlockerBelowMinimum is on a redemption whose sale cannot meet
	// the bill's minimum size or size step within the position.
	CashSweepBlockerBelowMinimum = "below_minimum_increment"
	// CashSweepBlockerBillUnitMismatch is on an invest row whose last preview's
	// broker WhatIf initial-margin change, divided by the order's expected
	// value at the assumed quantity unit, fell outside [0.005, 1.2]
	// (reviewer decisions 2026-09-30 15:25 and 15:45 CEST): the unit may be
	// wrong. It holds every submit of the currency's instrument until a
	// preview checks clean.
	CashSweepBlockerBillUnitMismatch = "bill_unit_mismatch"
)

// Bond session sources: the line's liquid or trading hours from its
// contract details, else the instrument's assumed hours
// (internal-docs/design/cash-sweep.md, A5).
const (
	BondSessionSourceLiquidHours  = "liquid_hours"
	BondSessionSourceTradingHours = "trading_hours"
	BondSessionSourceAssumed      = "assumed"
)

// TradeProposalCashSweepStatus is the sweep's account of one generation: the
// policy numbers and, per currency, the band figures and the state. Money is
// in each currency's own unit except MaxOrderNotionalBase; nil means
// unavailable, never zero. Absent from the snapshot while the bucket is not
// enabled.
type TradeProposalCashSweepStatus struct {
	Mode   string `json:"mode"`
	Shadow bool   `json:"shadow"`
	// Reason explains an account-level gap (no current currency ledger);
	// each currency then reads cash_unavailable.
	Reason string `json:"reason,omitempty"`
	// BaseCurrency is the account base; MaxOrderNotionalBase is the order cap
	// in it, nil until the owner writes max_order_notional.
	BaseCurrency         string   `json:"base_currency,omitempty"`
	MaxOrderNotionalBase *float64 `json:"max_order_notional_base,omitempty"`
	// TaxReviewedAt is the date the owner recorded; TaxReviewed is false
	// while it is unset, and every row then carries an advisory detail line.
	TaxReviewedAt string `json:"tax_reviewed_at,omitempty"`
	TaxReviewed   bool   `json:"tax_reviewed"`
	// NeedsYourNumber lists the bucket-level numbers only the owner can
	// write (max_order_notional).
	NeedsYourNumber []string                         `json:"needs_your_number,omitempty"`
	Currencies      []TradeProposalCashSweepCurrency `json:"currencies"`
	// Rows counts the proposals the sweep emitted in this generation.
	Rows int `json:"rows"`
}

// TradeProposalCashSweepCurrency is one currency's band. Cash is the lower of
// trade-date and settled cash; Committed is working buy orders plus armed
// queued buys; Free is Cash − Committed − KeepCash. PendingRedemptions is
// unsettled sale proceeds of cash equivalents, which count toward keep_cash
// so a redemption is not repeated before it settles.
type TradeProposalCashSweepCurrency struct {
	Currency string `json:"currency"`
	State    string `json:"state"`
	Reason   string `json:"reason,omitempty"`
	// Instruments and Fallback are the resolved declaration for the currency
	// (the owner's table, else Canary's compiled default).
	Instruments []string `json:"instruments"`
	Fallback    string   `json:"fallback,omitempty"`
	// NeedsYourNumber names the currency keys only the owner can write. The
	// EUR fallback ETF's symbol is listed here while bills still plan.
	NeedsYourNumber []string `json:"needs_your_number,omitempty"`
	KeepCash        float64  `json:"keep_cash"`
	MinTranche      float64  `json:"min_tranche"`
	MinMaturityDays int      `json:"min_maturity_days"`
	MaxMaturityDays int      `json:"max_maturity_days"`
	LadderRungs     int      `json:"ladder_rungs"`
	// ExchangeRate is the ledger rate (base units per unit of this currency)
	// the order cap is converted at.
	ExchangeRate  *float64 `json:"exchange_rate,omitempty"`
	TradeDateCash *float64 `json:"trade_date_cash,omitempty"`
	SettledCash   *float64 `json:"settled_cash,omitempty"`
	// SettledCashSource is broker (the ledger's SettledCash) or journal
	// (derived from Canary's order journal); empty while settled cash is
	// unknown.
	SettledCashSource  string   `json:"settled_cash_source,omitempty"`
	Cash               *float64 `json:"cash,omitempty"`
	Committed          *float64 `json:"committed,omitempty"`
	PendingRedemptions *float64 `json:"pending_redemptions,omitempty"`
	Free               *float64 `json:"free,omitempty"`
	// CashEquivalents is the broker market value of classified cash
	// equivalents in this currency; nil while a holding cannot be classified.
	CashEquivalents *float64 `json:"cash_equivalents,omitempty"`
	// CashLike is Cash plus CashEquivalents, present only when both are.
	CashLike *float64                     `json:"cash_like,omitempty"`
	Rungs    []TradeProposalCashSweepRung `json:"rungs,omitempty"`
	// Bill is the bill an invest row names, confirmed by contract details
	// and a quote. Evidence lists what each candidate check found when no
	// bill could be named (universe_unavailable, instrument_unresolved).
	Bill     *TradeProposalCashSweepBill `json:"bill,omitempty"`
	Evidence []string                    `json:"evidence,omitempty"`
}

// TradeProposalCashSweepBill is a resolved bill: its identifiers, maturity
// and the quote it was confirmed with. Price is per 100 of face (the ask,
// else the last, else the bid, else the close; PriceSource says which).
// QuantityUnit and PriceConvention are Canary's assumed broker conventions
// for the instrument (A5); the order sizes and prices by them. MinSize,
// SizeIncrement (order units) and MinTick come from the line's contract
// details and bound every order; Session is when a DAY order can fill.
type TradeProposalCashSweepBill struct {
	Instrument string `json:"instrument"`
	Source     string `json:"source"`
	ConID      int    `json:"con_id"`
	// SecType is the IBKR security type the bill resolved as (BILL or
	// BOND); the row's order carries it.
	SecType         string       `json:"sec_type,omitempty"`
	Symbol          string       `json:"symbol,omitempty"`
	ISIN            string       `json:"isin,omitempty"`
	CUSIP           string       `json:"cusip,omitempty"`
	Maturity        string       `json:"maturity"`
	DaysToMaturity  int          `json:"days_to_maturity"`
	MinSize         *float64     `json:"min_size,omitempty"`
	SizeIncrement   *float64     `json:"size_increment,omitempty"`
	MinTick         *float64     `json:"min_tick,omitempty"`
	Price           *float64     `json:"price,omitempty"`
	PriceSource     string       `json:"price_source,omitempty"`
	Quote           *BondQuote   `json:"quote,omitempty"`
	QuoteFresh      bool         `json:"quote_fresh"`
	QuantityUnit    string       `json:"quantity_unit"`
	PriceConvention string       `json:"price_convention"`
	Session         *BondSession `json:"session,omitempty"`
}

// CloneCashSweepBill deep-copies a resolved bill; nil stays nil.
func CloneCashSweepBill(in *TradeProposalCashSweepBill) *TradeProposalCashSweepBill {
	if in == nil {
		return nil
	}
	out := *in
	out.MinSize = cloneCashSweepFloat(in.MinSize)
	out.SizeIncrement = cloneCashSweepFloat(in.SizeIncrement)
	out.MinTick = cloneCashSweepFloat(in.MinTick)
	out.Price = cloneCashSweepFloat(in.Price)
	out.Quote = CloneBondQuote(in.Quote)
	out.Session = CloneBondSession(in.Session)
	return &out
}

// BondSession is when a bond line's DAY order can fill: windows in UTC,
// open inclusive and close exclusive, from the line's liquid or trading
// hours, else the instrument's assumed hours on weekdays (holidays are then
// not modelled). Label names the session for a reader.
type BondSession struct {
	Source   string              `json:"source"`
	Label    string              `json:"label,omitempty"`
	TimeZone string              `json:"time_zone,omitempty"`
	Windows  []BondSessionWindow `json:"windows,omitempty"`
}

// BondSessionWindow is one trading interval of a bond session.
type BondSessionWindow struct {
	Open  time.Time `json:"open"`
	Close time.Time `json:"close"`
}

// CloneBondSession deep-copies a bond session; nil stays nil.
func CloneBondSession(in *BondSession) *BondSession {
	if in == nil {
		return nil
	}
	out := *in
	out.Windows = slices.Clone(in.Windows)
	return &out
}

// TradeProposalCashSweepRung is one ladder rung: its target maturity in days
// and the face value of classified bills held on it.
type TradeProposalCashSweepRung struct {
	Rung       int     `json:"rung"`
	TargetDays int     `json:"target_days"`
	FaceValue  float64 `json:"face_value"`
}

// TradeProposalCashSweep is one sweep row's arithmetic, in the row's
// currency. An invest row names the rung and the maturity window the bill
// must fall in; a redemption names the gap it covers and the maturity it
// sells.
type TradeProposalCashSweep struct {
	Mode       string `json:"mode"`
	Side       string `json:"side"`
	Currency   string `json:"currency"`
	Instrument string `json:"instrument"`
	// QuantityUnit says what the proposal quantity counts: the bill's order
	// unit on an invest row (BondQuantityUnit*), the held position's unit on
	// a redemption (CashSweepQuantityPosition).
	QuantityUnit string `json:"quantity_unit"`
	// FaceValue is an invest order's face value (quantity × the unit's
	// face); EstimatedCost is that face at the bill's quoted price.
	FaceValue     float64 `json:"face_value,omitempty"`
	EstimatedCost float64 `json:"estimated_cost,omitempty"`
	// OrderAmount is the cash the order puts to work (invest) or the gap it
	// covers (redeem), before rounding to whole units.
	OrderAmount float64 `json:"order_amount"`
	Cash        float64 `json:"cash"`
	Committed   float64 `json:"committed"`
	KeepCash    float64 `json:"keep_cash"`
	Free        float64 `json:"free"`
	MinTranche  float64 `json:"min_tranche"`
	// Invest: the rung the tranche goes to, its target, and the window
	// [MinMaturityDays, MaxMaturityDays] the bill must mature in.
	Rung            int `json:"rung,omitempty"`
	TargetDays      int `json:"target_days,omitempty"`
	MinMaturityDays int `json:"min_maturity_days,omitempty"`
	MaxMaturityDays int `json:"max_maturity_days,omitempty"`
	// HeldToCap marks an invest order max_order_notional held below the free
	// cash; MaxOrderNotionalBase and ExchangeRate show the conversion.
	HeldToCap            bool    `json:"held_to_cap,omitempty"`
	MaxOrderNotionalBase float64 `json:"max_order_notional_base,omitempty"`
	ExchangeRate         float64 `json:"exchange_rate,omitempty"`
	// Redeem: the maturity sold (YYYY-MM-DD; empty for the ETF).
	MaturityDate string `json:"maturity_date,omitempty"`
	// Bill is the resolved bill an invest row names.
	Bill *TradeProposalCashSweepBill `json:"bill,omitempty"`
	// Session is when the row's order can fill: the bill's session on an
	// invest row, the held bill's on a redemption; nil for the ETF, which
	// follows its exchange calendar.
	Session *BondSession `json:"session,omitempty"`
}

// CloneCashSweepStatus deep-copies a sweep status; nil stays nil.
func CloneCashSweepStatus(in *TradeProposalCashSweepStatus) *TradeProposalCashSweepStatus {
	if in == nil {
		return nil
	}
	out := *in
	out.MaxOrderNotionalBase = cloneCashSweepFloat(in.MaxOrderNotionalBase)
	out.NeedsYourNumber = slices.Clone(in.NeedsYourNumber)
	out.Currencies = slices.Clone(in.Currencies)
	for i := range out.Currencies {
		c := &out.Currencies[i]
		c.Instruments = slices.Clone(c.Instruments)
		c.NeedsYourNumber = slices.Clone(c.NeedsYourNumber)
		c.ExchangeRate = cloneCashSweepFloat(c.ExchangeRate)
		c.TradeDateCash = cloneCashSweepFloat(c.TradeDateCash)
		c.SettledCash = cloneCashSweepFloat(c.SettledCash)
		c.Cash = cloneCashSweepFloat(c.Cash)
		c.Committed = cloneCashSweepFloat(c.Committed)
		c.PendingRedemptions = cloneCashSweepFloat(c.PendingRedemptions)
		c.Free = cloneCashSweepFloat(c.Free)
		c.CashEquivalents = cloneCashSweepFloat(c.CashEquivalents)
		c.CashLike = cloneCashSweepFloat(c.CashLike)
		c.Rungs = slices.Clone(c.Rungs)
		c.Bill = CloneCashSweepBill(c.Bill)
		c.Evidence = slices.Clone(c.Evidence)
	}
	return &out
}

// CloneProposalCashSweep copies a row's sweep block; nil stays nil.
func CloneProposalCashSweep(in *TradeProposalCashSweep) *TradeProposalCashSweep {
	if in == nil {
		return nil
	}
	out := *in
	out.Bill = CloneCashSweepBill(in.Bill)
	out.Session = CloneBondSession(in.Session)
	return &out
}

func cloneCashSweepFloat(in *float64) *float64 {
	if in == nil {
		return nil
	}
	return new(*in)
}

// BriefCashRow is the Ready movement's cash-like row: cash and classified cash
// equivalents per currency, present while the cash sweep is enabled. Nil
// figures are unavailable, never zero.
type BriefCashRow struct {
	BriefRowState
	Currencies []BriefCashCurrency `json:"currencies"`
}

// BriefCashCurrency is one currency's cash-like figures in its own unit.
// CashLike is the sum, nil when either part is unavailable.
type BriefCashCurrency struct {
	Currency        string   `json:"currency"`
	Cash            *float64 `json:"cash,omitempty"`
	CashEquivalents *float64 `json:"cash_equivalents,omitempty"`
	CashLike        *float64 `json:"cash_like,omitempty"`
}

// BriefReadyRowCash is the Ready row key of the cash-like row.
const BriefReadyRowCash = "cash"

// CloneBriefCashRow deep-copies the brief's cash row; nil stays nil.
func CloneBriefCashRow(in *BriefCashRow) *BriefCashRow {
	if in == nil {
		return nil
	}
	out := *in
	out.Currencies = slices.Clone(in.Currencies)
	for i := range out.Currencies {
		c := &out.Currencies[i]
		c.Cash = cloneCashSweepFloat(c.Cash)
		c.CashEquivalents = cloneCashSweepFloat(c.CashEquivalents)
		c.CashLike = cloneCashSweepFloat(c.CashLike)
	}
	return &out
}
