package rpc

import (
	"fmt"
	"strings"
	"time"
)

// MethodSetupsCoverage reads how live setup evaluations covered each session.
const MethodSetupsCoverage = "setups.coverage"

// SetupCoverageParams selects one retained session (default: the newest) and
// optionally one underlying symbol.
type SetupCoverageParams struct {
	Session string `json:"session,omitempty"`
	Symbol  string `json:"symbol,omitempty"`
}

// NormalizeSetupCoverageParams validates the session date and symbol filter.
func NormalizeSetupCoverageParams(p SetupCoverageParams) (SetupCoverageParams, error) {
	p.Session = strings.TrimSpace(p.Session)
	if p.Session != "" {
		d, err := time.Parse(time.DateOnly, p.Session)
		if err != nil || d.Format(time.DateOnly) != p.Session {
			return p, fmt.Errorf("session must be a YYYY-MM-DD market session date")
		}
	}
	if strings.TrimSpace(p.Symbol) == "" {
		p.Symbol = ""
		return p, nil
	}
	symbol, err := normalizeSetupSymbol(p.Symbol)
	if err != nil {
		return p, err
	}
	p.Symbol = symbol
	return p, nil
}

// SetupCoverageResult is the per-session instrument for live setup
// evaluations: how many each contract received against the session's
// completed five-minute bars, how many historical requests they issued and
// how they ended. It is operational evidence, never a signal or authority.
type SetupCoverageResult struct {
	Version int       `json:"version"`
	AsOf    time.Time `json:"as_of"`
	// SessionDate is the reported session; empty when nothing is retained.
	SessionDate  string    `json:"session_date,omitempty"`
	SessionOpen  time.Time `json:"session_open,omitzero"`
	SessionClose time.Time `json:"session_close,omitzero"`
	// SlotsCompleted counts the session's evaluable completed-bar slots by
	// AsOf: slot k completes at open + 5k minutes, and the close itself is
	// outside the regular session, so a full regular session has 77.
	SlotsCompleted int `json:"slots_completed"`
	// Sessions lists the retained session dates, newest first (at most 30).
	Sessions []string `json:"sessions"`
	// Truncated reports contracts beyond the per-session bound not recorded.
	Truncated bool                    `json:"truncated,omitempty"`
	Contracts []SetupCoverageContract `json:"contracts"`
}

// SetupCoverageContract is one contract's live setup evaluations in a session.
type SetupCoverageContract struct {
	Symbol string `json:"symbol"`
	ConID  int    `json:"con_id,omitempty"`
	// SlotsScheduled counts completed-bar slots from the one this contract
	// was first evaluated in through AsOf; SlotsCovered those that received
	// at least one evaluation.
	SlotsScheduled int `json:"slots_scheduled"`
	SlotsCovered   int `json:"slots_covered"`
	Evaluations    int `json:"evaluations"`
	// HistoryRequests counts historical-data requests these evaluations
	// issued (current-session reads and baseline ranges).
	HistoryRequests int `json:"history_requests"`
	// States counts evaluations by state; unavailable ones by their first
	// reason code, as "unavailable:<code>".
	States           map[string]int `json:"states"`
	FirstEvaluatedAt time.Time      `json:"first_evaluated_at"`
	LastEvaluatedAt  time.Time      `json:"last_evaluated_at"`
}
