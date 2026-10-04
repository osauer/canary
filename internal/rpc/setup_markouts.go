package rpc

import (
	"fmt"
	"strings"
	"time"
)

// MethodSetupsMarkouts reads scheduled, captured and missing entry markouts.
const MethodSetupsMarkouts = "setups.markouts"

// SetupMarkoutsKind labels every markout read as an entry diagnostic.
const SetupMarkoutsKind = "entry_diagnostic"

// Setup markout horizons. T30 is thirty regular-session minutes after the
// fill on the exchange calendar; NextClose is the scheduled close of the first
// regular session that opens after the fill.
const (
	SetupMarkoutHorizonT30       = "t30"
	SetupMarkoutHorizonNextClose = "next_close"
)

// Setup markout target states. Missing is terminal and never carries prices.
const (
	SetupMarkoutPending  = "pending"
	SetupMarkoutCaptured = "captured"
	SetupMarkoutMissing  = "missing"
)

// SetupMarkoutTarget is one scheduled post-fill quote capture for one broker
// execution of a Canary-placed preview-path order. It is an entry diagnostic:
// a displayed quote at a later clock, not a fill, a realized result or an
// accounting value. Quantities are in shares or contracts; prices and the
// markout are in the contract currency.
type SetupMarkoutTarget struct {
	OrderRef string `json:"order_ref"`
	ExecID   string `json:"exec_id"`
	Horizon  string `json:"horizon"`
	Status   string `json:"status"`
	// Reason names the failing condition for a missing target.
	Reason string `json:"reason,omitempty"`
	// Mode is the broker mode (paper or live) of the fill.
	Mode     string         `json:"mode,omitempty"`
	Contract ContractParams `json:"contract"`

	// Side is the execution side, BUY or SELL. A BUY fill is a long and is
	// marked at the bid; a SELL fill is a short and is marked at the ask.
	Side           string    `json:"side"`
	MarkSide       string    `json:"mark_side"`
	FillQuantity   float64   `json:"fill_quantity"`
	FillPrice      float64   `json:"fill_price"`
	FillAt         time.Time `json:"fill_at"`
	FillTimeSource string    `json:"fill_time_source"`
	// Commission is the execution commission when the journal records it.
	Commission       *float64 `json:"commission,omitempty"`
	CommissionStatus string   `json:"commission_status"`

	Calendar    string    `json:"calendar,omitempty"`
	TargetAt    time.Time `json:"target_at,omitzero"`
	ScheduledAt time.Time `json:"scheduled_at"`
	ResolvedAt  time.Time `json:"resolved_at,omitzero"`

	ReadStartedAt time.Time `json:"read_started_at,omitzero"`
	ReadAt        time.Time `json:"read_at,omitzero"`
	QuoteAsOf     time.Time `json:"quote_as_of,omitzero"`
	DataType      string    `json:"data_type,omitempty"`
	Bid           *float64  `json:"bid,omitempty"`
	Ask           *float64  `json:"ask,omitempty"`
	BidSize       *float64  `json:"bid_size,omitempty"`
	AskSize       *float64  `json:"ask_size,omitempty"`
	MarkPrice     *float64  `json:"mark_price,omitempty"`
	// Markout is (mark - fill price) x signed fill quantity x multiplier, with
	// BUY quantities positive and SELL quantities negative, so a positive value
	// favours the fill. Commission is not included.
	Markout  *float64 `json:"markout,omitempty"`
	Currency string   `json:"currency,omitempty"`
}

// SetupMarkoutsParams filters the markout read. Since is a YYYY-MM-DD date
// compared with the fill time at midnight America/New_York.
type SetupMarkoutsParams struct {
	OrderRef string `json:"order_ref,omitempty"`
	Since    string `json:"since,omitempty"`
	Symbol   string `json:"symbol,omitempty"`
}

// NormalizeSetupMarkoutsParams trims and validates the read filters.
func NormalizeSetupMarkoutsParams(p SetupMarkoutsParams) (SetupMarkoutsParams, error) {
	p.OrderRef = strings.TrimSpace(p.OrderRef)
	p.Since = strings.TrimSpace(p.Since)
	p.Symbol = strings.ToUpper(strings.TrimSpace(p.Symbol))
	if len(p.OrderRef) > 128 || len(p.Symbol) > 32 {
		return p, fmt.Errorf("order_ref or symbol is too long")
	}
	if _, err := SetupMarkoutsSince(p.Since); err != nil {
		return p, err
	}
	return p, nil
}

// SetupMarkoutsSince resolves Since to its New York midnight; empty is zero.
func SetupMarkoutsSince(since string) (time.Time, error) {
	if since == "" {
		return time.Time{}, nil
	}
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		return time.Time{}, err
	}
	day, err := time.ParseInLocation(time.DateOnly, since, loc)
	if err != nil || day.Format(time.DateOnly) != since {
		return time.Time{}, fmt.Errorf("since must be a YYYY-MM-DD date")
	}
	return day, nil
}

// SetupMarkoutsClock describes the capture schedule itself.
type SetupMarkoutsClock struct {
	// TrackingSince is when this daemon database began scheduling markouts;
	// earlier fills are never scheduled or backdated.
	TrackingSince time.Time `json:"tracking_since"`
	Pending       int       `json:"pending"`
	MaxPending    int       `json:"max_pending"`
	// NextTargetAt is the earliest pending target, when one exists.
	NextTargetAt       *time.Time `json:"next_target_at,omitempty"`
	CaptureLeadSeconds int        `json:"capture_lead_seconds"`
	MaxQuoteAgeSeconds int        `json:"max_quote_age_seconds"`
	RegularMinutes     int        `json:"regular_minutes"`
}

// SetupMarkoutsResult is the read-only markout ledger. Kind is always
// entry_diagnostic: these rows are not realized profit or accounting.
type SetupMarkoutsResult struct {
	Version int                  `json:"version"`
	Kind    string               `json:"kind"`
	Note    string               `json:"note"`
	AsOf    time.Time            `json:"as_of"`
	Clock   SetupMarkoutsClock   `json:"clock"`
	Targets []SetupMarkoutTarget `json:"targets"`
}
