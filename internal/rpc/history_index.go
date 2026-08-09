package rpc

import "time"

// MethodRulesHistory serves the rulebook transition timeline from the same
// daemon.db authority. Advisory/read-only end to end — nothing in
// these results touches submit eligibility or any broker-write path.
const MethodRulesHistory = "rules.history"

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

// RulesHistoryParams selects a window of persisted rulebook transitions;
// boundaries accept RFC3339 timestamps or YYYY-MM-DD UTC days.
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

// RulesHistoryResult is the rules.history envelope.
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

// MethodReconEquity serves the daemon.db statement-derived daily equity series
// joined with authoritative capital events. Read-only: retained Flex XML stays
// the original broker evidence, while SQLite holds its transactionally
// refreshed typed projection.
const MethodReconEquity = "recon.equity"

// ReconEquityParams selects a window of equity days. Boundary grammar
// uses the same boundary grammar; the default lookback is 90 days because
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
