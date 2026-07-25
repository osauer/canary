package rpc

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/osauer/ibkr/v2/internal/marketcal"
	"github.com/osauer/ibkr/v2/internal/risk"
)

// StressInput is the pure state input shared by the CLI and MCP tool. It
// deliberately consumes existing daemon snapshots instead of adding a second
// risk-data path: account margin, portfolio exposure, and market regime stay
// single-source-of-truth.
type StressInput struct {
	Account      AccountResult
	Positions    PositionsResult
	Regime       RegimeSnapshotResult
	MarketEvents MarketEventsResult
	Now          time.Time
}

// StressResult is the compact scheduled-monitor payload. The canary is
// stateless: it combines current broad-market regime with the current portfolio
// shape, then emits a fresh action snapshot. Fingerprint is the canonical
// alert identity for monitors; SourceFingerprints records the classified
// upstream state the canary consumed.
type StressResult struct {
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
	// snapshot concern the live portfolio enough to alert on". The policy has
	// exactly one copy, in internal/stress; the app alert gate and the SPA
	// preview gate read this field instead of re-deriving fit/exposure edge
	// cases. Nil means the producer predates the stamp — consumers fail open
	// to relevant, so version skew can add market-weather noise but never
	// suppress alert delivery.
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
	// contract for the Canary delivery behavior that existed at ad5b77b. It
	// does not grant any new delivery or pageability: consumers may use it only
	// to preserve that established occurrence and mode eligibility while the
	// main result truthfully adopts newer advisory health semantics.
	//
	// Nil means the producer predates this projection. A present projection is
	// strict and self-validating so consumers can fail closed on malformed
	// new-daemon data without treating version skew as corruption.
	EstablishedAlertProjection *EstablishedAlertProjection `json:"established_alert_projection,omitempty"`
}

// EstablishedAlertProjectionSchemaVersion identifies the strict compatibility
// projection schema consumed by the established Stress monitor.
const EstablishedAlertProjectionSchemaVersion = "stress-established-alert-v1"

// EstablishedStressFingerprintVersion identifies the frozen fingerprint
// version carried by EstablishedAlertProjection. It remains v1 even when the
// current Stress result advances its semantic projection.
const EstablishedStressFingerprintVersion = "stress-fp-v1"

// EstablishedAlertProjection atomically carries every producer-owned field
// the pre-shadow Stress monitor used for occurrence identity and delivery-mode
// eligibility. CanonicalFingerprint remains the established stress-fp-v1
// identity; it is not a new alert authority or a transport authorization.
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
		return errors.New("established alert occurrence eligibility is inconsistent")
	}
	if projection.ActOnlyEligible != wantActOnly {
		return errors.New("established alert act-only eligibility is inconsistent")
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
// mean the source timestamp is unavailable, not current.
type StressSourceAsOf struct {
	Account      time.Time `json:"account,omitzero"`
	Positions    time.Time `json:"positions,omitzero"`
	Regime       time.Time `json:"regime,omitzero"`
	MarketEvents time.Time `json:"market_events,omitzero"`
}

// StressSourceFingerprints carries optional semantic source identities. Nil
// means the source did not provide an identity.
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
}

// StressPortfolioSummary is a redacted portfolio-risk projection. Pointer
// metrics distinguish unavailable observations from measured zero values.
type StressPortfolioSummary struct {
	BaseCurrency         string                     `json:"base_currency,omitempty"`
	NetLiquidation       float64                    `json:"net_liquidation,omitempty"`
	CushionPct           *float64                   `json:"cushion_pct,omitempty"`
	LookAheadCushionPct  *float64                   `json:"look_ahead_cushion_pct,omitempty"`
	GrossExposurePctNLV  *float64                   `json:"gross_exposure_pct_nlv,omitempty"`
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
}

// HeldStress is a bounded, positions-only explanation of stress inside
// material held underlyings. It deliberately avoids option-chain fan-out; all
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

// StressMarketSummary combines regime, tape, and source-quality context used by
// Canary. Its cluster counts retain rankability and confirmation distinctions.
type StressMarketSummary struct {
	RegimeVerdict string        `json:"regime_verdict,omitempty"`
	RegimePosture RegimePosture `json:"regime_posture,omitzero"`
	RedClusters   int           `json:"red_clusters"`
	// EligibleRedClusters counts reds that passed the confirmation gates
	// (depth + persistence + freshness) — the only reds canary's
	// act/urgent-grade decisions key on. RedClusters keeps the visible
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
	AmbiguousClusters          []string `json:"ambiguous_clusters,omitempty"`
	PartialClusters            []string `json:"partial_clusters,omitempty"`
	ComputingClusters          []string `json:"computing_clusters,omitempty"`
	DegradedClusters           []string `json:"degraded_clusters,omitempty"`
	StaleClusters              []string `json:"stale_clusters,omitempty"`
	SPYPrice                   *float64 `json:"spy_price,omitempty"`
	SPYChangePct               *float64 `json:"spy_change_pct,omitempty"`
	VIX                        *float64 `json:"vix,omitempty"`
	VIXChangePct               *float64 `json:"vix_change_pct,omitempty"`
	// TapeSessionState classifies the official US cash-equity calendar date
	// the canary ran on. On a closed date (weekend/holiday) the direct
	// SPY/VIX day-change prints are frozen last-session values — the anchors
	// can even reset independently while closed — so they carry evidence but
	// cannot confirm severity. Empty means outside embedded calendar
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
// for direct-tape severity — the single policy copy behind both the canary
// tape row and the regime lifecycle tape terms. Trading dates (regular and
// early-close) keep full severity at any hour: pre/post/overnight prints are
// live (VIX prints overnight on weekdays). Closed dates (weekend/holiday)
// freeze the SPY/VIX day-change anchors at last-session values — which can
// even reset independently while closed — so frozen shocks carry evidence but
// confirm nothing until the next open. Outside embedded calendar coverage the
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
