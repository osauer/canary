package rpc

import "time"

// Policy plausibility severities. An error is an order path that can never
// work or a contradiction between limits; a warning is implausible against
// the book or the economics; info is a value nobody has reviewed yet.
const (
	PolicyCheckError = "error"
	PolicyCheckWarn  = "warn"
	PolicyCheckInfo  = "info"
)

// Policy plausibility categories, one per family of the check catalogue.
const (
	PolicyCheckCategoryContradiction = "contradiction"
	PolicyCheckCategoryBook          = "book"
	PolicyCheckCategoryEconomics     = "economics"
	PolicyCheckCategoryProvenance    = "provenance"
)

// PolicyCheckReport is the plausibility read of config.toml and every policy
// file, against each other and, when available, against the live book. It is
// advisory: it never changes a limit, a gate or an order. `canary policy
// check` exits non-zero only when Errors is above zero.
type PolicyCheckReport struct {
	AsOf time.Time `json:"as_of"`
	// Status is ok, warnings (no error) or errors.
	Status   string `json:"status"`
	Errors   int    `json:"errors"`
	Warnings int    `json:"warnings"`
	Infos    int    `json:"infos"`
	// Files lists every file the check read, with its review state.
	Files []PolicyCheckFile `json:"files,omitempty"`
	// Book summarises the live account the book checks used; nil when no
	// account was available and every book check was skipped.
	Book *PolicyCheckBookSummary `json:"book,omitempty"`
	// Skipped names the checks that did not run and why (no live account,
	// no positions, no FX rate).
	Skipped []string `json:"skipped,omitempty"`
	// Assumptions lists every number the check assumed because no policy or
	// config key carries it.
	Assumptions []string             `json:"assumptions,omitempty"`
	Findings    []PolicyCheckFinding `json:"findings"`
}

// PolicyCheckFile is one file the check read.
type PolicyCheckFile struct {
	Policy string `json:"policy"`
	Path   string `json:"path,omitempty"`
	// State is read, absent (compiled defaults run) or unreadable.
	State  string `json:"state"`
	Review string `json:"review,omitempty"`
	Error  string `json:"error,omitempty"`
}

// PolicyCheckBookSummary is the account the book checks measured against.
type PolicyCheckBookSummary struct {
	BaseCurrency   string    `json:"base_currency"`
	NetLiquidation float64   `json:"net_liquidation"`
	AsOf           time.Time `json:"as_of,omitzero"`
	Positions      int       `json:"positions"`
}

// PolicyCheckFinding is one implausible value or contradiction.
type PolicyCheckFinding struct {
	// Rule is the catalogue entry's stable id, such as cap_above_trading_max.
	Rule     string `json:"rule"`
	Severity string `json:"severity"`
	Category string `json:"category"`
	// Keys are the settings involved, each with its file and value.
	Keys []PolicyCheckKey `json:"keys"`
	// Message says in one sentence what is wrong and why it matters.
	Message string `json:"message"`
	// Suggestion is the value Canary suggests, with its reasoning; empty when
	// the fix is a decision rather than a number.
	Suggestion string `json:"suggestion,omitempty"`
}

// PolicyCheckKey is one setting a finding names.
type PolicyCheckKey struct {
	File  string `json:"file"`
	Key   string `json:"key"`
	Value string `json:"value"`
}
