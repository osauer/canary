package rpc

import "slices"

// The cash sweep (internal-docs/design/cash-sweep.md) puts idle cash to work
// in same-currency bills and never converts. It is the first bucket whose
// rows buy; every other bucket stays close-or-reduce only. Phase A ships it
// in shadow: every row carries instrument_support_required until a paper
// account proves bill contracts, quotes and orders per instrument.
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

	// CashSweepQuantityFace, CashSweepQuantityCash and
	// CashSweepQuantityPosition name what a sweep row's quantity counts: face
	// value in whole units of the row's currency (a bill), a cash amount in
	// whole units of the row's currency (an ETF buy, sized to shares at a
	// fresh quote once instrument support exists), or the held position's own
	// unit (a redemption).
	CashSweepQuantityFace     = "face_value"
	CashSweepQuantityCash     = "cash_amount"
	CashSweepQuantityPosition = "position"
)

// Cash sweep states, one per currency. The first three are the band verdict;
// no_instrument is a currency without a declared instrument; the last four
// are the unknown posture, in the order they are checked, and generate
// nothing.
const (
	CashSweepStateInvest                  = "invest"
	CashSweepStateRedeem                  = "redeem"
	CashSweepStateHold                    = "hold"
	CashSweepStateNoInstrument            = "no_instrument"
	CashSweepStateCashUnavailable         = "cash_unavailable"
	CashSweepStateSettlementUnknown       = "settlement_unknown"
	CashSweepStateEquivalentsUnclassified = "equivalents_unclassified"
	CashSweepStateNeedsYourNumber         = "needs_your_number"
)

// Cash sweep blocker codes a row can carry.
const (
	// CashSweepBlockerInstrumentSupport is on every Phase A row: Canary cannot
	// yet resolve, quote or order the instrument.
	CashSweepBlockerInstrumentSupport = "instrument_support_required"
	// CashSweepBlockerTaxReview is on every active row while the policy has
	// no tax_reviewed_at.
	CashSweepBlockerTaxReview = "tax_review_required"
	// CashSweepBlockerFreshQuote is on a redemption whose position mark is
	// stale.
	CashSweepBlockerFreshQuote = "fresh_bill_quote_required"
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
	// TaxReviewedAt is the date the owner recorded; empty means active rows
	// carry tax_review_required.
	TaxReviewedAt string `json:"tax_reviewed_at,omitempty"`
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
	ExchangeRate       *float64 `json:"exchange_rate,omitempty"`
	TradeDateCash      *float64 `json:"trade_date_cash,omitempty"`
	SettledCash        *float64 `json:"settled_cash,omitempty"`
	Cash               *float64 `json:"cash,omitempty"`
	Committed          *float64 `json:"committed,omitempty"`
	PendingRedemptions *float64 `json:"pending_redemptions,omitempty"`
	Free               *float64 `json:"free,omitempty"`
	// CashEquivalents is the broker market value of classified cash
	// equivalents in this currency; nil while a holding cannot be classified.
	CashEquivalents *float64                     `json:"cash_equivalents,omitempty"`
	Rungs           []TradeProposalCashSweepRung `json:"rungs,omitempty"`
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
	// QuantityUnit says what the proposal quantity counts (CashSweepQuantity*).
	QuantityUnit string `json:"quantity_unit"`
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
		c.Rungs = slices.Clone(c.Rungs)
	}
	return &out
}

// CloneProposalCashSweep copies a row's sweep block; nil stays nil.
func CloneProposalCashSweep(in *TradeProposalCashSweep) *TradeProposalCashSweep {
	if in == nil {
		return nil
	}
	out := *in
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
