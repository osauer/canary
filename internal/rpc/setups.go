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
type SetupBar struct {
	Start  time.Time `json:"start"`
	End    time.Time `json:"end"`
	Open   float64   `json:"open"`
	High   float64   `json:"high"`
	Low    float64   `json:"low"`
	Close  float64   `json:"close"`
	Volume int64     `json:"volume"`
}

// SetupBaseline is the actual same-slot observation from one comparable session.
type SetupBaseline struct {
	Date   string    `json:"date"`
	Start  time.Time `json:"start"`
	End    time.Time `json:"end"`
	Volume int64     `json:"volume"`
}

// SetupFeatures preserves measured values independently of activation and policy.
type SetupFeatures struct {
	SlotVolume     *int64   `json:"slot_volume,omitempty"`
	SlotAverage    *float64 `json:"slot_average,omitempty"`
	SpikeMultiple  *float64 `json:"spike_multiple,omitempty"`
	PriceChangePct *float64 `json:"price_change_pct,omitempty"`
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
	InputHash          string          `json:"input_hash"`
	PriceBasis         string          `json:"price_basis"`
	VolumeBasis        string          `json:"volume_basis"`
	PolicyChecked      bool            `json:"policy_checked"`
}
