package cli

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"reflect"
	"slices"
	"strings"
	"time"

	"github.com/osauer/canary/v2/internal/rpc"
)

const (
	opportunityBuilderSignalKind       = "constructive_breakout_v1"
	opportunityBuilderSignalSource     = "point_in_time_features"
	opportunityBuilderMinDollarVolume  = 50_000_000
	opportunityBuilderMaxExtension200D = 0.75
	opportunityBuilderMaxGapPct        = 25.0
)

// OpportunityPointInTimeRow is a captured, pre-signal research row whose
// features, split assignment, and labels retain their provenance.
type OpportunityPointInTimeRow struct {
	Date              string                         `json:"date,omitempty"`
	AsOf              time.Time                      `json:"as_of,omitzero"`
	Case              string                         `json:"case,omitempty"`
	Split             string                         `json:"split,omitempty"`
	SplitProvenance   OpportunitySplitProvenance     `json:"split_provenance,omitzero"`
	FeatureProvenance OpportunityFeatureProvenance   `json:"feature_provenance,omitzero"`
	LabelStatus       string                         `json:"label_status,omitempty"`
	MarketCluster     string                         `json:"market_cluster,omitempty"`
	Theme             string                         `json:"theme,omitempty"`
	Features          OpportunityPointInTimeFeatures `json:"features"`
	Trade             OpportunityBacktestTrade       `json:"trade"`
	Outcome           OpportunityBacktestOutcome     `json:"outcome"`
	Target            OpportunityBacktestTarget      `json:"target"`
	Notes             string                         `json:"notes,omitempty"`
}

// OpportunitySplitProvenance records how and when a tuning or holdout split was
// assigned, including whether it was preregistered before labels were known.
type OpportunitySplitProvenance struct {
	Source                  string    `json:"source,omitempty"`
	Method                  string    `json:"method,omitempty"`
	PlanID                  string    `json:"plan_id,omitempty"`
	AssignedAt              time.Time `json:"assigned_at,omitzero"`
	LabelStatusAtAssignment string    `json:"label_status_at_assignment,omitempty"`
	PreRegistered           bool      `json:"pre_registered,omitempty"`
}

// OpportunityFeatureProvenance identifies the source, construction method, and
// integrity checksum for captured point-in-time features.
type OpportunityFeatureProvenance struct {
	Source   string `json:"source,omitempty"`
	Method   string `json:"method,omitempty"`
	Checksum string `json:"checksum,omitempty"`
}

// IsZero reports whether no feature provenance fields are populated.
func (p OpportunityFeatureProvenance) IsZero() bool {
	return strings.TrimSpace(p.Source) == "" &&
		strings.TrimSpace(p.Method) == "" &&
		strings.TrimSpace(p.Checksum) == ""
}

// IsZero reports whether no split-assignment provenance is populated.
func (p OpportunitySplitProvenance) IsZero() bool {
	return strings.TrimSpace(p.Source) == "" &&
		strings.TrimSpace(p.Method) == "" &&
		strings.TrimSpace(p.PlanID) == "" &&
		p.AssignedAt.IsZero() &&
		strings.TrimSpace(p.LabelStatusAtAssignment) == "" &&
		!p.PreRegistered
}

// OpportunityPointInTimeFeatures contains the market, liquidity, trend, and
// optional macro inputs available at capture time.
type OpportunityPointInTimeFeatures struct {
	Instrument         string                   `json:"instrument,omitempty"`
	SecType            string                   `json:"sec_type,omitempty"`
	Exchange           string                   `json:"exchange,omitempty"`
	Currency           string                   `json:"currency,omitempty"`
	LocalSymbol        string                   `json:"local_symbol,omitempty"`
	TradingClass       string                   `json:"trading_class,omitempty"`
	InstrumentTags     []string                 `json:"instrument_tags,omitempty"`
	ScanPreset         string                   `json:"scan_preset,omitempty"`
	ScanType           string                   `json:"scan_type,omitempty"`
	ScanRank           int                      `json:"scan_rank,omitempty"`
	DataType           string                   `json:"data_type,omitempty"`
	FeedType           string                   `json:"feed_type,omitempty"`
	QuoteQuality       string                   `json:"quote_quality,omitempty"`
	Indicative         bool                     `json:"indicative,omitempty"`
	Stale              bool                     `json:"stale,omitempty"`
	StaleReason        string                   `json:"stale_reason,omitempty"`
	QuoteError         string                   `json:"quote_error,omitempty"`
	TechnicalError     string                   `json:"technical_error,omitempty"`
	SessionContext     *rpc.MarketSession       `json:"session_context,omitempty"`
	PriceAsOf          string                   `json:"price_as_of,omitempty"`
	PriceAt            time.Time                `json:"price_at,omitzero"`
	DataQuality        string                   `json:"data_quality,omitempty"`
	TrendState         string                   `json:"trend_state,omitempty"`
	Price              *float64                 `json:"price,omitempty"`
	SMA50              *float64                 `json:"sma_50,omitempty"`
	SMA200             *float64                 `json:"sma_200,omitempty"`
	PctAbove50DMA      *float64                 `json:"pct_above_50dma,omitempty"`
	PctAbove200DMA     *float64                 `json:"pct_above_200dma,omitempty"`
	RS63D              *float64                 `json:"rs_63d,omitempty"`
	RS126D             *float64                 `json:"rs_126d,omitempty"`
	AvgDollarVolume20D *float64                 `json:"avg_dollar_volume_20d,omitempty"`
	Volume             *int64                   `json:"volume,omitempty"`
	ChangePct          *float64                 `json:"change_pct,omitempty"`
	EventGapPct        *float64                 `json:"event_gap_pct,omitempty"`
	ExtendedChaseRisk  bool                     `json:"extended_chase_risk,omitempty"`
	Macro              *OpportunityMacroContext `json:"macro,omitempty"`
}

// OpportunityMacroContext captures the regime evidence attached to an
// opportunity row, including its source time and semantic fingerprint.
type OpportunityMacroContext struct {
	Source                     string          `json:"source,omitempty"`
	AsOf                       time.Time       `json:"as_of,omitzero"`
	Fingerprint                rpc.Fingerprint `json:"fingerprint,omitzero"`
	Label                      string          `json:"label,omitempty"`
	Tone                       string          `json:"tone,omitempty"`
	Stage                      string          `json:"stage,omitempty"`
	Severity                   string          `json:"severity,omitempty"`
	Readiness                  string          `json:"readiness,omitempty"`
	Confidence                 string          `json:"confidence,omitempty"`
	ClusterGreenCount          int             `json:"cluster_green_count,omitempty"`
	ClusterYellowCount         int             `json:"cluster_yellow_count,omitempty"`
	ClusterRedCount            int             `json:"cluster_red_count,omitempty"`
	ClusterRankedCount         int             `json:"cluster_ranked_count,omitempty"`
	ClusterEligibleRedCount    int             `json:"cluster_eligible_red_count,omitempty"`
	ClusterProvisionalRedCount int             `json:"cluster_provisional_red_count,omitempty"`
	Error                      string          `json:"error,omitempty"`
}

type opportunityCaptureOptions struct {
	Symbols          []string
	Preset           string
	Type             string
	Market           string
	Exchange         string
	Instrument       string
	Limit            int
	MinPrice         float64
	MinVolume        int64
	MinDollarVolume  float64
	RequireLive      bool
	ExcludePenny     bool
	IncludeETFs      bool
	IncludeRegime    bool
	Macro            *OpportunityMacroContext
	Split            string
	HoldoutPlan      string
	MarketCluster    string
	Theme            string
	Benchmark        string
	HorizonDays      int
	RoundTripCostBps float64
	CostModel        string
	LookbackDays     int
}

type opportunityCaptureAppendResult struct {
	Path              string
	Captured          int
	Appended          int
	SkippedDuplicates int
}

type opportunityCapturePreflightResult struct {
	Kind        string   `json:"kind"`
	Status      string   `json:"status"`
	RequireLive bool     `json:"require_live"`
	Mode        string   `json:"mode"`
	Blockers    []string `json:"blockers,omitempty"`
	NotAdvice   string   `json:"not_advice"`
}

type opportunityCapturePreflightError struct {
	Result opportunityCapturePreflightResult
}

func (e *opportunityCapturePreflightError) Error() string {
	if e == nil {
		return "--require-live preflight failed"
	}
	return "--require-live preflight failed: " + strings.Join(e.Result.Blockers, "; ")
}

func newOpportunityCapturePreflightError(useScanner bool, blockers []string) *opportunityCapturePreflightError {
	mode := "symbols"
	if useScanner {
		mode = "scanner"
	}
	return &opportunityCapturePreflightError{Result: opportunityCapturePreflightResult{
		Kind:        "opportunity_capture_preflight",
		Status:      "blocked",
		RequireLive: true,
		Mode:        mode,
		Blockers:    append([]string(nil), blockers...),
		NotAdvice:   "capture diagnostic only; not investment advice or a trade recommendation",
	}}
}

func readOpportunityPointInTimeRows(r io.Reader) ([]OpportunityPointInTimeRow, error) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	var out []OpportunityPointInTimeRow
	lineNo := 0
	for scanner.Scan() {
		lineNo++
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var row OpportunityPointInTimeRow
		if err := json.Unmarshal([]byte(line), &row); err != nil {
			return nil, fmt.Errorf("line %d: %w", lineNo, err)
		}
		if _, err := opportunityDecisionDateWithSession(row.Date, row.AsOf, row.Features.SessionContext); err != nil {
			return nil, fmt.Errorf("line %d: %w", lineNo, err)
		}
		out = append(out, row)
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

func buildOpportunityBacktestObservations(rows []OpportunityPointInTimeRow) []OpportunityBacktestObservation {
	out := make([]OpportunityBacktestObservation, 0, len(rows))
	for _, row := range rows {
		out = append(out, buildOpportunityBacktestObservation(row))
	}
	return out
}

func validateOpportunityPointInTimeRowsScored(rows []OpportunityPointInTimeRow) error {
	seen := map[string]int{}
	for i, row := range rows {
		lineNo := i + 1
		if err := validateOpportunityPointInTimeSplitProvenance(row); err != nil {
			return fmt.Errorf("line %d: %w", lineNo, err)
		}
		if err := validateOpportunityFeatureProvenance(row.FeatureProvenance, row.Features); err != nil {
			return fmt.Errorf("line %d: %w", lineNo, err)
		}
		if err := validateOpportunityLabelProvenance(lineNo, row.Date, row.AsOf, row.Features, row.LabelStatus, row.Target, row.Outcome, row.Trade); err != nil {
			return err
		}
		key, err := opportunityPointInTimeLedgerKey(row)
		if err != nil {
			return fmt.Errorf("line %d: %w", lineNo, err)
		}
		if prev, ok := seen[key]; ok {
			return fmt.Errorf("line %d: duplicate opportunity PIT row; first seen on line %d", lineNo, prev)
		}
		seen[key] = lineNo
	}
	return nil
}

func validateOpportunityBacktestObservationsSourced(rows []OpportunityBacktestObservation) error {
	seen := map[string]int{}
	for i, row := range rows {
		lineNo := i + 1
		if err := validateOpportunityObservationSplitProvenance(row); err != nil {
			return fmt.Errorf("line %d: %w", lineNo, err)
		}
		if err := validateOpportunityFeatureProvenance(row.FeatureProvenance, row.Features); err != nil {
			return fmt.Errorf("line %d: %w", lineNo, err)
		}
		if err := validateOpportunityLabelProvenance(lineNo, row.Date, row.AsOf, row.Features, row.LabelStatus, row.Target, row.Outcome, row.Trade); err != nil {
			return err
		}
		if err := validateOpportunitySignalProvenance(lineNo, row); err != nil {
			return err
		}
		key, err := opportunityBacktestObservationLedgerKey(row)
		if err != nil {
			return fmt.Errorf("line %d: %w", lineNo, err)
		}
		if prev, ok := seen[key]; ok {
			return fmt.Errorf("line %d: duplicate opportunity observation; first seen on line %d", lineNo, prev)
		}
		seen[key] = lineNo
	}
	return nil
}

func validateOpportunityFeatureProvenance(provenance OpportunityFeatureProvenance, features OpportunityPointInTimeFeatures) error {
	var missing []string
	if strings.TrimSpace(provenance.Source) == "" {
		missing = append(missing, "source")
	}
	if strings.TrimSpace(provenance.Method) == "" {
		missing = append(missing, "method")
	}
	if strings.TrimSpace(provenance.Checksum) == "" {
		missing = append(missing, "checksum")
	}
	if len(missing) > 0 {
		return fmt.Errorf("feature_provenance.%s required before opportunity backtest; capture or rebuild from point-in-time rows", strings.Join(missing, ","))
	}
	expected, err := opportunityFeatureChecksum(features)
	if err != nil {
		return err
	}
	if strings.TrimSpace(provenance.Checksum) != expected {
		return fmt.Errorf("feature_provenance.checksum mismatch; point-in-time features were edited after capture")
	}
	return nil
}

func opportunityFeatureProvenance(source, method string, features OpportunityPointInTimeFeatures) OpportunityFeatureProvenance {
	checksum, _ := opportunityFeatureChecksum(features)
	return OpportunityFeatureProvenance{
		Source:   strings.TrimSpace(source),
		Method:   strings.TrimSpace(method),
		Checksum: checksum,
	}
}

func opportunityFeatureChecksum(features OpportunityPointInTimeFeatures) (string, error) {
	b, err := json.Marshal(features)
	if err != nil {
		return "", fmt.Errorf("feature_provenance.checksum: %w", err)
	}
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

func validateOpportunitySignalProvenance(lineNo int, row OpportunityBacktestObservation) error {
	if reflect.DeepEqual(row.Features, OpportunityPointInTimeFeatures{}) {
		return fmt.Errorf("line %d: unverified signal provenance: opportunity observations must carry point-in-time features; rebuild from scored PIT rows with canary backtest build-opportunity", lineNo)
	}
	expected := opportunityPointInTimeSignal(row.Features)
	if !opportunitySignalsEqual(row.Signal, expected) {
		return fmt.Errorf("line %d: unverified signal provenance: signal does not match point-in-time features; rebuild from scored PIT rows with canary backtest build-opportunity", lineNo)
	}
	return nil
}

func opportunitySignalsEqual(got, want OpportunityBacktestSignal) bool {
	return got.Fired == want.Fired &&
		strings.TrimSpace(got.Kind) == strings.TrimSpace(want.Kind) &&
		strings.TrimSpace(got.Confidence) == strings.TrimSpace(want.Confidence) &&
		strings.TrimSpace(got.Source) == strings.TrimSpace(want.Source) &&
		slices.Equal(got.Reasons, want.Reasons)
}

func validateOpportunityPointInTimeSplitProvenance(row OpportunityPointInTimeRow) error {
	if err := validateOpportunitySplitProvenance(row.Split, row.SplitProvenance); err != nil {
		return err
	}
	return validateOpportunitySplitProvenanceTiming(row.Split, row.SplitProvenance, row.Date, row.AsOf, row.Features.SessionContext)
}

func validateOpportunityObservationSplitProvenance(row OpportunityBacktestObservation) error {
	if err := validateOpportunitySplitProvenance(row.Split, row.SplitProvenance); err != nil {
		return err
	}
	return validateOpportunitySplitProvenanceTiming(row.Split, row.SplitProvenance, row.Date, row.AsOf, row.Features.SessionContext)
}

func validateOpportunitySplitProvenance(split string, provenance OpportunitySplitProvenance) error {
	cleanSplit := cleanOpportunityBacktestSplit(split)
	if cleanSplit == "holdout" {
		var missing []string
		if strings.TrimSpace(provenance.Source) == "" {
			missing = append(missing, "source")
		}
		if strings.TrimSpace(provenance.Method) == "" {
			missing = append(missing, "method")
		}
		if strings.TrimSpace(provenance.PlanID) == "" {
			missing = append(missing, "plan_id")
		}
		if provenance.AssignedAt.IsZero() {
			missing = append(missing, "assigned_at")
		}
		if strings.TrimSpace(provenance.LabelStatusAtAssignment) == "" {
			missing = append(missing, "label_status_at_assignment")
		}
		if !provenance.PreRegistered && !opportunitySplitProvenanceRetrospective(provenance) {
			missing = append(missing, "pre_registered")
		}
		if len(missing) > 0 {
			return fmt.Errorf("split_provenance.%s required when split is holdout; capture holdout rows with --holdout-plan before scoring", strings.Join(missing, ","))
		}
		if strings.TrimSpace(provenance.LabelStatusAtAssignment) != "unscored_forward_window_pending" {
			return fmt.Errorf("split_provenance.label_status_at_assignment must be unscored_forward_window_pending when split is holdout")
		}
		return nil
	}
	if strings.TrimSpace(provenance.PlanID) != "" {
		return fmt.Errorf("split_provenance.plan_id is only valid when split is holdout")
	}
	return nil
}

func opportunitySplitProvenanceRetrospective(provenance OpportunitySplitProvenance) bool {
	return strings.TrimSpace(provenance.Method) == opportunityHistoricalPanelSplitMethod
}

func validateOpportunitySplitProvenanceTiming(split string, provenance OpportunitySplitProvenance, date string, asOf time.Time, session *rpc.MarketSession) error {
	if cleanOpportunityBacktestSplit(split) != "holdout" {
		return nil
	}
	deadline, exclusive, err := opportunitySplitAssignmentDeadline(date, asOf, session)
	if err != nil {
		return err
	}
	assignedAt := provenance.AssignedAt.UTC()
	if exclusive {
		if !assignedAt.Before(deadline) {
			return fmt.Errorf("split_provenance.assigned_at must be before the next session date for holdout rows")
		}
		return nil
	}
	if assignedAt.After(deadline) {
		return fmt.Errorf("split_provenance.assigned_at must be no later than as_of for holdout rows")
	}
	return nil
}

func opportunitySplitAssignmentDeadline(date string, asOf time.Time, session *rpc.MarketSession) (time.Time, bool, error) {
	if err := validateOpportunityDecisionDateAuthority(date, session); err != nil {
		return time.Time{}, false, err
	}
	if !asOf.IsZero() {
		return asOf.UTC(), false, nil
	}
	d, err := opportunityDecisionDateWithSession(date, asOf, session)
	if err != nil {
		return time.Time{}, false, err
	}
	return d.AddDate(0, 0, 1).UTC(), true, nil
}

func validateOpportunityLabelProvenance(lineNo int, date string, asOf time.Time, features OpportunityPointInTimeFeatures, labelStatus string, target OpportunityBacktestTarget, outcome OpportunityBacktestOutcome, trade OpportunityBacktestTrade) error {
	if strings.TrimSpace(labelStatus) != "scored" {
		return fmt.Errorf("line %d: label_status must be scored before opportunity backtest", lineNo)
	}
	if strings.TrimSpace(target.Kind) == "" {
		return fmt.Errorf("line %d: target.kind is required before opportunity backtest", lineNo)
	}
	if strings.TrimSpace(target.Source) == "" || strings.TrimSpace(target.Method) == "" {
		return fmt.Errorf("line %d: target.source and target.method are required before opportunity backtest", lineNo)
	}
	if strings.TrimSpace(outcome.EntryDate) == "" || strings.TrimSpace(outcome.ExitDate) == "" {
		return fmt.Errorf("line %d: outcome entry_date and exit_date are required before opportunity backtest", lineNo)
	}
	if err := validateOpportunityOutcomeChronology(date, asOf, features.SessionContext, outcome); err != nil {
		return fmt.Errorf("line %d: %w", lineNo, err)
	}
	if err := validateOpportunityEntryRuleChronology(date, asOf, features.SessionContext, trade, outcome); err != nil {
		return fmt.Errorf("line %d: %w", lineNo, err)
	}
	if strings.TrimSpace(outcome.PriceSource) == "" || strings.TrimSpace(outcome.BenchmarkSource) == "" {
		return fmt.Errorf("line %d: outcome price_source and benchmark_source are required before opportunity backtest", lineNo)
	}
	if strings.TrimSpace(outcome.Formula) == "" || strings.TrimSpace(outcome.PriceBasis) == "" {
		return fmt.Errorf("line %d: outcome formula and price_basis are required before opportunity backtest", lineNo)
	}
	if strings.TrimSpace(outcome.SourceChecksum) == "" || strings.TrimSpace(outcome.BenchmarkSourceChecksum) == "" {
		return fmt.Errorf("line %d: outcome source_checksum and benchmark_source_checksum are required before opportunity backtest", lineNo)
	}
	if trade.RoundTripCostBps == nil {
		return fmt.Errorf("line %d: trade.round_trip_cost_bps is required before opportunity backtest", lineNo)
	}
	return nil
}

func validateOpportunityEntryRuleChronology(date string, asOf time.Time, session *rpc.MarketSession, trade OpportunityBacktestTrade, outcome OpportunityBacktestOutcome) error {
	if err := validateOpportunityEntryRuleSupported(trade.EntryRule); err != nil {
		return err
	}
	if cleanOpportunityEntryRule(trade.EntryRule) != "next_close" {
		return nil
	}
	entry, err := parseOpportunityDate(outcome.EntryDate)
	if err != nil {
		return fmt.Errorf("invalid outcome.entry_date %q: %w", outcome.EntryDate, err)
	}
	decision, err := opportunityDecisionDateWithSession(date, asOf, session)
	if err != nil {
		return err
	}
	if !entry.After(decision) {
		return fmt.Errorf("outcome.entry_date must be after the observation date for entry_rule next_close")
	}
	return nil
}

func validateOpportunityOutcomeChronology(date string, asOf time.Time, session *rpc.MarketSession, outcome OpportunityBacktestOutcome) error {
	entry, err := parseOpportunityDate(outcome.EntryDate)
	if err != nil {
		return fmt.Errorf("invalid outcome.entry_date %q: %w", outcome.EntryDate, err)
	}
	exit, err := parseOpportunityDate(outcome.ExitDate)
	if err != nil {
		return fmt.Errorf("invalid outcome.exit_date %q: %w", outcome.ExitDate, err)
	}
	if exit.Before(entry) {
		return fmt.Errorf("outcome.exit_date must be on or after outcome.entry_date")
	}
	decision, err := opportunityDecisionDateWithSession(date, asOf, session)
	if err != nil {
		return err
	}
	if entry.Before(decision) {
		return fmt.Errorf("outcome.entry_date must be on or after the observation date")
	}
	return nil
}

func opportunityDecisionDateWithSession(date string, asOf time.Time, session *rpc.MarketSession) (time.Time, error) {
	if err := validateOpportunityDecisionDateAuthority(date, session); err != nil {
		return time.Time{}, err
	}
	if strings.TrimSpace(date) != "" {
		return parseOpportunityDate(date)
	}
	if session != nil && strings.TrimSpace(session.Date) != "" {
		return parseOpportunityDate(session.Date)
	}
	if !asOf.IsZero() {
		asOf = asOf.UTC()
		return time.Date(asOf.Year(), asOf.Month(), asOf.Day(), 0, 0, 0, 0, time.UTC), nil
	}
	return time.Time{}, fmt.Errorf("date, session_context.date, or as_of is required")
}

func validateOpportunityDecisionDateAuthority(date string, session *rpc.MarketSession) error {
	date = strings.TrimSpace(date)
	if date == "" || session == nil || strings.TrimSpace(session.Date) == "" {
		return nil
	}
	sessionDate, err := parseOpportunityDate(session.Date)
	if err != nil {
		return fmt.Errorf("invalid session_context.date %q: %w", session.Date, err)
	}
	rowDate, err := parseOpportunityDate(date)
	if err != nil {
		return fmt.Errorf("invalid date %q: %w", date, err)
	}
	if !rowDate.Equal(sessionDate) {
		return fmt.Errorf("date %s disagrees with session_context.date %s", rowDate.Format("2006-01-02"), sessionDate.Format("2006-01-02"))
	}
	return nil
}

func validateOpportunityBacktestOutcomesObservable(rows []OpportunityBacktestObservation, runAt time.Time) error {
	if runAt.IsZero() {
		return nil
	}
	runAt = runAt.UTC()
	runDate := time.Date(runAt.Year(), runAt.Month(), runAt.Day(), 0, 0, 0, 0, time.UTC)
	for i, row := range rows {
		exit, err := parseOpportunityDate(row.Outcome.ExitDate)
		if err != nil {
			return fmt.Errorf("line %d: invalid outcome.exit_date %q: %w", i+1, row.Outcome.ExitDate, err)
		}
		if exit.After(runDate) {
			return fmt.Errorf("line %d: outcome.exit_date %s is after backtest run date %s", i+1, row.Outcome.ExitDate, runDate.Format("2006-01-02"))
		}
	}
	return nil
}

func captureOpportunityPointInTimeRows(ctx context.Context, env *Env, opts opportunityCaptureOptions) ([]OpportunityPointInTimeRow, error) {
	if env == nil || env.Conn == nil {
		return nil, fmt.Errorf("daemon connection is required for capture-opportunity")
	}
	symbols := normalizeOpportunityCaptureSymbols(opts.Symbols)
	useScanner := len(symbols) == 0
	if opts.RequireLive {
		if err := opportunityCaptureRequireLivePreflight(ctx, env, useScanner); err != nil {
			return nil, err
		}
	}
	if opts.IncludeRegime && opts.Macro == nil {
		opts.Macro = opportunityCaptureRegimeContext(ctx, env)
	}
	if len(symbols) > 0 {
		return captureOpportunitySymbolPointInTimeRows(ctx, env, symbols, opts)
	}
	scanParams := rpc.ScanRunParams{
		Preset:          opts.Preset,
		Type:            opts.Type,
		Exchange:        opts.Exchange,
		Instrument:      opts.Instrument,
		Limit:           opts.Limit,
		MinPrice:        opts.MinPrice,
		MinVolume:       opts.MinVolume,
		MinDollarVolume: opts.MinDollarVolume,
		RequireLive:     opts.RequireLive,
		ExcludePenny:    opts.ExcludePenny,
	}
	var scan rpc.ScanResult
	if err := env.Conn.Call(ctx, rpc.MethodScanRun, scanParams, &scan); err != nil {
		return nil, fmt.Errorf("scan: %w", err)
	}
	symbols = opportunityCaptureSymbols(scan.Rows, opts.IncludeETFs)
	if len(symbols) == 0 {
		return nil, nil
	}
	quotes, quoteErrors, err := opportunityCaptureQuoteSnapshots(ctx, env, symbols, opts)
	if err != nil {
		return nil, err
	}
	benchmark := strings.ToUpper(strings.TrimSpace(opts.Benchmark))
	if benchmark == "" {
		benchmark = "QQQ"
	}
	lookback := opts.LookbackDays
	if lookback <= 0 {
		lookback = 420
	}
	var technical rpc.TechnicalResult
	if err := env.Conn.Call(ctx, rpc.MethodTechnical, rpc.TechnicalParams{
		Symbols:      symbols,
		Benchmark:    benchmark,
		LookbackDays: lookback,
	}, &technical); err != nil {
		return nil, fmt.Errorf("technical: %w", err)
	}
	return opportunityPointInTimeRowsFromSnapshots(scan, quotes, quoteErrors, technical, opts), nil
}

func captureOpportunitySymbolPointInTimeRows(ctx context.Context, env *Env, symbols []string, opts opportunityCaptureOptions) ([]OpportunityPointInTimeRow, error) {
	quotes, quoteErrors, err := opportunityCaptureQuoteSnapshots(ctx, env, symbols, opts)
	if err != nil {
		return nil, err
	}
	benchmark := strings.ToUpper(strings.TrimSpace(opts.Benchmark))
	if benchmark == "" {
		benchmark = "QQQ"
	}
	lookback := opts.LookbackDays
	if lookback <= 0 {
		lookback = 420
	}
	var technical rpc.TechnicalResult
	technicalErr := ""
	if err := env.Conn.Call(ctx, rpc.MethodTechnical, rpc.TechnicalParams{
		Symbols:      symbols,
		Benchmark:    benchmark,
		LookbackDays: lookback,
		Market:       strings.TrimSpace(opts.Market),
		Exchange:     strings.ToUpper(strings.TrimSpace(opts.Exchange)),
	}, &technical); err != nil {
		technicalErr = err.Error()
	}
	return opportunityPointInTimeRowsFromSymbolSnapshots(symbols, quotes, quoteErrors, technical, technicalErr, opts), nil
}

func opportunityCaptureRegimeContext(ctx context.Context, env *Env) *OpportunityMacroContext {
	out := &OpportunityMacroContext{Source: rpc.MethodRegimeSnapshot}
	if env == nil || env.Conn == nil {
		out.Error = "daemon connection is required for regime context"
		return out
	}
	var regime rpc.RegimeSnapshotResult
	if err := env.Conn.Call(ctx, rpc.MethodRegimeSnapshot, rpc.RegimeSnapshotParams{}, &regime); err != nil {
		out.Error = err.Error()
		return out
	}
	return opportunityMacroContextFromRegime(regime)
}

func opportunityMacroContextFromRegime(regime rpc.RegimeSnapshotResult) *OpportunityMacroContext {
	posture := regime.Posture
	if strings.TrimSpace(posture.Tone) == "" {
		posture = rpc.BuildRegimePosture(&regime)
	}
	fingerprint := regime.Fingerprint
	if strings.TrimSpace(fingerprint.Key) == "" {
		fingerprint = rpc.BuildRegimeFingerprint(&regime)
	}
	label := strings.TrimSpace(posture.Label)
	if label == "" {
		label = strings.TrimSpace(regime.Summary.Label)
	}
	if label == "" {
		label = strings.TrimSpace(regime.Composite.Verdict)
	}
	confidence := strings.TrimSpace(posture.Confidence)
	if confidence == "" {
		confidence = strings.TrimSpace(regime.Lifecycle.Confidence)
	}
	if confidence == "" {
		confidence = strings.TrimSpace(regime.Summary.Confidence)
	}
	return &OpportunityMacroContext{
		Source:                     rpc.MethodRegimeSnapshot,
		AsOf:                       regime.AsOf,
		Fingerprint:                fingerprint,
		Label:                      label,
		Tone:                       strings.TrimSpace(posture.Tone),
		Stage:                      strings.TrimSpace(posture.Stage),
		Severity:                   strings.TrimSpace(posture.Severity),
		Readiness:                  strings.TrimSpace(posture.Readiness),
		Confidence:                 confidence,
		ClusterGreenCount:          regime.Composite.ClusterGreenCount,
		ClusterYellowCount:         regime.Composite.ClusterYellowCount,
		ClusterRedCount:            regime.Composite.ClusterRedCount,
		ClusterRankedCount:         regime.Composite.ClusterRankedCount,
		ClusterEligibleRedCount:    regime.Composite.ClusterEligibleRedCount,
		ClusterProvisionalRedCount: regime.Composite.ClusterProvisionalRedCount,
	}
}

func cloneOpportunityMacroContext(in *OpportunityMacroContext) *OpportunityMacroContext {
	if in == nil {
		return nil
	}
	out := *in
	return &out
}

func opportunityCaptureRequireLivePreflight(ctx context.Context, env *Env, useScanner bool) error {
	var health rpc.HealthResult
	if err := env.Conn.Call(ctx, rpc.MethodStatusHealth, nil, &health); err != nil {
		return fmt.Errorf("status preflight: %w", err)
	}
	blockers := opportunityCaptureHealthBlockers(health, useScanner)
	if len(blockers) > 0 {
		return newOpportunityCapturePreflightError(useScanner, blockers)
	}
	return nil
}

func opportunityCaptureHealthBlockers(health rpc.HealthResult, useScanner bool) []string {
	var blockers []string
	if !health.Connected {
		reason := "gateway_unavailable"
		if strings.TrimSpace(health.LastError) != "" {
			reason += ": " + strings.TrimSpace(health.LastError)
		}
		return []string{reason}
	}
	required := map[string]struct{}{
		"quote":   {},
		"history": {},
	}
	if useScanner {
		required["scanner"] = struct{}{}
	}
	seen := map[string]struct{}{}
	for _, sub := range health.Subsystems {
		name := strings.ToLower(strings.TrimSpace(sub.Name))
		if _, ok := required[name]; !ok {
			continue
		}
		seen[name] = struct{}{}
		if !opportunityCaptureSubsystemReady(sub.Status) {
			blockers = append(blockers, formatSubsystemConcern(sub))
		}
	}
	for name := range required {
		if _, ok := seen[name]; !ok {
			blockers = append(blockers, name+" status missing")
		}
	}
	return blockers
}

func opportunityCaptureSubsystemReady(status string) bool {
	return strings.EqualFold(strings.TrimSpace(status), "ready")
}

func opportunityCaptureQuoteSnapshots(ctx context.Context, env *Env, symbols []string, opts opportunityCaptureOptions) (map[string]rpc.Quote, map[string]string, error) {
	quotes := make(map[string]rpc.Quote, len(symbols))
	errorsBySymbol := map[string]string{}
	for _, symbol := range symbols {
		contract := rpc.ContractParams{
			Symbol:   symbol,
			SecType:  "STK",
			Market:   strings.TrimSpace(opts.Market),
			Exchange: strings.ToUpper(strings.TrimSpace(opts.Exchange)),
		}
		if contract.Market == "" && contract.Exchange == "" {
			contract.Currency = "USD"
		}
		var q rpc.Quote
		err := env.Conn.Call(ctx, rpc.MethodQuoteSnapshot, rpc.QuoteSnapshotParams{
			Contract:         contract,
			TimeoutMs:        5000,
			IncludeLiquidity: true,
		}, &q)
		if err != nil {
			if isGatewayUnavailable(err.Error()) {
				return nil, nil, fmt.Errorf("quote %s: %w", symbol, err)
			}
			errorsBySymbol[symbol] = err.Error()
			continue
		}
		quotes[symbol] = q
	}
	return quotes, errorsBySymbol, nil
}

func normalizeOpportunityCaptureSymbols(raw []string) []string {
	seen := map[string]struct{}{}
	out := make([]string, 0, len(raw))
	for _, symbol := range raw {
		symbol = strings.ToUpper(strings.TrimSpace(symbol))
		if symbol == "" {
			continue
		}
		if _, ok := seen[symbol]; ok {
			continue
		}
		seen[symbol] = struct{}{}
		out = append(out, symbol)
	}
	return out
}

func opportunityCaptureSymbols(rows []rpc.ScanRow, includeETFs bool) []string {
	seen := map[string]struct{}{}
	var symbols []string
	for _, row := range rows {
		symbol := strings.ToUpper(strings.TrimSpace(row.Symbol))
		if symbol == "" {
			continue
		}
		if !includeETFs && opportunityScanRowHasTag(row, "etf", "leveraged_etp") {
			continue
		}
		if _, ok := seen[symbol]; ok {
			continue
		}
		seen[symbol] = struct{}{}
		symbols = append(symbols, symbol)
	}
	return symbols
}

func opportunityPointInTimeRowsFromSnapshots(scan rpc.ScanResult, quotes map[string]rpc.Quote, quoteErrors map[string]string, technical rpc.TechnicalResult, opts opportunityCaptureOptions) []OpportunityPointInTimeRow {
	techBySymbol := map[string]rpc.TechnicalRow{}
	for _, row := range technical.Rows {
		techBySymbol[strings.ToUpper(strings.TrimSpace(row.Symbol))] = row
	}
	asOf := scan.AsOf
	if asOf.IsZero() {
		asOf = technical.AsOf
	}
	date := ""
	if !asOf.IsZero() {
		date = asOf.Format("2006-01-02")
	}
	split := opportunityCaptureSplit(opts.Split)
	splitProvenance := opportunityCaptureSplitProvenance(split, opts.HoldoutPlan, asOf)
	benchmark := strings.ToUpper(strings.TrimSpace(opts.Benchmark))
	if benchmark == "" {
		benchmark = "QQQ"
	}
	horizonDays := opts.HorizonDays
	if horizonDays <= 0 {
		horizonDays = 126
	}
	costBps := opts.RoundTripCostBps
	if costBps < 0 {
		costBps = 0
	}
	costModel := strings.TrimSpace(opts.CostModel)
	if costModel == "" {
		costModel = fmt.Sprintf("flat-%.0fbps-capture", costBps)
	}
	cluster := strings.TrimSpace(opts.MarketCluster)
	if cluster == "" {
		cluster = "scanner:" + strings.TrimSpace(opportunityNonEmptyString(scan.Type, scan.Preset))
	}
	theme := strings.TrimSpace(opts.Theme)
	if theme == "" {
		theme = "live_opportunity_capture"
	}
	out := make([]OpportunityPointInTimeRow, 0, len(scan.Rows))
	for _, scanRow := range scan.Rows {
		symbol := strings.ToUpper(strings.TrimSpace(scanRow.Symbol))
		if symbol == "" || (!opts.IncludeETFs && opportunityScanRowHasTag(scanRow, "etf", "leveraged_etp")) {
			continue
		}
		tech, techOK := techBySymbol[symbol]
		quote, quoteOK := quotes[symbol]
		features := opportunityCaptureFeatures(scan, scanRow, quote, quoteOK, quoteErrors[symbol], tech, techOK)
		features.Macro = cloneOpportunityMacroContext(opts.Macro)
		row := OpportunityPointInTimeRow{
			Date:              opportunityCaptureRowDate(date, features),
			AsOf:              asOf,
			Case:              fmt.Sprintf("%s rank %d %s", opportunityNonEmptyString(scan.Type, scan.Preset), scanRow.Rank, symbol),
			Split:             split,
			SplitProvenance:   splitProvenance,
			FeatureProvenance: opportunityFeatureProvenance("capture-opportunity", "scanner_features_v1", features),
			LabelStatus:       "unscored_forward_window_pending",
			MarketCluster:     cluster,
			Theme:             theme,
			Features:          features,
			Trade: OpportunityBacktestTrade{
				Instrument:       symbol,
				EntryRule:        "next_close",
				HorizonDays:      horizonDays,
				Benchmark:        benchmark,
				RoundTripCostBps: &costBps,
				CostModel:        costModel,
			},
			Notes: "captured point-in-time candidate; add target/outcome labels only after the forward window is observable",
		}
		out = append(out, row)
	}
	return out
}

func opportunityPointInTimeRowsFromSymbolSnapshots(symbols []string, quotes map[string]rpc.Quote, quoteErrors map[string]string, technical rpc.TechnicalResult, technicalErr string, opts opportunityCaptureOptions) []OpportunityPointInTimeRow {
	techBySymbol := map[string]rpc.TechnicalRow{}
	for _, row := range technical.Rows {
		techBySymbol[strings.ToUpper(strings.TrimSpace(row.Symbol))] = row
	}
	asOf := technical.AsOf
	if asOf.IsZero() {
		for _, symbol := range symbols {
			if q := quotes[symbol]; !q.AsOf.IsZero() {
				asOf = q.AsOf
				break
			}
		}
	}
	if asOf.IsZero() {
		asOf = time.Now()
	}
	date := asOf.Format("2006-01-02")
	split := opportunityCaptureSplit(opts.Split)
	splitProvenance := opportunityCaptureSplitProvenance(split, opts.HoldoutPlan, asOf)
	benchmark := strings.ToUpper(strings.TrimSpace(opts.Benchmark))
	if benchmark == "" {
		benchmark = "QQQ"
	}
	horizonDays := opts.HorizonDays
	if horizonDays <= 0 {
		horizonDays = 126
	}
	costBps := opts.RoundTripCostBps
	if costBps < 0 {
		costBps = 0
	}
	costModel := strings.TrimSpace(opts.CostModel)
	if costModel == "" {
		costModel = fmt.Sprintf("flat-%.0fbps-capture", costBps)
	}
	cluster := strings.TrimSpace(opts.MarketCluster)
	if cluster == "" {
		cluster = "symbols:manual"
	}
	theme := strings.TrimSpace(opts.Theme)
	if theme == "" {
		theme = "live_opportunity_capture"
	}
	out := make([]OpportunityPointInTimeRow, 0, len(symbols))
	for i, symbol := range symbols {
		tech, techOK := techBySymbol[symbol]
		quote, quoteOK := quotes[symbol]
		features := opportunityCaptureFeaturesFromQuote(symbol, quote, quoteOK, quoteErrors[symbol], tech, techOK, technicalErr)
		features.Macro = cloneOpportunityMacroContext(opts.Macro)
		row := OpportunityPointInTimeRow{
			Date:              opportunityCaptureRowDate(date, features),
			AsOf:              asOf,
			Case:              fmt.Sprintf("manual symbol %d %s", i+1, symbol),
			Split:             split,
			SplitProvenance:   splitProvenance,
			FeatureProvenance: opportunityFeatureProvenance("capture-opportunity", "symbol_features_v1", features),
			LabelStatus:       "unscored_forward_window_pending",
			MarketCluster:     cluster,
			Theme:             theme,
			Features:          features,
			Trade: OpportunityBacktestTrade{
				Instrument:       symbol,
				EntryRule:        "next_close",
				HorizonDays:      horizonDays,
				Benchmark:        benchmark,
				RoundTripCostBps: &costBps,
				CostModel:        costModel,
			},
			Notes: "captured point-in-time candidate; add target/outcome labels only after the forward window is observable",
		}
		out = append(out, row)
	}
	return out
}

func opportunityCaptureRowDate(fallback string, features OpportunityPointInTimeFeatures) string {
	if features.SessionContext != nil && strings.TrimSpace(features.SessionContext.Date) != "" {
		return strings.TrimSpace(features.SessionContext.Date)
	}
	return strings.TrimSpace(fallback)
}

func opportunityCaptureSplitProvenance(split, holdoutPlan string, assignedAt time.Time) OpportunitySplitProvenance {
	cleanSplit := cleanOpportunityBacktestSplit(split)
	method := "capture_cli_tuning_default_v1"
	if cleanSplit == "holdout" {
		method = "capture_cli_explicit_holdout_v1"
	}
	return OpportunitySplitProvenance{
		Source:                  "capture-opportunity",
		Method:                  method,
		PlanID:                  strings.TrimSpace(holdoutPlan),
		AssignedAt:              assignedAt,
		LabelStatusAtAssignment: "unscored_forward_window_pending",
		PreRegistered:           true,
	}
}

func opportunityNonEmptyString(v, fallback string) string {
	if strings.TrimSpace(v) != "" {
		return v
	}
	return fallback
}

func opportunityCaptureSplit(raw string) string {
	split := cleanOpportunityBacktestSplit(raw)
	if split == "unknown" {
		return "tuning"
	}
	return split
}

func opportunityCaptureFeatures(scan rpc.ScanResult, scanRow rpc.ScanRow, quote rpc.Quote, quoteOK bool, quoteErr string, tech rpc.TechnicalRow, techOK bool) OpportunityPointInTimeFeatures {
	if !quoteOK && strings.TrimSpace(quoteErr) == "" {
		quoteErr = "quote unavailable"
	}
	price := quote.QuotePrice
	if price == nil {
		price = quote.Price
	}
	if price == nil {
		price = tech.Price
	}
	if price == nil {
		price = scanRow.Last
	}
	avgDollarVolume := quote.AvgDollarVolume20D
	if avgDollarVolume == nil {
		avgDollarVolume = tech.AvgDollarVolume20D
	}
	if avgDollarVolume == nil {
		avgDollarVolume = scanRow.AvgDollarVolume20D
	}
	changePct := quote.QuoteChangePct
	if changePct == nil {
		changePct = quote.ChangePct
	}
	if changePct == nil {
		changePct = scanRow.ChangePct
	}
	priceAsOf := strings.TrimSpace(quote.QuotePriceAsOf)
	if priceAsOf == "" {
		priceAsOf = quote.PriceAsOf
	}
	if priceAsOf == "" {
		priceAsOf = scanRow.PriceAsOf
	}
	priceAt := quote.QuotePriceAt
	if priceAt.IsZero() {
		priceAt = quote.PriceAt
	}
	if priceAt.IsZero() {
		priceAt = scanRow.PriceAt
	}
	volume := quote.Volume
	if volume == nil {
		volume = scanRow.Volume
	}
	secType := strings.TrimSpace(scanRow.SecType)
	if secType == "" {
		secType = quote.Contract.SecType
	}
	if secType == "" {
		secType = "STK"
	}
	exchange := strings.TrimSpace(scanRow.Exchange)
	if exchange == "" {
		exchange = quote.Contract.Exchange
	}
	currency := strings.TrimSpace(scanRow.Currency)
	if currency == "" {
		currency = quote.Contract.Currency
	}
	localSymbol := strings.TrimSpace(scanRow.LocalSymbol)
	if localSymbol == "" {
		localSymbol = quote.Contract.LocalSymbol
	}
	tradingClass := strings.TrimSpace(scanRow.TradingClass)
	if tradingClass == "" {
		tradingClass = quote.Contract.TradingClass
	}
	dataType := strings.TrimSpace(quote.DataType)
	if dataType == "" {
		dataType = scanRow.DataType
	}
	feedType := strings.TrimSpace(quote.FeedType)
	if feedType == "" {
		feedType = scanRow.FeedType
	}
	return OpportunityPointInTimeFeatures{
		Instrument:         strings.ToUpper(strings.TrimSpace(scanRow.Symbol)),
		SecType:            secType,
		Exchange:           exchange,
		Currency:           currency,
		LocalSymbol:        localSymbol,
		TradingClass:       tradingClass,
		InstrumentTags:     slices.Clone(scanRow.InstrumentTags),
		ScanPreset:         scan.Preset,
		ScanType:           scan.Type,
		ScanRank:           scanRow.Rank,
		DataType:           dataType,
		FeedType:           feedType,
		QuoteQuality:       quote.QuoteQuality,
		Indicative:         quote.Indicative,
		Stale:              quote.Stale,
		StaleReason:        quote.StaleReason,
		QuoteError:         strings.TrimSpace(quoteErr),
		SessionContext:     quote.SessionContext,
		PriceAsOf:          priceAsOf,
		PriceAt:            priceAt,
		DataQuality:        opportunityCaptureDataQuality(quoteErr, "", quote.Stale, tech, techOK),
		TechnicalError:     strings.TrimSpace(tech.Error),
		TrendState:         tech.TrendState,
		Price:              price,
		SMA50:              tech.SMA50,
		SMA200:             tech.SMA200,
		PctAbove50DMA:      tech.PctAbove50DMA,
		PctAbove200DMA:     tech.PctAbove200DMA,
		RS63D:              tech.RS63D,
		RS126D:             tech.RS126D,
		AvgDollarVolume20D: avgDollarVolume,
		Volume:             volume,
		ChangePct:          changePct,
		EventGapPct:        changePct,
	}
}

func opportunityCaptureFeaturesFromQuote(symbol string, quote rpc.Quote, quoteOK bool, quoteErr string, tech rpc.TechnicalRow, techOK bool, technicalErr string) OpportunityPointInTimeFeatures {
	if !quoteOK && strings.TrimSpace(quoteErr) == "" {
		quoteErr = "quote unavailable"
	}
	price := quote.QuotePrice
	if price == nil {
		price = quote.Price
	}
	if price == nil {
		price = tech.Price
	}
	avgDollarVolume := quote.AvgDollarVolume20D
	if avgDollarVolume == nil {
		avgDollarVolume = tech.AvgDollarVolume20D
	}
	changePct := quote.QuoteChangePct
	if changePct == nil {
		changePct = quote.ChangePct
	}
	priceAsOf := strings.TrimSpace(quote.QuotePriceAsOf)
	if priceAsOf == "" {
		priceAsOf = quote.PriceAsOf
	}
	priceAt := quote.QuotePriceAt
	if priceAt.IsZero() {
		priceAt = quote.PriceAt
	}
	volume := quote.Volume
	secType := quote.Contract.SecType
	if secType == "" {
		secType = "STK"
	}
	instrument := strings.ToUpper(strings.TrimSpace(quote.Symbol))
	if instrument == "" {
		instrument = strings.ToUpper(strings.TrimSpace(quote.Contract.Symbol))
	}
	if instrument == "" {
		instrument = strings.ToUpper(strings.TrimSpace(symbol))
	}
	return OpportunityPointInTimeFeatures{
		Instrument:         instrument,
		SecType:            secType,
		Exchange:           quote.Contract.Exchange,
		Currency:           quote.Contract.Currency,
		LocalSymbol:        quote.Contract.LocalSymbol,
		TradingClass:       quote.Contract.TradingClass,
		DataType:           quote.DataType,
		FeedType:           quote.FeedType,
		QuoteQuality:       quote.QuoteQuality,
		Indicative:         quote.Indicative,
		Stale:              quote.Stale,
		StaleReason:        quote.StaleReason,
		QuoteError:         strings.TrimSpace(quoteErr),
		TechnicalError:     strings.TrimSpace(opportunityNonEmptyString(tech.Error, technicalErr)),
		SessionContext:     quote.SessionContext,
		PriceAsOf:          priceAsOf,
		PriceAt:            priceAt,
		DataQuality:        opportunityCaptureDataQuality(quoteErr, technicalErr, quote.Stale, tech, techOK),
		TrendState:         tech.TrendState,
		Price:              price,
		SMA50:              tech.SMA50,
		SMA200:             tech.SMA200,
		PctAbove50DMA:      tech.PctAbove50DMA,
		PctAbove200DMA:     tech.PctAbove200DMA,
		RS63D:              tech.RS63D,
		RS126D:             tech.RS126D,
		AvgDollarVolume20D: avgDollarVolume,
		Volume:             volume,
		ChangePct:          changePct,
		EventGapPct:        changePct,
	}
}

func opportunityCaptureDataQuality(quoteErr, technicalErr string, stale bool, tech rpc.TechnicalRow, techOK bool) string {
	if strings.TrimSpace(quoteErr) != "" {
		return "quote_error"
	}
	if strings.TrimSpace(technicalErr) != "" {
		return "technical_error"
	}
	if strings.TrimSpace(tech.Error) != "" {
		return "technical_error"
	}
	if stale {
		return "stale_quote"
	}
	if !techOK {
		return "technical_missing"
	}
	return tech.DataQuality
}

func opportunityCaptureRowsSatisfyingLiveContext(rows []OpportunityPointInTimeRow) ([]OpportunityPointInTimeRow, []string) {
	filtered := make([]OpportunityPointInTimeRow, 0, len(rows))
	var skipped []string
	for _, row := range rows {
		blockers := opportunityCaptureLiveContextBlockers(row.Features)
		if len(blockers) == 0 {
			filtered = append(filtered, row)
			continue
		}
		instrument := cleanOpportunityBacktestInstrument(opportunityNonEmptyString(row.Trade.Instrument, row.Features.Instrument))
		skipped = append(skipped, fmt.Sprintf("%s: %s", instrument, strings.Join(blockers, ",")))
	}
	return filtered, skipped
}

func opportunityCaptureLiveContextBlockers(f OpportunityPointInTimeFeatures) []string {
	var blockers []string
	dataQuality := strings.ToLower(strings.TrimSpace(f.DataQuality))
	switch dataQuality {
	case "ok":
	case "":
		blockers = append(blockers, "data_quality_missing")
	default:
		blockers = append(blockers, "data_quality_"+dataQuality)
	}
	dataType := strings.ToLower(strings.TrimSpace(f.DataType))
	switch dataType {
	case rpc.MarketDataLive, opportunityHistoricalBarDataType:
	case "":
		blockers = append(blockers, "data_type_missing")
	default:
		blockers = append(blockers, "data_type_"+dataType)
	}
	quoteQuality := strings.ToLower(strings.TrimSpace(f.QuoteQuality))
	switch quoteQuality {
	case "firm", opportunityHistoricalBarQuoteQuality:
	case "":
		blockers = append(blockers, "quote_quality_missing")
	default:
		blockers = append(blockers, "quote_quality_"+quoteQuality)
	}
	if f.Stale {
		blockers = append(blockers, "quote_stale")
	}
	if f.Indicative {
		blockers = append(blockers, "quote_indicative")
	}
	if strings.TrimSpace(f.QuoteError) != "" {
		blockers = append(blockers, "quote_error")
	}
	if strings.TrimSpace(f.TechnicalError) != "" {
		blockers = append(blockers, "technical_error")
	}
	if f.Price == nil || *f.Price <= 0 {
		blockers = append(blockers, "price_missing")
	}
	if f.SessionContext == nil {
		blockers = append(blockers, "session_context_missing")
	} else {
		state := strings.ToLower(strings.TrimSpace(f.SessionContext.State))
		if !f.SessionContext.IsOpen || (state != "regular" && state != "early_close") {
			if state == "" {
				state = "unknown"
			}
			blockers = append(blockers, "session_"+state)
		}
	}
	return blockers
}

func opportunityScanRowHasTag(row rpc.ScanRow, tags ...string) bool {
	for _, got := range row.InstrumentTags {
		got = strings.ToLower(strings.TrimSpace(got))
		for _, want := range tags {
			if got == strings.ToLower(strings.TrimSpace(want)) {
				return true
			}
		}
	}
	return false
}

func buildOpportunityBacktestObservation(row OpportunityPointInTimeRow) OpportunityBacktestObservation {
	trade := row.Trade
	if strings.TrimSpace(trade.Instrument) == "" {
		trade.Instrument = row.Features.Instrument
	}
	return OpportunityBacktestObservation{
		Date:              row.Date,
		AsOf:              row.AsOf,
		Case:              row.Case,
		Split:             row.Split,
		SplitProvenance:   row.SplitProvenance,
		FeatureProvenance: row.FeatureProvenance,
		LabelStatus:       row.LabelStatus,
		MarketCluster:     row.MarketCluster,
		Theme:             row.Theme,
		Features:          row.Features,
		Signal:            opportunityPointInTimeSignal(row.Features),
		Trade:             trade,
		Outcome:           row.Outcome,
		Target:            row.Target,
		Notes:             row.Notes,
	}
}

func writeOpportunityBacktestObservationsJSONL(w io.Writer, rows []OpportunityBacktestObservation) error {
	enc := json.NewEncoder(w)
	for _, row := range rows {
		if err := enc.Encode(row); err != nil {
			return err
		}
	}
	return nil
}

func writeOpportunityPointInTimeRowsJSONL(w io.Writer, rows []OpportunityPointInTimeRow) error {
	enc := json.NewEncoder(w)
	for _, row := range rows {
		if err := enc.Encode(row); err != nil {
			return err
		}
	}
	return nil
}

func appendOpportunityPointInTimeRowsJSONL(path string, rows []OpportunityPointInTimeRow) (opportunityCaptureAppendResult, []OpportunityPointInTimeRow, error) {
	path = strings.TrimSpace(path)
	res := opportunityCaptureAppendResult{Path: path, Captured: len(rows)}
	if path == "" {
		return res, nil, fmt.Errorf("path is required")
	}
	seen := map[string]struct{}{}
	needsLeadingNewline := false
	f, err := os.Open(path)
	if err != nil {
		if !os.IsNotExist(err) {
			return res, nil, fmt.Errorf("open existing ledger: %w", err)
		}
	} else {
		existing, readErr := readOpportunityPointInTimeRows(f)
		if readErr != nil {
			_ = f.Close()
			return res, nil, fmt.Errorf("read existing ledger: %w", readErr)
		}
		stat, statErr := f.Stat()
		if statErr != nil {
			_ = f.Close()
			return res, nil, fmt.Errorf("stat existing ledger: %w", statErr)
		}
		if stat.Size() > 0 {
			if _, seekErr := f.Seek(-1, io.SeekEnd); seekErr != nil {
				_ = f.Close()
				return res, nil, fmt.Errorf("inspect existing ledger: %w", seekErr)
			}
			var last [1]byte
			if _, readErr := f.Read(last[:]); readErr != nil {
				_ = f.Close()
				return res, nil, fmt.Errorf("inspect existing ledger: %w", readErr)
			}
			needsLeadingNewline = last[0] != '\n'
		}
		if closeErr := f.Close(); closeErr != nil {
			return res, nil, fmt.Errorf("close existing ledger: %w", closeErr)
		}
		for i, row := range existing {
			key, keyErr := opportunityPointInTimeLedgerKey(row)
			if keyErr != nil {
				return res, nil, fmt.Errorf("existing row %d: %w", i+1, keyErr)
			}
			seen[key] = struct{}{}
		}
	}

	appended := make([]OpportunityPointInTimeRow, 0, len(rows))
	for i, row := range rows {
		key, keyErr := opportunityPointInTimeLedgerKey(row)
		if keyErr != nil {
			return res, nil, fmt.Errorf("captured row %d: %w", i+1, keyErr)
		}
		if _, ok := seen[key]; ok {
			res.SkippedDuplicates++
			continue
		}
		seen[key] = struct{}{}
		appended = append(appended, row)
	}
	res.Appended = len(appended)
	if len(appended) == 0 {
		return res, appended, nil
	}

	out, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return res, nil, fmt.Errorf("open append ledger: %w", err)
	}
	if needsLeadingNewline {
		if _, err := fmt.Fprintln(out); err != nil {
			_ = out.Close()
			return res, nil, fmt.Errorf("write ledger newline: %w", err)
		}
	}
	if err := writeOpportunityPointInTimeRowsJSONL(out, appended); err != nil {
		_ = out.Close()
		return res, nil, fmt.Errorf("write ledger: %w", err)
	}
	if err := out.Close(); err != nil {
		return res, nil, fmt.Errorf("close append ledger: %w", err)
	}
	return res, appended, nil
}

func opportunityPointInTimeLedgerKey(row OpportunityPointInTimeRow) (string, error) {
	date, err := opportunityDecisionDateString(row.Date, row.AsOf, row.Features.SessionContext)
	if err != nil {
		return "", err
	}
	instrument := strings.ToUpper(strings.TrimSpace(row.Trade.Instrument))
	if instrument == "" {
		instrument = strings.ToUpper(strings.TrimSpace(row.Features.Instrument))
	}
	if instrument == "" {
		return "", fmt.Errorf("instrument is required")
	}
	entryRule := strings.ToLower(strings.TrimSpace(row.Trade.EntryRule))
	if entryRule == "" {
		return "", fmt.Errorf("trade.entry_rule is required")
	}
	if row.Trade.HorizonDays <= 0 {
		return "", fmt.Errorf("trade.horizon_days must be positive")
	}
	return strings.Join([]string{
		"opportunity-pit-v1",
		date,
		instrument,
		strings.ToUpper(strings.TrimSpace(row.Features.SecType)),
		strings.ToUpper(strings.TrimSpace(row.Features.Exchange)),
		strings.ToUpper(strings.TrimSpace(row.Features.Currency)),
		entryRule,
		fmt.Sprintf("%d", row.Trade.HorizonDays),
	}, "\x00"), nil
}

func opportunityBacktestObservationLedgerKey(row OpportunityBacktestObservation) (string, error) {
	date, err := opportunityDecisionDateString(row.Date, row.AsOf, row.Features.SessionContext)
	if err != nil {
		if strings.TrimSpace(row.Date) != "" || !row.AsOf.IsZero() || (row.Features.SessionContext != nil && strings.TrimSpace(row.Features.SessionContext.Date) != "") {
			return "", err
		}
		date = strings.TrimSpace(row.Outcome.EntryDate)
	}
	if date == "" {
		return "", fmt.Errorf("date, as_of, or outcome.entry_date is required")
	}
	instrument := strings.ToUpper(strings.TrimSpace(row.Trade.Instrument))
	if instrument == "" {
		return "", fmt.Errorf("trade.instrument is required")
	}
	entryRule := strings.ToLower(strings.TrimSpace(row.Trade.EntryRule))
	if entryRule == "" {
		return "", fmt.Errorf("trade.entry_rule is required")
	}
	if row.Trade.HorizonDays <= 0 {
		return "", fmt.Errorf("trade.horizon_days must be positive")
	}
	return strings.Join([]string{
		"opportunity-observation-v1",
		date,
		instrument,
		entryRule,
		fmt.Sprintf("%d", row.Trade.HorizonDays),
	}, "\x00"), nil
}

func opportunityDecisionDateString(date string, asOf time.Time, session *rpc.MarketSession) (string, error) {
	decision, err := opportunityDecisionDateWithSession(date, asOf, session)
	if err != nil {
		return "", err
	}
	return decision.Format("2006-01-02"), nil
}

func opportunityPointInTimeSignal(f OpportunityPointInTimeFeatures) OpportunityBacktestSignal {
	return opportunityConstructiveBreakoutSignal(f)
}

func opportunityConstructiveBreakoutSignal(f OpportunityPointInTimeFeatures) OpportunityBacktestSignal {
	var reasons []string
	dataQuality := strings.ToLower(strings.TrimSpace(f.DataQuality))
	switch dataQuality {
	case "ok":
	case "":
		reasons = append(reasons, "data_quality_missing")
	default:
		reasons = append(reasons, "data_quality_not_ok")
	}
	dataType := strings.ToLower(strings.TrimSpace(f.DataType))
	switch dataType {
	case rpc.MarketDataLive, opportunityHistoricalBarDataType:
	case "":
		reasons = append(reasons, "data_type_missing")
	default:
		reasons = append(reasons, "data_type_not_live")
	}
	quoteQuality := strings.ToLower(strings.TrimSpace(f.QuoteQuality))
	switch quoteQuality {
	case "firm", opportunityHistoricalBarQuoteQuality:
	case "":
		reasons = append(reasons, "quote_quality_missing")
	case "missing", "prev_close", "stale":
		reasons = append(reasons, "quote_quality_"+quoteQuality)
	default:
		reasons = append(reasons, "quote_quality_not_firm")
	}
	if f.Stale {
		reasons = append(reasons, "quote_stale")
	}
	if f.Indicative {
		reasons = append(reasons, "quote_indicative")
	}
	if strings.TrimSpace(f.QuoteError) != "" {
		reasons = append(reasons, "quote_error")
	}
	if strings.TrimSpace(f.TechnicalError) != "" {
		reasons = append(reasons, "technical_error")
	}
	if f.Price == nil || *f.Price <= 0 {
		reasons = append(reasons, "price_missing")
	}
	if f.SessionContext == nil {
		reasons = append(reasons, "session_context_missing")
	} else {
		state := strings.ToLower(strings.TrimSpace(f.SessionContext.State))
		if !f.SessionContext.IsOpen || (state != "regular" && state != "early_close") {
			if state == "" {
				state = "unknown"
			}
			reasons = append(reasons, "session_"+state)
		}
	}
	pct50 := f.PctAbove50DMA
	if pct50 == nil && f.Price != nil && f.SMA50 != nil && *f.SMA50 > 0 {
		v := (*f.Price - *f.SMA50) / *f.SMA50
		pct50 = &v
	}
	if pct50 == nil {
		reasons = append(reasons, "pct_above_50dma_missing")
	} else if *pct50 <= 0 {
		reasons = append(reasons, "below_50dma")
	}
	pct200 := f.PctAbove200DMA
	if pct200 == nil && f.Price != nil && f.SMA200 != nil && *f.SMA200 > 0 {
		v := (*f.Price - *f.SMA200) / *f.SMA200
		pct200 = &v
	}
	if pct200 == nil {
		reasons = append(reasons, "pct_above_200dma_missing")
	} else {
		if *pct200 <= 0 {
			reasons = append(reasons, "below_200dma")
		}
		if *pct200 > opportunityBuilderMaxExtension200D {
			reasons = append(reasons, "extended_from_200dma")
		}
	}
	if f.RS63D == nil || *f.RS63D <= 0 {
		reasons = append(reasons, "rs_63d_not_positive")
	}
	if f.RS126D == nil || *f.RS126D <= 0 {
		reasons = append(reasons, "rs_126d_not_positive")
	}
	if f.AvgDollarVolume20D == nil || *f.AvgDollarVolume20D < opportunityBuilderMinDollarVolume {
		reasons = append(reasons, "liquidity_below_min")
	}
	if f.EventGapPct != nil && *f.EventGapPct > opportunityBuilderMaxGapPct {
		reasons = append(reasons, "event_gap_too_large")
	}
	if f.ExtendedChaseRisk {
		reasons = append(reasons, "extended_chase_risk")
	}
	fired := len(reasons) == 0
	confidence := "low"
	if fired {
		confidence = "medium"
		reasons = append(reasons, "passed_constructive_breakout_v1")
	}
	return OpportunityBacktestSignal{
		Fired:      fired,
		Kind:       opportunityBuilderSignalKind,
		Confidence: confidence,
		Source:     opportunityBuilderSignalSource,
		Reasons:    reasons,
	}
}
