package rpc

import (
	"fmt"
	"math"
	"strings"
	"time"
)

// MethodSetupsEvaluate evaluates observed entry evidence without changing policy.
const MethodSetupsEvaluate = "setups.evaluate"

// SetupSpec is one fixed, versioned observation template, not a rule language.
type SetupSpec struct {
	Version          int     `json:"version"`
	Template         string  `json:"template"`
	Revision         string  `json:"revision"`
	SpikeMultiple    float64 `json:"spike_multiple"`
	ResponseBars     int     `json:"response_bars"`
	BaselineSessions int     `json:"baseline_sessions"`
}

// NormalizeSetupSpec validates the small volume-turn contract and fills defaults.
func NormalizeSetupSpec(s SetupSpec) (SetupSpec, error) {
	if s.Version == 0 {
		s.Version = 1
	}
	if s.Template == "" {
		s.Template = "volume_turn_v1"
	}
	if s.SpikeMultiple == 0 {
		s.SpikeMultiple = 3
	}
	if s.ResponseBars == 0 {
		s.ResponseBars = 2
	}
	if s.BaselineSessions == 0 {
		s.BaselineSessions = 20
	}
	if s.Version != 1 || s.Template != "volume_turn_v1" {
		return s, fmt.Errorf("unsupported setup version or template")
	}
	if strings.TrimSpace(s.Revision) == "" || len(s.Revision) > 128 {
		return s, fmt.Errorf("spec revision is required (at most 128 bytes)")
	}
	for _, r := range s.Revision {
		if r < 33 || r > 126 {
			return s, fmt.Errorf("spec revision must be a printable token")
		}
	}
	if math.IsNaN(s.SpikeMultiple) || math.IsInf(s.SpikeMultiple, 0) || s.SpikeMultiple < 1 || s.SpikeMultiple > 100 {
		return s, fmt.Errorf("spike_multiple must be finite and between 1 and 100")
	}
	if s.ResponseBars < 1 || s.ResponseBars > 6 {
		return s, fmt.Errorf("response_bars must be between 1 and 6")
	}
	if s.BaselineSessions != 20 {
		return s, fmt.Errorf("volume_turn_v1 requires 20 baseline sessions")
	}
	return s, nil
}

// SetupEvaluateParams selects one underlying and an optional past decision clock.
type SetupEvaluateParams struct {
	Spec     SetupSpec      `json:"spec"`
	Contract ContractParams `json:"contract"`
	At       time.Time      `json:"at,omitzero"`
}

// NormalizeSetupEvaluateParams refuses unsupported instruments and future clocks.
func NormalizeSetupEvaluateParams(p SetupEvaluateParams, now time.Time) (SetupEvaluateParams, error) {
	var err error
	p.Spec, err = NormalizeSetupSpec(p.Spec)
	if err != nil {
		return p, err
	}
	if p.Contract.Symbol, err = normalizeSetupSymbol(p.Contract.Symbol); err != nil {
		return p, err
	}
	if p.Contract.SecType == "" {
		p.Contract.SecType = "STK"
	}
	if p.Contract.Exchange == "" {
		p.Contract.Exchange = "SMART"
	}
	if p.Contract.Currency == "" {
		p.Contract.Currency = "USD"
	}
	if p.Contract.SecType != "STK" || p.Contract.Currency != "USD" || p.Contract.ConID < 0 || p.Contract.ConID > math.MaxInt32 {
		return p, fmt.Errorf("setups currently supports US USD stocks only")
	}
	if !p.At.IsZero() && (p.At.After(now) || p.At.Before(now.AddDate(-1, 0, 0))) {
		return p, fmt.Errorf("at must be within the preceding year and not in the future")
	}
	return p, nil
}

// normalizeSetupSymbol upper-cases one printable ASCII underlying token.
func normalizeSetupSymbol(symbol string) (string, error) {
	symbol = strings.ToUpper(strings.TrimSpace(symbol))
	if symbol == "" || len(symbol) > 32 || strings.ContainsAny(symbol, "\r\n\t, ") {
		return symbol, fmt.Errorf("one underlying symbol is required")
	}
	for _, r := range symbol {
		if r < 33 || r > 126 {
			return symbol, fmt.Errorf("underlying symbol must be a printable ASCII token")
		}
	}
	return symbol, nil
}

// SetupBar is a completed five-minute regular-session OHLCV observation.
// Trades is IBKR's trade count for the bar, absent when the source did not
// report one; volume divided by trades is the bar's average trade size.
type SetupBar struct {
	Start  time.Time `json:"start"`
	End    time.Time `json:"end"`
	Open   float64   `json:"open"`
	High   float64   `json:"high"`
	Low    float64   `json:"low"`
	Close  float64   `json:"close"`
	Volume int64     `json:"volume"`
	Trades *int64    `json:"trades,omitempty"`
}

// SetupUsual is one clock slot's mean across the comparable prior sessions:
// what that time of day usually looks like. Trades is absent unless every
// prior session reported a trade count for the slot.
type SetupUsual struct {
	End    time.Time `json:"end"`
	Volume float64   `json:"volume"`
	Trades *float64  `json:"trades,omitempty"`
}

// SetupTraceStep is the rule's reading at the close of a completed bar where
// it changed, reconstructed from the bars this evaluation holds; it holds for
// every later bar until the next step. It shows how the state developed; it is
// not evidence that a later-corrected bar was known.
type SetupTraceStep struct {
	End              time.Time  `json:"end"`
	State            string     `json:"state"`
	Reason           string     `json:"reason"`
	SpikeAt          *time.Time `json:"spike_at,omitempty"`
	FirstConfirmedAt *time.Time `json:"first_confirmed_at,omitempty"`
	ConfirmationType string     `json:"confirmation_type,omitempty"`
}

// SetupRecentSession is one of the latest comparable prior sessions in hourly
// bars, aggregated from the baseline's own five-minute bars: the days before
// today, with no extra broker read.
type SetupRecentSession struct {
	Date string     `json:"date"`
	Bars []SetupBar `json:"bars"`
}

// SetupBaseline is the actual same-slot observation from one comparable session.
type SetupBaseline struct {
	Date   string    `json:"date"`
	Start  time.Time `json:"start"`
	End    time.Time `json:"end"`
	Volume int64     `json:"volume"`
}

// SetupFeatures preserves measured values independently of activation and policy.
// TradeSize is the average trade over the latest TradeSizeBars completed bars
// (at most an hour) and TradeSizeUsual the same clock slots' usual average;
// both are absent when any bar or slot lacks a trade count.
type SetupFeatures struct {
	SlotVolume     *int64   `json:"slot_volume,omitempty"`
	SlotAverage    *float64 `json:"slot_average,omitempty"`
	SpikeMultiple  *float64 `json:"spike_multiple,omitempty"`
	PriceChangePct *float64 `json:"price_change_pct,omitempty"`
	TradeSize      *float64 `json:"trade_size,omitempty"`
	TradeSizeUsual *float64 `json:"trade_size_usual,omitempty"`
	TradeSizeBars  int      `json:"trade_size_bars,omitempty"`
}

// SetupResult is dated observation evidence, never option or order authority.
type SetupResult struct {
	Version            int             `json:"version"`
	Spec               SetupSpec       `json:"spec"`
	Contract           ContractParams  `json:"contract"`
	EvaluatedAt        time.Time       `json:"evaluated_at"`
	ObservedAt         time.Time       `json:"observed_at"`
	BaselineObservedAt time.Time       `json:"baseline_observed_at,omitzero"`
	SessionDate        string          `json:"session_date"`
	EvidenceKind       string          `json:"evidence_kind"`
	SetupMatch         *bool           `json:"setup_match"`
	State              string          `json:"state"`
	Reasons            []string        `json:"reasons"`
	SpikeAt            *time.Time      `json:"spike_at,omitempty"`
	FirstConfirmedAt   *time.Time      `json:"first_confirmed_at,omitempty"`
	FirstAvailableAt   *time.Time      `json:"first_available_at,omitempty"`
	ConfirmationType   string          `json:"confirmation_type,omitempty"`
	ValidUntil         *time.Time      `json:"valid_until,omitempty"`
	BaselineSessions   int             `json:"baseline_sessions"`
	Features           SetupFeatures   `json:"features"`
	Bars               []SetupBar      `json:"bars"`
	Baseline           []SetupBaseline `json:"baseline"`
	// Usual covers every slot of today's session, ahead of the latest bar
	// too: it comes from prior sessions, never from today's later bars.
	Usual          []SetupUsual         `json:"usual,omitempty"`
	Trace          []SetupTraceStep     `json:"trace,omitempty"`
	RecentSessions []SetupRecentSession `json:"recent_sessions,omitempty"`
	InputHash      string               `json:"input_hash"`
	PriceBasis     string               `json:"price_basis"`
	VolumeBasis    string               `json:"volume_basis"`
	PolicyChecked  bool                 `json:"policy_checked"`
}
