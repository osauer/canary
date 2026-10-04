package rpc

import (
	"fmt"
	"math"
	"time"
)

// MethodSetupsOptions discovers standard calls without preparing an order.
const MethodSetupsOptions = "setups.options"

// SetupOptionsParams selects a listing or an exact owner-chosen call tuple.
type SetupOptionsParams struct {
	Underlying ContractParams `json:"underlying"`
	Expiry     string         `json:"expiry,omitempty"`
	Strike     *float64       `json:"strike,omitempty"`
}

// NormalizeSetupOptionsParams limits discovery to exact US stock underlyings.
func NormalizeSetupOptionsParams(p SetupOptionsParams, now time.Time) (SetupOptionsParams, error) {
	base, err := NormalizeSetupEvaluateParams(SetupEvaluateParams{Spec: SetupSpec{Revision: "option-discovery"}, Contract: p.Underlying}, now)
	if err != nil {
		return p, err
	}
	p.Underlying = base.Contract
	if p.Underlying.ConID <= 0 || p.Underlying.Exchange != "SMART" || p.Underlying.Expiry != "" || p.Underlying.Right != "" || p.Underlying.Strike != 0 {
		return p, fmt.Errorf("option discovery requires an exact SMART USD stock underlying")
	}
	if p.Expiry != "" {
		loc, err := time.LoadLocation("America/New_York")
		if err != nil {
			return p, err
		}
		d, err := time.ParseInLocation("20060102", p.Expiry, loc)
		if err != nil || d.Format("20060102") != p.Expiry || p.Expiry < now.In(loc).Format("20060102") {
			return p, fmt.Errorf("expiry must be a current or future YYYYMMDD date")
		}
	}
	if p.Strike != nil && (p.Expiry == "" || *p.Strike <= 0 || math.IsNaN(*p.Strike) || math.IsInf(*p.Strike, 0)) {
		return p, fmt.Errorf("strike requires an expiry and a finite positive value")
	}
	return p, nil
}

// SetupOptionExpiry is an observed standard-class expiry date.
type SetupOptionExpiry struct {
	Date string `json:"date"`
}

// SetupOptionCall is a listed strike and optional indicative quote evidence.
type SetupOptionCall struct {
	Strike float64   `json:"strike"`
	Bid    *float64  `json:"bid,omitempty"`
	Ask    *float64  `json:"ask,omitempty"`
	AsOf   time.Time `json:"as_of,omitzero"`
	Status string    `json:"status"`
}

// Setup option quote statuses. Listed strike rows are not_requested; only the
// exact selected call carries a quote object, which is quoted or missing.
const (
	SetupQuoteQuoted       = "quoted"
	SetupQuoteMissing      = "missing"
	SetupQuoteNotRequested = "not_requested"
)

// SetupOptionQuote is the one dated quote read for the exact selected call.
// Status is quoted only when bid and ask are both finite and positive, bid is
// at most ask, AsOf is no older than 60 seconds and DataType is a broker-labelled
// live, delayed, frozen or delayed-frozen mode. A missing quote carries no
// prices, so a zero never reads as a premium. It is display evidence for
// choosing a limit, never an order preview or order authority.
type SetupOptionQuote struct {
	Bid      *float64  `json:"bid,omitempty"`
	Ask      *float64  `json:"ask,omitempty"`
	AsOf     time.Time `json:"as_of,omitzero"`
	DataType string    `json:"data_type,omitempty"`
	Status   string    `json:"status"`
}

// SetupOptionsResult is bounded discovery evidence, never order authority.
// Quote is present only with Contract, at the exact-call stage.
type SetupOptionsResult struct {
	Version    int                 `json:"version"`
	Underlying ContractParams      `json:"underlying"`
	AsOf       time.Time           `json:"as_of"`
	Expiries   []SetupOptionExpiry `json:"expiries"`
	Expiry     string              `json:"expiry,omitempty"`
	Calls      []SetupOptionCall   `json:"calls"`
	Contract   *ContractParams     `json:"contract,omitempty"`
	Quote      *SetupOptionQuote   `json:"quote,omitempty"`
	Truncated  bool                `json:"truncated,omitempty"`
}
