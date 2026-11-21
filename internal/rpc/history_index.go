package rpc

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/osauer/canary/v3/internal/marketcal"
	"github.com/osauer/canary/v3/internal/risk"
	"io"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"
)

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

// EquityDayEntry is one derived statement-equity day. The newest statement
// decides the value; SourceStmt names the oldest retained file in the unbroken
// run that reported that same value, and WhenGenerated is its generation.
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

// Market-event method, schema, kind, flag, status, severity, and role constants
// form the stable allowlisted vocabulary of the market-events wire contract.
const (
	MethodMarketEventsSnapshot = "market_events.snapshot"

	MarketEventsKind = "ibkr.market_events"
	// MarketEventsSchemaVersion identifies a stable wire schema.
	MarketEventsSchemaVersion = "market-events-v1"
	// MarketEventsFingerprintVersion identifies a semantic fingerprint projection.
	MarketEventsFingerprintVersion = "market-events-fp-v3"

	MarketEventBorrowInventoryTight = "borrow_inventory_tight"
	MarketEventBorrowFeeExtreme     = "borrow_fee_extreme"
	MarketEventRegSHOThreshold      = "reg_sho_threshold"
	MarketEventLULDPause            = "luld_pause"
	MarketEventLULDRecent           = MarketEventLULDPause
	MarketEventHaltRegulatoryOrNews = "halt_regulatory_or_news"

	// MarketEventStatusActive and MarketEventStatusRecent are the whole
	// flag lifecycle: Active → Recent → dropped after the retention
	// window. Source-row health uses the SourceStatus* family instead.
	MarketEventStatusActive = "active"
	MarketEventStatusRecent = "recent"

	MarketEventSeverityWatch = "watch"
	MarketEventSeverityAct   = "act"
	MarketEventSeverityBlock = "block"

	MarketEventRoleContext          = "context"
	MarketEventRoleProposalModifier = "proposal_modifier"
	MarketEventRoleHardBlocker      = "hard_blocker"

	BorrowFeeCoverageGlobal        = "global"
	BorrowFeeCoveragePortfolioOnly = "portfolio_only"

	BorrowFeeCoverageObserved     = "observed"
	BorrowFeeCoverageMissing      = "missing"
	BorrowFeeCoverageNotEntitled  = "not_entitled"
	BorrowFeeCoverageUnavailable  = "unavailable"
	BorrowFeeCoverageStale        = "stale"
	BorrowFeeCoverageScaleUnknown = "scale_unverified"

	BorrowFeeSourceBulkShortStock = "ibkr_short_stock_availability"
	BorrowFeeSourceTWSHistorical  = "ibkr_tws_historical"
	BorrowFeeDataTypeBulkFeeRate  = "bulk_fee_rate"
	BorrowFeeDataTypeHistorical   = "FEE_RATE"

	BorrowFeeEntitlementObserved    = "observed"
	BorrowFeeEntitlementNotEntitled = "not_entitled"
	BorrowFeeEntitlementUnknown     = "unknown"

	BorrowFeeScalePercentAnnualized = "percent_annualized"
	BorrowFeeScaleUnverified        = "unverified"
)

// MarketEventsParams selects one or more held-name symbols. Empty scope asks
// the daemon for its default observed universe; callers do not select sources.
type MarketEventsParams struct {
	Symbol  string   `json:"symbol,omitempty"`
	Symbols []string `json:"symbols,omitempty"`
}

// MarketEventsResult is the daemon-authored market-event snapshot. Empty flags
// are conclusive only when SourceHealth establishes complete, current coverage.
type MarketEventsResult struct {
	Kind          string                       `json:"kind"`
	SchemaVersion string                       `json:"schema_version"`
	AsOf          time.Time                    `json:"as_of"`
	Symbols       []string                     `json:"symbols,omitempty"`
	Flags         []MarketEventFlag            `json:"flags,omitempty"`
	BySymbol      map[string][]MarketEventFlag `json:"by_symbol,omitempty"`
	SourceHealth  []SourceHealth               `json:"source_health,omitempty"`
	// BorrowFeeCoverage makes source scope and exact-contract completeness
	// explicit. In particular, portfolio-only historical FEE_RATE rows remain
	// policy-ineligible until their broker numeric scale is commissioned.
	BorrowFeeCoverage []MarketEventBorrowFeeCoverage `json:"borrow_fee_coverage,omitempty"`
	Fingerprint       Fingerprint                    `json:"fingerprint,omitzero"`
	WarningDetails    []DataWarning                  `json:"warning_details,omitempty"`
	NotExecution      string                         `json:"not_execution,omitempty"`
}

// MarketEventBorrowFeeCoverage is one typed borrow-fee observation or gap.
// ContractConID and ContractFingerprint are absent only for symbol-level gaps
// where no exact currently-held short-stock contract was available. FeeRate is
// nullable so unavailable evidence can never collapse to a zero fee.
type MarketEventBorrowFeeCoverage struct {
	Symbol              string         `json:"symbol"`
	ContractConID       int            `json:"contract_con_id,omitempty"`
	ContractFingerprint string         `json:"contract_fingerprint,omitempty"`
	CoverageScope       string         `json:"coverage_scope"`
	Status              string         `json:"status"`
	Reason              string         `json:"reason,omitempty"`
	Source              string         `json:"source"`
	DataType            string         `json:"data_type"`
	AsOf                time.Time      `json:"as_of,omitzero"`
	ObservedAt          time.Time      `json:"observed_at,omitzero"`
	FeeRate             *float64       `json:"fee_rate,omitempty"`
	Entitlement         string         `json:"entitlement"`
	ScaleStatus         string         `json:"scale_status"`
	PolicyEligible      bool           `json:"policy_eligible"`
	LastFailure         *SourceFailure `json:"last_failure,omitempty"`
}

// MarketEventFlag is one allowlisted observed-data finding. Optional timestamps
// and Value remain absent when unavailable rather than being zero-filled.
type MarketEventFlag struct {
	ID             string        `json:"id"`
	Symbol         string        `json:"symbol"`
	Label          string        `json:"label"`
	Status         string        `json:"status"`
	Severity       string        `json:"severity"`
	Role           string        `json:"role"`
	Source         string        `json:"source"`
	SourceURL      string        `json:"source_url,omitempty"`
	AsOf           time.Time     `json:"as_of,omitzero"`
	ObservedAt     time.Time     `json:"observed_at,omitzero"`
	ExpiresAt      time.Time     `json:"expires_at,omitzero"`
	Value          *float64      `json:"value,omitempty"`
	Unit           string        `json:"unit,omitempty"`
	Details        []string      `json:"details,omitempty"`
	WarningDetails []DataWarning `json:"warning_details,omitempty"`
}

// RegimeAuthorityStatus classifies the availability of the daemon-owned
// last-good regime snapshot. It is intentionally independent of any one
// indicator or upstream source.
type RegimeAuthorityStatus string

// Regime authority failure codes identify why a complete last-good snapshot is
// unavailable or why a refresh could not be published.
const (
	// RegimeAuthorityUnavailable means no complete last-good snapshot exists.
	// A regime.snapshot request in this state fails with CodeRegimeUnavailable;
	// the value is retained for typed diagnostics and cache-state tests.
	RegimeAuthorityUnavailable RegimeAuthorityStatus = "unavailable"
	// RegimeAuthorityFresh means the response is the current last-good snapshot
	// and remains within the daemon's configured freshness window.
	RegimeAuthorityFresh RegimeAuthorityStatus = "fresh"
	// RegimeAuthorityStale means the daemon served an intact last-good snapshot
	// outside its freshness window. It must never mean a partial refresh.
	RegimeAuthorityStale RegimeAuthorityStatus = "stale"
)

// RegimeAuthorityFailureCode is a stable, redacted classification of why the
// authority has no newer complete snapshot. Raw source, broker, path, or
// persistence error text does not belong on this contract.
type RegimeAuthorityFailureCode string

// Regime authority failure codes distinguish absence, refresh failure, publish
// failure, invalid persistence, and invalid wall-clock evidence.
const (
	RegimeAuthorityFailureNone                  RegimeAuthorityFailureCode = ""
	RegimeAuthorityFailureNoLastGood            RegimeAuthorityFailureCode = "no_last_good"
	RegimeAuthorityFailureRefreshTimeout        RegimeAuthorityFailureCode = "refresh_timeout"
	RegimeAuthorityFailureRefreshIncomplete     RegimeAuthorityFailureCode = "refresh_incomplete"
	RegimeAuthorityFailureRefreshFailed         RegimeAuthorityFailureCode = "refresh_failed"
	RegimeAuthorityFailurePublishFailed         RegimeAuthorityFailureCode = "publish_failed"
	RegimeAuthorityFailureInvalidPersistedState RegimeAuthorityFailureCode = "invalid_persisted_state"
	// RegimeAuthorityFailureClockInvalid means the authority's last successful
	// commit is ahead of the daemon's current wall clock. The intact snapshot is
	// retained as stale context, but refresh and publication stay fail-closed
	// until the clock catches up.
	RegimeAuthorityFailureClockInvalid RegimeAuthorityFailureCode = "clock_invalid"
)

// RegimeAuthorityHealth is the source-neutral projection of the daemon's
// regime snapshot authority. LastSuccessAgeSeconds is a pointer because zero
// is meaningful immediately after a successful publish, while nil means no
// last-good snapshot has ever been accepted.
//
// Refreshing reports an authority-owned refresh. It is not tied to the
// lifetime of the request that observed it. FailureCode classifies the latest
// failed attempt or the reason no last-good snapshot exists; it does not
// invalidate an existing last-good snapshot.
type RegimeAuthorityHealth struct {
	Status                RegimeAuthorityStatus      `json:"status"`
	Refreshing            bool                       `json:"refreshing"`
	LastSuccessAt         *time.Time                 `json:"last_success_at,omitempty"`
	LastSuccessAgeSeconds *int64                     `json:"last_success_age_seconds,omitempty"`
	FailureCode           RegimeAuthorityFailureCode `json:"failure_code,omitempty"`
}

// ValidateRegimeAuthorityHealth rejects ambiguous or internally inconsistent
// projections before they cross an adapter boundary.
func ValidateRegimeAuthorityHealth(health RegimeAuthorityHealth) error {
	switch health.Status {
	case RegimeAuthorityUnavailable, RegimeAuthorityFresh, RegimeAuthorityStale:
	default:
		return fmt.Errorf("invalid regime authority status %q", health.Status)
	}
	if !validRegimeAuthorityFailureCode(health.FailureCode) {
		return fmt.Errorf("invalid regime authority failure code %q", health.FailureCode)
	}

	hasSuccessTime := health.LastSuccessAt != nil
	hasSuccessAge := health.LastSuccessAgeSeconds != nil
	if hasSuccessTime != hasSuccessAge {
		return errors.New("regime authority last-success time and age must appear together")
	}
	if hasSuccessTime {
		if health.LastSuccessAt.IsZero() {
			return errors.New("regime authority last_success_at must not be zero")
		}
		if *health.LastSuccessAgeSeconds < 0 {
			return errors.New("regime authority last_success_age_seconds must not be negative")
		}
	}

	switch health.Status {
	case RegimeAuthorityUnavailable:
		if hasSuccessTime {
			return errors.New("unavailable regime authority must not claim a last-good snapshot")
		}
		if !health.Refreshing && health.FailureCode == RegimeAuthorityFailureNone {
			return errors.New("idle unavailable regime authority requires a failure code")
		}
	case RegimeAuthorityFresh, RegimeAuthorityStale:
		if !hasSuccessTime {
			return errors.New("available regime authority requires last-success time and age")
		}
		if health.FailureCode == RegimeAuthorityFailureNoLastGood || health.FailureCode == RegimeAuthorityFailureInvalidPersistedState {
			return errors.New("available regime authority cannot report a no-last-good failure")
		}
	}
	return nil
}

func validRegimeAuthorityFailureCode(code RegimeAuthorityFailureCode) bool {
	switch code {
	case RegimeAuthorityFailureNone,
		RegimeAuthorityFailureNoLastGood,
		RegimeAuthorityFailureRefreshTimeout,
		RegimeAuthorityFailureRefreshIncomplete,
		RegimeAuthorityFailureRefreshFailed,
		RegimeAuthorityFailurePublishFailed,
		RegimeAuthorityFailureInvalidPersistedState,
		RegimeAuthorityFailureClockInvalid:
		return true
	default:
		return false
	}
}

// MarshalJSON validates authority-state coherence before encoding.
func (health RegimeAuthorityHealth) MarshalJSON() ([]byte, error) {
	if err := ValidateRegimeAuthorityHealth(health); err != nil {
		return nil, err
	}
	type wire RegimeAuthorityHealth
	return json.Marshal(wire(health))
}

// UnmarshalJSON rejects unknown, missing, null, trailing, or incoherent data.
func (health *RegimeAuthorityHealth) UnmarshalJSON(data []byte) error {
	if health == nil {
		return errors.New("cannot unmarshal regime authority health into nil receiver")
	}
	type wire RegimeAuthorityHealth
	var decoded wire
	if err := decodeExactRegimeAuthorityJSONObject(data, &decoded); err != nil {
		return err
	}
	value := RegimeAuthorityHealth(decoded)
	if err := ValidateRegimeAuthorityHealth(value); err != nil {
		return err
	}
	*health = value
	return nil
}

func decodeExactRegimeAuthorityJSONObject(data []byte, destination any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	opening, err := decoder.Token()
	if err != nil {
		return err
	}
	if delimiter, ok := opening.(json.Delim); !ok || delimiter != '{' {
		return errors.New("regime authority health must be a JSON object")
	}

	required := map[string]bool{
		"status":     false,
		"refreshing": false,
	}
	allowed := map[string]struct{}{
		"status":                   {},
		"refreshing":               {},
		"last_success_at":          {},
		"last_success_age_seconds": {},
		"failure_code":             {},
	}
	seen := make(map[string]struct{}, len(allowed))
	object := make(map[string]json.RawMessage, len(allowed))
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return err
		}
		key, ok := token.(string)
		if !ok {
			return errors.New("regime authority health contains a non-string key")
		}
		if _, ok := allowed[key]; !ok {
			return fmt.Errorf("regime authority health contains unknown key %q", key)
		}
		if _, duplicate := seen[key]; duplicate {
			return fmt.Errorf("regime authority health contains duplicate key %q", key)
		}
		seen[key] = struct{}{}
		var raw json.RawMessage
		if err := decoder.Decode(&raw); err != nil {
			return err
		}
		if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			return fmt.Errorf("regime authority health key %q must not be null", key)
		}
		object[key] = raw
		if _, ok := required[key]; ok {
			required[key] = true
		}
	}
	closing, err := decoder.Token()
	if err != nil {
		return err
	}
	if delimiter, ok := closing.(json.Delim); !ok || delimiter != '}' {
		return errors.New("regime authority health object is not closed")
	}
	for key, present := range required {
		if !present {
			return fmt.Errorf("regime authority health is missing key %q", key)
		}
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("regime authority health has trailing JSON")
		}
		return err
	}

	encoded, err := json.Marshal(object)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(encoded, destination); err != nil {
		return err
	}
	return nil
}

// Input currency is the single model for how a regime input reports whether it
// roll-up, never the primitive: judging currency per cluster and consuming it as

// Currency grades separate what a defect does to the read: a fatal-grade unit
const (
	RegimeCurrencyGradeNone    = "none"
	RegimeCurrencyGradeDegrade = "degrade"
	RegimeCurrencyGradeFatal   = "fatal"
)

// RegimeCurrencyMayConfirm reports whether evidence at this currency may
// fail closed until its authority is decided, not inherit confirmation.
func RegimeCurrencyMayConfirm(class string) bool {
	return regimeCurrencyClass(class) == RegimeFreshnessFresh
}

// RegimeCurrencyScheduled reports the two classes that explain an absent newer
// identical authority and callers must treat them alike.
func RegimeCurrencyScheduled(class string) bool {
	switch regimeCurrencyClass(class) {
	case RegimeFreshnessNotDue, RegimeFreshnessPending:
		return true
	default:
		return false
	}
}

// RegimeCurrencyGrade maps a currency class to what it costs the read.
func RegimeCurrencyGrade(class string) string {
	switch regimeCurrencyClass(class) {
	case RegimeFreshnessFresh, RegimeFreshnessNotDue, RegimeFreshnessPending:
		return RegimeCurrencyGradeNone
	case RegimeFreshnessStale:
		return RegimeCurrencyGradeDegrade
	default:
		return RegimeCurrencyGradeFatal
	}
}

func regimeCurrencyClass(class string) string {
	return strings.ToLower(strings.TrimSpace(class))
}

// regimeCurrencyRank orders the classes so a roll-up can take the worst.
// Unknown and empty classes rank with overdue — untyped evidence fails closed.
func regimeCurrencyRank(class string) int {
	switch regimeCurrencyClass(class) {
	case RegimeFreshnessFresh:
		return 0
	case RegimeFreshnessNotDue:
		return 1
	case RegimeFreshnessPending:
		return 2
	case RegimeFreshnessStale:
		return 3
	default:
		return 4
	}
}

func worseRegimeCurrency(a, b string) string {
	if regimeCurrencyRank(b) > regimeCurrencyRank(a) {
		return b
	}
	return a
}

// RegimeRowCurrency is one row's currency: its served cadence class, reconciled
func RegimeRowCurrency(status string, freshness *RegimeFreshness) string {
	if freshness == nil {
		return RegimeFreshnessOverdue
	}
	usable := false
	switch strings.ToLower(strings.TrimSpace(status)) {
	case RegimeStatusOK:
		usable = true
	case RegimeStatusStale:
		// A stale row can carry a scheduled or bounded class, never a fresh one.
		usable = regimeCurrencyClass(freshness.Class) != RegimeFreshnessFresh
	}
	if !usable {
		return RegimeFreshnessOverdue
	}
	switch regimeCurrencyClass(freshness.Class) {
	case RegimeFreshnessFresh:
		return RegimeFreshnessFresh
	case RegimeFreshnessNotDue:
		return RegimeFreshnessNotDue
	case RegimeFreshnessPending:
		return RegimeFreshnessPending
	case RegimeFreshnessStale:
		return RegimeFreshnessStale
	default:
		return RegimeFreshnessOverdue
	}
}

// RegimeClusterCurrency rolls a cluster's rows up to their worst currency. A
func RegimeClusterCurrency(r RegimeSnapshotResult, name string) string {
	rows := regimeLifecycleClusterRows(r, name)
	if len(rows) == 0 {
		return RegimeFreshnessOverdue
	}
	class := RegimeFreshnessFresh
	for _, row := range rows {
		class = worseRegimeCurrency(class, RegimeRowCurrency(row.status, row.freshness))
	}
	switch class {
	case RegimeFreshnessNotDue, RegimeFreshnessPending:
		if !regimeClusterWithinMaxAge(r, name) {
			return RegimeFreshnessOverdue
		}
	}
	return class
}

// regimeClusterWithinMaxAge bounds a scheduled class by the served staleness
// policy: every row of the cluster must be within it (regimeRowWithinMaxAge).
func regimeClusterWithinMaxAge(r RegimeSnapshotResult, name string) bool {
	name = strings.ToLower(strings.TrimSpace(name))
	found := false
	for _, row := range regimeRows(&r) {
		if RegimeIndicatorCluster(row.indicator) != name {
			continue
		}
		found = true
		if !regimeRowWithinMaxAge(r, row) {
			return false
		}
	}
	return found
}

// regimeRowWithinMaxAge bounds one row's scheduled class by its cluster's
// served staleness policy.
func regimeRowWithinMaxAge(r RegimeSnapshotResult, row regimeRow) bool {
	maxAge := RegimeSourceMaxAgeSeconds(RegimeIndicatorCluster(row.indicator))
	if maxAge <= 0 {
		return true
	}
	now := r.AsOf
	// Only the VVIX daily-close producer supplies this calendar proof.
	// Other rows retain their original wall-clock bound.
	if row.indicator == RegimeIndicatorVolOfVol && row.freshness != nil &&
		row.freshness.Class == RegimeFreshnessNotDue && row.freshness.NextDueAt != nil {
		return !now.IsZero() && now.Before(*row.freshness.NextDueAt)
	}
	return now.IsZero() || row.asOf.IsZero() || now.Sub(row.asOf) < time.Duration(maxAge)*time.Second
}

// regimeRowDark reports a row nobody can read: failed, unavailable or
// overdue by its own currency, missing a field it needs to band, or older
// than its cluster's age bound (for VVIX on its dated not-due schedule, past
// its next due close). A valid typed not-due or pending schedule inside the
// bound is not dark, and neither is a reading typed stale: the only one the
// daemon serves is a VIX3M tick carried inside its tolerance (15 minutes,
// VIX within 1%), which degrades readiness and is still a reading. Counted
// as dark, one missed poll beside a yellow co-sign flipped quiet to
// early_warning for one refresh (owner, 2026-11-19 06:19 CET: "Yes, apply").
// A carry past the tolerance is refused and the row is in error, so it is
// dark as before. The row is judged by
// itself, never by its cluster-mate. These are the row-level parts of the
// input verdict (regimeLifecycleInputVerdict); the verdict also reads the
// served data-quality items and source health, which are stamped after the
// summary reads the warning openers, so a defect only they carry is not a
// dark witness.
func regimeRowDark(r RegimeSnapshotResult, row regimeRow) bool {
	if RegimeCurrencyGrade(RegimeRowCurrency(row.status, row.freshness)) == RegimeCurrencyGradeFatal {
		return true
	}
	return regimeSourceMissingRequiredFields(row.status, row.band, row.fieldsMissing) || !regimeRowWithinMaxAge(r, row)
}

// gammaBlockedOnSessionCadenceOnly reports whether the only thing keeping a
// gamma result from ranking is that it was computed for a different session.
// Decided from the typed gate list, not from reason prose: a result blocked on
// coverage, OI, model source, entitlement, or pacing is a real defect however
// its cadence reads.
func gammaBlockedOnSessionCadenceOnly(c *GammaZeroComputed) bool {
	if c == nil {
		return false
	}
	return gammaQualityBlockedOnSessionCadence(c.Quality)
}

func gammaQualityBlockedOnSessionCadence(q *GammaSignalQuality) bool {
	if q == nil {
		return false
	}
	blocked := false
	for _, gate := range q.Gates {
		if gate.Status != GammaQualityGateBlock {
			continue
		}
		switch gate.Name {
		case GammaQualityGateFreshness:
			if !strings.EqualFold(strings.TrimSpace(q.Freshness), GammaFreshnessSessionMismatch) {
				return false
			}
		case GammaQualityGateSPXCoverage:
			// The combined node carries the SPX slice's verdict; descend once.
			spx, ok := q.ByUnderlying["SPX"]
			if !ok || !gammaQualityBlockedOnSessionCadence(&spx) {
				return false
			}
		default:
			return false
		}
		blocked = true
	}
	return blocked
}

// RegimeVIXTapeCurrency is the currency of the VIX day-change leg on its own.
// that times out, and losing it must not demote a live VIX print. A live tick
// is the only current state for a leg that publishes continuously on weekdays.
func RegimeVIXTapeCurrency(r RegimeSnapshotResult) string {
	q := r.VIXTermStructure.VIXQuality
	if q == nil || q.AsOf.IsZero() || r.VIXTermStructure.VIX == nil {
		return RegimeFreshnessOverdue
	}
	if !strings.EqualFold(strings.TrimSpace(q.FreshnessClass), FreshnessLive) {
		return RegimeFreshnessOverdue
	}
	return RegimeFreshnessFresh
}

// RegimeSPYTapeCurrency is the SPY print's own currency, the one test the
// stage vote, the required-input gate and the stress read apply to the SPY
// move. A print dated within the current New York trading date is current,
// live or frozen after the close, until the date rolls over; a prior
// session's print, or no print, is not. The row it rides on (HYG/SPY) and
// that row's cluster-mate do not decide it. A snapshot with no clock of its
// own has no date to compare and keeps the print. Closed dates are the
// session test's (regimeTapeConfirmable), not this one's.
func RegimeSPYTapeCurrency(r RegimeSnapshotResult) string {
	q := r.HYGSPYDivergence.SPYQuality
	if q == nil || q.AsOf.IsZero() || r.HYGSPYDivergence.SPYChangePct == nil {
		return RegimeFreshnessOverdue
	}
	switch strings.ToLower(strings.TrimSpace(q.FreshnessClass)) {
	case FreshnessLive, FreshnessFrozen:
	default:
		return RegimeFreshnessOverdue
	}
	if !r.AsOf.IsZero() && regimeNewYorkDate(q.AsOf) != regimeNewYorkDate(r.AsOf) {
		return RegimeFreshnessOverdue
	}
	return RegimeFreshnessFresh
}

// regimeNewYorkDate is the New York calendar date of t.
func regimeNewYorkDate(t time.Time) string {
	if ny, err := time.LoadLocation("America/New_York"); err == nil {
		t = t.In(ny)
	}
	return t.Format("2006-01-02")
}

// This file is the single copy of regime confirmation policy: eligibility

// Indicator keys, shared with the daemon streak store and the eligibility
const (
	RegimeIndicatorVIXTerm   = "vix_term"
	RegimeIndicatorVolOfVol  = "vol_of_vol"
	RegimeIndicatorHYGSPY    = "hyg_spy"
	RegimeIndicatorCredit    = "credit_spreads"
	RegimeIndicatorFunding   = "funding_stress"
	RegimeIndicatorUSDJPY    = "usdjpy"
	RegimeIndicatorGammaZero = "gamma_zero"
	RegimeIndicatorBreadth   = "breadth"
)

// Cluster indexes for the six-cluster combination. Order is part of the
// contract (lifecycle evidence and source-health rows iterate it).
const (
	RegimeClusterEquityVol = iota
	RegimeClusterCredit
	RegimeClusterFunding
	RegimeClusterFX
	RegimeClusterGamma
	RegimeClusterBreadth
	regimeClusterCount
)

// RegimeClusterNames are the wire names for the six clusters, indexed by the
// RegimeCluster* constants.
var RegimeClusterNames = []string{"vol", "credit", "funding", "fx", "gamma", "breadth"}

// RegimeVerdictFloor is the minimum ranked-cluster count required to claim a
// verdict above "insufficient signal".
const RegimeVerdictFloor = 3

// RegimeCurrencyPolicyVersion identifies the input-currency policy a decision
// event was produced under (internal-docs/design/regime-input-currency.md).
// Behaviour changes in how inputs report currency alter the daily fingerprint
// sequence, so the calibration corpus has to be partitionable: a backtest must
// never blend days either side of a cutover. Bump on every change to how a
// class is assigned or consumed.
const RegimeCurrencyPolicyVersion = "regime-currency-v1"

// RegimeGate is one indicator's confirmation-eligibility policy. Depth units
type RegimeGate struct {
	MinSessions int
	MinDepth    float64
	FastDepth   float64
}

// regimeGates is the per-indicator eligibility policy table from
// never left green. Move them only against a corpus that actually contains
var regimeGates = map[string]RegimeGate{
	// depth = VIX/VIX3M ratio. Inversion is already discrete; fast path on a
	// deep day-one inversion.
	RegimeIndicatorVIXTerm: {MinSessions: 2, MinDepth: 1.00, FastDepth: 1.05},
	// depth = VVIX level.
	// Since the row turns red at 120 (vvix_daily_v3) a red is at the fast
	// depth on its first session, and the minimum depth no longer binds.
	RegimeIndicatorVolOfVol: {MinSessions: 2, MinDepth: 110, FastDepth: 120},
	// depth = percent below the 50DMA ((dma-price)/dma*100). 0.25% is the
	// noise floor; a 1% break is eligible day one.
	RegimeIndicatorHYGSPY: {MinSessions: 2, MinDepth: 0.25, FastDepth: 1.0},
	// depth = HY OAS percent. The red band itself stays the gate: the band's
	RegimeIndicatorCredit: {MinSessions: 1, FastDepth: 6.5},
	// depth = CP−bill spread in bp. Red levels are already deep, streak 1.
	RegimeIndicatorFunding: {MinSessions: 1, FastDepth: 105},
	// depth = weekly yen strengthening in percent (−WeeklyChange). Speed is
	// the depth (≥2%/week), streak 1 by design — August-2024 carry unwinds
	// play out in three sessions. The one deviation from the rule: it yields
	// 2.6, but a calm fortnight of journal already printed 4.58%/week, so the
	// scale would saturate on ordinary weeks. 7.0 is the August-2024 unwind
	// this indicator exists to catch.
	RegimeIndicatorUSDJPY: {MinSessions: 1, FastDepth: 7.0},
	// depth = percent below gamma-zero (−gap_pct); a wholly-short profile
	RegimeIndicatorGammaZero: {MinSessions: 1, MinDepth: 0.5, FastDepth: 4.5},
	// depth = points below the 40% band floor (40 - pct_above_50dma).
	RegimeIndicatorBreadth: {MinSessions: 2, MinDepth: 2.0, FastDepth: 10.0},
}

// GammaIndexWeight is the weight one index carries in the combined SPY+SPX
func GammaIndexWeight(key string, c *GammaZeroComputed) float64 {
	if c != nil && c.GammaTotalAbs > 0 {
		return c.GammaTotalAbs
	}
	if key == "SPX" {
		return 100
	}
	return 1
}

// GammaCombinedGapPct is the gamma-weighted mean of the per-index gaps on a
// combined-scope result. nil when the result is not combined scope or no
// index reports a crossing.
func GammaCombinedGapPct(c *GammaZeroComputed) *float64 {
	if c == nil || c.Scope != GammaZeroScopeCombined || len(c.PerIndex) == 0 {
		return nil
	}
	var sum, weight float64
	for _, key := range []string{"SPY", "SPX"} {
		sub := c.PerIndex[key]
		if sub == nil || sub.GapPct == nil {
			continue
		}
		w := GammaIndexWeight(key, sub)
		sum += *sub.GapPct * w
		weight += w
	}
	if weight <= 0 {
		return nil
	}
	gap := sum / weight
	return &gap
}

// RegimeGammaDepth extracts gamma's eligibility depth in percent below
func RegimeGammaDepth(c *GammaZeroComputed) *float64 {
	if c == nil {
		return nil
	}
	if c.Scope == GammaZeroScopeCombined && len(c.PerIndex) > 0 {
		return gammaCombinedDepth(c)
	}
	return gammaIndexDepth(c)
}

// gammaIndexDepth is one index's depth: percent below its gamma-zero, or the
// extreme a wholly-short profile with no crossing earns — dealers short across
// the whole modelled band is the most amplifying reading gamma has, and it has
// no line left to measure a distance from. nil when the index reports neither.
func gammaIndexDepth(c *GammaZeroComputed) *float64 {
	if c == nil {
		return nil
	}
	if c.GapPct != nil {
		d := -*c.GapPct
		if c.ProfileMetrics != nil {
			d = 0
			if c.ProfileMetrics.GEXAtSpot < 0 {
				d = math.Abs(*c.GapPct)
			}
			if c.ProfileMetrics.GEXAtSpot > 0 {
				d = -math.Abs(*c.GapPct)
			}
		}
		return &d
	}
	if c.GammaSign == "negative" {
		d := 100.0
		return &d
	}
	return nil
}

// gammaCombinedDepth is the |GEX|-weighted mean of the per-index depths. It
func gammaCombinedDepth(c *GammaZeroComputed) *float64 {
	var sum, weight float64
	for _, key := range []string{"SPY", "SPX"} {
		sub := c.PerIndex[key]
		d := gammaIndexDepth(sub)
		if d == nil {
			continue
		}
		w := GammaIndexWeight(key, sub)
		sum += *d * w
		weight += w
	}
	if weight <= 0 {
		return nil
	}
	depth := sum / weight
	return &depth
}

// RegimeIndicatorCluster maps an indicator key to its cluster wire name.
func RegimeIndicatorCluster(indicator string) string {
	switch indicator {
	case RegimeIndicatorVIXTerm, RegimeIndicatorVolOfVol:
		return "vol"
	case RegimeIndicatorHYGSPY, RegimeIndicatorCredit:
		return "credit"
	case RegimeIndicatorFunding:
		return "funding"
	case RegimeIndicatorUSDJPY:
		return "fx"
	case RegimeIndicatorGammaZero:
		return "gamma"
	case RegimeIndicatorBreadth:
		return "breadth"
	default:
		return ""
	}
}

// RegimeEligibilityInput is one red row's gate evidence. Depth is in the
// indicator's gate units; nil means the indicator has no separate depth
// metric (the band threshold is the depth gate). StreakSessions <= 0 is
// treated as 1 (fresh install / deleted store).
type RegimeEligibilityInput struct {
	Indicator      string
	Band           string
	Depth          *float64
	StreakSessions int
	Fresh          bool
	FreshnessClass string
	Latched        bool
}

// EvaluateRegimeEligibility applies the depth/persistence/freshness gates to
// streak once earned, but never overrides freshness: overdue data drops
func EvaluateRegimeEligibility(in RegimeEligibilityInput) *RegimeEligibility {
	if strings.ToLower(strings.TrimSpace(in.Band)) != "red" {
		return nil
	}
	gate, ok := regimeGates[in.Indicator]
	if !ok {
		gate = RegimeGate{MinSessions: 1}
	}
	sessions := max(in.StreakSessions, 1)
	out := &RegimeEligibility{}
	// Currency is an allowlist on fresh, never a denylist of known-bad classes:
	// a class added later must fail closed until its authority is decided
	if !in.Fresh || !RegimeCurrencyMayConfirm(in.FreshnessClass) {
		// A stress witness that earned eligibility in its window keeps it
		// through its own not-due window: no newer observation can exist, so
		// the close of the window is not evidence that the stress ended.
		// Overdue, stale or pending evidence still drops it, and a co-sign's
		// prior-date reading stays context.
		if in.Latched && regimeCurrencyClass(in.FreshnessClass) == RegimeFreshnessNotDue &&
			RegimeIndicatorRole(in.Indicator) == RegimeRoleWitness {
			out.Eligible, out.Latched = true, true
			return out
		}
		// The latch rides along so a reader can tell a confirmation the
		// currency gap blocked from a red that never confirmed.
		out.Latched = in.Latched
		out.Reasons = append(out.Reasons, regimeEligibilityCurrencyReason(in.FreshnessClass))
		return out
	}
	if in.Latched {
		out.Eligible = true
		out.Latched = true
		return out
	}
	fastOK := gate.FastDepth > 0 && in.Depth != nil && *in.Depth >= gate.FastDepth
	depthOK := gate.MinDepth <= 0 || in.Depth == nil || *in.Depth >= gate.MinDepth
	switch {
	case fastOK:
		out.Eligible = true
	case !depthOK:
		out.Reasons = append(out.Reasons, "depth_below_min")
	case sessions < gate.MinSessions:
		out.Reasons = append(out.Reasons, streakReason(sessions, gate.MinSessions))
	default:
		out.Eligible = true
	}
	return out
}

// regimeEligibilityCurrencyReason names the currency that blocked confirmation.
// The reason tokens are the stable vocabulary renderers and the decisions
// journal already carry; a class with no token of its own reports data_overdue,
// which is what it costs the row.
func regimeEligibilityCurrencyReason(class string) string {
	switch strings.ToLower(strings.TrimSpace(class)) {
	case RegimeFreshnessNotDue:
		return "data_not_due"
	case RegimeFreshnessPending:
		return "data_refresh_pending"
	case RegimeFreshnessStale:
		return "data_stale"
	default:
		return "data_overdue"
	}
}

func streakReason(sessions, want int) string {
	return "streak_" + strconv.Itoa(sessions) + "_of_" + strconv.Itoa(want)
}

// RegimeClusterBands is the shared cluster combination: Raw worst-of row
// gates. Eligible[i] is only meaningful where Raw[i] == "red".
type RegimeClusterBands struct {
	Raw       []string
	Confirmed []string
	Eligible  []bool
	// witness is each cluster's band as a stress witness, "" where the
	// cluster has none (see BuildRegimeClusterBands).
	witness []string
	// cashWidening reports a current cash credit row whose HY OAS has widened
	// by RegimeCreditWideningYellowPP or more over 20 observations.
	cashWidening bool
	// rows is what each red row is worth, by indicator (RowRed).
	rows map[string]string
	// witnessEligible marks the clusters whose witness row is an eligible red.
	witnessEligible []bool
	// darkWitnesses and weakCoSigns list, in row order, the witness rows
	// nobody can read (regimeWitnessRowDark) and the co-sign rows served at
	// yellow or red. Together they open the dark-witness warning.
	darkWitnesses, weakCoSigns []string
	// darkWarns is the dark-witness warning: a dark witness beside a current
	// weak co-sign, in fewer witness clusters than the blanking floor.
	darkWarns bool
	// witnessRedLit marks the clusters with a witness row that is red and
	// not dark; witnessStandsFresh, those with a witness row that is an
	// eligible red on a current reading. A witness is judged by its own row,
	// whatever its cluster-mate serves.
	witnessRedLit, witnessStandsFresh []bool
}

// RegimeContextNote is how every surface words a co-sign red with no stress
// witness beside it. The daemon serves it with the rows it applies to, so no
// reader keeps a copy. The sentence, and the "Normal regime" headline and
// AMBER row label that go with it, are approved (owner, 2026-11-19 06:19
// CET: "Approve as written").
const RegimeContextNote = "Context, not a stress reading on its own"

// Roles of a regime row. A stress witness measures stress directly and can
// start a warning. A co-sign strengthens what a witness has started and
// opens nothing by itself.
const (
	RegimeRoleWitness = "witness"
	RegimeRoleCoSign  = "co_sign"
)

// regimeRowRoles is the one list of which row has which role. The cluster
// combination, the lifecycle, the stress read, the regime summary and the
// row notes all read it.
var regimeRowRoles = map[string]string{
	RegimeIndicatorVIXTerm:   RegimeRoleWitness,
	RegimeIndicatorVolOfVol:  RegimeRoleWitness,
	RegimeIndicatorCredit:    RegimeRoleWitness,
	RegimeIndicatorFunding:   RegimeRoleWitness,
	RegimeIndicatorHYGSPY:    RegimeRoleCoSign,
	RegimeIndicatorUSDJPY:    RegimeRoleCoSign,
	RegimeIndicatorGammaZero: RegimeRoleCoSign,
	RegimeIndicatorBreadth:   RegimeRoleCoSign,
}

// RegimeIndicatorRole returns a regime row's role.
func RegimeIndicatorRole(indicator string) string {
	return regimeRowRoles[indicator]
}

// RegimeWitnessNames lists the reader's names of the stress witness rows in
// row order, from the one role list.
func RegimeWitnessNames() []string {
	var out []string
	for _, row := range regimeRows(&RegimeSnapshotResult{}) {
		if RegimeIndicatorRole(row.indicator) == RegimeRoleWitness {
			out = append(out, RegimeRowName(row.indicator))
		}
	}
	return out
}

// What a red row is worth in the combination. A red that counts is eligible
// and, for a co-sign, has a standing witness beside it. A red that is waiting
// may still count: a witness red that is not eligible yet, or a co-sign red
// beside a standing witness that has not passed its own gate. A co-sign red
// with no witness standing is context.
//
// Beside a stress witness nobody can read, a co-sign red with no witness
// standing is weak: the dark-witness warning names it as its cause.
const (
	RegimeRedCounts  = "counts"
	RegimeRedWaiting = "waiting"
	RegimeRedContext = "context"
	RegimeRedWeak    = "weak"
)

// RegimeWeakNote is how every surface words a co-sign red that a
// dark-witness warning names as its cause.
const RegimeWeakNote = "Weak beside a stress reading that is unavailable"

// RowRed says what the red of one row is worth (RegimeRedCounts,
// RegimeRedWaiting, RegimeRedContext), or "" when the row is not red.
func (b RegimeClusterBands) RowRed(indicator string) string {
	return b.rows[indicator]
}

// RegimeRowStanding is one row's role and, when the row is red, what that
// red is worth in the cluster combination. Note carries the context sentence
// for a red that is context, so a reader prints it without a copy of its own.
type RegimeRowStanding struct {
	Indicator string `json:"indicator"`
	Name      string `json:"name"`
	Role      string `json:"role"`
	Red       string `json:"red,omitempty"`
	Note      string `json:"note,omitempty"`
}

// RegimeRowStandings derives the standings of the eight rows from the one
// place that decides them, BuildRegimeClusterBands.
func RegimeRowStandings(r *RegimeSnapshotResult) []RegimeRowStanding {
	if r == nil {
		return nil
	}
	cb := BuildRegimeClusterBands(r)
	rows := regimeRows(r)
	out := make([]RegimeRowStanding, 0, len(rows))
	for _, row := range rows {
		standing := RegimeRowStanding{
			Indicator: row.indicator, Name: RegimeRowName(row.indicator),
			Role: RegimeIndicatorRole(row.indicator), Red: cb.RowRed(row.indicator),
		}
		switch standing.Red {
		case RegimeRedContext:
			standing.Note = RegimeContextNote
		case RegimeRedWeak:
			standing.Note = RegimeWeakNote
		}
		out = append(out, standing)
	}
	return out
}

// regimeRow is one served row as the combination reads it.
type regimeRow struct {
	indicator string
	cluster   int
	band      string
	eligible  bool
	status    string
	freshness *RegimeFreshness
	asOf      time.Time
	// fieldsMissing is the row's own list of required fields it could not
	// fill (RegimeVIXTerm.FieldsMissing and its siblings).
	fieldsMissing []string
	// latchedBlocked marks a red whose streak had earned eligibility and may
	// not confirm now: its input is dark or not current.
	latchedBlocked bool
}

// regimeRows lists the eight rows in indicator order with their served
// bands. Dealer gamma votes only with a rankable profile.
func regimeRows(r *RegimeSnapshotResult) []regimeRow {
	row := func(indicator string, cluster int, meta RegimeIndicatorMeta, status string, fieldsMissing []string) regimeRow {
		return regimeRow{
			indicator: indicator, cluster: cluster, band: meta.Band, eligible: redEligible(meta), status: status,
			freshness: meta.Freshness, asOf: metaAsOf(meta).Time, fieldsMissing: fieldsMissing,
			latchedBlocked: meta.Band == "red" && meta.Eligibility != nil && meta.Eligibility.Latched && !meta.Eligibility.Eligible,
		}
	}
	gamma := row(RegimeIndicatorGammaZero, RegimeClusterGamma, r.GammaZero.RegimeIndicatorMeta, r.GammaZero.Status, r.GammaZero.FieldsMissing)
	gamma.band, gamma.eligible = rankableLifecycleGammaBand(r.GammaZero), gammaRedEligible(r.GammaZero)
	return []regimeRow{
		row(RegimeIndicatorVIXTerm, RegimeClusterEquityVol, r.VIXTermStructure.RegimeIndicatorMeta, r.VIXTermStructure.Status, r.VIXTermStructure.FieldsMissing),
		row(RegimeIndicatorVolOfVol, RegimeClusterEquityVol, r.VolOfVol.RegimeIndicatorMeta, r.VolOfVol.Status, nil),
		row(RegimeIndicatorHYGSPY, RegimeClusterCredit, r.HYGSPYDivergence.RegimeIndicatorMeta, r.HYGSPYDivergence.Status, r.HYGSPYDivergence.FieldsMissing),
		row(RegimeIndicatorCredit, RegimeClusterCredit, r.CreditSpreads.RegimeIndicatorMeta, r.CreditSpreads.Status, r.CreditSpreads.FieldsMissing),
		row(RegimeIndicatorFunding, RegimeClusterFunding, r.FundingStress.RegimeIndicatorMeta, r.FundingStress.Status, r.FundingStress.FieldsMissing),
		row(RegimeIndicatorUSDJPY, RegimeClusterFX, r.USDJPY.RegimeIndicatorMeta, r.USDJPY.Status, r.USDJPY.FieldsMissing),
		gamma,
		row(RegimeIndicatorBreadth, RegimeClusterBreadth, r.Breadth.RegimeIndicatorMeta, r.Breadth.Status, r.Breadth.FieldsMissing),
	}
}

// RegimeCreditWideningYellowPP is the 20-observation HY OAS widening, in
// percentage points, at which the cash credit row turns yellow. It is the one
// copy of that number: the row classifier bands on it, and from the same step
// the cash row opens a warning as a stress witness (BuildRegimeClusterBands).
const RegimeCreditWideningYellowPP = 0.50

// regimeRowNames is the reader's name of each regime row. The regime summary
// and the stress read word their sentences from it, and the stress read
// serves those names to the app, so no surface keeps a list of its own.
var regimeRowNames = map[string]string{
	RegimeIndicatorVIXTerm:   "volatility term structure",
	RegimeIndicatorVolOfVol:  "vol-of-vol",
	RegimeIndicatorHYGSPY:    "ETF credit proxy",
	RegimeIndicatorCredit:    "cash credit spreads",
	RegimeIndicatorFunding:   "funding spread",
	RegimeIndicatorUSDJPY:    "FX carry proxy",
	RegimeIndicatorGammaZero: "dealer gamma",
	RegimeIndicatorBreadth:   "breadth",
}

// RegimeRowName returns the reader's name of a regime row.
func RegimeRowName(indicator string) string {
	return regimeRowNames[indicator]
}

// RegimeCashWideningName names a widening of cash credit spreads where a
// surface names what warns without confirming.
func RegimeCashWideningName() string {
	return RegimeRowName(RegimeIndicatorCredit) + " widening"
}

// EligibleRedCount counts clusters that survive downgrades as red AND carry
// eligible evidence — the only reds that may confirm stress.
func (b RegimeClusterBands) EligibleRedCount() int {
	n := 0
	for i, band := range b.Confirmed {
		if band == "red" && i < len(b.Eligible) && b.Eligible[i] {
			n++
		}
	}
	return n
}

// witnessStands reports a cluster whose own stress witness is an eligible
// red, as distinct from a cluster that confirms on a co-sign row.
func (b RegimeClusterBands) witnessStands(i int) bool {
	return b.confirms(i) && i < len(b.witness) && b.witness[i] == "red" && i < len(b.witnessEligible) && b.witnessEligible[i]
}

// confirms reports a cluster that is red and may confirm stress.
func (b RegimeClusterBands) confirms(i int) bool {
	return i < len(b.Confirmed) && b.Confirmed[i] == "red" && i < len(b.Eligible) && b.Eligible[i]
}

// witnessRed reports a stress witness in cluster i whose red does not
// confirm: it is provisional or does not stand. A cluster that is red only
// through a co-sign row is not one, whatever else its witness shows.
func (b RegimeClusterBands) witnessRed(i int) bool {
	return i < len(b.witness) && b.witness[i] == "red" && !b.confirms(i)
}

// widening reports cash credit widening in cluster i while the cluster does
// not confirm: a witness that warns without a red.
func (b RegimeClusterBands) widening(i int) bool {
	return i == RegimeClusterCredit && b.cashWidening && !b.confirms(i)
}

// provisionalWitnessCount counts the stress witnesses that warn without
// confirming: a witness red or cash credit widening, once per cluster.
// Co-sign reds are context and are not counted.
//
// A witness nobody can read beside a weak co-sign warns without confirming
// too (darkWitnessWarns), once per cluster it is in.
func (b RegimeClusterBands) provisionalWitnessCount() int {
	n := 0
	for i := range b.witness {
		if b.witnessRed(i) || b.widening(i) || (b.darkWitnessWarns() && b.darkIn(i)) {
			n++
		}
	}
	return n
}

// darkWitnessWarns reports a stress witness nobody can read beside a co-sign
// at yellow or red. Owner ruling 2026-11-18 14:47 CET: asked "When a real
// stress input is dark and weak readings are yellow or red, what should
// Regime say?", the owner answered "Early warning". It keeps the operator
// decision of 2026-08-11 that unknown cash evidence never softens a live
// warning: what cannot be read is assumed to be the worst. One exception,
// in the same direction: when the dark rows hid a confirmation, the stage
// blanks to data_quality and the rulebook keeps the stricter latch
// (regimeLifecycleOutageHidesConfirmation, owner instruction 2026-11-21).
func (b RegimeClusterBands) darkWitnessWarns() bool {
	return b.darkWarns
}

// darkIn reports a dark witness row in cluster i.
func (b RegimeClusterBands) darkIn(i int) bool {
	return slices.ContainsFunc(b.darkWitnesses, func(indicator string) bool {
		return i < len(RegimeClusterNames) && RegimeIndicatorCluster(indicator) == RegimeClusterNames[i]
	})
}

// RegimeDarkWitnessCause names what a dark-witness warning rests on, by the
// reader's names in row order: the witness rows nobody can read and the
// co-sign rows at yellow or red. Both are empty when the warning is not open.
func RegimeDarkWitnessCause(r *RegimeSnapshotResult) (unavailable, weak []string) {
	cb := BuildRegimeClusterBands(r)
	if !cb.darkWitnessWarns() {
		return nil, nil
	}
	for _, indicator := range cb.darkWitnesses {
		unavailable = append(unavailable, RegimeRowName(indicator))
	}
	for _, indicator := range cb.weakCoSigns {
		weak = append(weak, RegimeRowName(indicator))
	}
	return unavailable, weak
}

// witnessYellows counts the clusters whose stress witness reads yellow.
func (b RegimeClusterBands) witnessYellows() int {
	n := 0
	for _, band := range b.witness {
		if band == "yellow" {
			n++
		}
	}
	return n
}

// BuildRegimeClusterBands combines served row bands into the six cluster
// bands and gives each red the weight of its role (regimeRowRoles).
//
// A stress witness measures stress directly: the two equity-vol rows,
// funding, and the cash credit row. A witness stands when its red is
// eligible.
//
// The HYG proxy, the yen, dealer gamma and breadth are co-signs: they
// strengthen what a witness has started. A co-sign red stays red, and may
// confirm, only while a witness stands; otherwise its cluster reads yellow.
// Co-signs never corroborate each other and never rescue a witness. In the
// history replay of 2026-11-18 the proxy was red on 30% of sessions and the
// market did no worse after it than after any other session
// (internal-docs/design/regime-calibration.md, "Stress witnesses and
// co-signs").
func BuildRegimeClusterBands(r *RegimeSnapshotResult) RegimeClusterBands {
	if r == nil {
		return RegimeClusterBands{}
	}
	rows := regimeRows(r)
	raw := make([]string, regimeClusterCount)
	witness := make([]string, regimeClusterCount)
	eligible := make([]bool, regimeClusterCount)
	witnessEligible := make([]bool, regimeClusterCount)
	witnessRedLit := make([]bool, regimeClusterCount)
	witnessStandsFresh := make([]bool, regimeClusterCount)
	var darkWitnesses, weakCoSigns []string
	stands := false
	for _, row := range rows {
		raw[row.cluster] = strongestLifecycleBand(raw[row.cluster], row.band)
		if RegimeIndicatorRole(row.indicator) == RegimeRoleWitness {
			witness[row.cluster] = strongestLifecycleBand(witness[row.cluster], row.band)
			witnessEligible[row.cluster] = witnessEligible[row.cluster] || row.eligible
			stands = stands || row.eligible
			dark := regimeRowDark(*r, row)
			if dark {
				darkWitnesses = append(darkWitnesses, row.indicator)
			}
			witnessRedLit[row.cluster] = witnessRedLit[row.cluster] || (row.band == "red" && !dark)
			// Scheduled evidence is current evidence: an eligible red on a
			// valid typed not-due schedule (the latched term inversion after
			// the close) stands as a fresh one does.
			witnessStandsFresh[row.cluster] = witnessStandsFresh[row.cluster] || (row.eligible && !dark)
		} else if (row.band == "yellow" || row.band == "red") && !regimeRowDark(*r, row) {
			// A weak co-sign is a current reading at yellow or red; a band
			// held on an overdue row is not a reading.
			weakCoSigns = append(weakCoSigns, row.indicator)
		}
	}
	darkClusters := map[string]bool{}
	for _, indicator := range darkWitnesses {
		darkClusters[RegimeIndicatorCluster(indicator)] = true
	}
	// Two or more witness clusters nobody can read are data incomplete by the
	// required-input floor (RegimeCurrencyBlankFloor), not a warning.
	darkWarns := len(darkWitnesses) > 0 && len(weakCoSigns) > 0 && len(darkClusters) < RegimeCurrencyBlankFloor
	confirmed := append([]string(nil), raw...)
	state := map[string]string{}
	for _, row := range rows {
		isWitness := RegimeIndicatorRole(row.indicator) == RegimeRoleWitness
		if row.band == "red" {
			switch {
			case !isWitness && !stands && darkWarns:
				// Beside a witness nobody can read the co-sign is what the
				// warning names: weak, not context.
				state[row.indicator] = RegimeRedWeak
			case !isWitness && !stands:
				state[row.indicator] = RegimeRedContext
			case row.eligible:
				state[row.indicator] = RegimeRedCounts
			default:
				state[row.indicator] = RegimeRedWaiting
			}
		}
		// An eligible red makes its cluster eligible, except the proxy's
		// inside the credit cluster with no witness standing: it cannot make
		// a cash red that may not confirm into one that does.
		if row.eligible && (isWitness || stands || row.cluster != RegimeClusterCredit) {
			eligible[row.cluster] = true
		}
	}
	for i := range confirmed {
		if confirmed[i] == "red" && witness[i] != "red" && !stands {
			confirmed[i] = "yellow"
		}
	}
	// A widening of cash spreads warns from the cash row's own yellow step,
	// before the row is red: in the history replay of 2026-11-18 it preceded
	// a 5% fall on 44% of 36 sessions, in five episodes. Only a current row
	// counts; an old series is a data gap, not a warning.
	widening := RegimeCurrencyMayConfirm(RegimeRowCurrency(r.CreditSpreads.Status, r.CreditSpreads.Freshness)) &&
		pctAtLeast(r.CreditSpreads.HY20DChange, RegimeCreditWideningYellowPP)
	return RegimeClusterBands{
		Raw: raw, Confirmed: confirmed, Eligible: eligible, witness: witness, cashWidening: widening, rows: state,
		witnessEligible: witnessEligible, darkWitnesses: darkWitnesses, weakCoSigns: weakCoSigns, darkWarns: darkWarns,
		witnessRedLit: witnessRedLit, witnessStandsFresh: witnessStandsFresh,
	}
}

func redEligible(meta RegimeIndicatorMeta) bool {
	return meta.Band == "red" && meta.Eligibility != nil && meta.Eligibility.Eligible
}

// gammaRedEligible additionally requires the rankability gate the gamma vote
func gammaRedEligible(g RegimeGammaZero) bool {
	return rankableLifecycleGammaBand(g) == "red" && g.Eligibility != nil && g.Eligibility.Eligible
}

// ApplyRegimeClusterTallies fills the cluster-level counts on a composite
func ApplyRegimeClusterTallies(c *RegimeComposite, cb RegimeClusterBands) {
	if c == nil {
		return
	}
	c.ClusterGreenCount, c.ClusterYellowCount, c.ClusterRedCount = 0, 0, 0
	c.ClusterRankedCount, c.ClusterUnrankedCount = 0, 0
	for _, band := range cb.Confirmed {
		switch band {
		case "green":
			c.ClusterGreenCount++
			c.ClusterRankedCount++
		case "yellow":
			c.ClusterYellowCount++
			c.ClusterRankedCount++
		case "red":
			c.ClusterRedCount++
			c.ClusterRankedCount++
		default:
			c.ClusterUnrankedCount++
		}
	}
	c.ClusterEligibleRedCount = cb.EligibleRedCount()
	c.ClusterProvisionalRedCount = cb.provisionalWitnessCount()
}

// RegimeHeadline is the single wording table for the regime headline. Both
func RegimeHeadline(c RegimeComposite, stage string) string {
	switch {
	case strings.EqualFold(strings.TrimSpace(stage), LifecycleDataQuality):
		return "Market state undefined — data incomplete"
	case c.ClusterRankedCount == 0:
		return "No usable signal yet"
	case c.ClusterRankedCount < RegimeVerdictFloor:
		return "Insufficient signal — too few inputs ready"
	case c.ClusterUnrankedCount == 0 && c.ClusterEligibleRedCount == c.ClusterRankedCount:
		return "Full risk-off conditions"
	case c.ClusterEligibleRedCount >= 3:
		return "Broad stress regime"
	case stageConfirmsStress(stage):
		return "Confirmed stress regime"
	// A calm stage keeps the calm headline: whatever is red or yellow there
	// is a co-sign with no stress witness beside it, shown as context.
	case stageCalm(stage):
		return "Normal regime"
	case c.ClusterRedCount >= 1 || c.ClusterEligibleRedCount >= 1:
		return "Stress signal present"
	// A provisional witness warns without confirming: a witness red the
	// cluster logic held back for depth, persistence or corroboration, or
	// cash credit widening. The headline must not read louder than the
	// composite's own verdict.
	case c.ClusterProvisionalRedCount == 1:
		return "Watch: one unconfirmed stress signal"
	case c.ClusterProvisionalRedCount > 1:
		return "Watch: unconfirmed stress signals"
	case c.ClusterYellowCount >= 3:
		return "Elevated stress watch"
	default:
		return "Normal regime"
	}
}

func stageCalm(stage string) bool {
	switch strings.ToLower(strings.TrimSpace(stage)) {
	case LifecycleQuiet, LifecycleOpportunity, LifecycleStabilization:
		return true
	default:
		return false
	}
}

func stageConfirmsStress(stage string) bool {
	switch strings.ToLower(strings.TrimSpace(stage)) {
	case LifecycleConfirmedStress, LifecyclePanic:
		return true
	default:
		return false
	}
}

// GammaTransitionGapPct is the ± band, in percent of the zero-gamma level,
// inside which dealer positioning reads as transitional rather than long or
// short gamma. Single copy: the daemon gamma rows and every CLI renderer
// classify through GammaRegimeFromGap, and prose that names the band derives
// its number from this constant.
const GammaTransitionGapPct = 2.0

// GammaRegimeFromGap maps the signed spot-vs-zero-gamma gap (percent of the
// zero-gamma level, positive = spot above) to its wire regime label. A nil
// classifier must not claim direction.
func GammaRegimeFromGap(gapPct *float64) string {
	if gapPct == nil {
		return "transition_gamma"
	}
	switch {
	case *gapPct > GammaTransitionGapPct:
		return "long_gamma"
	case *gapPct >= -GammaTransitionGapPct:
		return "transition_gamma"
	default:
		return "short_gamma"
	}
}

// GammaBucketRegime classifies one horizon bucket (0DTE / 1-7 / term) from
func GammaBucketRegime(spot float64, zero *float64, sign string) string {
	if zero != nil && *zero > 0 {
		gap := (spot - *zero) / *zero * 100
		return GammaRegimeFromGap(&gap)
	}
	switch sign {
	case "positive":
		return "long_gamma"
	case "negative":
		return "short_gamma"
	}
	return ""
}

// HeuristicThresholds builds the heuristic/pending-backtest threshold
func HeuristicThresholds(label, green, yellow, red, trip string) *RegimeThresholds {
	return &RegimeThresholds{
		Label:           label,
		Green:           green,
		Yellow:          yellow,
		Red:             red,
		Trip:            trip,
		Heuristic:       true,
		PendingBacktest: true,
	}
}

// regimeThresholdText is one indicator's published band prose. Label is the
// threshold-set version identity, persisted as
// regime_indicators.thresholds_label so the calibration corpus can be
// partitioned by the values a decision was produced under — not a display
// name, which every renderer supplies for itself.
type regimeThresholdText struct{ label, green, yellow, red, trip string }

// regimeThresholdTexts is the single source for published band prose.
// classifiers are inclusive, so a reading exactly on the line was documented
var regimeThresholdTexts = map[string]regimeThresholdText{
	RegimeIndicatorVIXTerm:   {"vix_term_structure_v1", "VIX/VIX3M < 0.92", "0.92 <= VIX/VIX3M < 1.00", "VIX/VIX3M >= 1.00", "trips >=1.00"},
	RegimeIndicatorVolOfVol:  {"vvix_daily_v3", "VVIX < 90, or at level but not rising", "VVIX >= 90 and +3% over 5 sessions", "VVIX >= 120", "trips >=120"},
	RegimeIndicatorHYGSPY:    {"hyg_spy_credit_proxy_v1", "HYG >= 50-day SMA", "HYG < 50-day SMA", "HYG < 50-day SMA and SPY >= 97% of 52-week high", "trips HYG <50dma with SPY >=97% of 52w high"},
	RegimeIndicatorCredit:    {"hy_ig_oas_v1", "HY OAS < 4.0 and 20d widening < 0.50 pp", "HY OAS 4.0-5.5 or 20d widening >= 0.50 pp", "HY OAS >= 5.5 or 20d widening >= 1.00 pp", "trips HY OAS >=5.5"},
	RegimeIndicatorFunding:   {"funding_cp_tbill_v2", "spread < 25 bp, or at level but not widening", "spread >= 25 bp and +10 bp over 5 publications", "spread >= 75 bp", "trips >=75 bp"},
	RegimeIndicatorUSDJPY:    {"usd_jpy_carry_proxy_v1", "yen strengthening < 1% over the week", "yen strengthening 1-2% over the week", "yen strengthening >= 2% over the week", "trips yen +2%/week"},
	RegimeIndicatorGammaZero: {"dealer_gamma_v3", "spot > 2% above gamma-zero or profile wholly long-gamma", "spot within +/-2% of gamma-zero or mixed gamma profile", "spot > 2% below gamma-zero, profile wholly short-gamma, or dominant/equal exposure is amplifying", "trips spot >2% below gamma-zero"},
	RegimeIndicatorBreadth:   {"spx_breadth_50dma_v1", "SPX members above 50-DMA > 55%", "40% <= members above 50-DMA <= 55%", "members above 50-DMA < 40%", "trips <40% (50d)"},
}

// RegimeThresholdsFor returns the published band prose for an indicator, or
// result is embedded per-snapshot and must not be shared across them.
func RegimeThresholdsFor(indicator string) *RegimeThresholds {
	t, ok := regimeThresholdTexts[indicator]
	if !ok {
		return nil
	}
	return HeuristicThresholds(t.label, t.green, t.yellow, t.red, t.trip)
}

// RegimeGateFor returns the confirmation gate used by the daemon for one
// indicator. Presentation code may explain this gate; it must not alter it.
func RegimeGateFor(indicator string) (RegimeGate, bool) {
	gate, ok := regimeGates[indicator]
	return gate, ok
}

// StressInput is the pure state input shared by the CLI and MCP tool. It
// risk-data path: account margin, portfolio exposure, and market regime stay
type StressInput struct {
	Account      AccountResult
	Positions    PositionsResult
	Regime       RegimeSnapshotResult
	MarketEvents MarketEventsResult
	// Concentration is the Rulebook's reading of single-issuer concentration
	// (rule 1's worst-case loss cap and rule 16's delta swing). The stress
	// read never measures concentration itself; nil reads as unavailable.
	Concentration *StressConcentration
	// NetExposure is the Rulebook's rule 15 reading of the whole book's net
	// market exposure. The stress read keeps no net-exposure measure or level
	// of its own (amendment 16); nil reads as unavailable.
	NetExposure *StressNetExposure
	// MarginHeadroom is the Rulebook's rule 19 reading of margin headroom,
	// the broker's excess liquidity as a share of NLV. The stress read keeps
	// no margin-cushion level of its own (amendment 19); nil reads as
	// unavailable.
	MarginHeadroom *StressMarginHeadroom
	Now            time.Time
}

// StressMarginHeadroom projects Rulebook rule 19 (margin_headroom) for the
// stress read: the broker's excess liquidity as a share of NLV and rule 19's
// two bands from rulebook-policy.toml. The rule reads downward: watch
// strictly below WatchPct, act strictly below ActPct.
type StressMarginHeadroom struct {
	// Status is rule 19's status: pass, watch, act, unknown, or
	// not_evaluated when the rule is off. Empty when the reading is
	// unavailable; Reason then says why.
	Status string `json:"status,omitempty"`
	// PctNLV is rule 19's observed headroom, % of NLV; absent when rule 19
	// could not measure it.
	PctNLV   *float64 `json:"pct_nlv,omitempty"`
	WatchPct *float64 `json:"watch_pct,omitempty"`
	ActPct   *float64 `json:"act_pct,omitempty"`
	// RuleReason is rule 19's own reason code when it is unknown or off
	// (excess_liquidity_unavailable, rule_off, an account reason).
	RuleReason string `json:"rule_reason,omitempty"`
	// Reason names why the reading is unavailable, when it is.
	Reason string `json:"reason,omitempty"`
}

// StressMarginHeadroomFromRules projects rule 19 from a Rulebook result for
// the stress read. A missing or disabled result, or one without a rule 19
// row, reads as unavailable with a reason.
func StressMarginHeadroomFromRules(res *RulesResult) *StressMarginHeadroom {
	out := &StressMarginHeadroom{}
	switch {
	case res == nil:
		out.Reason = "no current Rulebook result"
		return out
	case !res.Enabled:
		out.Reason = "the Rulebook is turned off"
		return out
	}
	for _, row := range res.Rules {
		if row.ID != risk.RuleMarginHeadroom {
			continue
		}
		out.Status = row.Status
		out.PctNLV = cloneFloatPtr(row.Observed)
		out.WatchPct, out.ActPct = cloneFloatPtr(row.WatchThreshold), cloneFloatPtr(row.ActThreshold)
		if row.Status == risk.RuleStatusUnknown || row.Status == risk.RuleStatusNotEvaluated {
			out.RuleReason = row.Reason
		}
		return out
	}
	out.Reason = "the Rulebook result has no rule 19 row"
	return out
}

// StressNetExposure projects Rulebook rule 15 (net_exposure) for the stress
// read: the book's signed stock-equivalent exposure with hedges netted, as a
// magnitude share of NLV, rule 15's two bands from rulebook-policy.toml and
// the regime set that produced them.
type StressNetExposure struct {
	// Status is rule 15's status: pass, watch, act, unknown, or
	// not_evaluated when the rule is off. Empty when the reading is
	// unavailable; Reason then says why.
	Status string `json:"status,omitempty"`
	// PctNLV is rule 15's observed magnitude, % of NLV. It is a proven lower
	// bound when IsLowerBound is set, and absent when rule 15 could not
	// measure the book.
	PctNLV       *float64 `json:"pct_nlv,omitempty"`
	IsLowerBound bool     `json:"is_lower_bound,omitempty"`
	// Direction is "long" or "short" when rule 15 names the side it flags.
	Direction string   `json:"direction,omitempty"`
	WatchPct  *float64 `json:"watch_pct,omitempty"`
	ActPct    *float64 `json:"act_pct,omitempty"`
	// RegimeSet is the regime threshold set (calm, early_warning or
	// confirmed) whose bands produced rule 15's verdict (amendment 17). The
	// stress read calls an act urgent only while it is confirmed.
	RegimeSet string `json:"regime_set,omitempty"`
	// RuleReason is rule 15's own reason code when it is unknown or off
	// (greeks_gap, rule_off, positions_pending and the like).
	RuleReason string `json:"rule_reason,omitempty"`
	// Reason names why the reading is unavailable, when it is.
	Reason string `json:"reason,omitempty"`
}

// StressNetExposureFromRules projects rule 15 from a Rulebook result for the
// stress read. A missing or disabled result, or one without a rule 15 row,
// reads as unavailable with a reason.
func StressNetExposureFromRules(res *RulesResult) *StressNetExposure {
	out := &StressNetExposure{}
	switch {
	case res == nil:
		out.Reason = "no current Rulebook result"
		return out
	case !res.Enabled:
		out.Reason = "the Rulebook is turned off"
		return out
	}
	for _, row := range res.Rules {
		if row.ID != risk.RuleNetExposure {
			continue
		}
		out.Status = row.Status
		out.PctNLV = cloneFloatPtr(row.Observed)
		out.IsLowerBound = row.ObservedIsLowerBound
		out.WatchPct, out.ActPct = cloneFloatPtr(row.WatchThreshold), cloneFloatPtr(row.ActThreshold)
		out.RegimeSet = row.RegimeSet
		if row.Status == risk.RuleStatusUnknown || row.Status == risk.RuleStatusNotEvaluated {
			out.RuleReason = row.Reason
		}
		// Rule 15 lists, at watch or act, the contributors on the side it
		// flags; their signed share of NLV names that side.
		if row.Status == risk.RuleStatusWatch || row.Status == risk.RuleStatusAct {
			for _, o := range row.Offenders {
				if o.Observed > 0 {
					out.Direction = "long"
					break
				}
				if o.Observed < 0 {
					out.Direction = "short"
					break
				}
			}
		}
		return out
	}
	out.Reason = "the Rulebook result has no rule 15 row"
	return out
}

// StressConcentration projects the Rulebook's concentration rows for the
// stress read: rule 1's driving issuer with its worst-case loss and bands,
// and rule 16's largest delta swing with its watch level.
type StressConcentration struct {
	// Status is rule 1's status (pass, watch, act or unknown); empty when the
	// Rulebook result carried no rule 1 row.
	Status string `json:"status,omitempty"`
	// Issuer is the issuer rule 1's verdict rests on.
	Issuer           string   `json:"issuer,omitempty"`
	LossPctNLV       *float64 `json:"worst_case_loss_pct_nlv,omitempty"`
	LossIsLowerBound bool     `json:"worst_case_loss_is_lower_bound,omitempty"`
	WatchPct         *float64 `json:"watch_pct,omitempty"`
	ActPct           *float64 `json:"act_pct,omitempty"`
	// DeltaStatus, DeltaIssuer, DeltaPctNLV and DeltaWatchPct are rule 16's.
	DeltaStatus   string   `json:"delta_status,omitempty"`
	DeltaIssuer   string   `json:"delta_issuer,omitempty"`
	DeltaPctNLV   *float64 `json:"delta_pct_nlv,omitempty"`
	DeltaWatchPct *float64 `json:"delta_watch_pct,omitempty"`
	// Reason names why the reading is unavailable, when it is.
	Reason string `json:"reason,omitempty"`
}

// StressConcentrationFromRules projects a Rulebook result for the stress
// read. A missing or disabled result reads as unavailable with a reason.
func StressConcentrationFromRules(res *RulesResult) *StressConcentration {
	out := &StressConcentration{}
	switch {
	case res == nil:
		out.Reason = "no current Rulebook result"
		return out
	case !res.Enabled:
		out.Reason = "the Rulebook is turned off"
		return out
	}
	for _, row := range res.Rules {
		switch row.ID {
		case risk.RuleSingleNameExposure:
			out.Status = row.Status
			out.LossPctNLV = cloneFloatPtr(row.Observed)
			out.LossIsLowerBound = row.ObservedIsLowerBound
			out.WatchPct, out.ActPct = cloneFloatPtr(row.WatchThreshold), cloneFloatPtr(row.ActThreshold)
			for _, o := range row.Offenders {
				if o.Issuer != nil && o.Issuer.WorstCaseLossBase > 0 {
					out.Issuer = o.Symbol
					break
				}
			}
			if out.Issuer == "" && len(row.Offenders) > 0 && row.Status != risk.RuleStatusUnknown {
				out.Issuer = row.Offenders[0].Symbol
			}
		case risk.RuleDeltaSwing:
			out.DeltaStatus = row.Status
			out.DeltaPctNLV = cloneFloatPtr(row.Observed)
			out.DeltaWatchPct = cloneFloatPtr(row.Threshold)
			if len(row.Offenders) > 0 && row.Status == risk.RuleStatusWatch {
				out.DeltaIssuer = row.Offenders[0].Symbol
			}
		}
	}
	if out.Status == "" {
		out.Reason = "the Rulebook result has no rule 1 row"
	}
	return out
}

func cloneFloatPtr(v *float64) *float64 {
	if v == nil {
		return nil
	}
	c := *v
	return &c
}

// StressResult is the compact scheduled-monitor payload. The stress read is
// stateless: it combines current broad-market regime with the current portfolio
// shape, then emits a fresh action snapshot. Fingerprint is the canonical
// alert identity for monitors; SourceFingerprints records the classified
// upstream state the stress read consumed.
type StressResult struct {
	// AccountScope binds this assessment to matching available account and
	// positions authority. Nil means no shared account/mode was established;
	// it grants no execution authority and is not part of alert fingerprints.
	AccountScope       *AccountDataScope        `json:"account_scope,omitempty"`
	AsOf               time.Time                `json:"as_of"`
	SourceAsOf         StressSourceAsOf         `json:"source_as_of,omitzero"`
	Fingerprint        Fingerprint              `json:"fingerprint"`
	SourceFingerprints StressSourceFingerprints `json:"source_fingerprints,omitzero"`
	SourceHealth       []SourceHealth           `json:"source_health,omitempty"`
	Policy             string                   `json:"policy,omitempty"`
	PolicyProfile      string                   `json:"policy_profile,omitempty"`
	PolicyVersion      string                   `json:"policy_version,omitempty"`
	PolicyFingerprint  Fingerprint              `json:"policy_fingerprint,omitzero"`
	Action             string                   `json:"action,omitempty"`
	MarketConfirmation string                   `json:"market_confirmation,omitempty"`
	PortfolioFit       string                   `json:"portfolio_fit,omitempty"`
	// PortfolioAlertRelevant is the producer-stamped verdict for "does this
	// exactly one copy, in internal/stress; the app alert gate and the SPA
	// cases. Nil means the producer predates the stamp — consumers fail open
	PortfolioAlertRelevant *bool                   `json:"portfolio_alert_relevant,omitempty"`
	InputHealth            string                  `json:"input_health,omitempty"`
	Direction              risk.SignalDirection    `json:"direction,omitempty"`
	Severity               risk.SignalSeverity     `json:"severity"`
	PlannerModeHint        risk.PlannerMode        `json:"planner_mode_hint,omitempty"`
	PlannerReadiness       risk.PlannerReadiness   `json:"planner_readiness,omitempty"`
	Summary                string                  `json:"summary"`
	PrimaryDrivers         []risk.SignalID         `json:"primary_drivers,omitempty"`
	Signals                []risk.Signal           `json:"signals,omitempty"`
	Rows                   []StressRow             `json:"rows"`
	Portfolio              StressPortfolioSummary  `json:"portfolio"`
	Market                 StressMarketSummary     `json:"market"`
	MarketIndicators       []StressMarketIndicator `json:"market_indicators,omitempty"`
	Warnings               []string                `json:"warnings,omitempty"`
	NotExecution           string                  `json:"not_execution"`
	// EstablishedAlertProjection is the producer-authored compatibility
	// strict and self-validating so consumers can fail closed on malformed
	EstablishedAlertProjection *EstablishedAlertProjection `json:"established_alert_projection,omitempty"`
}

// EstablishedAlertProjectionSchemaVersion identifies the strict compatibility
const EstablishedAlertProjectionSchemaVersion = "stress-established-alert-v1"

// EstablishedStressFingerprintVersion identifies the Stress-labelled wrapper
// carried by EstablishedAlertProjection. Its key retains the exact pre-rename
const EstablishedStressFingerprintVersion = "stress-fp-v1"

// EstablishedAlertProjection atomically carries every producer-owned field
// the pre-shadow Stress monitor used for occurrence identity and delivery-mode
// eligibility. CanonicalFingerprint carries the established key under the
// Stress-labelled compatibility version; it is not a new alert authority or a
// transport authorization.
type EstablishedAlertProjection struct {
	SchemaVersion        string              `json:"schema_version"`
	CanonicalFingerprint Fingerprint         `json:"canonical_fingerprint"`
	OccurrenceEligible   bool                `json:"occurrence_eligible"`
	ActOnlyEligible      bool                `json:"act_only_eligible"`
	Action               string              `json:"action"`
	MarketConfirmation   string              `json:"market_confirmation"`
	Severity             risk.SignalSeverity `json:"severity"`
	PortfolioRelevant    bool                `json:"portfolio_relevant"`
}

// ValidateEstablishedAlertProjection rejects missing, unknown, or internally
// inconsistent compatibility data. Eligibility is checked against the frozen
// v1 schema semantics; adapters must not re-derive or extend those semantics.
func ValidateEstablishedAlertProjection(projection EstablishedAlertProjection) error {
	if projection.SchemaVersion != EstablishedAlertProjectionSchemaVersion {
		return fmt.Errorf("invalid established alert projection schema version %q", projection.SchemaVersion)
	}
	if err := validateEstablishedAlertFingerprint(projection.CanonicalFingerprint); err != nil {
		return err
	}
	if !validEstablishedAlertAction(projection.Action) {
		return fmt.Errorf("invalid established alert action %q", projection.Action)
	}
	if !validEstablishedMarketConfirmation(projection.MarketConfirmation) {
		return fmt.Errorf("invalid established alert market confirmation %q", projection.MarketConfirmation)
	}
	if !validEstablishedAlertSeverity(projection.Severity) {
		return fmt.Errorf("invalid established alert severity %q", projection.Severity)
	}
	actCondition := establishedAlertSeverityAtLeastAct(projection.Severity) ||
		projection.Action == "defend" ||
		projection.Action == "rebalance" ||
		projection.Action == "confirm_inputs"
	wantOccurrence := projection.PortfolioRelevant &&
		(projection.Severity == risk.SeverityWatch || establishedAlertSeverityAtLeastAct(projection.Severity) || actCondition)
	wantActOnly := wantOccurrence && actCondition
	if projection.OccurrenceEligible != wantOccurrence {
		return Defect(errors.New("established alert occurrence eligibility is inconsistent"))
	}
	if projection.ActOnlyEligible != wantActOnly {
		return Defect(errors.New("established alert act-only eligibility is inconsistent"))
	}
	return nil
}

func validateEstablishedAlertFingerprint(fingerprint Fingerprint) error {
	if fingerprint.Version != EstablishedStressFingerprintVersion {
		return fmt.Errorf("invalid established alert fingerprint version %q", fingerprint.Version)
	}
	const prefix = "sha256:"
	if !strings.HasPrefix(fingerprint.Key, prefix) {
		return errors.New("established alert fingerprint must use sha256")
	}
	decoded, err := hex.DecodeString(strings.TrimPrefix(fingerprint.Key, prefix))
	if err != nil || len(decoded) != 32 {
		return errors.New("established alert fingerprint must contain a 32-byte sha256 digest")
	}
	return nil
}

func validEstablishedAlertAction(action string) bool {
	switch action {
	case "stand_down", "watch", "defend", "rebalance", "deploy", "confirm_inputs":
		return true
	default:
		return false
	}
}

func validEstablishedMarketConfirmation(confirmation string) bool {
	switch confirmation {
	case "none", "partial", "confirmed", "blocked":
		return true
	default:
		return false
	}
}

func validEstablishedAlertSeverity(severity risk.SignalSeverity) bool {
	switch severity {
	case risk.SeverityObserve, risk.SeverityWatch, risk.SeverityAct, risk.SeverityUrgent:
		return true
	default:
		return false
	}
}

func establishedAlertSeverityAtLeastAct(severity risk.SignalSeverity) bool {
	return severity == risk.SeverityAct || severity == risk.SeverityUrgent
}

// MarshalJSON validates the projection before encoding it.
func (projection EstablishedAlertProjection) MarshalJSON() ([]byte, error) {
	if err := ValidateEstablishedAlertProjection(projection); err != nil {
		return nil, err
	}
	type wire EstablishedAlertProjection
	return json.Marshal(wire(projection))
}

// UnmarshalJSON rejects unknown, missing, null, trailing, or inconsistent data.
func (projection *EstablishedAlertProjection) UnmarshalJSON(data []byte) error {
	if projection == nil {
		return errors.New("cannot unmarshal established alert projection into nil receiver")
	}
	type projectionWire struct {
		SchemaVersion        *string              `json:"schema_version"`
		CanonicalFingerprint *Fingerprint         `json:"canonical_fingerprint"`
		OccurrenceEligible   *bool                `json:"occurrence_eligible"`
		ActOnlyEligible      *bool                `json:"act_only_eligible"`
		Action               *string              `json:"action"`
		MarketConfirmation   *string              `json:"market_confirmation"`
		Severity             *risk.SignalSeverity `json:"severity"`
		PortfolioRelevant    *bool                `json:"portfolio_relevant"`
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var decoded projectionWire
	if err := decoder.Decode(&decoded); err != nil {
		return err
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("established alert projection has trailing JSON")
		}
		return err
	}
	if decoded.SchemaVersion == nil || decoded.CanonicalFingerprint == nil ||
		decoded.OccurrenceEligible == nil || decoded.ActOnlyEligible == nil ||
		decoded.Action == nil || decoded.MarketConfirmation == nil ||
		decoded.Severity == nil || decoded.PortfolioRelevant == nil {
		return errors.New("established alert projection is missing a required field")
	}
	value := EstablishedAlertProjection{
		SchemaVersion:        *decoded.SchemaVersion,
		CanonicalFingerprint: *decoded.CanonicalFingerprint,
		OccurrenceEligible:   *decoded.OccurrenceEligible,
		ActOnlyEligible:      *decoded.ActOnlyEligible,
		Action:               *decoded.Action,
		MarketConfirmation:   *decoded.MarketConfirmation,
		Severity:             *decoded.Severity,
		PortfolioRelevant:    *decoded.PortfolioRelevant,
	}
	if err := ValidateEstablishedAlertProjection(value); err != nil {
		return err
	}
	*projection = value
	return nil
}

// StressSourceAsOf records each source snapshot's observation time. Zero times
type StressSourceAsOf struct {
	Account      time.Time `json:"account,omitzero"`
	Positions    time.Time `json:"positions,omitzero"`
	Regime       time.Time `json:"regime,omitzero"`
	MarketEvents time.Time `json:"market_events,omitzero"`
}

// StressSourceFingerprints carries optional semantic source identities. Nil
type StressSourceFingerprints struct {
	Account      *Fingerprint `json:"account,omitempty"`
	Positions    *Fingerprint `json:"positions,omitempty"`
	Regime       *Fingerprint `json:"regime,omitempty"`
	MarketEvents *Fingerprint `json:"market_events,omitempty"`
}

// StressRow is one bounded, daemon-derived advisory finding.
type StressRow struct {
	Title     string               `json:"title"`
	Direction risk.SignalDirection `json:"direction,omitempty"`
	Severity  risk.SignalSeverity  `json:"severity"`
	Guidance  string               `json:"guidance"`
	Evidence  string               `json:"evidence,omitempty"`
}

// StressMarketIndicator is a display-ready market observation; its status is
// advisory evidence rather than execution authority.
type StressMarketIndicator struct {
	Name    string `json:"name"`
	Status  string `json:"status"` // green | amber | red | context | n/a
	AsOf    string `json:"as_of,omitempty"`
	Reading string `json:"reading,omitempty"`
	Comment string `json:"comment,omitempty"`
	// Trip is the served trigger anchor a gauge face prints beside Reading:
	// the red trip, prefixed with the amber band prose while the row is amber,
	// so an amber face names the line it crossed. A renderer must never
	// supply its own cutoff text.
	Trip string `json:"trip,omitempty"`
}

// StressPortfolioSummary is a redacted portfolio-risk projection. Pointer
type StressPortfolioSummary struct {
	BaseCurrency   string  `json:"base_currency,omitempty"`
	NetLiquidation float64 `json:"net_liquidation,omitempty"`
	// CushionPct keeps its public name but carries Rulebook rule 19's
	// measure (amendment 19): the broker's excess liquidity as % of NLV, the
	// worse of the current and the look-ahead figure when both are reported.
	// It is absent whenever rule 19 did not measure the headroom.
	CushionPct *float64 `json:"cushion_pct,omitempty"`
	// LookAheadCushionPct is the broker's look-ahead excess liquidity as % of
	// NLV, as the stress read derives it. No stress verdict rests on it: rule
	// 19 judges the broker's reported look-ahead figure itself (amendment 19
	// R3), and the margin row quotes this one only when rule 19 did not
	// measure.
	LookAheadCushionPct *float64 `json:"look_ahead_cushion_pct,omitempty"`
	// CushionTripPct is rule 19's watch band, the level headroom may not fall
	// below: the trip a gauge prints beside CushionPct. Absent with it.
	CushionTripPct      *float64 `json:"cushion_trip_pct,omitempty"`
	GrossExposurePctNLV *float64 `json:"gross_exposure_pct_nlv,omitempty"`
	// NetDeltaPctNLV keeps its public name but carries Rulebook rule 15's
	// measure (amendment 16): the book's net stock-equivalent exposure with
	// hedges netted, as a magnitude % of NLV. It is a proven lower bound when
	// NetExposure.IsLowerBound is set and absent whenever rule 15 did not
	// measure the book.
	NetDeltaPctNLV       *float64                   `json:"net_delta_pct_nlv,omitempty"`
	GrossDeltaPctNLV     *float64                   `json:"gross_delta_pct_nlv,omitempty"`
	LargestExposure      string                     `json:"largest_exposure,omitempty"`
	LargestExposurePct   *float64                   `json:"largest_exposure_pct_nlv,omitempty"`
	LargestDeltaExposure string                     `json:"largest_delta_exposure,omitempty"`
	LargestDeltaPctNLV   *float64                   `json:"largest_delta_pct_nlv,omitempty"`
	DailyPnLPct          *float64                   `json:"daily_pnl_pct,omitempty"`
	OptionGreeks         string                     `json:"option_greeks,omitempty"`
	ProtectionCoverage   *ProtectionCoverageSummary `json:"protection_coverage,omitempty"`
	HeldStress           []HeldStress               `json:"held_stress,omitempty"`
	// Concentration is the Rulebook's concentration reading the stress read
	// used for its concentration row and signals.
	Concentration *StressConcentration `json:"concentration,omitempty"`
	// NetExposure is the Rulebook's rule 15 reading the stress read used for
	// its exposure row and net_delta_high signal, with rule 15's bands.
	NetExposure *StressNetExposure `json:"net_exposure,omitempty"`
	// MarginHeadroom is the Rulebook's rule 19 reading the stress read used
	// for its margin row and margin_cushion_low signal, with rule 19's bands.
	MarginHeadroom *StressMarginHeadroom `json:"margin_headroom,omitempty"`
	// BondRisk is the held bonds' book (bond-risk.md, phase 1) the "Rates
	// and credit" row reads; nil when no bond is held.
	BondRisk *risk.BondBook `json:"bond_risk,omitempty"`
	// GrossExcludingBondsPctNLV is the broker's gross position value less the
	// held bonds' value, as % of NLV: shown beside the equity exposure row's
	// gross figure, which includes bonds, without changing its verdict.
	GrossExcludingBondsPctNLV *float64 `json:"gross_excluding_bonds_pct_nlv,omitempty"`

	// ExposureUnmeasured names the held underlyings that contributed nothing to
	// the book, so a threshold comparison against them can only prove a breach,
	// never a clean pass. Empty on a fully measured book.
	ExposureUnmeasured []string `json:"exposure_unmeasured,omitempty"`
}

// HeldStress is a bounded, positions-only explanation of stress inside
// fields come from the existing positions/account snapshot.
type HeldStress struct {
	Underlying            string            `json:"underlying"`
	MaterialReasons       []string          `json:"material_reasons,omitempty"`
	MarketValuePctNLV     *float64          `json:"market_value_pct_nlv,omitempty"`
	DeltaPctNLV           *float64          `json:"delta_pct_nlv,omitempty"`
	DailyPnLPctNLV        *float64          `json:"daily_pnl_pct_nlv,omitempty"`
	NearExpiryDeltaPctNLV *float64          `json:"near_expiry_delta_pct_nlv,omitempty"`
	NearExpiryGamma       *float64          `json:"near_expiry_gamma,omitempty"`
	NearExpiryMinDTE      *int              `json:"near_expiry_min_dte,omitempty"`
	LiquidityFlags        []string          `json:"liquidity_flags,omitempty"`
	MarketFlags           []MarketEventFlag `json:"market_flags,omitempty"`
	SignalIDs             []risk.SignalID   `json:"signal_ids,omitempty"`
}

// StressUnconfirmedSignal is one thing a surface may name beside the
// confirmed reds, with the reader's name of the row behind it
// (RegimeRowName) and what it is, so no reader re-derives either.
type StressUnconfirmedSignal struct {
	Cluster string `json:"cluster"`
	Name    string `json:"name"`
	Role    string `json:"role"`
}

// Roles of a StressUnconfirmedSignal. A witness is a stress witness that is
// red without confirming, and the widening is cash credit spreads widening:
// both warn. A waiting signal is a co-sign red beside a standing witness that
// has yet to pass its own gate. Context is a co-sign red with no witness
// standing.
//
// A dark-witness warning (RegimeWarningWitnessUnavailable) adds the witness
// rows nobody can read as unavailable and every co-sign row at yellow or red
// as weak, also one listed as context.
const (
	StressSignalWitness     = "witness"
	StressSignalWidening    = "widening"
	StressSignalWaiting     = "waiting"
	StressSignalContext     = "context"
	StressSignalUnavailable = "unavailable"
	StressSignalWeak        = "weak"
)

// StressMarketSummary combines regime, tape, and source-quality context used by
// Stress. Its cluster counts retain rankability and confirmation distinctions.
type StressMarketSummary struct {
	RegimeVerdict string        `json:"regime_verdict,omitempty"`
	RegimePosture RegimePosture `json:"regime_posture,omitzero"`
	RedClusters   int           `json:"red_clusters"`
	// EligibleRedClusters counts reds that passed the confirmation gates
	// (depth + persistence + freshness) — the only reds used by
	// Stress act/urgent-grade decisions. RedClusters keeps the visible
	// (confirmed-band) reds for watch-grade evidence; the difference is
	// disclosed in UnconfirmedRedClusterNames.
	EligibleRedClusters        int      `json:"eligible_red_clusters"`
	EligibleRedClusterNames    []string `json:"eligible_red_cluster_names,omitempty"`
	YellowClusters             int      `json:"yellow_clusters"`
	RankedClusters             int      `json:"ranked_clusters"`
	UnrankedClusters           int      `json:"unranked_clusters"`
	RedClusterNames            []string `json:"red_cluster_names,omitempty"`
	YellowClusterNames         []string `json:"yellow_cluster_names,omitempty"`
	UnconfirmedRedClusterNames []string `json:"unconfirmed_red_cluster_names,omitempty"`
	// UnconfirmedSignals is what a surface may name beside the confirmed
	// reds, in cluster order with the cash widening last: each unconfirmed
	// red cluster and, when it opens a warning, the cash credit widening.
	UnconfirmedSignals []StressUnconfirmedSignal `json:"unconfirmed_signals,omitempty"`
	// ContextNote is the sentence a reader prints before the signals whose
	// role is context. Served with them so no reader keeps a copy.
	ContextNote string `json:"context_note,omitempty"`
	// WarningOpeners is RegimeWarningOpeners for the regime read behind this
	// summary: the arms that open a market warning.
	WarningOpeners    []string `json:"warning_openers,omitempty"`
	AmbiguousClusters []string `json:"ambiguous_clusters,omitempty"`
	PartialClusters   []string `json:"partial_clusters,omitempty"`
	ComputingClusters []string `json:"computing_clusters,omitempty"`
	DegradedClusters  []string `json:"degraded_clusters,omitempty"`
	StaleClusters     []string `json:"stale_clusters,omitempty"`
	SPYPrice          *float64 `json:"spy_price,omitempty"`
	SPYChangePct      *float64 `json:"spy_change_pct,omitempty"`
	// SPYChangeNotCurrent marks a SPY day change on a trading date whose print
	// is not current (RegimeSPYTapeCurrency): shown, but no decision reads it.
	SPYChangeNotCurrent bool     `json:"spy_change_not_current,omitempty"`
	VIX                 *float64 `json:"vix,omitempty"`
	VIXChangePct        *float64 `json:"vix_change_pct,omitempty"`
	// TapeSessionState classifies the official US cash-equity calendar date
	// coverage: severity behaves as before (fail-open).
	TapeSessionState  string     `json:"tape_session_state,omitempty"`
	TapeSessionReason string     `json:"tape_session_reason,omitempty"`
	TapeNextOpen      *time.Time `json:"tape_next_open,omitempty"`
}

// TapeSessionState values shared by StressMarketSummary and
// RegimeSnapshotResult. Trading dates keep full direct-tape severity at any
// hour (pre/post/overnight moves are live prints the tape-shock row exists to
// catch); closed dates demote frozen tape shocks to observe and bar them from
// entering or holding tape-driven lifecycle stages until the next open
// re-evaluates them from live prints.
const (
	// TapeSessionTradingDate means the calendar date has an official session.
	TapeSessionTradingDate = "trading_date"
	// TapeSessionClosedDate means direct tape changes are frozen context only.
	TapeSessionClosedDate = "closed_date"
)

// TapeSessionFor classifies the official US cash-equity calendar date at now
// freeze the SPY/VIX day-change anchors at last-session values — which can
// state stays empty and consumers fail open to full severity.
func TapeSessionFor(now time.Time) (state, reason string, nextOpen *time.Time) {
	sess, err := marketcal.New().SessionAt(marketcal.MarketUSEquity, now)
	if err != nil {
		return "", "", nil
	}
	switch sess.State {
	case marketcal.StateClosed, marketcal.StateHoliday:
		return TapeSessionClosedDate, sess.Reason, sess.NextOpen
	case marketcal.StateRegular, marketcal.StateEarlyClose:
		return TapeSessionTradingDate, "", nil
	default:
		return "", "", nil
	}
}
