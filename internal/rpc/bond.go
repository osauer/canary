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
	SecType        string   `json:"sec_type,omitempty"`
	ISIN           string   `json:"isin,omitempty"`
	CUSIP          string   `json:"cusip,omitempty"`
	Issuer         string   `json:"issuer,omitempty"`
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

// OrderBondTerms is a BOND order's instrument conventions and its line's
// order grid, carried on the draft so the signed preview says what one unit
// and one price point mean. Canary previews a BOND only for a cash_sweep row
// (internal-docs/design/cash-sweep.md): the proposal engine sets the
// conventions, the preview reads the grid from the line's contract details.
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
}

// CloneOrderBondTerms copies bond terms; nil stays nil.
func CloneOrderBondTerms(in *OrderBondTerms) *OrderBondTerms {
	if in == nil {
		return nil
	}
	out := *in
	return &out
}
