package rpc

import "time"

// MethodMarketBond resolves one bond or bill by ISIN or CUSIP and reads one
// quote for it. It is a read: contract details and a short-lived quote under
// the daemon's market-data budget, never an order.
const MethodMarketBond = "market.bond"

// Bond classes. A bill is a zero-coupon government security with at most
// 397 days from issue to maturity; every other bond line is a bond.
// Unresolved means Canary could not read the contract's details.
const (
	BondClassBill       = "bill"
	BondClassBond       = "bond"
	BondClassUnresolved = "unresolved"
)

// Bond quantity units and price conventions: Canary's reading of how IBKR
// counts and prices each instrument (A5, internal-docs/design/cash-sweep.md).
// The cash sweep's orders size and price by them; the post-install proof
// checks them.
const (
	// BondQuantityUnitFace1000 counts one unit as 1,000 of face value.
	BondQuantityUnitFace1000 = "face_1000"
	// BondQuantityUnitFace1 counts one unit as 1 of face value.
	BondQuantityUnitFace1 = "face_1"
	// BondQuantityUnitShares counts ETF shares.
	BondQuantityUnitShares = "shares"
	// BondPriceConventionPer100 quotes a bond as a price per 100 of face.
	BondPriceConventionPer100 = "per_100_par"
	// BondPriceConventionPerShare quotes an ETF per share.
	BondPriceConventionPerShare = "per_share"
)

// MarketBondParams names one bond by identifier. Currency is optional: an
// empty currency follows the identifier (a CUSIP or a US ISIN is USD; other
// ISINs follow their issuer country where it has one currency).
// SecType is the IBKR security type asked first, BILL or BOND (empty reads
// as BOND); an identifier of a vocabulary bill is also asked as the
// instrument's own types.
type MarketBondParams struct {
	Identifier string `json:"identifier"`
	Currency   string `json:"currency,omitempty"`
	SecType    string `json:"sec_type,omitempty"`
	TimeoutMs  int    `json:"timeout_ms,omitempty"`
}

// BondContract is a resolved bond line. Dates are YYYY-MM-DD; nil numbers
// were not sent.
type BondContract struct {
	ConID  int    `json:"con_id"`
	Symbol string `json:"symbol,omitempty"`
	// SecType is the IBKR security type the line resolved as: BILL or BOND.
	SecType string `json:"sec_type,omitempty"`
	ISIN    string `json:"isin,omitempty"`
	CUSIP   string `json:"cusip,omitempty"`
	Issuer  string `json:"issuer,omitempty"`
	// Ratings is IBKR's ratings field as sent, when it sends one; shown, never
	// read for a decision.
	Ratings string `json:"ratings,omitempty"`
	// TradingClass and MarketName are IBKR's as sent: the only fields that
	// may carry IBKR's own symbol for a bond, whose symbol field arrives
	// empty. Shown, never read for a decision.
	TradingClass   string   `json:"trading_class,omitempty"`
	MarketName     string   `json:"market_name,omitempty"`
	Class          string   `json:"class"`
	Currency       string   `json:"currency"`
	Exchange       string   `json:"exchange,omitempty"`
	Maturity       string   `json:"maturity,omitempty"`
	IssueDate      string   `json:"issue_date,omitempty"`
	DaysToMaturity *int     `json:"days_to_maturity,omitempty"`
	Coupon         *float64 `json:"coupon,omitempty"`
	MinSize        *float64 `json:"min_size,omitempty"`
	SizeIncrement  *float64 `json:"size_increment,omitempty"`
	MinTick        *float64 `json:"min_tick,omitempty"`
	// QuantityUnit and PriceConvention are Canary's assumed conventions for
	// the line (Bond* constants, A5).
	QuantityUnit    string `json:"quantity_unit,omitempty"`
	PriceConvention string `json:"price_convention"`
}

// BondQuote is one bond quote: prices per 100 of face, yields in percent as
// the broker sends them. Fresh means a live bid or ask arrived during this
// read; anything else (delayed, frozen, last or close only) is stale and
// says why.
type BondQuote struct {
	Bid             *float64  `json:"bid,omitempty"`
	Ask             *float64  `json:"ask,omitempty"`
	Last            *float64  `json:"last,omitempty"`
	Close           *float64  `json:"close,omitempty"`
	BidYield        *float64  `json:"bid_yield,omitempty"`
	AskYield        *float64  `json:"ask_yield,omitempty"`
	LastYield       *float64  `json:"last_yield,omitempty"`
	DataType        string    `json:"data_type,omitempty"`
	Fresh           bool      `json:"fresh"`
	StaleReason     string    `json:"stale_reason,omitempty"`
	PriceConvention string    `json:"price_convention"`
	AsOf            time.Time `json:"as_of"`
}

// HasPrice reports whether the quote carries any price at all.
func (q *BondQuote) HasPrice() bool {
	return q != nil && (q.Bid != nil || q.Ask != nil || q.Last != nil || q.Close != nil)
}

// MarketBondResult is a read-only resolution and quote check. Resolved says
// contract details named exactly one line; Quoted says a quote carried a
// price. Reason explains any gap; nothing is read as zero.
type MarketBondResult struct {
	Identifier     string `json:"identifier"`
	IdentifierType string `json:"identifier_type"`
	Currency       string `json:"currency"`
	// SecTypes are the IBKR security types asked, in order; SecTypesNote
	// says why more than the requested one was asked.
	SecTypes     []string      `json:"sec_types,omitempty"`
	SecTypesNote string        `json:"sec_types_note,omitempty"`
	Resolved     bool          `json:"resolved"`
	Lines        int           `json:"lines"`
	Contract     *BondContract `json:"contract,omitempty"`
	// Session exposes the same contract-hours or explicitly assumed session
	// used by bill proposal readiness. It does not establish order authority.
	Session *BondSession `json:"session,omitempty"`
	Quoted  bool         `json:"quoted"`
	Quote   *BondQuote   `json:"quote,omitempty"`
	Reason  string       `json:"reason,omitempty"`
	// Attempts are the request forms the contract lookup sent, in order,
	// and the frames IBKR answered each with; ServerVersion is the
	// negotiated version the frames were decoded for, and LookupAsOf when
	// the lookup ran (a lookup is reused for a day, a miss for ten
	// minutes).
	Attempts      []BondLookupAttempt `json:"attempts,omitempty"`
	ServerVersion int                 `json:"server_version,omitempty"`
	LookupAsOf    time.Time           `json:"lookup_as_of,omitzero"`
	AsOf          time.Time           `json:"as_of"`
}

// Bond lookup attempt outcomes.
const (
	BondAttemptLine     = "line"
	BondAttemptNoLine   = "no_line"
	BondAttemptRejected = "rejected"
	BondAttemptFailed   = "failed"
)

// BondLookupAttempt is one reqContractDetails a bond lookup sent: the form,
// the request as sent (an empty exchange is sent empty), and IBKR's answer.
// Outcome is line, no_line, rejected (Code and Message are IBKR's) or
// failed (no answer); Message otherwise says why the form found no line.
// Lines counts the lines the form answered that the lookup kept; Frames are
// the frames that named the request id, in arrival order.
type BondLookupAttempt struct {
	Form          string            `json:"form"`
	ReqID         int               `json:"req_id,omitempty"`
	SecType       string            `json:"sec_type"`
	Symbol        string            `json:"symbol,omitempty"`
	SecIDType     string            `json:"sec_id_type,omitempty"`
	SecID         string            `json:"sec_id,omitempty"`
	ConID         int               `json:"con_id,omitempty"`
	Exchange      string            `json:"exchange"`
	Currency      string            `json:"currency"`
	Outcome       string            `json:"outcome"`
	Code          int               `json:"code,omitempty"`
	Message       string            `json:"message,omitempty"`
	Lines         int               `json:"lines"`
	Frames        []BondLookupFrame `json:"frames"`
	FramesOmitted int               `json:"frames_omitted,omitempty"`
}

// BondLookupFrame is one inbound frame that named a lookup's request id,
// reduced to identifiers as sent: kind is bondContractData,
// contractData, contractDataEnd, error or other; msg_id and fields are the
// message id and raw field count; layout the layout a contract frame was
// decoded in. line says the frame became a line (complete: the whole frame
// decoded); note why it did not, or what the line lacks.
type BondLookupFrame struct {
	MsgID       int    `json:"msg_id"`
	Kind        string `json:"kind"`
	Fields      int    `json:"fields"`
	Layout      string `json:"layout,omitempty"`
	ConID       string `json:"con_id,omitempty"`
	SecType     string `json:"sec_type,omitempty"`
	Symbol      string `json:"symbol,omitempty"`
	CUSIP       string `json:"cusip,omitempty"`
	LocalSymbol string `json:"local_symbol,omitempty"`
	Exchange    string `json:"exchange,omitempty"`
	Currency    string `json:"currency,omitempty"`
	Maturity    string `json:"maturity,omitempty"`
	Code        int    `json:"code,omitempty"`
	Line        bool   `json:"line"`
	Complete    bool   `json:"complete,omitempty"`
	Note        string `json:"note,omitempty"`
}

// PositionBond classifies one held BOND row. The row itself stays in the
// positions' stocks list with its valuation, so no consumer that sums
// holdings loses it; this entry adds what a bond is: bill or bond, maturity,
// identifiers and currency.
type PositionBond struct {
	ResolutionSource string `json:"resolution_source,omitempty"`
	// Effective maturity provenance; public evidence never changes raw broker frames.
	MaturitySource     string    `json:"maturity_source,omitempty"`
	MaturitySourceAsOf time.Time `json:"maturity_source_as_of,omitzero"`
	ConID              int       `json:"con_id"`
	Symbol             string    `json:"symbol"`
	Currency           string    `json:"currency"`
	Class              string    `json:"class"`
	ISIN               string    `json:"isin,omitempty"`
	CUSIP              string    `json:"cusip,omitempty"`
	Issuer             string    `json:"issuer,omitempty"`
	Maturity           string    `json:"maturity,omitempty"`
	DaysToMaturity     *int      `json:"days_to_maturity,omitempty"`
	Coupon             *float64  `json:"coupon,omitempty"`
	Quantity           float64   `json:"quantity"`
	Mark               float64   `json:"mark"`
	MarketValue        float64   `json:"market_value_ccy"`
	// Reason says why an unresolved row could not be classified.
	Reason string `json:"reason,omitempty"`
	// Bond risk at the mark (internal-docs/design/bond-risk.md, phase 1):
	// the issuer evidence, the yield and modified duration, and in the
	// account base the market value, the value of a 0.01-point move (DV01)
	// and the loss on a one-point rise in yields. Nil while unmeasured, and
	// RiskUnmeasured then says why.
	EvidenceIssuer    string   `json:"evidence_issuer,omitempty"`
	IssuerClass       string   `json:"issuer_class,omitempty"`
	EvidenceSource    string   `json:"evidence_source,omitempty"`
	YieldPct          *float64 `json:"yield_pct,omitempty"`
	ModifiedDuration  *float64 `json:"modified_duration,omitempty"`
	MarketValueBase   *float64 `json:"market_value_base,omitempty"`
	DV01Base          *float64 `json:"dv01_base,omitempty"`
	RateShockLossBase *float64 `json:"rate_shock_loss_base,omitempty"`
	RiskUnmeasured    string   `json:"risk_unmeasured,omitempty"`
}

// CloneBondQuote deep-copies a bond quote; nil stays nil.
func CloneBondQuote(in *BondQuote) *BondQuote {
	if in == nil {
		return nil
	}
	out := *in
	for _, p := range []**float64{&out.Bid, &out.Ask, &out.Last, &out.Close, &out.BidYield, &out.AskYield, &out.LastYield} {
		*p = cloneCashSweepFloat(*p)
	}
	return &out
}

// OrderBondRequest names one bill or bond for an order: its ISIN or CUSIP
// and the face amount in the bond's currency.
type OrderBondRequest struct {
	Identifier string  `json:"identifier"`
	Face       float64 `json:"face"`
}

// OrderBondInstrumentByIdentifier is the Instrument of a bond order the
// owner named by identifier (internal-docs/design/bond-orders.md), as
// against a cash_sweep row's vocabulary bill.
const OrderBondInstrumentByIdentifier = "by_identifier"

// Issuer classes a bond buy is admitted under.
const (
	BondIssuerGovernment      = "government"
	BondIssuerInvestmentGrade = "investment_grade"
)

// OrderBondTerms is a BOND order's instrument conventions and its line's
// order grid, carried on the draft so the signed preview says what one unit
// and one price point mean. A cash_sweep row's terms come from the proposal
// engine (internal-docs/design/cash-sweep.md); a bond named by identifier
// gets its terms and issuer evidence from the daemon
// (internal-docs/design/bond-orders.md). The preview reads the grid from the
// line's contract details.
type OrderBondTerms struct {
	ResolutionSource string `json:"resolution_source,omitempty"`
	// Exact maturity and identity reviewed in the proposal, rechecked at preview.
	Maturity        string  `json:"maturity,omitempty"`
	MaturitySource  string  `json:"maturity_source,omitempty"`
	CUSIP           string  `json:"cusip,omitempty"`
	ISIN            string  `json:"isin,omitempty"`
	Instrument      string  `json:"instrument"`
	QuantityUnit    string  `json:"quantity_unit"`
	FacePerUnit     float64 `json:"face_per_unit"`
	PriceConvention string  `json:"price_convention"`
	MinTick         float64 `json:"min_tick,omitempty"`
	MinSize         float64 `json:"min_size,omitempty"`
	SizeIncrement   float64 `json:"size_increment,omitempty"`
	// FaceValue is quantity × FacePerUnit, in the contract's currency.
	FaceValue float64 `json:"face_value,omitempty"`
	// TradingCapExemptUpToBase is set only by the daemon, for a cash_sweep
	// row whose policy writes bills_exempt_from_trading_max_notional = true:
	// the bill order may pass the order cap in force ([order_limits]) up to
	// this base notional (the sweep's order cap in force), never beyond.
	// Zero exempts nothing.
	TradingCapExemptUpToBase float64 `json:"trading_cap_exempt_up_to_base,omitempty"`
	// The issuer evidence a buy named by identifier was admitted by: the
	// class (BondIssuer*), the issuer, the source and when it was read or
	// published, and the annual coupon in percent. Empty on a cash_sweep row
	// and on a sale.
	IssuerClass    string    `json:"issuer_class,omitempty"`
	Issuer         string    `json:"issuer,omitempty"`
	EvidenceSource string    `json:"evidence_source,omitempty"`
	EvidenceAsOf   time.Time `json:"evidence_as_of,omitzero"`
	Coupon         *float64  `json:"coupon,omitempty"`
	// AccruedBound bounds the accrued interest a buy pays on top of its
	// price: one year of coupon on the face, in the contract's currency. The
	// order's value for the order cap includes it.
	AccruedBound float64 `json:"accrued_bound,omitempty"`
	// A buy's rate risk at its limit price (bond-risk.md, phase 1): yield to
	// maturity in percent, modified duration in years, and the loss in the
	// contract's currency on a one-point rise in yields. Nil when the bond
	// cannot be measured (a sweep bill carries no coupon here).
	YieldPct         *float64 `json:"yield_pct,omitempty"`
	ModifiedDuration *float64 `json:"modified_duration,omitempty"`
	RateShockLoss    *float64 `json:"rate_shock_loss,omitempty"`
}

// CloneOrderBondTerms copies bond terms; nil stays nil.
func CloneOrderBondTerms(in *OrderBondTerms) *OrderBondTerms {
	if in == nil {
		return nil
	}
	out := *in
	if in.Coupon != nil {
		out.Coupon = new(*in.Coupon)
	}
	for _, p := range []**float64{&out.YieldPct, &out.ModifiedDuration, &out.RateShockLoss} {
		if *p != nil {
			*p = new(**p)
		}
	}
	return &out
}
