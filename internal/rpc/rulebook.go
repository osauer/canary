package rpc

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"time"

	"github.com/osauer/canary/v2/internal/risk"
)

// MethodRulesSnapshot returns the daily trading-rulebook checklist evaluated
// against the current book. Advisory-only: nothing in this result may alter
// submit eligibility or any gated broker-write path.
const MethodRulesSnapshot = "rules.snapshot"

// RulebookPolicyFingerprintVersion labels the advisory rulebook policy
// fingerprint's JSON projection. Sibling-policy pins compare policy ID and
// version rather than fingerprint keys, and journals remain point-in-time
// records.
const RulebookPolicyFingerprintVersion = "rulebook-fp-v3"

// RulesSnapshotParams selects optional evaluation scope. Zero value means the
// full 14-rule checklist over all held names.
type RulesSnapshotParams struct {
	// Symbol narrows per-name offender lists to one underlying; portfolio
	// rules still evaluate portfolio-wide.
	Symbol string `json:"symbol,omitempty"`
}

// EarningsInfo is the per-name earnings context the rules consumed, so
// surfaces can show where each date came from.
type EarningsInfo struct {
	Symbol string `json:"symbol"`
	// Date is the next earnings date in ET (YYYY-MM-DD), empty when unknown.
	Date string `json:"date,omitempty"`
	// TimeOfDay is "amc", "bmo", or "" when unspecified.
	TimeOfDay string `json:"time_of_day,omitempty"`
	// Estimated marks provider-flagged estimated (unconfirmed) dates.
	Estimated bool `json:"estimated,omitempty"`
	// Source is fetched | override | broker_identity | security_type |
	// verified_terminal | unknown. Provider-level provenance lives in Providers;
	// Terminal carries the exact-contract evidence when no future issuer earnings
	// event applies.
	Source string `json:"source"`
	// SecurityType names the held security type when Source is security_type —
	// the canonical spelling of a type that has no issuer earnings at all. It is
	// the whole authority behind that classification, so consumers re-derive the
	// exemption from this field rather than trusting Source alone.
	SecurityType string `json:"security_type,omitempty"`
	// Status is date or a typed unresolved outcome. Conflicting provider
	// dates never populate Date.
	Status string `json:"status,omitempty"`
	// Reason is a stable aggregate explanation such as single_source or
	// conflicting_sources; it never contains provider free text.
	Reason string `json:"reason,omitempty"`
	// ObservedAt is when the fetched value was last confirmed from the
	// provider; zero for overrides and unknowns.
	ObservedAt time.Time              `json:"observed_at,omitzero"`
	Stale      bool                   `json:"stale,omitempty"`
	Providers  []EarningsProviderInfo `json:"providers,omitempty"`
	Identity   *EarningsIdentityInfo  `json:"identity,omitempty"`
	Terminal   *EarningsTerminalInfo  `json:"terminal,omitempty"`
}

// Earnings statuses are the closed aggregate/provider outcome vocabulary.
const (
	EarningsStatusDate                    = "date"
	EarningsStatusNoDatePublished         = "no_date_published"
	EarningsStatusUnsupportedSecurity     = "unsupported_security"
	EarningsStatusFormatChange            = "format_change"
	EarningsStatusTransportFailure        = "transport_failure"
	EarningsStatusConflictingSources      = "conflicting_sources"
	EarningsStatusNotApplicable           = "not_applicable"
	EarningsStatusTerminalNonReporting    = "terminal_non_reporting"
	EarningsStatusTerminalEvidenceExpired = "terminal_evidence_expired"
)

// EarningsIdentityInfo discloses the independent broker applicability read
// without exposing the held contract ID or raw broker StockType.
type EarningsIdentityInfo struct {
	Outcome              string         `json:"outcome"`
	NotApplicable        bool           `json:"not_applicable,omitempty"`
	AttemptedAt          time.Time      `json:"attempted_at,omitzero"`
	ProofObservedAt      time.Time      `json:"proof_observed_at,omitzero"`
	ProofOutcome         string         `json:"proof_outcome,omitempty"`
	AuthorityRevision    int64          `json:"authority_revision,omitempty"`
	AuthorityFingerprint string         `json:"authority_fingerprint,omitempty"`
	ObservationID        string         `json:"observation_id,omitempty"`
	AuthorityBinding     string         `json:"authority_binding,omitempty"`
	NextAttempt          *time.Time     `json:"next_attempt,omitempty"`
	LastFailure          *SourceFailure `json:"last_failure,omitempty"`
}

// BuildEarningsIdentityAuthorityBinding binds one public earnings projection
// to the exact symbol and opaque proof receipt it describes. The digest exposes
// neither the raw database receipt ID nor broker identity fields; consumers can
// recompute it to reject cross-symbol or cross-proof substitution.
func BuildEarningsIdentityAuthorityBinding(symbol string, identity EarningsIdentityInfo) string {
	if strings.TrimSpace(symbol) == "" || strings.TrimSpace(symbol) != symbol ||
		identity.AuthorityRevision <= 0 || strings.TrimSpace(identity.AuthorityFingerprint) == "" ||
		identity.ProofObservedAt.IsZero() || identity.ProofOutcome != EarningsStatusNotApplicable ||
		strings.TrimSpace(identity.ObservationID) == "" {
		return ""
	}
	payload, err := json.Marshal(struct {
		Kind                 string    `json:"kind"`
		Version              int       `json:"version"`
		Symbol               string    `json:"symbol"`
		AuthorityRevision    int64     `json:"authority_revision"`
		AuthorityFingerprint string    `json:"authority_fingerprint"`
		ProofObservedAt      time.Time `json:"proof_observed_at"`
		ProofOutcome         string    `json:"proof_outcome"`
		ObservationID        string    `json:"observation_id"`
	}{
		Kind: "earnings_identity_authority_binding", Version: 1,
		Symbol: symbol, AuthorityRevision: identity.AuthorityRevision,
		AuthorityFingerprint: identity.AuthorityFingerprint, ProofObservedAt: identity.ProofObservedAt.UTC(),
		ProofOutcome: identity.ProofOutcome, ObservationID: identity.ObservationID,
	})
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(payload)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// EarningsTerminalInfo is compiled, reviewed evidence that one exact broker
// contract no longer has a future issuer earnings cycle. It is deliberately
// contract-bound rather than symbol-wide: ticker reuse or a different listing
// must fall back to ordinary provider resolution. RevalidateAfter is a hard
// fail-closed boundary; expired evidence becomes unknown until the catalog is
// reviewed and updated. AuthorityReviewedAt is the monotonic catalog watermark
// that also survives explicit record revocation.
type EarningsTerminalInfo struct {
	ContractConID        int                         `json:"contract_con_id"`
	Issuer               string                      `json:"issuer"`
	CIK                  string                      `json:"cik,omitempty"`
	Classification       string                      `json:"classification"`
	EffectiveDate        string                      `json:"effective_date"`
	VerifiedAt           time.Time                   `json:"verified_at"`
	RevalidateAfter      time.Time                   `json:"revalidate_after"`
	AuthorityRevision    int64                       `json:"authority_revision"`
	AuthorityReviewedAt  time.Time                   `json:"authority_reviewed_at"`
	AuthorityFingerprint string                      `json:"authority_fingerprint"`
	AuthorityBinding     string                      `json:"authority_binding,omitempty"`
	Evidence             []EarningsEvidenceReference `json:"evidence"`
}

// BuildEarningsTerminalAuthorityBinding binds one public terminal projection
// to the exact symbol and contract authority it describes. The digest excludes
// issuer text and evidence prose while retaining every typed field needed to
// reject cross-symbol or cross-contract substitution.
func BuildEarningsTerminalAuthorityBinding(symbol string, terminal EarningsTerminalInfo) string {
	if strings.TrimSpace(symbol) == "" || strings.TrimSpace(symbol) != symbol ||
		terminal.ContractConID <= 0 || terminal.AuthorityRevision <= 0 ||
		strings.TrimSpace(terminal.AuthorityFingerprint) == "" ||
		strings.TrimSpace(terminal.EffectiveDate) == "" ||
		strings.TrimSpace(terminal.Classification) == "" || terminal.VerifiedAt.IsZero() ||
		terminal.AuthorityReviewedAt.IsZero() || terminal.RevalidateAfter.IsZero() {
		return ""
	}
	payload, err := json.Marshal(struct {
		Kind                 string    `json:"kind"`
		Version              int       `json:"version"`
		Symbol               string    `json:"symbol"`
		ContractConID        int       `json:"contract_con_id"`
		AuthorityRevision    int64     `json:"authority_revision"`
		AuthorityFingerprint string    `json:"authority_fingerprint"`
		EffectiveDate        string    `json:"effective_date"`
		VerifiedAt           time.Time `json:"verified_at"`
		AuthorityReviewedAt  time.Time `json:"authority_reviewed_at"`
		RevalidateAfter      time.Time `json:"revalidate_after"`
		Classification       string    `json:"classification"`
	}{
		Kind: "earnings_terminal_authority_binding", Version: 1,
		Symbol: symbol, ContractConID: terminal.ContractConID,
		AuthorityRevision: terminal.AuthorityRevision, AuthorityFingerprint: terminal.AuthorityFingerprint,
		EffectiveDate: terminal.EffectiveDate, VerifiedAt: terminal.VerifiedAt.UTC(),
		AuthorityReviewedAt: terminal.AuthorityReviewedAt.UTC(), RevalidateAfter: terminal.RevalidateAfter.UTC(),
		Classification: terminal.Classification,
	})
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(payload)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// EarningsEvidenceReference is one allowlisted primary-source document used
// by the terminal classification. These strings are compiled authority, not
// instructions parsed from live provider content.
type EarningsEvidenceReference struct {
	Authority string `json:"authority"`
	Document  string `json:"document"`
	URL       string `json:"url"`
}

// EarningsProviderInfo is one provider's latest typed outcome. A transport
// failure may coexist with a retained LastGoodDate, but Date is populated only
// when the latest attempt itself returned a usable date.
type EarningsProviderInfo struct {
	Provider     string         `json:"provider"`
	Status       string         `json:"status"`
	Date         string         `json:"date,omitempty"`
	TimeOfDay    string         `json:"time_of_day,omitempty"`
	Estimated    bool           `json:"estimated,omitempty"`
	ObservedAt   time.Time      `json:"observed_at,omitzero"`
	AttemptedAt  time.Time      `json:"attempted_at,omitzero"`
	NextAttempt  *time.Time     `json:"next_attempt,omitempty"`
	LastGoodDate string         `json:"last_good_date,omitempty"`
	LastFailure  *SourceFailure `json:"last_failure,omitempty"`
}

// RulesResult is the rules.snapshot payload. Rows come from the pure
// internal/risk evaluator; this envelope adds provenance and input health.
type RulesResult struct {
	AsOf time.Time `json:"as_of"`
	// Enabled mirrors features.rulebook.enabled; when false Rules is empty
	// and Status says disabled.
	Enabled bool   `json:"enabled"`
	Status  string `json:"status"` // ok | degraded | disabled
	// Rules holds all rows in rulebook order; Ranked holds indexes into
	// Rules sorted hardest-first so renderers agree on ordering without
	// re-deriving it.
	Rules  []risk.RuleRow `json:"rules"`
	Ranked []int          `json:"ranked,omitempty"`
	// BreachCounts summarizes row counts by status for compact surfaces.
	BreachCounts map[string]int `json:"breach_counts,omitempty"`
	// InputHealth is the result-level gate: when positions or account are
	// pending/stale/absent every portfolio-dependent row is unknown, never
	// pass. Canonical snapshots carry exactly one entry for account,
	// positions, earnings, regime_stage, and tape.
	InputHealth []SourceHealth `json:"input_health,omitempty"`
	Earnings    []EarningsInfo `json:"earnings,omitempty"`
	// Policy provenance, mirroring proposals/Stress.
	PolicyID          string       `json:"policy_id"`
	PolicyVersion     int          `json:"policy_version"`
	PolicyFingerprint *Fingerprint `json:"policy_fingerprint,omitempty"`
	// BaseCurrency scopes every *_base impact figure.
	BaseCurrency string `json:"base_currency,omitempty"`
}
