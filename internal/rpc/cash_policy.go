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
	// CodeConfirmationUnverifiable means the save changes an [order_limits]
	// key and Canary could not verify the owner's device confirmation itself:
	// the constitution pins no key for the credential, or the envelope does
	// not carry a signature over these exact terms. Nothing was written.
	CodeConfirmationUnverifiable = "confirmation_unverifiable"
)

// Preset ids policy.cash.get derives from the files' values (owner decisions
// 2026-10-07 08:30 CEST). Nothing is stored as a label: a desk whose five
// covered keys match a preset's table reads that preset, any other reads
// custom.
const (
	CashPolicyPresetCautious   = "cautious"
	CashPolicyPresetBalanced   = "balanced"
	CashPolicyPresetAggressive = "aggressive"
	CashPolicyPresetCustom     = "custom"
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
	// CashPolicySectionOrderLimits is the risk constitution's [order_limits]
	// table, the second file these methods edit (owner decision 2026-10-07
	// 08:33 CEST): the order cap's floor and share of NLV and the option
	// contracts per order. Every change to it needs the owner's device,
	// verified by Canary, in both directions.
	CashPolicySectionOrderLimits = "order_limits"
)

// CashPolicySnapshot is the cash settings as the daemon reads them: every key
// in scope with its value, where the value comes from, Canary's written
// default, its bounds and help, plus the facts that give the numbers meaning.
// Revision names the bytes of both files, "<protection digest>+<constitution
// digest>": check and apply name it, so either file changed since the read
// is a conflict. Desk passes it back unchanged.
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
	// ConfirmationWindowSeconds is [cash] confirmation_window in force, in
	// seconds: how long after a save the owner's device confirmed a further
	// save from the same Desk console session may rely on that confirmation,
	// when it lets no more reach the broker; 0 means every save asks the
	// device. It is file-only (owner decision 2026-10-06 15:31 CEST), and
	// Confirmation is Canary's sentence for it.
	ConfirmationWindowSeconds int64  `json:"confirmation_window_seconds"`
	Confirmation              string `json:"confirmation"`
	// Constitution is the risk-policy.toml side: the file the order_limits
	// settings live in, in the same terms as the protection file above.
	// Absent on a Canary that predates the order caps in these settings.
	Constitution *CashPolicyFile `json:"constitution,omitempty"`
	// Preset is the stance the files' values derive to; Presets lists every
	// preset with its values and what they come to at today's NLV; Restore
	// offers the owner's own values a preset replaced, while it can. All
	// three are absent on an older Canary.
	Preset  *CashPolicyPreset        `json:"preset,omitempty"`
	Presets []CashPolicyPresetOption `json:"presets,omitempty"`
	Restore *CashPolicyRestore       `json:"restore,omitempty"`
	// Device says which of the owner's credentials Canary can verify itself
	// ([desk_device] in the constitution), which every order cap change
	// needs; Message is Canary's sentence while it can verify none.
	Device *CashPolicyDevice `json:"device,omitempty"`
}

// CashPolicyFile is one policy file's state as these methods read it: its
// path, one of the CashPolicyFile* states with Canary's sentence for any
// state other than ok, its policy_version against the one in force, and
// whether a save may write it.
type CashPolicyFile struct {
	Path           string `json:"path"`
	FileState      string `json:"file_state"`
	Message        string `json:"message,omitempty"`
	PolicyVersion  int    `json:"policy_version"`
	InForceVersion int    `json:"in_force_version"`
	Writable       bool   `json:"writable"`
}

// CashPolicyPreset is the stance derived from the values in force or in the
// files: Id is a CashPolicyPreset* id, Label and Explainer its words,
// Revision the dated preset table the values matched (a later table keeps
// its predecessors, so a release never relabels a desk custom), and Note
// what the reader should know: the cash sweep not set up yet (the stance
// then follows the order caps alone), or a file compared from the policy in
// force because the file on disk is not the one Canary runs.
type CashPolicyPreset struct {
	ID        string `json:"id"`
	Label     string `json:"label"`
	Explainer string `json:"explainer"`
	Revision  string `json:"revision"`
	Note      string `json:"note,omitempty"`
}

// CashPolicyPresetOption is one preset the owner may apply: its five values
// by key and what they come to at today's NLV.
type CashPolicyPresetOption struct {
	ID        string                `json:"id"`
	Label     string                `json:"label"`
	Explainer string                `json:"explainer"`
	Revision  string                `json:"revision"`
	Values    map[string]any        `json:"values"`
	Facts     CashPolicyPresetFacts `json:"facts"`
}

// CashPolicyPresetFacts is what a preset comes to at today's net liquidation
// value: the reserve kept as cash and the order cap on new orders, each as
// an amount in base currency and in Canary's words, and the option contracts
// per order. NLVUnknown is true when NLV cannot be read now; the amounts are
// then the ones that apply without it (the floors).
type CashPolicyPresetFacts struct {
	ReserveBase  float64 `json:"reserve_base"`
	ReserveText  string  `json:"reserve_text"`
	OrderCapBase float64 `json:"order_cap_base"`
	OrderCapText string  `json:"order_cap_text"`
	Contracts    int     `json:"contracts"`
	NLVUnknown   bool    `json:"nlv_unknown,omitempty"`
}

// CashPolicyRestore offers the owner's own values back after a preset
// replaced them: the values of the covered keys as they stood before the
// most recent save from custom to a preset (completed, for keys that save
// left alone, from the preset it applied), the time of that save and the
// preset. Served only while the files read a preset and no one has changed
// them since Canary's last save.
type CashPolicyRestore struct {
	SavedAt time.Time      `json:"saved_at"`
	Preset  string         `json:"preset"`
	Values  map[string]any `json:"values"`
}

// CashPolicyDevice says which credentials Canary verifies itself, as Desk
// names them (companion:<key id>, passkey:<credential id>), from the
// constitution's [desk_device]; Message is Canary's sentence while none is
// pinned or the table does not parse.
type CashPolicyDevice struct {
	Verifiable []string `json:"verifiable"`
	Message    string   `json:"message,omitempty"`
}

// CashPolicySections carries what each section shows besides its settings.
type CashPolicySections struct {
	Leveling CashPolicySection `json:"leveling"`
	Sweep    CashPolicySection `json:"sweep"`
	// OrderLimits is the constitution's [order_limits]: Present while the
	// table is complete, Fact the order cap in force in Canary's words.
	OrderLimits CashPolicySection `json:"order_limits"`
}

// CashPolicySection is one feature's header. Present says the file has its
// table. Fact is the line under the header; Authority says who sends the
// feature's orders; Cap states the order cap in force that bounds them.
// PreAuthorised reports that [cash] pre_authorised lists the feature, so the
// daemon sends its orders itself after the veto window once it is on and
// active; a save that makes it do so says that first.
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
	// BaseCurrency names the unit of every base-currency value in Changes,
	// so a reviewer can word a value itself from From and To.
	BaseCurrency string `json:"base_currency,omitempty"`
	// PresetFrom and PresetTo are the stance of the files now and of the
	// checked draft (CashPolicyPreset* ids). They are not part of the terms
	// or the signed review: the headline Desk shows comes from here.
	PresetFrom string `json:"preset_from,omitempty"`
	PresetTo   string `json:"preset_to,omitempty"`
	// ConstitutionPolicyVersion is risk-policy.toml's version and
	// ConstitutionSavedVersion the one a save of this draft writes; 0 when
	// the draft changes no [order_limits] key.
	ConstitutionPolicyVersion int `json:"constitution_policy_version,omitempty"`
	ConstitutionSavedVersion  int `json:"constitution_saved_version,omitempty"`
	// DeviceRequired is true when apply will take only a fresh device
	// confirmation: the draft has a consequence at the broker, or changes an
	// [order_limits] key (both directions; Canary verifies the confirmation
	// itself, so Device in the snapshot must list the credential).
	DeviceRequired bool `json:"device_required"`
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
// signed envelope. Canary verifies the envelope itself when the constitution
// pins the credential's key ([desk_device]); a save that changes an
// [order_limits] key is refused unless it does. Otherwise the envelope is
// kept for audit, as before.
//
// The envelope is JSON. For the companion: {"credential":"companion",
// "key_id","challenge","signature","review_json"}, where signature is the
// 64-byte r||s over SHA-256 of "desk-companion-policy-v1\n<desk action
// id>\n<digest>\n<challenge>", and digest is the hex SHA-256 of
// "desk-policy-digest-v1\n<action id>\n<terms digest>\n<hex SHA-256 of the
// terms>\n<terms>\n<review_json>" (the chain the companion itself checks
// before signing). For the passkey: {"credential":"passkey","credential_id",
// "authenticator_data","client_data_json","signature","review_json"}, the
// WebAuthn assertion whose challenge is <32-byte nonce><SHA-256 of
// "desk-passkey-policy-v1\n<action id>\n<digest>\n" plus the nonce>, the
// relying party localhost, user verification set, and the DER signature
// over authenticator data plus the SHA-256 of client data. Binary values are
// unpadded base64url. The terms are the request's own.
//
// A save may instead rely on an earlier save the owner's device confirmed in
// the same Desk console session, when it lets no more reach the broker (owner
// decision 2026-10-06 15:31 CEST): ConfirmedBy names the earlier save's
// request id, Credential is the earlier save's credential and Envelope Desk's
// record of the reliance. Canary works out the window itself: the earlier
// save's receipt time plus [cash] confirmation_window in force now. It
// accepts the reliance only for a save with no consequence at the broker,
// inside that window, and only on a save it recorded with a fresh
// confirmation by the same credential.
type CashPolicyConfirmation struct {
	DeskActionID string `json:"desk_action_id"`
	Credential   string `json:"credential"`
	Envelope     string `json:"envelope"`
	ConfirmedBy  string `json:"confirmed_by,omitempty"`
}

// CashPolicyApplyResult is the snapshot after a save, with the receipt's
// request id, the version written, whether Canary runs it, and whether this
// was a retry of a saved request (nothing written again). ConfirmedUntil is,
// for a save the device confirmed, when the window it opens ends at the
// window in force now (zero when the window is 0s), and for a save that
// relied on an earlier one, the end of the window it relied on.
type CashPolicyApplyResult struct {
	CashPolicySnapshot
	RequestID      string    `json:"request_id"`
	SavedVersion   int       `json:"saved_version"`
	InForce        bool      `json:"in_force"`
	Replay         bool      `json:"replay"`
	ConfirmedUntil time.Time `json:"confirmed_until,omitzero"`
	// ConstitutionSavedVersion is the risk-policy.toml version written, 0
	// when the save changed no [order_limits] key. Verified names the
	// credential whose signature Canary verified itself, empty for a save it
	// kept for audit only. Partial is set when the constitution was written
	// and the protection file was not: the save is half done, and the files
	// read custom by value.
	ConstitutionSavedVersion int                `json:"constitution_saved_version,omitempty"`
	Verified                 string             `json:"verified,omitempty"`
	Partial                  *CashPolicyPartial `json:"partial,omitempty"`
}

// CashPolicyPartial reports a two-file save that stopped after its first
// file: Saved names the section written (order_limits), NotSaved the one not
// written (cash) and Reason why, in Canary's words.
type CashPolicyPartial struct {
	Saved    string `json:"saved"`
	NotSaved string `json:"not_saved"`
	Reason   string `json:"reason"`
}
