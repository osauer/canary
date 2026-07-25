package rpc

import "time"

// MethodRegimeHistory serves the post-cutover regime-decision timeline from
// the daemon's authoritative daemon.db event store. Read-only: these results
// never feed policy or broker-write authority.
const MethodRegimeHistory = "regime.history"

// MethodRulesHistory serves the rulebook transition timeline from the same
// daemon.db authority. Advisory/read-only end to end — nothing in
// these results touches submit eligibility or any broker-write path.
const MethodRulesHistory = "rules.history"

// RegimeHistoryParams selects a window of persisted regime decisions.
// Boundary grammar mirrors orders.history: RFC3339 timestamps or
// YYYY-MM-DD UTC days.
type RegimeHistoryParams struct {
	// Since is the inclusive lower boundary: RFC3339, or YYYY-MM-DD
	// meaning the start of that UTC day. Empty = 7 days before Until.
	Since string `json:"since,omitempty"`
	// Until is the upper boundary: RFC3339 (exclusive), or YYYY-MM-DD
	// meaning that whole UTC day stays included. Empty = now.
	Until string `json:"until,omitempty"`
	// Stage filters on the exact lifecycle stage word (for example
	// early_warning). Empty matches all stages.
	Stage string `json:"stage,omitempty"`
	// Limit caps returned rows, newest first; default 50, max 500.
	Limit int `json:"limit,omitempty"`
}

// RegimeHistoryEntry is one persisted regime decision. Free-text fields
// (Verdict) are event data for display, never parsed into authority.
type RegimeHistoryEntry struct {
	At                 time.Time `json:"at"`
	SessionKey         string    `json:"session_key,omitempty"`
	TapeSession        string    `json:"tape_session,omitempty"`
	Stage              string    `json:"stage"`
	Severity           string    `json:"severity,omitempty"`
	Readiness          string    `json:"readiness,omitempty"`
	Confidence         string    `json:"confidence,omitempty"`
	Verdict            string    `json:"verdict,omitempty"`
	ClusterRed         int       `json:"cluster_red_count"`
	ClusterYellow      int       `json:"cluster_yellow_count"`
	ClusterEligibleRed int       `json:"cluster_eligible_red_count"`
	Fingerprint        string    `json:"fingerprint,omitempty"`
}

// HistoryIndexHealth is retained in history result shapes for wire
// compatibility. The daemon.db authority has no asynchronous JSONL ingest or
// journal-byte freshness comparison; storage availability is reported by the
// RPC outcome and daemon health surface.
type HistoryIndexHealth struct {
	// LastIngestAt is a retired legacy-ingest field and is zero for direct
	// daemon.db history reads.
	LastIngestAt time.Time `json:"last_ingest_at,omitzero"`
	// IngestedBytes is a retired legacy journal watermark.
	IngestedBytes int64 `json:"ingested_bytes"`
	// JournalBytes is a retired legacy journal-size field.
	JournalBytes int64 `json:"journal_bytes"`
}

// RegimeHistoryResult is the regime.history envelope: the filtered window,
// newest first, with total-vs-returned counts and a legacy-shaped compatibility
// health block.
type RegimeHistoryResult struct {
	AsOf       time.Time            `json:"as_of"`
	Since      time.Time            `json:"since"`
	Until      time.Time            `json:"until"`
	Entries    []RegimeHistoryEntry `json:"entries"`
	Count      int                  `json:"count"`
	TotalCount int                  `json:"total_count"`
	Limit      int                  `json:"limit"`
	Truncated  bool                 `json:"truncated"`
	Index      HistoryIndexHealth   `json:"index"`
}

// RulesHistoryParams selects a window of persisted rulebook transitions;
// boundary and limit semantics match RegimeHistoryParams.
type RulesHistoryParams struct {
	Since string `json:"since,omitempty"`
	Until string `json:"until,omitempty"`
	// Rule filters on the exact rule id (for example
	// single_name_exposure). Empty matches all rules.
	Rule  string `json:"rule,omitempty"`
	Limit int    `json:"limit,omitempty"`
}

// RuleTransitionEntry is one persisted rule status transition. Evidence is
// event free text for display, never parsed into authority.
type RuleTransitionEntry struct {
	At                time.Time `json:"at"`
	Rule              string    `json:"rule"`
	Status            string    `json:"status"`
	Was               string    `json:"was,omitempty"`
	Evidence          string    `json:"evidence,omitempty"`
	PolicyID          string    `json:"policy_id,omitempty"`
	PolicyVersion     int       `json:"policy_version,omitempty"`
	PolicyFingerprint string    `json:"policy_fingerprint,omitempty"`
}

// RulesHistoryResult is the rules.history envelope — the same shape as
// RegimeHistoryResult with rule-transition entries.
type RulesHistoryResult struct {
	AsOf       time.Time             `json:"as_of"`
	Since      time.Time             `json:"since"`
	Until      time.Time             `json:"until"`
	Entries    []RuleTransitionEntry `json:"entries"`
	Count      int                   `json:"count"`
	TotalCount int                   `json:"total_count"`
	Limit      int                   `json:"limit"`
	Truncated  bool                  `json:"truncated"`
	Index      HistoryIndexHealth    `json:"index"`
}

// MethodStressHistory serves the post-cutover stress-decision timeline from
// daemon.db. Read-only event evidence — nothing here touches submit eligibility
// or any broker-write path.
const MethodStressHistory = "stress.history"

// StressHistoryParams selects a window of persisted stress decisions;
// boundary and limit semantics match RegimeHistoryParams.
type StressHistoryParams struct {
	Since string `json:"since,omitempty"`
	Until string `json:"until,omitempty"`
	// Severity filters on the exact decision severity word (for example
	// watch, act). Empty matches all severities.
	Severity string `json:"severity,omitempty"`
	// Action filters on the exact decision action word (for example defend,
	// watch). Empty matches all actions.
	Action string `json:"action,omitempty"`
	Limit  int    `json:"limit,omitempty"`
}

// StressHistoryEntry is one persisted stress decision. Summary is event free
// text for display, never parsed into authority.
type StressHistoryEntry struct {
	At          time.Time `json:"at"`
	SessionKey  string    `json:"session_key,omitempty"`
	Fingerprint string    `json:"fingerprint,omitempty"`
	Account     string    `json:"account,omitempty"`
	AccountMode string    `json:"account_mode,omitempty"`
	Action      string    `json:"action,omitempty"`
	Severity    string    `json:"severity"`
	Direction   string    `json:"direction,omitempty"`
	MarketStage string    `json:"market_stage,omitempty"`
	// PortfolioAlertRelevant mirrors the producer-stamped verdict; nil
	// means the event predates the stamp.
	PortfolioAlertRelevant *bool  `json:"portfolio_alert_relevant,omitempty"`
	InputHealth            string `json:"input_health,omitempty"`
	Summary                string `json:"summary,omitempty"`
}

// StressHistoryResult is the stress.history envelope — the phase-1 history
// envelope with stress entries.
type StressHistoryResult struct {
	AsOf       time.Time            `json:"as_of"`
	Since      time.Time            `json:"since"`
	Until      time.Time            `json:"until"`
	Entries    []StressHistoryEntry `json:"entries"`
	Count      int                  `json:"count"`
	TotalCount int                  `json:"total_count"`
	Limit      int                  `json:"limit"`
	Truncated  bool                 `json:"truncated"`
	Index      HistoryIndexHealth   `json:"index"`
}

// MethodReconEquity serves the daemon.db statement-derived daily equity series
// joined with authoritative capital events. Read-only: retained Flex XML stays
// the original broker evidence, while SQLite holds its transactionally
// refreshed typed projection.
const MethodReconEquity = "recon.equity"

// ReconEquityParams selects a window of equity days. Boundary grammar
// matches RegimeHistoryParams; the default lookback is 90 days because
// the series is daily-granular.
type ReconEquityParams struct {
	Since string `json:"since,omitempty"`
	Until string `json:"until,omitempty"`
	// Limit caps returned days, newest first; default 200, max 1000.
	Limit int `json:"limit,omitempty"`
}

// EquityDayEntry is one derived statement-equity day. SourceStmt names the
// retained statement file the value came from; WhenGenerated is the
// restatement authority (newest statement wins per day).
type EquityDayEntry struct {
	Day           string    `json:"day"`
	AccountID     string    `json:"account_id"`
	EquityBase    float64   `json:"equity_base"`
	SourceStmt    string    `json:"source_stmt"`
	WhenGenerated time.Time `json:"when_generated,omitzero"`
}

// CapitalEventEntry is one authoritative declared-capital event rendered
// alongside the equity series.
type CapitalEventEntry struct {
	At          time.Time `json:"at"`
	Type        string    `json:"type"`
	AmountBase  float64   `json:"amount_base,omitempty"`
	EffectiveAt time.Time `json:"effective_at,omitzero"`
	Note        string    `json:"note,omitempty"`
	Origin      string    `json:"origin,omitempty"`
	ReportID    string    `json:"report_id,omitempty"`
}

// ReconEquityResult is the recon.equity envelope: the equity-day window
// newest first, capital events over the same window (hard-capped, newest
// first), and two legacy-shaped health blocks retained for wire compatibility.
type ReconEquityResult struct {
	AsOf            time.Time           `json:"as_of"`
	Since           time.Time           `json:"since"`
	Until           time.Time           `json:"until"`
	Days            []EquityDayEntry    `json:"days"`
	Count           int                 `json:"count"`
	TotalCount      int                 `json:"total_count"`
	Limit           int                 `json:"limit"`
	Truncated       bool                `json:"truncated"`
	Events          []CapitalEventEntry `json:"events"`
	EventsTruncated bool                `json:"events_truncated"`
	Index           HistoryIndexHealth  `json:"index"`
	Statements      HistoryIndexHealth  `json:"statements"`
}
