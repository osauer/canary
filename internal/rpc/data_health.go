package rpc

import "time"

// Data-health methods serve producer-owned observations; only Check requests work.
const (
	MethodDataHealth        = "data.health"
	MethodDataCheck         = "data.check"
	DataHealthSchemaVersion = 1
)

// DataHealthParams selects a bounded page of one report revision.
type DataHealthParams struct {
	Offset   int    `json:"offset,omitempty"`
	Limit    int    `json:"limit,omitempty"`
	Revision string `json:"revision,omitempty"`
}

// DataHealthResult is the daemon's authoritative data-health report. Adapters
// preserve its assessment; receipt or browser health is a separate observation.
type DataHealthResult struct {
	SchemaVersion int                 `json:"schema_version"`
	Revision      string              `json:"revision"`
	AsOf          time.Time           `json:"as_of"`
	ValidUntil    time.Time           `json:"valid_until"`
	ScopeState    string              `json:"scope_state"`
	Check         *DataCheckResult    `json:"check,omitempty"`
	Summary       DataHealthSummary   `json:"summary"`
	Concerns      []DataHealthConcern `json:"concerns"`
	Sources       []DataSourceHealth  `json:"sources"`
	Offset        int                 `json:"offset"`
	NextOffset    *int                `json:"next_offset,omitempty"`
	Complete      bool                `json:"complete"`
}

// DataHealthSummary counts producer services/data products and distinct observed causes.
// Instruments and requested chart windows never create sources or source problems.
// Unverified is separate from known failures; a complete page is not coverage.
type DataHealthSummary struct {
	State       string `json:"state"`
	Label       string `json:"label"`
	Total       int    `json:"total"`
	Required    int    `json:"required"`
	Current     int    `json:"current"`
	Limited     int    `json:"limited"`
	Unavailable int    `json:"unavailable"`
	Unverified  int    `json:"unverified"`
	Problems    int    `json:"problems"`
}

// DataSourceHealth separates data actually received from the latest access
// attempt and its impact. IDs identify services or data products, never individual
// instruments. Instrument availability and chart coverage stay in their own DTOs.
// It contains no symbols, prices, balances or broker free text.
type DataSourceHealth struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Provider string `json:"provider"`
	Kind     string `json:"kind"`
	Required bool   `json:"required"`
	State    string `json:"state"`
	// Availability reports observed acquisition, independently of delivery mode,
	// scheduled cadence, applicability and analytical usability. Absent optional
	// dimensions mean unsupported; they must never be interpreted as healthy.
	Availability     string                 `json:"availability,omitempty"`
	CadenceState     string                 `json:"cadence_state,omitempty"`
	Applicability    string                 `json:"applicability,omitempty"`
	Usability        string                 `json:"usability,omitempty"`
	UsabilityReason  string                 `json:"usability_reason,omitempty"`
	Delivery         string                 `json:"delivery,omitempty"`
	Receiving        string                 `json:"receiving"`
	Detail           string                 `json:"detail,omitempty"`
	Affects          []string               `json:"affects"`
	DataType         string                 `json:"data_type,omitempty"`
	SourceAt         time.Time              `json:"source_at,omitzero"`
	SourceDate       string                 `json:"source_date,omitempty"`
	SourceTimeKind   string                 `json:"source_time_kind,omitempty"`
	ReceivedAt       time.Time              `json:"received_at,omitzero"`
	CheckedAt        time.Time              `json:"checked_at,omitzero"`
	ValidUntil       time.Time              `json:"valid_until,omitzero"`
	NextAttempt      time.Time              `json:"next_attempt,omitzero"`
	Action           string                 `json:"action,omitempty"`
	Access           *DataAccessObservation `json:"access,omitempty"`
	Failure          *SourceFailure         `json:"failure,omitempty"`
	ProblemIDs       []string               `json:"problem_ids,omitempty"`
	DerivedFrom      []string               `json:"derived_from,omitempty"`
	FirstObserved    time.Time              `json:"first_observed,omitzero"`
	LastSuccess      time.Time              `json:"last_success,omitzero"`
	History          []DataHealthTransition `json:"history,omitempty"`
	HistoryTruncated bool                   `json:"history_truncated,omitempty"`
}

// DataHealthTransition is historical diagnostic evidence, never restored access
// authority. Observation gaps and session changes cannot establish continuity.
type DataHealthTransition struct {
	At       time.Time `json:"at"`
	State    string    `json:"state"`
	DataType string    `json:"data_type,omitempty"`
	Reason   string    `json:"reason,omitempty"`
}

// DataCheckResult counts checked source services, not individual reference probes.
// It reports bounded acquisition outcomes, not subscription repair.
type DataCheckResult struct {
	StartedAt   time.Time `json:"started_at"`
	CompletedAt time.Time `json:"completed_at,omitzero"`
	Running     bool      `json:"running"`
	Reused      int       `json:"reused"`
	Checked     int       `json:"checked"`
	Limited     int       `json:"limited"`
	Failed      int       `json:"failed"`
	Deferred    int       `json:"deferred"`
	Detail      string    `json:"detail"`
}

// HealthVerdict is daemon-authored operational posture, separate from trade
// eligibility. Client-version or local delivery diagnostics stay outside it.
type HealthVerdict struct {
	State  string `json:"state"`
	Reason string `json:"reason"`
}

// DataAccessObservation is a historical access attempt, separate from received data.
type DataAccessObservation struct {
	Code       int       `json:"code"`
	Reason     string    `json:"reason"`
	ObservedAt time.Time `json:"observed_at"`
	RetryAt    time.Time `json:"retry_at"`
}

// DataHealthConcern is a bounded producer-ranked concern. Unknown coverage is
// distinct from a confirmed problem and never increments the problem count.
type DataHealthConcern struct {
	SourceID string `json:"source_id"`
	State    string `json:"state"`
	Label    string `json:"label"`
}
