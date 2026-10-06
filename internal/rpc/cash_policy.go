package rpc

import (
	"encoding/json"
	"time"
)

// Cash policy settings: the owner's cash management settings in the
// protection policy file ([cash.leveling] and [cash.sweep]), read, checked and
// written by the daemon for Desk's Settings screen. Like
// settings.cash_sweep.get and set_priority, these methods are in no
// catalogue: not in MCP, not in the CLI and not in any agent tool grant.
// The daemon owns the file: the read, the validation, the write, the
// policy_version raise, the backup and the provenance comment.
const (
	MethodCashPolicyGet   = "policy.cash.get"
	MethodCashPolicyCheck = "policy.cash.check"
	MethodCashPolicyApply = "policy.cash.apply"
)

// Error codes policy.cash.apply reports besides CodeSettingsConflict (the
// file changed since it was read; nothing was written) and bad_request.
const (
	// CodePolicyInvalid means Canary's loader refuses the changed file;
	// nothing was written.
	CodePolicyInvalid = "policy_invalid"
	// CodePolicyUnwritable means the file cannot be saved from here (drift,
	// refused, missing, or a table that is not a plain section); nothing was
	// written.
	CodePolicyUnwritable = "policy_unwritable"
	// CodeConfirmationRequired means the request carried no owner
	// confirmation; nothing was written.
	CodeConfirmationRequired = "confirmation_required"
	// CodeRequestReused means the request id already names other terms.
	CodeRequestReused = "request_reused"
)

// File states of the protection policy file as policy.cash.get reads it.
const (
	// CashPolicyFileOK is a file Canary runs as written.
	CashPolicyFileOK = "ok"
	// CashPolicyFileAhead is a valid file with a higher policy_version than
	// the policy in force; Canary adopts it at its next reread.
	CashPolicyFileAhead = "ahead"
	// CashPolicyFileDrift is a file edited without a higher policy_version;
	// Canary keeps the policy in force and does not adopt the edits.
	CashPolicyFileDrift = "drift"
	// CashPolicyFileRefused is a file Canary's loader refuses.
	CashPolicyFileRefused = "refused"
	// CashPolicyFileMissing is no file at the configured path.
	CashPolicyFileMissing = "missing"
)

// Sources of a setting's value.
const (
	// CashPolicySourceFile is a value written in the file.
	CashPolicySourceFile = "file"
	// CashPolicySourceCanaryDefault is a key the file leaves out whose
	// built-in value applies; for a per-currency settlement float, the
	// common keep_cash applies.
	CashPolicySourceCanaryDefault = "canary_default"
	// CashPolicySourceNotWritten is a key the file leaves out that the
	// feature waits for: it holds until the key is written.
	CashPolicySourceNotWritten = "not_written"
)

// Setting types, units and sections.
const (
	CashPolicyTypeNumber  = "number"
	CashPolicyTypeInteger = "integer"
	CashPolicyTypeBool    = "bool"
	CashPolicyTypeChoice  = "choice"

	// CashPolicyUnitBase is the account's base currency at the ledger rate.
	CashPolicyUnitBase = "base"
	// CashPolicyUnitOwn is each currency's own unit.
	CashPolicyUnitOwn = "own"
	// CashPolicyUnitPctNLV is a percentage of net liquidation value.
	CashPolicyUnitPctNLV = "pct_nlv"
	// CashPolicyUnitBP is basis points.
	CashPolicyUnitBP = "bp"
	// CashPolicyUnitDays is whole calendar days.
	CashPolicyUnitDays = "days"

	CashPolicySectionLeveling = "leveling"
	CashPolicySectionSweep    = "sweep"
)

// CashPolicySnapshot is the cash settings as the daemon reads them: every key
// in scope with its value, where the value comes from, Canary's written
// default, its bounds and help, plus the facts that give the numbers meaning.
// Revision is the hash of the file's bytes: check and apply name it, so a
// file changed since the read is a conflict.
type CashPolicySnapshot struct {
	Revision string `json:"revision"`
	Path     string `json:"path"`
	// FileState is one of the CashPolicyFile* states; Message is Canary's
	// sentence for any state other than ok.
	FileState string `json:"file_state"`
	Message   string `json:"message,omitempty"`
	// LegacyLayout reports a file that still keeps the sweep under
	// [buckets.cash_sweep]; Canary reads and writes it there and moves it to
	// [cash.sweep] at its next start.
	LegacyLayout bool `json:"legacy_layout,omitempty"`
	// PolicyVersion is the file's policy_version; InForceVersion the one
	// Canary runs.
	PolicyVersion  int  `json:"policy_version"`
	InForceVersion int  `json:"in_force_version"`
	Writable       bool `json:"writable"`
	// AsOf is the time of the figures in the facts.
	AsOf         time.Time            `json:"as_of"`
	BaseCurrency string               `json:"base_currency,omitempty"`
	Sections     CashPolicySections   `json:"sections"`
	Settings     []CashPolicySetting  `json:"settings"`
	Currencies   []CashPolicyCurrency `json:"currencies"`
	// RatesThrough is the latest statement day the interest rates read.
	RatesThrough string              `json:"rates_through,omitempty"`
	Findings     []CashPolicyFinding `json:"findings"`
}

// CashPolicySections carries what each section shows besides its settings.
type CashPolicySections struct {
	Leveling CashPolicySection `json:"leveling"`
	Sweep    CashPolicySection `json:"sweep"`
}

// CashPolicySection is one feature's header. Present says the file has its
// table. Fact is the line under the header; Authority says who sends the
// feature's orders; Cap states the order cap in force that bounds them.
// PreAuthorised reports that [cash] pre_authorised lists the feature, so the
// daemon sends its orders itself after the veto window; while it does, the
// screen cannot switch the feature on or raise its mode (the file decides).
type CashPolicySection struct {
	Present       bool   `json:"present"`
	Fact          string `json:"fact,omitempty"`
	Authority     string `json:"authority,omitempty"`
	Cap           string `json:"cap,omitempty"`
	PreAuthorised bool   `json:"pre_authorised,omitempty"`
}

// CashPolicySetting is one key in scope. Value is the file's value while the
// file is ok or ahead, else the value Canary runs; nil when neither has one.
// Default is the value Canary writes (nil when it has none); Reset says the
// section's Reset to Canary defaults restores it. Removable marks a key a
// change may remove with null (a per-currency settlement float). A choice
// carries its values and, in the same order, their labels.
type CashPolicySetting struct {
	Key          string   `json:"key"`
	Section      string   `json:"section"`
	Currency     string   `json:"currency,omitempty"`
	Label        string   `json:"label"`
	Help         string   `json:"help"`
	Value        any      `json:"value"`
	Source       string   `json:"source"`
	Default      any      `json:"default"`
	Type         string   `json:"type"`
	Choices      []string `json:"choices,omitempty"`
	ChoiceLabels []string `json:"choice_labels,omitempty"`
	Unit         string   `json:"unit,omitempty"`
	Min          *float64 `json:"min,omitempty"`
	MinExclusive bool     `json:"min_exclusive,omitempty"`
	Max          *float64 `json:"max,omitempty"`
	Reset        bool     `json:"reset,omitempty"`
	Removable    bool     `json:"removable,omitempty"`
	Fact         string   `json:"fact,omitempty"`
}

// CashPolicyCurrency is one currency row: the ledger's trade-date cash in its
// own unit (nil while unknown), whether currency leveling trades a pair for
// it, its interest rates from the broker's statements (annual decimals, each
// with the last day read; StandIn marks a rate taken from the currency's other
// side) and what the sweep buys in it.
type CashPolicyCurrency struct {
	Currency        string   `json:"currency"`
	Cash            *float64 `json:"cash,omitempty"`
	Pair            bool     `json:"pair"`
	LoanRate        *float64 `json:"loan_rate,omitempty"`
	LoanRateThrough string   `json:"loan_rate_through,omitempty"`
	LoanRateStandIn bool     `json:"loan_rate_stand_in,omitempty"`
	CashRate        *float64 `json:"cash_rate,omitempty"`
	CashRateThrough string   `json:"cash_rate_through,omitempty"`
	CashRateStandIn bool     `json:"cash_rate_stand_in,omitempty"`
	Buys            string   `json:"buys"`
}

// CashPolicyFinding is one `canary policy check` finding that names a cash
// key: Severity is error, warn or info; Keys are the dotted keys it names.
type CashPolicyFinding struct {
	Rule     string   `json:"rule"`
	Severity string   `json:"severity"`
	Keys     []string `json:"keys"`
	Text     string   `json:"text"`
}

// CashPolicyCheckRequest names a draft: the revision it was made against and
// each changed key's new value. A null value removes the key (only a
// per-currency settlement float can be removed).
type CashPolicyCheckRequest struct {
	ExpectedRevision string                     `json:"expected_revision"`
	Changes          map[string]json.RawMessage `json:"changes"`
}

// CashPolicyCheckResult is Canary's verdict on a draft; it writes nothing.
// Conflict means the expected revision no longer matches and nothing else was
// computed. Errors are per key, in Canary's words without the key path. Terms
// and Digest are present only for a draft with no errors and at least one
// change: apply takes exactly these terms.
type CashPolicyCheckResult struct {
	Conflict     bool                `json:"conflict"`
	Revision     string              `json:"revision"`
	Errors       map[string]string   `json:"errors"`
	Changes      []CashPolicyChange  `json:"changes"`
	Consequences []string            `json:"consequences"`
	Findings     []CashPolicyFinding `json:"findings"`
	Facts        map[string]string   `json:"facts"`
	// PolicyVersion is the file's version and SavedVersion the one a save
	// writes.
	PolicyVersion int    `json:"policy_version"`
	SavedVersion  int    `json:"saved_version,omitempty"`
	Terms         string `json:"terms,omitempty"`
	Digest        string `json:"digest,omitempty"`
}

// CashPolicyChange is one changed key: its value in the file now (nil when
// the file leaves it out, with FromSource saying what applies) and the new
// value (nil removes it), each also in Canary's words with its unit, as the
// review shows them.
type CashPolicyChange struct {
	Key        string `json:"key"`
	Label      string `json:"label"`
	Unit       string `json:"unit,omitempty"`
	Currency   string `json:"currency,omitempty"`
	From       any    `json:"from"`
	FromSource string `json:"from_source"`
	To         any    `json:"to"`
	FromText   string `json:"from_text"`
	ToText     string `json:"to_text"`
}

// CashPolicyApplyRequest saves the terms a check returned. Confirmation is
// the owner's device confirmation Desk obtained for exactly these terms; the
// daemon keeps it for audit and cannot verify its signature itself. Origin is
// audited and must be agent (empty reads agent): only Desk's console reaches
// this method.
type CashPolicyApplyRequest struct {
	Terms        string                  `json:"terms"`
	Digest       string                  `json:"digest"`
	RequestID    string                  `json:"request_id"`
	Origin       string                  `json:"origin,omitempty"`
	Confirmation *CashPolicyConfirmation `json:"confirmation"`
}

// CashPolicyConfirmation names the Desk action the owner confirmed, the
// credential that confirmed it (passkey:<id> or companion:<key id>) and the
// signed envelope, kept for audit only.
//
// A save may instead rely on an earlier save the owner's device confirmed in
// the same Desk console session, inside Desk's window, when it lets no more
// reach the broker (owner decision 2026-10-06 15:31 CEST). On a save the
// device confirmed, ConfirmedUntil is the end of the window that confirmation
// opens. On a save that relies on one, ConfirmedBy names the earlier save's
// request id and ConfirmedUntil repeats that window's end; Credential is the
// earlier save's credential and Envelope Desk's record of the reliance.
// Canary accepts it only for a save with no consequence at the broker, before
// ConfirmedUntil, and only on a save it recorded with a fresh confirmation by
// the same credential and the same window end.
type CashPolicyConfirmation struct {
	DeskActionID   string    `json:"desk_action_id"`
	Credential     string    `json:"credential"`
	Envelope       string    `json:"envelope"`
	ConfirmedBy    string    `json:"confirmed_by,omitempty"`
	ConfirmedUntil time.Time `json:"confirmed_until,omitzero"`
}

// CashPolicyApplyResult is the snapshot after a save, with the receipt's
// request id, the version written, whether Canary runs it, and whether this
// was a retry of a saved request (nothing written again).
type CashPolicyApplyResult struct {
	CashPolicySnapshot
	RequestID    string `json:"request_id"`
	SavedVersion int    `json:"saved_version"`
	InForce      bool   `json:"in_force"`
	Replay       bool   `json:"replay"`
}
