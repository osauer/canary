package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/osauer/ibkr/v2/internal/risk"
	"github.com/osauer/ibkr/v2/internal/rpc"
	stressengine "github.com/osauer/ibkr/v2/internal/stress"
)

// CanaryBacktestObservation is one point-in-time canary input and its labelled
// forward stress target.
type CanaryBacktestObservation struct {
	Date          string                   `json:"date,omitempty"`
	AsOf          time.Time                `json:"as_of,omitzero"`
	Case          string                   `json:"case,omitempty"`
	MarketCluster string                   `json:"market_cluster,omitempty"`
	Account       rpc.AccountResult        `json:"account"`
	Positions     rpc.PositionsResult      `json:"positions"`
	Regime        rpc.RegimeSnapshotResult `json:"regime"`
	Target        CanaryBacktestTarget     `json:"target"`
	Notes         string                   `json:"notes,omitempty"`
}

// CanaryBacktestTarget records the forward-window stress label used to score a
// canary observation.
type CanaryBacktestTarget struct {
	Stress            bool     `json:"stress"`
	Kind              string   `json:"kind,omitempty"`
	Scope             string   `json:"scope,omitempty"`
	WindowDays        int      `json:"window_days,omitempty"`
	DaysToStress      *int     `json:"days_to_stress,omitempty"`
	MaxSPYDrawdownPct *float64 `json:"max_spy_drawdown_pct,omitempty"`
	VIXShockPct       *float64 `json:"vix_shock_pct,omitempty"`
	Notes             string   `json:"notes,omitempty"`
}

// CanaryBacktestResult contains row-level canary evaluations and aggregate
// detection, lifecycle, and regime-lift metrics for one replay.
type CanaryBacktestResult struct {
	RunAt        time.Time                      `json:"run_at"`
	Policy       string                         `json:"policy"`
	Observations []CanaryBacktestRowResult      `json:"observations"`
	Metrics      CanaryBacktestMetrics          `json:"metrics"`
	RegimeOnly   CanaryBacktestMetrics          `json:"regime_only"`
	Lifecycle    BacktestLifecycleMetrics       `json:"lifecycle"`
	Events       BacktestEventMetrics           `json:"events"`
	Categories   []CanaryBacktestClusterMetrics `json:"categories,omitempty"`
	RegimeLift   CanaryBacktestRegimeLift       `json:"regime_lift,omitzero"`
	Clusters     []CanaryBacktestClusterMetrics `json:"clusters,omitempty"`
	Findings     []string                       `json:"findings,omitempty"`
	NotAdvice    string                         `json:"not_advice"`
}

// CanaryBacktestRowResult records the canary decision and scoring flags for one
// labelled observation.
type CanaryBacktestRowResult struct {
	Date               string                `json:"date,omitempty"`
	Case               string                `json:"case,omitempty"`
	MarketCluster      string                `json:"market_cluster,omitempty"`
	TargetStress       bool                  `json:"target_stress"`
	TargetKind         string                `json:"target_kind,omitempty"`
	TargetScope        string                `json:"target_scope,omitempty"`
	WindowDays         int                   `json:"window_days,omitempty"`
	DaysToStress       *int                  `json:"days_to_stress,omitempty"`
	MaxSPYDrawdownPct  *float64              `json:"max_spy_drawdown_pct,omitempty"`
	VIXShockPct        *float64              `json:"vix_shock_pct,omitempty"`
	Direction          risk.SignalDirection  `json:"direction,omitempty"`
	Action             string                `json:"action,omitempty"`
	MarketConfirmation string                `json:"market_confirmation,omitempty"`
	PortfolioFit       string                `json:"portfolio_fit,omitempty"`
	InputHealth        string                `json:"input_health,omitempty"`
	Severity           risk.SignalSeverity   `json:"severity"`
	PlannerMode        risk.PlannerMode      `json:"planner_mode,omitempty"`
	PlannerReadiness   risk.PlannerReadiness `json:"planner_readiness,omitempty"`
	PrimaryDrivers     []risk.SignalID       `json:"primary_drivers,omitempty"`
	LifecycleStage     string                `json:"lifecycle_stage,omitempty"`
	SignalWatch        bool                  `json:"signal_watch"`
	DefensiveWatch     bool                  `json:"defensive_watch"`
	DefensiveAct       bool                  `json:"defensive_act"`
	RebalanceWatch     bool                  `json:"rebalance_watch"`
	DataQualityWatch   bool                  `json:"data_quality_watch"`
	Blocked            bool                  `json:"blocked"`
	EarlyWarning       bool                  `json:"early_warning"`
	ConfirmedStress    bool                  `json:"confirmed_stress"`
	Panic              bool                  `json:"panic"`
	Stabilization      bool                  `json:"stabilization"`
	Opportunity        bool                  `json:"opportunity"`
	RegimeOnlyWatch    bool                  `json:"regime_only_watch"`
	RegimeOnlyAct      bool                  `json:"regime_only_act"`
	Canary             *rpc.StressResult     `json:"canary,omitempty"`
}

// CanaryBacktestMetrics summarizes row-level watch and defensive-action
// classification performance.
type CanaryBacktestMetrics struct {
	Observations         int      `json:"observations"`
	TargetStress         int      `json:"target_stress"`
	NonStress            int      `json:"non_stress"`
	SignalWatch          int      `json:"signal_watch"`
	DefensiveWatch       int      `json:"defensive_watch"`
	DefensiveAct         int      `json:"defensive_act"`
	RebalanceWatch       int      `json:"rebalance_watch"`
	DataQualityWatch     int      `json:"data_quality_watch"`
	Blocked              int      `json:"blocked"`
	SignalTruePositive   int      `json:"signal_true_positive"`
	SignalFalsePositive  int      `json:"signal_false_positive"`
	SignalMiss           int      `json:"signal_miss"`
	SignalPrecision      *float64 `json:"signal_precision,omitempty"`
	SignalRecall         *float64 `json:"signal_recall,omitempty"`
	SignalFalseAlarmRate *float64 `json:"signal_false_alarm_rate,omitempty"`
	SignalAvgLeadDays    *float64 `json:"signal_avg_lead_days,omitempty"`
	WatchTruePositive    int      `json:"watch_true_positive"`
	WatchFalsePositive   int      `json:"watch_false_positive"`
	WatchMiss            int      `json:"watch_miss"`
	WatchPrecision       *float64 `json:"watch_precision,omitempty"`
	WatchRecall          *float64 `json:"watch_recall,omitempty"`
	WatchFalseAlarmRate  *float64 `json:"watch_false_alarm_rate,omitempty"`
	WatchAvgLeadDays     *float64 `json:"watch_avg_lead_days,omitempty"`
	ActTruePositive      int      `json:"act_true_positive"`
	ActFalsePositive     int      `json:"act_false_positive"`
	ActMiss              int      `json:"act_miss"`
	ActPrecision         *float64 `json:"act_precision,omitempty"`
	ActRecall            *float64 `json:"act_recall,omitempty"`
	ActFalseAlarmRate    *float64 `json:"act_false_alarm_rate,omitempty"`
	ActAvgLeadDays       *float64 `json:"act_avg_lead_days,omitempty"`
}

// CanaryBacktestClusterMetrics associates canary metrics with one named
// category or market cluster.
type CanaryBacktestClusterMetrics struct {
	Name    string                `json:"name"`
	Metrics CanaryBacktestMetrics `json:"metrics"`
}

// RegimeBacktestObservation is one point-in-time regime snapshot and its
// labelled forward stress target.
type RegimeBacktestObservation struct {
	Date          string                   `json:"date,omitempty"`
	AsOf          time.Time                `json:"as_of,omitzero"`
	Case          string                   `json:"case,omitempty"`
	MarketCluster string                   `json:"market_cluster,omitempty"`
	Regime        rpc.RegimeSnapshotResult `json:"regime"`
	Target        RegimeBacktestTarget     `json:"target"`
	Notes         string                   `json:"notes,omitempty"`
}

// RegimeBacktestTarget records the forward-window stress label used to score a
// regime observation.
type RegimeBacktestTarget struct {
	Stress            bool     `json:"stress"`
	Kind              string   `json:"kind,omitempty"`
	Scope             string   `json:"scope,omitempty"`
	WindowDays        int      `json:"window_days,omitempty"`
	DaysToStress      *int     `json:"days_to_stress,omitempty"`
	MaxSPYDrawdownPct *float64 `json:"max_spy_drawdown_pct,omitempty"`
	VIXShockPct       *float64 `json:"vix_shock_pct,omitempty"`
	Notes             string   `json:"notes,omitempty"`
}

// RegimeBacktestResult contains row-level regime evaluations and aggregate
// detection, lifecycle, and baseline metrics for one replay.
type RegimeBacktestResult struct {
	RunAt        time.Time                      `json:"run_at"`
	Policy       string                         `json:"policy"`
	Observations []RegimeBacktestRowResult      `json:"observations"`
	Metrics      RegimeBacktestMetrics          `json:"metrics"`
	Baseline     RegimeBacktestMetrics          `json:"baseline"`
	Lifecycle    BacktestLifecycleMetrics       `json:"lifecycle"`
	Events       BacktestEventMetrics           `json:"events"`
	Clusters     []RegimeBacktestClusterMetrics `json:"clusters,omitempty"`
	Findings     []string                       `json:"findings,omitempty"`
	NotAdvice    string                         `json:"not_advice"`
}

// RegimeBacktestRowResult records the regime verdict, evidence counts, and
// scoring flags for one labelled observation.
type RegimeBacktestRowResult struct {
	Date              string                    `json:"date,omitempty"`
	Case              string                    `json:"case,omitempty"`
	MarketCluster     string                    `json:"market_cluster,omitempty"`
	TargetStress      bool                      `json:"target_stress"`
	TargetKind        string                    `json:"target_kind,omitempty"`
	TargetScope       string                    `json:"target_scope,omitempty"`
	Scored            bool                      `json:"scored"`
	WindowDays        int                       `json:"window_days,omitempty"`
	DaysToStress      *int                      `json:"days_to_stress,omitempty"`
	MaxSPYDrawdownPct *float64                  `json:"max_spy_drawdown_pct,omitempty"`
	VIXShockPct       *float64                  `json:"vix_shock_pct,omitempty"`
	Verdict           string                    `json:"verdict,omitempty"`
	RedClusters       int                       `json:"red_clusters"`
	YellowClusters    int                       `json:"yellow_clusters"`
	RankedClusters    int                       `json:"ranked_clusters"`
	UnrankedClusters  int                       `json:"unranked_clusters"`
	RedClusterNames   []string                  `json:"red_cluster_names,omitempty"`
	LifecycleStage    string                    `json:"lifecycle_stage,omitempty"`
	StressWatch       bool                      `json:"stress_watch"`
	StressSignal      bool                      `json:"stress_signal"`
	DataQualityWatch  bool                      `json:"data_quality_watch"`
	EarlyWarning      bool                      `json:"early_warning"`
	ConfirmedStress   bool                      `json:"confirmed_stress"`
	Panic             bool                      `json:"panic"`
	Stabilization     bool                      `json:"stabilization"`
	Opportunity       bool                      `json:"opportunity"`
	BaselineWatch     bool                      `json:"baseline_watch"`
	BaselineStress    bool                      `json:"baseline_stress"`
	Regime            *rpc.RegimeSnapshotResult `json:"regime,omitempty"`
}

// RegimeBacktestMetrics summarizes scored regime watch and stress-signal
// performance; out-of-scope rows are counted separately.
type RegimeBacktestMetrics struct {
	Observations         int      `json:"observations"`
	ScoredObservations   int      `json:"scored_observations"`
	OutOfScope           int      `json:"out_of_scope"`
	TargetStress         int      `json:"target_stress"`
	NonStress            int      `json:"non_stress"`
	StressWatch          int      `json:"stress_watch"`
	StressSignal         int      `json:"stress_signal"`
	DataQualityWatch     int      `json:"data_quality_watch"`
	WatchTruePositive    int      `json:"watch_true_positive"`
	WatchFalsePositive   int      `json:"watch_false_positive"`
	WatchMiss            int      `json:"watch_miss"`
	WatchPrecision       *float64 `json:"watch_precision,omitempty"`
	WatchRecall          *float64 `json:"watch_recall,omitempty"`
	WatchFalseAlarmRate  *float64 `json:"watch_false_alarm_rate,omitempty"`
	WatchAvgLeadDays     *float64 `json:"watch_avg_lead_days,omitempty"`
	StressTruePositive   int      `json:"stress_true_positive"`
	StressFalsePositive  int      `json:"stress_false_positive"`
	StressMiss           int      `json:"stress_miss"`
	StressPrecision      *float64 `json:"stress_precision,omitempty"`
	StressRecall         *float64 `json:"stress_recall,omitempty"`
	StressFalseAlarmRate *float64 `json:"stress_false_alarm_rate,omitempty"`
	StressAvgLeadDays    *float64 `json:"stress_avg_lead_days,omitempty"`
}

// RegimeBacktestClusterMetrics associates regime metrics with one named market
// cluster.
type RegimeBacktestClusterMetrics struct {
	Name    string                `json:"name"`
	Metrics RegimeBacktestMetrics `json:"metrics"`
}

// BacktestLifecycleMetrics summarizes detection and false-start behavior across
// stress lifecycle stages.
type BacktestLifecycleMetrics struct {
	Observations                        int      `json:"observations"`
	TargetStress                        int      `json:"target_stress"`
	NonStress                           int      `json:"non_stress"`
	LaterConfirmedStress                int      `json:"later_confirmed_stress"`
	MajorStress                         int      `json:"major_stress"`
	EarlyWarning                        int      `json:"early_warning"`
	EarlyWarningTruePositive            int      `json:"early_warning_true_positive"`
	EarlyWarningFalsePositive           int      `json:"early_warning_false_positive"`
	EarlyWarningMiss                    int      `json:"early_warning_miss"`
	EarlyWarningPrecision               *float64 `json:"early_warning_precision,omitempty"`
	EarlyWarningRecall                  *float64 `json:"early_warning_recall,omitempty"`
	EarlyWarningFalseCalmRally          int      `json:"early_warning_false_calm_rally"`
	EarlyWarningMedianLeadDays          *float64 `json:"early_warning_median_lead_days,omitempty"`
	ConfirmedStress                     int      `json:"confirmed_stress"`
	ConfirmedStressTruePositive         int      `json:"confirmed_stress_true_positive"`
	ConfirmedStressFalsePositive        int      `json:"confirmed_stress_false_positive"`
	ConfirmedStressMiss                 int      `json:"confirmed_stress_miss"`
	ConfirmedStressPrecision            *float64 `json:"confirmed_stress_precision,omitempty"`
	ConfirmedStressRecall               *float64 `json:"confirmed_stress_recall,omitempty"`
	PanicCount                          int      `json:"panic_count"`
	PanicTruePositive                   int      `json:"panic_true_positive"`
	PanicMiss                           int      `json:"panic_miss"`
	PanicRecall                         *float64 `json:"panic_recall,omitempty"`
	Stabilization                       int      `json:"stabilization"`
	Opportunity                         int      `json:"opportunity"`
	StabilizationOpportunityFalseStarts int      `json:"stabilization_opportunity_false_starts"`
	DataQualityBlocked                  int      `json:"data_quality_blocked"`
}

// BacktestEventMetrics summarizes episode-level detection so consecutive rows
// from one stress event are not treated as independent events.
type BacktestEventMetrics struct {
	Events                       int      `json:"events"`
	TargetStressEvents           int      `json:"target_stress_events"`
	NonStressEvents              int      `json:"non_stress_events"`
	WatchEvents                  int      `json:"watch_events"`
	WatchTruePositiveEvents      int      `json:"watch_true_positive_events"`
	WatchFalsePositiveEvents     int      `json:"watch_false_positive_events"`
	WatchMissEvents              int      `json:"watch_miss_events"`
	WatchPrecision               *float64 `json:"watch_precision,omitempty"`
	WatchRecall                  *float64 `json:"watch_recall,omitempty"`
	ConfirmedStressEvents        int      `json:"confirmed_stress_events"`
	ConfirmedStressTruePositive  int      `json:"confirmed_stress_true_positive_events"`
	ConfirmedStressFalsePositive int      `json:"confirmed_stress_false_positive_events"`
	ConfirmedStressMiss          int      `json:"confirmed_stress_miss_events"`
	ConfirmedStressPrecision     *float64 `json:"confirmed_stress_precision,omitempty"`
	ConfirmedStressRecall        *float64 `json:"confirmed_stress_recall,omitempty"`
	PanicEvents                  int      `json:"panic_events"`
	PanicRecall                  *float64 `json:"panic_recall,omitempty"`
}

// CanaryBacktestRegimeLift compares canary watch recall with the regime-only
// baseline on portfolio-stress rows.
type CanaryBacktestRegimeLift struct {
	PortfolioStressRows         int      `json:"portfolio_stress_rows"`
	RegimeOnlyWatchTruePositive int      `json:"regime_only_watch_true_positive"`
	CanaryWatchTruePositive     int      `json:"canary_watch_true_positive"`
	CanaryAddedTruePositive     int      `json:"canary_added_true_positive"`
	RegimeOnlyRecall            *float64 `json:"regime_only_recall,omitempty"`
	CanaryRecall                *float64 `json:"canary_recall,omitempty"`
}

// OpportunityBacktestObservation is one point-in-time research signal, trade
// model, realized outcome, and labelled opportunity target.
type OpportunityBacktestObservation struct {
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
	Signal            OpportunityBacktestSignal      `json:"signal"`
	Trade             OpportunityBacktestTrade       `json:"trade"`
	Outcome           OpportunityBacktestOutcome     `json:"outcome"`
	Target            OpportunityBacktestTarget      `json:"target"`
	Notes             string                         `json:"notes,omitempty"`
}

// OpportunityBacktestSignal records whether a research rule fired and the
// provenance and reasons it reported.
type OpportunityBacktestSignal struct {
	Fired      bool     `json:"fired"`
	Kind       string   `json:"kind,omitempty"`
	Confidence string   `json:"confidence,omitempty"`
	Source     string   `json:"source,omitempty"`
	Reasons    []string `json:"reasons,omitempty"`
}

// OpportunityBacktestTrade describes the instrument, horizon, benchmark, and
// execution-cost assumptions used to score an observation.
type OpportunityBacktestTrade struct {
	Instrument       string   `json:"instrument,omitempty"`
	EntryRule        string   `json:"entry_rule,omitempty"`
	HorizonDays      int      `json:"horizon_days,omitempty"`
	Benchmark        string   `json:"benchmark,omitempty"`
	RoundTripCostBps *float64 `json:"round_trip_cost_bps,omitempty"`
	CostModel        string   `json:"cost_model,omitempty"`
}

// OpportunityBacktestOutcome contains the observed forward return, benchmark,
// excursion, and source-integrity measurements for a trade horizon.
type OpportunityBacktestOutcome struct {
	EntryDate                string   `json:"entry_date,omitempty"`
	ExitDate                 string   `json:"exit_date,omitempty"`
	EntryPrice               *float64 `json:"entry_price,omitempty"`
	ExitPrice                *float64 `json:"exit_price,omitempty"`
	PriceSource              string   `json:"price_source,omitempty"`
	BenchmarkSource          string   `json:"benchmark_source,omitempty"`
	Formula                  string   `json:"formula,omitempty"`
	PriceBasis               string   `json:"price_basis,omitempty"`
	SourceChecksum           string   `json:"source_checksum,omitempty"`
	BenchmarkSourceChecksum  string   `json:"benchmark_source_checksum,omitempty"`
	ForwardReturnPct         float64  `json:"forward_return_pct"`
	BenchmarkReturnPct       float64  `json:"benchmark_return_pct"`
	ExcessReturnPct          float64  `json:"excess_return_pct"`
	MaxAdverseExcursionPct   float64  `json:"max_adverse_excursion_pct"`
	MaxFavorableExcursionPct float64  `json:"max_favorable_excursion_pct"`
}

// OpportunityBacktestTarget records the labelled opportunity outcome and its
// source and method.
type OpportunityBacktestTarget struct {
	Opportunity bool   `json:"opportunity"`
	Scope       string `json:"scope,omitempty"`
	Kind        string `json:"kind,omitempty"`
	Source      string `json:"source,omitempty"`
	Method      string `json:"method,omitempty"`
	Notes       string `json:"notes,omitempty"`
}

// OpportunityBacktestResult contains evaluated rows, portfolio simulation,
// evidence sufficiency, diagnostics, and aggregate metrics for one replay.
type OpportunityBacktestResult struct {
	RunAt        time.Time                           `json:"run_at"`
	Policy       string                              `json:"policy"`
	Observations []OpportunityBacktestRowResult      `json:"observations"`
	Metrics      OpportunityBacktestMetrics          `json:"metrics"`
	Simulation   OpportunityBacktestSimulation       `json:"simulation"`
	Evidence     OpportunityBacktestEvidence         `json:"evidence"`
	Diagnostics  OpportunityBacktestDiagnostics      `json:"diagnostics,omitzero"`
	Clusters     []OpportunityBacktestClusterMetrics `json:"clusters,omitempty"`
	Findings     []string                            `json:"findings,omitempty"`
	NotAdvice    string                              `json:"not_advice"`
}

// OpportunityBacktestSimulation summarizes a bounded-slot portfolio replay and
// its explicit limitations.
type OpportunityBacktestSimulation struct {
	Model                string                             `json:"model,omitempty"`
	Signals              int                                `json:"signals"`
	FilledSignals        int                                `json:"filled_signals"`
	SkippedSignals       int                                `json:"skipped_signals"`
	MaxSlots             int                                `json:"max_slots"`
	MaxConcurrent        int                                `json:"max_concurrent"`
	AvgConcurrent        *float64                           `json:"avg_concurrent,omitempty"`
	InvestedExposureDays int                                `json:"invested_exposure_days"`
	PortfolioReturnPct   *float64                           `json:"portfolio_return_pct,omitempty"`
	BenchmarkReturnPct   *float64                           `json:"benchmark_return_pct,omitempty"`
	ExcessReturnPct      *float64                           `json:"excess_return_pct,omitempty"`
	TurnoverPct          *float64                           `json:"turnover_pct,omitempty"`
	AvgHoldDays          *float64                           `json:"avg_hold_days,omitempty"`
	CashDragDays         int                                `json:"cash_drag_days"`
	WindowStart          string                             `json:"window_start,omitempty"`
	WindowEnd            string                             `json:"window_end,omitempty"`
	Limitations          []string                           `json:"limitations,omitempty"`
	MarkToMarket         *OpportunityMarkToMarketSimulation `json:"mark_to_market,omitempty"`
	Holdout              *OpportunityBacktestSimulation     `json:"holdout,omitempty"`
}

// OpportunityMarkToMarketSimulation summarizes bar-by-bar portfolio and
// benchmark performance with source provenance and data-quality limits.
type OpportunityMarkToMarketSimulation struct {
	Model                      string   `json:"model,omitempty"`
	Trades                     int      `json:"trades"`
	Bars                       int      `json:"bars"`
	MinTradeMarks              int      `json:"min_trade_marks"`
	MaxTradeMarkGapDays        int      `json:"max_trade_mark_gap_days"`
	PriceSource                string   `json:"price_source,omitempty"`
	SourceChecksum             string   `json:"source_checksum,omitempty"`
	SourceManifest             string   `json:"source_manifest,omitempty"`
	SourceManifestChecksum     string   `json:"source_manifest_checksum,omitempty"`
	SourceProvider             string   `json:"source_provider,omitempty"`
	SourceMethod               string   `json:"source_method,omitempty"`
	SourceCreatedAt            string   `json:"source_created_at,omitempty"`
	SourceQuality              string   `json:"source_quality,omitempty"`
	SourceWarnings             []string `json:"source_warnings,omitempty"`
	BarSources                 []string `json:"bar_sources,omitempty"`
	PriceBasis                 string   `json:"price_basis,omitempty"`
	PortfolioReturnPct         *float64 `json:"portfolio_return_pct,omitempty"`
	BenchmarkReturnPct         *float64 `json:"benchmark_return_pct,omitempty"`
	ExcessReturnPct            *float64 `json:"excess_return_pct,omitempty"`
	MaxDrawdownPct             *float64 `json:"max_drawdown_pct,omitempty"`
	BenchmarkMaxDrawdownPct    *float64 `json:"benchmark_max_drawdown_pct,omitempty"`
	WorstBarReturnPct          *float64 `json:"worst_bar_return_pct,omitempty"`
	BestBarReturnPct           *float64 `json:"best_bar_return_pct,omitempty"`
	BarReturnVolPct            *float64 `json:"bar_return_vol_pct,omitempty"`
	BenchmarkBarReturnVolPct   *float64 `json:"benchmark_bar_return_vol_pct,omitempty"`
	EndPortfolioEquityMultiple *float64 `json:"end_portfolio_equity_multiple,omitempty"`
	EndBenchmarkEquityMultiple *float64 `json:"end_benchmark_equity_multiple,omitempty"`
	Limitations                []string `json:"limitations,omitempty"`
}

type opportunitySimulationTrade struct {
	row         OpportunityBacktestRowResult
	entry       time.Time
	exit        time.Time
	netReturn   float64
	benchReturn float64
}

// OpportunityBacktestEvidence reports whether a replay satisfies the minimum
// sample, holdout, concentration, cost, and mark-to-market evidence gates.
type OpportunityBacktestEvidence struct {
	Status                          string                           `json:"status"`
	MinObservations                 int                              `json:"min_observations"`
	MinSignalFired                  int                              `json:"min_signal_fired"`
	MinTargetOpportunity            int                              `json:"min_target_opportunity"`
	MinNonOpportunity               int                              `json:"min_non_opportunity"`
	MinSignalInstruments            int                              `json:"min_signal_instruments"`
	MinSignalClusters               int                              `json:"min_signal_clusters"`
	MinHoldoutObservations          int                              `json:"min_holdout_observations"`
	MinHoldoutSignalFired           int                              `json:"min_holdout_signal_fired"`
	MinHoldoutTargetOpportunity     int                              `json:"min_holdout_target_opportunity"`
	MinHoldoutNonOpportunity        int                              `json:"min_holdout_non_opportunity"`
	MinHoldoutSignalInstruments     int                              `json:"min_holdout_signal_instruments"`
	MinHoldoutSignalClusters        int                              `json:"min_holdout_signal_clusters"`
	MinPortfolioFilledSignals       int                              `json:"min_portfolio_filled_signals"`
	MaxSignalInstrumentShare        float64                          `json:"max_signal_instrument_share"`
	MaxSignalClusterShare           float64                          `json:"max_signal_cluster_share"`
	MaxHoldoutSignalInstrumentShare float64                          `json:"max_holdout_signal_instrument_share"`
	MaxHoldoutSignalClusterShare    float64                          `json:"max_holdout_signal_cluster_share"`
	MaxMarkToMarketDrawdownPct      float64                          `json:"max_mark_to_market_drawdown_pct"`
	MaxMarkToMarketGapDays          int                              `json:"max_mark_to_market_gap_days"`
	MinMarkToMarketExcessToDrawdown float64                          `json:"min_mark_to_market_excess_to_drawdown"`
	Needs                           OpportunityBacktestEvidenceNeeds `json:"needs"`
	Reasons                         []string                         `json:"reasons,omitempty"`
}

// OpportunityBacktestEvidenceNeeds quantifies remaining evidence deficits for
// an opportunity replay.
type OpportunityBacktestEvidenceNeeds struct {
	AdditionalObservations             int `json:"additional_observations"`
	AdditionalSignalFired              int `json:"additional_signal_fired"`
	AdditionalTargetOpportunity        int `json:"additional_target_opportunity"`
	AdditionalNonOpportunity           int `json:"additional_non_opportunity"`
	AdditionalSignalInstruments        int `json:"additional_signal_instruments"`
	AdditionalSignalClusters           int `json:"additional_signal_clusters"`
	AdditionalHoldoutObservations      int `json:"additional_holdout_observations"`
	AdditionalHoldoutSignalFired       int `json:"additional_holdout_signal_fired"`
	AdditionalHoldoutTargetOpportunity int `json:"additional_holdout_target_opportunity"`
	AdditionalHoldoutNonOpportunity    int `json:"additional_holdout_non_opportunity"`
	AdditionalHoldoutSignalInstruments int `json:"additional_holdout_signal_instruments"`
	AdditionalHoldoutSignalClusters    int `json:"additional_holdout_signal_clusters"`
	UnknownSplitObservations           int `json:"unknown_split_observations"`
	RetrospectiveHoldoutObservations   int `json:"retrospective_holdout_observations"`
	MissingCostSignalFired             int `json:"missing_cost_signal_fired"`
	SignalContextBlocked               int `json:"signal_context_blocked"`
}

// OpportunityBacktestRowResult records signal classification and cost-adjusted
// outcome measurements for one observation.
type OpportunityBacktestRowResult struct {
	Date                 string                     `json:"date,omitempty"`
	Case                 string                     `json:"case,omitempty"`
	Split                string                     `json:"split,omitempty"`
	SplitProvenance      OpportunitySplitProvenance `json:"split_provenance,omitzero"`
	LabelStatus          string                     `json:"label_status,omitempty"`
	Holdout              bool                       `json:"holdout"`
	RetrospectiveHoldout bool                       `json:"retrospective_holdout,omitempty"`
	MarketCluster        string                     `json:"market_cluster,omitempty"`
	Theme                string                     `json:"theme,omitempty"`
	TargetOpportunity    bool                       `json:"target_opportunity"`
	TargetKind           string                     `json:"target_kind,omitempty"`
	TargetScope          string                     `json:"target_scope,omitempty"`
	SignalFired          bool                       `json:"signal_fired"`
	SignalKind           string                     `json:"signal_kind,omitempty"`
	SignalConfidence     string                     `json:"signal_confidence,omitempty"`
	SignalSource         string                     `json:"signal_source,omitempty"`
	SignalReasons        []string                   `json:"signal_reasons,omitempty"`
	SignalContextBlocked bool                       `json:"signal_context_blocked,omitempty"`
	TruePositive         bool                       `json:"true_positive"`
	FalsePositive        bool                       `json:"false_positive"`
	Miss                 bool                       `json:"miss"`
	PositiveExcess       bool                       `json:"positive_excess"`
	ExecutionCostPct     *float64                   `json:"execution_cost_pct,omitempty"`
	NetExcessReturnPct   *float64                   `json:"net_excess_return_pct,omitempty"`
	PositiveNetExcess    *bool                      `json:"positive_net_excess,omitempty"`
	Trade                OpportunityBacktestTrade   `json:"trade"`
	Outcome              OpportunityBacktestOutcome `json:"outcome"`
	sourceObservation    *OpportunityBacktestObservation
}

// OpportunityBacktestMetrics summarizes classification, holdout, cost-adjusted
// return, concentration, and excursion measurements.
type OpportunityBacktestMetrics struct {
	Observations                         int      `json:"observations"`
	TargetOpportunity                    int      `json:"target_opportunity"`
	NonOpportunity                       int      `json:"non_opportunity"`
	TuningObservations                   int      `json:"tuning_observations"`
	HoldoutObservations                  int      `json:"holdout_observations"`
	UnknownSplitObservations             int      `json:"unknown_split_observations"`
	RetrospectiveHoldoutObservations     int      `json:"retrospective_holdout_observations"`
	SignalContextBlocked                 int      `json:"signal_context_blocked"`
	HoldoutSignalContextBlocked          int      `json:"holdout_signal_context_blocked"`
	HoldoutTargetOpportunity             int      `json:"holdout_target_opportunity"`
	HoldoutNonOpportunity                int      `json:"holdout_non_opportunity"`
	SignalFired                          int      `json:"signal_fired"`
	HoldoutSignalFired                   int      `json:"holdout_signal_fired"`
	HoldoutCostedSignalFired             int      `json:"holdout_costed_signal_fired"`
	HoldoutMissingCostSignalFired        int      `json:"holdout_missing_cost_signal_fired"`
	HoldoutPositiveNetExcess             int      `json:"holdout_positive_net_excess"`
	HoldoutNegativeNetExcess             int      `json:"holdout_negative_net_excess"`
	HoldoutNetExcessHitRate              *float64 `json:"holdout_net_excess_hit_rate,omitempty"`
	HoldoutNetExcessHitRateLower95       *float64 `json:"holdout_net_excess_hit_rate_lower_95,omitempty"`
	HoldoutAvgNetExcessReturnPct         *float64 `json:"holdout_avg_net_excess_return_pct,omitempty"`
	HoldoutAvgNetExcessReturnLower95Pct  *float64 `json:"holdout_avg_net_excess_return_lower_95_pct,omitempty"`
	HoldoutCostedCandidates              int      `json:"holdout_costed_candidates"`
	HoldoutPositiveCandidateNetExcess    int      `json:"holdout_positive_candidate_net_excess"`
	HoldoutNegativeCandidateNetExcess    int      `json:"holdout_negative_candidate_net_excess"`
	HoldoutCandidateNetExcessHitRate     *float64 `json:"holdout_candidate_net_excess_hit_rate,omitempty"`
	HoldoutAvgCandidateNetExcessPct      *float64 `json:"holdout_avg_candidate_net_excess_pct,omitempty"`
	HoldoutMedianCandidateNetExcessPct   *float64 `json:"holdout_median_candidate_net_excess_pct,omitempty"`
	HoldoutNonFiredCostedCandidates      int      `json:"holdout_non_fired_costed_candidates"`
	HoldoutAvgNonFiredCandidateNetPct    *float64 `json:"holdout_avg_non_fired_candidate_net_pct,omitempty"`
	HoldoutMedianNonFiredCandidateNetPct *float64 `json:"holdout_median_non_fired_candidate_net_pct,omitempty"`
	HoldoutFiredVsCandidateAvgLiftPct    *float64 `json:"holdout_fired_vs_candidate_avg_lift_pct,omitempty"`
	HoldoutFiredVsCandidateMedianLiftPct *float64 `json:"holdout_fired_vs_candidate_median_lift_pct,omitempty"`
	HoldoutFiredVsNonFiredAvgLiftPct     *float64 `json:"holdout_fired_vs_non_fired_avg_lift_pct,omitempty"`
	HoldoutFiredVsNonFiredMedianLiftPct  *float64 `json:"holdout_fired_vs_non_fired_median_lift_pct,omitempty"`
	HoldoutDistinctSignalInstruments     int      `json:"holdout_distinct_signal_instruments"`
	HoldoutMaxSignalInstrument           string   `json:"holdout_max_signal_instrument,omitempty"`
	HoldoutMaxSignalInstrumentFired      int      `json:"holdout_max_signal_instrument_fired,omitempty"`
	HoldoutMaxSignalInstrumentShare      *float64 `json:"holdout_max_signal_instrument_share,omitempty"`
	HoldoutDistinctSignalClusters        int      `json:"holdout_distinct_signal_clusters"`
	HoldoutMaxSignalCluster              string   `json:"holdout_max_signal_cluster,omitempty"`
	HoldoutMaxSignalClusterFired         int      `json:"holdout_max_signal_cluster_fired,omitempty"`
	HoldoutMaxSignalClusterShare         *float64 `json:"holdout_max_signal_cluster_share,omitempty"`
	DistinctSignalInstruments            int      `json:"distinct_signal_instruments"`
	MaxSignalInstrument                  string   `json:"max_signal_instrument,omitempty"`
	MaxSignalInstrumentFired             int      `json:"max_signal_instrument_fired,omitempty"`
	MaxSignalInstrumentShare             *float64 `json:"max_signal_instrument_share,omitempty"`
	DistinctSignalClusters               int      `json:"distinct_signal_clusters"`
	MaxSignalCluster                     string   `json:"max_signal_cluster,omitempty"`
	MaxSignalClusterFired                int      `json:"max_signal_cluster_fired,omitempty"`
	MaxSignalClusterShare                *float64 `json:"max_signal_cluster_share,omitempty"`
	TruePositive                         int      `json:"true_positive"`
	FalsePositive                        int      `json:"false_positive"`
	Miss                                 int      `json:"miss"`
	Precision                            *float64 `json:"precision,omitempty"`
	Recall                               *float64 `json:"recall,omitempty"`
	FalseAlarmRate                       *float64 `json:"false_alarm_rate,omitempty"`
	PositiveExcess                       int      `json:"positive_excess"`
	NegativeExcess                       int      `json:"negative_excess"`
	ExcessHitRate                        *float64 `json:"excess_hit_rate,omitempty"`
	ExcessHitRateLower95                 *float64 `json:"excess_hit_rate_lower_95,omitempty"`
	CostedSignalFired                    int      `json:"costed_signal_fired"`
	MissingCostSignalFired               int      `json:"missing_cost_signal_fired"`
	PositiveNetExcess                    int      `json:"positive_net_excess"`
	NegativeNetExcess                    int      `json:"negative_net_excess"`
	NetExcessHitRate                     *float64 `json:"net_excess_hit_rate,omitempty"`
	NetExcessHitRateLower95              *float64 `json:"net_excess_hit_rate_lower_95,omitempty"`
	CostedCandidates                     int      `json:"costed_candidates"`
	PositiveCandidateNetExcess           int      `json:"positive_candidate_net_excess"`
	NegativeCandidateNetExcess           int      `json:"negative_candidate_net_excess"`
	CandidateNetExcessHitRate            *float64 `json:"candidate_net_excess_hit_rate,omitempty"`
	AvgCandidateNetExcessPct             *float64 `json:"avg_candidate_net_excess_pct,omitempty"`
	MedianCandidateNetExcessPct          *float64 `json:"median_candidate_net_excess_pct,omitempty"`
	NonFiredCostedCandidates             int      `json:"non_fired_costed_candidates"`
	AvgNonFiredCandidateNetPct           *float64 `json:"avg_non_fired_candidate_net_pct,omitempty"`
	MedianNonFiredCandidateNetPct        *float64 `json:"median_non_fired_candidate_net_pct,omitempty"`
	FiredVsCandidateAvgLiftPct           *float64 `json:"fired_vs_candidate_avg_lift_pct,omitempty"`
	FiredVsCandidateMedianLiftPct        *float64 `json:"fired_vs_candidate_median_lift_pct,omitempty"`
	FiredVsNonFiredAvgLiftPct            *float64 `json:"fired_vs_non_fired_avg_lift_pct,omitempty"`
	FiredVsNonFiredMedianLiftPct         *float64 `json:"fired_vs_non_fired_median_lift_pct,omitempty"`
	AvgForwardReturnPct                  *float64 `json:"avg_forward_return_pct,omitempty"`
	AvgBenchmarkReturnPct                *float64 `json:"avg_benchmark_return_pct,omitempty"`
	AvgExcessReturnPct                   *float64 `json:"avg_excess_return_pct,omitempty"`
	AvgExcessReturnLower95Pct            *float64 `json:"avg_excess_return_lower_95_pct,omitempty"`
	MedianExcessReturnPct                *float64 `json:"median_excess_return_pct,omitempty"`
	WorstExcessReturnPct                 *float64 `json:"worst_excess_return_pct,omitempty"`
	BestExcessReturnPct                  *float64 `json:"best_excess_return_pct,omitempty"`
	AvgExecutionCostPct                  *float64 `json:"avg_execution_cost_pct,omitempty"`
	AvgNetExcessReturnPct                *float64 `json:"avg_net_excess_return_pct,omitempty"`
	AvgNetExcessReturnLower95Pct         *float64 `json:"avg_net_excess_return_lower_95_pct,omitempty"`
	MedianNetExcessReturnPct             *float64 `json:"median_net_excess_return_pct,omitempty"`
	WorstNetExcessReturnPct              *float64 `json:"worst_net_excess_return_pct,omitempty"`
	BestNetExcessReturnPct               *float64 `json:"best_net_excess_return_pct,omitempty"`
	AvgMaxAdverseExcursionPct            *float64 `json:"avg_max_adverse_excursion_pct,omitempty"`
	AvgMaxFavorableExcursionPct          *float64 `json:"avg_max_favorable_excursion_pct,omitempty"`
}

// OpportunityBacktestClusterMetrics associates opportunity metrics with one
// named market cluster.
type OpportunityBacktestClusterMetrics struct {
	Name    string                     `json:"name"`
	Metrics OpportunityBacktestMetrics `json:"metrics"`
}

type canaryBacktestAccumulator struct {
	metrics         CanaryBacktestMetrics
	signalLeadDays  int
	signalLeadCount int
	watchLeadDays   int
	watchLeadCount  int
	actLeadDays     int
	actLeadCount    int
}

type regimeBacktestAccumulator struct {
	metrics         RegimeBacktestMetrics
	watchLeadDays   int
	watchLeadCount  int
	stressLeadDays  int
	stressLeadCount int
}

type opportunityBacktestAccumulator struct {
	metrics                                  OpportunityBacktestMetrics
	forwardReturn                            float64
	benchmarkReturn                          float64
	excessReturn                             float64
	adverseExcursion                         float64
	favorableExcursion                       float64
	excessReturns                            []float64
	executionCost                            float64
	netExcessReturn                          float64
	netExcessReturns                         []float64
	holdoutNetExcessReturn                   float64
	holdoutNetExcessReturns                  []float64
	candidateNetExcessReturn                 float64
	candidateNetExcessReturns                []float64
	nonFiredCandidateNetExcessReturn         float64
	nonFiredCandidateNetExcessReturns        []float64
	holdoutCandidateNetExcessReturn          float64
	holdoutCandidateNetExcessReturns         []float64
	holdoutNonFiredCandidateNetExcessReturn  float64
	holdoutNonFiredCandidateNetExcessReturns []float64
	signalInstruments                        map[string]int
	signalClusters                           map[string]int
	holdoutSignalInstruments                 map[string]int
	holdoutSignalClusters                    map[string]int
	outcomeCount                             int
}

func runBacktest(ctx context.Context, env *Env, args []string) int {
	fs := flagSet(env, "backtest")
	inputPath := fs.String("input", "", "JSONL point-in-time observations")
	jsonOut := fs.Bool("json", false, "emit machine-readable JSON")
	opportunityMaxSlots := fs.Int("max-slots", 10, "opportunity backtest equal-weight portfolio simulation slot count")
	scoreBarsPath := fs.String("bars", "", "score-opportunity JSONL daily adjusted bars")
	scoreBarsManifestPath := fs.String("bars-manifest", "", "score/backtest opportunity bars sidecar manifest")
	captureSymbols := fs.String("symbols", "", "capture-opportunity explicit comma-separated symbols; bypass scanner")
	capturePreset := fs.String("preset", "top-movers", "capture-opportunity scanner preset")
	captureType := fs.String("type", "", "capture-opportunity ad-hoc scanCode")
	captureMarket := fs.String("market", "", "capture-opportunity stock routing shortcut for explicit symbols: us | de")
	captureExchange := fs.String("exchange", "", "capture-opportunity ad-hoc locationCode")
	captureInstrument := fs.String("instrument", "", "capture-opportunity scanner instrument")
	captureLimit := fs.Int("limit", 10, "capture-opportunity max rows")
	captureMinPrice := fs.Float64("min-price", 5, "capture-opportunity minimum last price")
	captureMinVolume := fs.Int64("min-volume", 0, "capture-opportunity minimum share volume")
	captureMinDollarVolume := fs.Float64("min-dollar-volume", 50_000_000, "capture-opportunity minimum last×volume dollar-volume")
	captureRequireLive := fs.Bool("require-live", true, "capture-opportunity require live regular-session quote context")
	captureExcludePenny := fs.Bool("exclude-penny", true, "capture-opportunity drop rows below $5")
	captureIncludeETFs := fs.Bool("include-etfs", false, "capture-opportunity keep ETF/leveraged ETP scanner rows")
	captureIncludeRegime := fs.Bool("include-regime", false, "capture-opportunity attach current regime context for macro-veto research plans")
	captureSplit := fs.String("split", "tuning", "capture-opportunity split label: tuning or explicit holdout")
	captureHoldoutPlan := fs.String("holdout-plan", "", "capture-opportunity holdout pre-registration plan id; required with --split holdout")
	captureMarketCluster := fs.String("market-cluster", "", "capture-opportunity market cluster label")
	captureTheme := fs.String("theme", "live_opportunity_capture", "capture-opportunity theme label")
	captureBenchmark := fs.String("benchmark", "QQQ", "capture-opportunity benchmark")
	captureHorizonDays := fs.Int("horizon-days", 126, "capture-opportunity forward horizon for later scoring")
	captureRoundTripCostBps := fs.Float64("round-trip-cost-bps", 50, "capture-opportunity assumed round-trip cost in bps")
	captureCostModel := fs.String("cost-model", "", "capture-opportunity cost model label")
	captureLookbackDays := fs.Int("lookback-days", 420, "capture-opportunity technical lookback")
	captureAppendPath := fs.String("append", "", "capture-opportunity append unique rows to a PIT JSONL ledger")
	scoreTargetPolicy := fs.String("target-policy", "", "score-opportunity target label policy: net-excess-positive")
	panelStartDate := fs.String("start-date", "", "build-opportunity-pit first decision date YYYY-MM-DD")
	panelEndDate := fs.String("end-date", "", "build-opportunity-pit last decision date YYYY-MM-DD")
	panelHoldoutStartDate := fs.String("holdout-start-date", "", "build-opportunity-pit first holdout decision date YYYY-MM-DD; requires --holdout-plan")
	panelSampleStepBars := fs.Int("sample-step-bars", 21, "build-opportunity-pit sample every N trading bars")
	researchPlan := fs.String("plan", "all", "research-opportunity plan id, comma-list, or all")
	researchListPlans := fs.Bool("list-plans", false, "research-opportunity list predeclared plans")
	if err := fs.Parse(args); err != nil {
		return parseExit(err)
	}
	rest := fs.Args()
	if len(rest) != 1 || (rest[0] != "canary" && rest[0] != "regime" && rest[0] != "opportunity" && rest[0] != "research-opportunity" && rest[0] != "build-regime" && rest[0] != "build-opportunity" && rest[0] != "build-opportunity-pit" && rest[0] != "score-opportunity" && rest[0] != "capture-opportunity" && rest[0] != "export-opportunity-bars") {
		return fail(env, "backtest: usage: ibkr backtest canary|regime|opportunity|build-regime|build-opportunity --input PATH [--json] | ibkr backtest build-opportunity-pit --bars BARS.jsonl --bars-manifest MANIFEST.json [--symbols SYM[,SYM...]] [--sample-step-bars 21] [--holdout-start-date YYYY-MM-DD --holdout-plan ID] | ibkr backtest opportunity --input PATH [--max-slots N] [--bars BARS.jsonl] [--bars-manifest MANIFEST.json] | ibkr backtest research-opportunity --input SCORED_PIT.jsonl [--plan all|ID[,ID...]] [--max-slots N] [--bars BARS.jsonl] [--bars-manifest MANIFEST.json] [--json] | ibkr backtest score-opportunity --input PIT.jsonl --bars BARS.jsonl [--bars-manifest MANIFEST.json] [--target-policy net-excess-positive] | ibkr backtest capture-opportunity [--preset top-movers | --symbols SYM[,SYM...]] [--include-regime] [--split tuning|holdout] [--holdout-plan ID] [--append PATH] [--json] | ibkr backtest export-opportunity-bars --symbols SYM[,SYM...] --bars BARS.jsonl --bars-manifest MANIFEST.json [--benchmark QQQ] [--lookback-days 420] [--json]")
	}
	if rest[0] == "research-opportunity" && *researchListPlans {
		if *jsonOut {
			return printJSON(env, opportunityResearchPlanSummaries())
		}
		for _, plan := range opportunityResearchPlanSummaries() {
			fmt.Fprintf(env.Stdout, "%-30s %-18s %s\n", plan.ID, plan.Family, plan.Description)
		}
		return 0
	}
	if rest[0] == "export-opportunity-bars" {
		opts := opportunityPriceBarExportOptions{
			Symbols:         splitSymbols(*captureSymbols),
			Benchmark:       strings.TrimSpace(*captureBenchmark),
			LookbackDays:    *captureLookbackDays,
			BarsPath:        strings.TrimSpace(*scoreBarsPath),
			ManifestPath:    strings.TrimSpace(*scoreBarsManifestPath),
			ExporterVersion: env.Version,
		}
		fetch := func(ctx context.Context, symbol string, days int, whatToShow string) (rpc.HistoryDailyResult, error) {
			var res rpc.HistoryDailyResult
			err := env.Conn.Call(ctx, rpc.MethodHistoryDaily, rpc.HistoryDailyParams{
				Symbol:     symbol,
				Days:       days,
				WhatToShow: whatToShow,
			}, &res)
			return res, err
		}
		res, err := exportOpportunityPriceBars(ctx, opts, fetch, time.Now())
		if err != nil {
			return fail(env, "backtest export-opportunity-bars: %v", err)
		}
		if *jsonOut {
			return printJSON(env, res)
		}
		return renderOpportunityPriceBarExportText(env, env.Stdout, res)
	}
	if rest[0] == "build-opportunity-pit" {
		if strings.TrimSpace(*scoreBarsPath) == "" {
			return fail(env, "backtest build-opportunity-pit: --bars is required")
		}
		if strings.TrimSpace(*scoreBarsManifestPath) == "" {
			return fail(env, "backtest build-opportunity-pit: --bars-manifest is required")
		}
		ledger, err := readOpportunityPriceBarLedgerFromFileWithManifest(*scoreBarsPath, *scoreBarsManifestPath)
		if err != nil {
			return fail(env, "backtest build-opportunity-pit: %v", err)
		}
		rows, err := buildOpportunityPointInTimeRowsFromBars(ledger, opportunityHistoricalPanelOptions{
			Symbols:          splitSymbols(*captureSymbols),
			Benchmark:        strings.TrimSpace(*captureBenchmark),
			StartDate:        strings.TrimSpace(*panelStartDate),
			EndDate:          strings.TrimSpace(*panelEndDate),
			HoldoutStartDate: strings.TrimSpace(*panelHoldoutStartDate),
			HoldoutPlan:      strings.TrimSpace(*captureHoldoutPlan),
			SampleStepBars:   *panelSampleStepBars,
			HorizonDays:      *captureHorizonDays,
			RoundTripCostBps: *captureRoundTripCostBps,
			CostModel:        strings.TrimSpace(*captureCostModel),
			MarketCluster:    strings.TrimSpace(*captureMarketCluster),
			Theme:            strings.TrimSpace(*captureTheme),
		})
		if err != nil {
			return fail(env, "backtest build-opportunity-pit: %v", err)
		}
		if *jsonOut {
			return printJSON(env, rows)
		}
		if err := writeOpportunityPointInTimeRowsJSONL(env.Stdout, rows); err != nil {
			return fail(env, "backtest: encode opportunity pit jsonl: %v", err)
		}
		return 0
	}
	if rest[0] == "capture-opportunity" {
		cleanSplit := cleanOpportunityBacktestSplit(*captureSplit)
		if rawSplit := strings.TrimSpace(*captureSplit); rawSplit != "" && cleanSplit == "unknown" {
			return fail(env, "backtest capture-opportunity: --split must be tuning or holdout")
		}
		if cleanSplit == "holdout" && strings.TrimSpace(*captureHoldoutPlan) == "" {
			return fail(env, "backtest capture-opportunity: --holdout-plan is required when --split holdout")
		}
		if cleanSplit != "holdout" && strings.TrimSpace(*captureHoldoutPlan) != "" {
			return fail(env, "backtest capture-opportunity: --holdout-plan requires --split holdout")
		}
		opts := opportunityCaptureOptions{
			Symbols:          splitSymbols(*captureSymbols),
			Preset:           strings.TrimSpace(*capturePreset),
			Type:             strings.TrimSpace(*captureType),
			Market:           strings.TrimSpace(*captureMarket),
			Exchange:         strings.TrimSpace(*captureExchange),
			Instrument:       strings.TrimSpace(*captureInstrument),
			Limit:            *captureLimit,
			MinPrice:         *captureMinPrice,
			MinVolume:        *captureMinVolume,
			MinDollarVolume:  *captureMinDollarVolume,
			RequireLive:      *captureRequireLive,
			ExcludePenny:     *captureExcludePenny,
			IncludeETFs:      *captureIncludeETFs,
			IncludeRegime:    *captureIncludeRegime,
			Split:            strings.TrimSpace(*captureSplit),
			HoldoutPlan:      strings.TrimSpace(*captureHoldoutPlan),
			MarketCluster:    strings.TrimSpace(*captureMarketCluster),
			Theme:            strings.TrimSpace(*captureTheme),
			Benchmark:        strings.TrimSpace(*captureBenchmark),
			HorizonDays:      *captureHorizonDays,
			RoundTripCostBps: *captureRoundTripCostBps,
			CostModel:        strings.TrimSpace(*captureCostModel),
			LookbackDays:     *captureLookbackDays,
		}
		if opts.Type != "" || opts.Exchange != "" {
			opts.Preset = ""
		}
		rows, err := captureOpportunityPointInTimeRows(ctx, env, opts)
		if err != nil {
			var preflight *opportunityCapturePreflightError
			if *jsonOut && errors.As(err, &preflight) {
				if code := printJSON(env, preflight.Result); code != 0 {
					return code
				}
				return 1
			}
			return fail(env, "backtest capture-opportunity: %v", err)
		}
		if opts.RequireLive && len(rows) > 0 {
			filtered, skipped := opportunityCaptureRowsSatisfyingLiveContext(rows)
			if len(skipped) > 0 && env != nil && env.Stderr != nil {
				fmt.Fprintf(env.Stderr, "ibkr: backtest capture-opportunity skipped %d/%d row(s) that failed --require-live: %s\n", len(skipped), len(rows), strings.Join(skipped, "; "))
			}
			if len(filtered) == 0 {
				return fail(env, "backtest capture-opportunity: no rows satisfied --require-live; %s", strings.Join(skipped, "; "))
			}
			rows = filtered
		}
		if path := strings.TrimSpace(*captureAppendPath); path != "" {
			res, appended, err := appendOpportunityPointInTimeRowsJSONL(path, rows)
			if err != nil {
				return fail(env, "backtest capture-opportunity: append %s: %v", path, err)
			}
			if env != nil && env.Stderr != nil {
				fmt.Fprintf(env.Stderr, "ibkr: backtest capture-opportunity appended %d/%d row(s) to %s (%d duplicate skipped)\n", res.Appended, res.Captured, res.Path, res.SkippedDuplicates)
			}
			rows = appended
		}
		if *jsonOut {
			return printJSON(env, rows)
		}
		if err := writeOpportunityPointInTimeRowsJSONL(env.Stdout, rows); err != nil {
			return fail(env, "backtest: encode opportunity capture jsonl: %v", err)
		}
		return 0
	}
	if strings.TrimSpace(*inputPath) == "" {
		return fail(env, "backtest: --input is required")
	}
	f, err := os.Open(*inputPath)
	if err != nil {
		return fail(env, "backtest: open %s: %v", *inputPath, err)
	}
	defer f.Close()

	if rest[0] == "score-opportunity" {
		rows, err := readOpportunityPointInTimeRows(f)
		if err != nil {
			return fail(env, "backtest: %v", err)
		}
		if strings.TrimSpace(*scoreBarsPath) == "" {
			return fail(env, "backtest score-opportunity: --bars is required")
		}
		ledger, err := readOpportunityPriceBarLedgerFromFileWithManifest(*scoreBarsPath, *scoreBarsManifestPath)
		if err != nil {
			return fail(env, "backtest score-opportunity: %v", err)
		}
		scored, err := scoreOpportunityPointInTimeRowsWithOptions(rows, ledger, opportunityScoreOptions{
			TargetPolicy: strings.TrimSpace(*scoreTargetPolicy),
		})
		if err != nil {
			return fail(env, "backtest score-opportunity: %v", err)
		}
		if *jsonOut {
			return printJSON(env, scored)
		}
		if err := writeOpportunityPointInTimeRowsJSONL(env.Stdout, scored); err != nil {
			return fail(env, "backtest: encode opportunity scored jsonl: %v", err)
		}
		return 0
	}

	if rest[0] == "build-regime" {
		rows, err := readRegimePointInTimeRows(f)
		if err != nil {
			return fail(env, "backtest: %v", err)
		}
		observations := buildRegimeBacktestObservations(rows)
		if *jsonOut {
			return printJSON(env, observations)
		}
		if err := writeRegimeBacktestObservationsJSONL(env.Stdout, observations); err != nil {
			return fail(env, "backtest: encode regime jsonl: %v", err)
		}
		return 0
	}
	if rest[0] == "build-opportunity" {
		rows, err := readOpportunityPointInTimeRows(f)
		if err != nil {
			return fail(env, "backtest: %v", err)
		}
		if err := validateOpportunityPointInTimeRowsScored(rows); err != nil {
			return fail(env, "backtest build-opportunity: %v", err)
		}
		observations := buildOpportunityBacktestObservations(rows)
		if *jsonOut {
			return printJSON(env, observations)
		}
		if err := writeOpportunityBacktestObservationsJSONL(env.Stdout, observations); err != nil {
			return fail(env, "backtest: encode opportunity jsonl: %v", err)
		}
		return 0
	}
	if rest[0] == "research-opportunity" {
		rows, err := readOpportunityPointInTimeRows(f)
		if err != nil {
			return fail(env, "backtest: %v", err)
		}
		if err := validateOpportunityPointInTimeRowsScored(rows); err != nil {
			return fail(env, "backtest research-opportunity: %v", err)
		}
		runAt := time.Now()
		if err := validateOpportunityBacktestOutcomesObservable(buildOpportunityBacktestObservations(rows), runAt); err != nil {
			return fail(env, "backtest research-opportunity: %v", err)
		}
		var ledger *opportunityPriceBarLedger
		if strings.TrimSpace(*scoreBarsPath) != "" {
			loaded, err := readOpportunityPriceBarLedgerFromFileWithManifest(*scoreBarsPath, *scoreBarsManifestPath)
			if err != nil {
				return fail(env, "backtest research-opportunity: %v", err)
			}
			ledger = &loaded
		}
		res, err := runOpportunityResearch(rows, runAt, opportunityResearchOptions{
			Plan:     strings.TrimSpace(*researchPlan),
			MaxSlots: *opportunityMaxSlots,
			Ledger:   ledger,
		})
		if err != nil {
			return fail(env, "backtest research-opportunity: %v", err)
		}
		if *jsonOut {
			return printJSON(env, res)
		}
		return renderOpportunityResearchText(env, env.Stdout, &res)
	}

	if rest[0] == "regime" {
		observations, err := readRegimeBacktestObservations(f)
		if err != nil {
			return fail(env, "backtest: %v", err)
		}
		res := runRegimeBacktest(observations, time.Now())
		if *jsonOut {
			return printJSON(env, res)
		}
		return renderRegimeBacktestText(env, env.Stdout, &res)
	}
	if rest[0] == "opportunity" {
		observations, err := readOpportunityBacktestObservations(f)
		if err != nil {
			return fail(env, "backtest: %v", err)
		}
		if err := validateOpportunityBacktestObservationsSourced(observations); err != nil {
			return fail(env, "backtest opportunity: %v", err)
		}
		runAt := time.Now()
		if err := validateOpportunityBacktestOutcomesObservable(observations, runAt); err != nil {
			return fail(env, "backtest opportunity: %v", err)
		}
		res := runOpportunityBacktestWithSlots(observations, runAt, *opportunityMaxSlots)
		if strings.TrimSpace(*scoreBarsPath) != "" {
			ledger, err := readOpportunityPriceBarLedgerFromFileWithManifest(*scoreBarsPath, *scoreBarsManifestPath)
			if err != nil {
				return fail(env, "backtest opportunity: %v", err)
			}
			if err := applyOpportunityMarkToMarketSimulation(&res, ledger); err != nil {
				return fail(env, "backtest opportunity: mark-to-market: %v", err)
			}
		}
		if *jsonOut {
			return printJSON(env, res)
		}
		return renderOpportunityBacktestText(env, env.Stdout, &res)
	}

	observations, err := readCanaryBacktestObservations(f)
	if err != nil {
		return fail(env, "backtest: %v", err)
	}
	res := runCanaryBacktest(observations, time.Now())
	if *jsonOut {
		return printJSON(env, res)
	}
	return renderCanaryBacktestText(env, env.Stdout, &res)
}

func readCanaryBacktestObservations(r io.Reader) ([]CanaryBacktestObservation, error) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	var out []CanaryBacktestObservation
	lineNo := 0
	for scanner.Scan() {
		lineNo++
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var row CanaryBacktestObservation
		if err := json.Unmarshal([]byte(line), &row); err != nil {
			return nil, fmt.Errorf("line %d: %w", lineNo, err)
		}
		if row.Date == "" && row.AsOf.IsZero() {
			return nil, fmt.Errorf("line %d: date or as_of is required", lineNo)
		}
		if row.Date != "" {
			if _, err := time.Parse("2006-01-02", row.Date); err != nil {
				return nil, fmt.Errorf("line %d: invalid date %q: %w", lineNo, row.Date, err)
			}
		}
		out = append(out, row)
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

func readRegimeBacktestObservations(r io.Reader) ([]RegimeBacktestObservation, error) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	var out []RegimeBacktestObservation
	lineNo := 0
	for scanner.Scan() {
		lineNo++
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var row RegimeBacktestObservation
		if err := json.Unmarshal([]byte(line), &row); err != nil {
			return nil, fmt.Errorf("line %d: %w", lineNo, err)
		}
		if row.Date == "" && row.AsOf.IsZero() {
			return nil, fmt.Errorf("line %d: date or as_of is required", lineNo)
		}
		if row.Date != "" {
			if _, err := time.Parse("2006-01-02", row.Date); err != nil {
				return nil, fmt.Errorf("line %d: invalid date %q: %w", lineNo, row.Date, err)
			}
		}
		out = append(out, row)
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

func readOpportunityBacktestObservations(r io.Reader) ([]OpportunityBacktestObservation, error) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	var out []OpportunityBacktestObservation
	lineNo := 0
	for scanner.Scan() {
		lineNo++
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var row OpportunityBacktestObservation
		if err := json.Unmarshal([]byte(line), &row); err != nil {
			return nil, fmt.Errorf("line %d: %w", lineNo, err)
		}
		if row.Date == "" && row.AsOf.IsZero() {
			return nil, fmt.Errorf("line %d: date or as_of is required", lineNo)
		}
		if row.Date != "" {
			if _, err := time.Parse("2006-01-02", row.Date); err != nil {
				return nil, fmt.Errorf("line %d: invalid date %q: %w", lineNo, row.Date, err)
			}
		}
		out = append(out, row)
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

func runCanaryBacktest(observations []CanaryBacktestObservation, runAt time.Time) CanaryBacktestResult {
	if runAt.IsZero() {
		runAt = time.Now()
	}
	res := CanaryBacktestResult{
		RunAt:     runAt,
		Policy:    stressengine.PolicyName(),
		NotAdvice: "Backtest diagnostic only; not investment advice or a trade recommendation.",
	}
	total := &canaryBacktestAccumulator{}
	regimeOnly := &canaryBacktestAccumulator{}
	byCluster := map[string]*canaryBacktestAccumulator{}
	byCategory := map[string]*canaryBacktestAccumulator{}
	for _, obs := range observations {
		row := runCanaryBacktestObservation(obs)
		res.Observations = append(res.Observations, row)
		total.add(row)
		regimeOnly.add(canaryRegimeOnlyBacktestRow(row))
		cluster := cleanBacktestCluster(row.MarketCluster)
		if byCluster[cluster] == nil {
			byCluster[cluster] = &canaryBacktestAccumulator{}
		}
		byCluster[cluster].add(row)
		for _, category := range canaryBacktestCategories(row) {
			if byCategory[category] == nil {
				byCategory[category] = &canaryBacktestAccumulator{}
			}
			byCategory[category].add(row)
		}
	}
	res.Metrics = total.result()
	res.RegimeOnly = regimeOnly.result()
	res.Lifecycle = canaryBacktestLifecycleMetrics(res.Observations)
	res.Events = canaryBacktestEventMetrics(res.Observations)
	res.RegimeLift = canaryBacktestRegimeLift(res.Observations)
	names := make([]string, 0, len(byCluster))
	for name := range byCluster {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		res.Clusters = append(res.Clusters, CanaryBacktestClusterMetrics{Name: name, Metrics: byCluster[name].result()})
	}
	categoryNames := make([]string, 0, len(byCategory))
	for name := range byCategory {
		categoryNames = append(categoryNames, name)
	}
	slices.Sort(categoryNames)
	for _, name := range categoryNames {
		res.Categories = append(res.Categories, CanaryBacktestClusterMetrics{Name: name, Metrics: byCategory[name].result()})
	}
	res.Findings = canaryBacktestFindings(res)
	return res
}

func runRegimeBacktest(observations []RegimeBacktestObservation, runAt time.Time) RegimeBacktestResult {
	if runAt.IsZero() {
		runAt = time.Now()
	}
	res := RegimeBacktestResult{
		RunAt:     runAt,
		Policy:    "risk-regime-dashboard",
		NotAdvice: "Backtest diagnostic only; not investment advice or a trade recommendation.",
	}
	total := &regimeBacktestAccumulator{}
	baseline := &regimeBacktestAccumulator{}
	byCluster := map[string]*regimeBacktestAccumulator{}
	for _, obs := range observations {
		row := runRegimeBacktestObservation(obs)
		res.Observations = append(res.Observations, row)
		total.add(row)
		baseline.add(regimeBaselineBacktestRow(row))
		cluster := cleanBacktestCluster(row.MarketCluster)
		if byCluster[cluster] == nil {
			byCluster[cluster] = &regimeBacktestAccumulator{}
		}
		byCluster[cluster].add(row)
	}
	res.Metrics = total.result()
	res.Baseline = baseline.result()
	res.Lifecycle = regimeBacktestLifecycleMetrics(res.Observations)
	res.Events = regimeBacktestEventMetrics(res.Observations)
	names := make([]string, 0, len(byCluster))
	for name := range byCluster {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		res.Clusters = append(res.Clusters, RegimeBacktestClusterMetrics{Name: name, Metrics: byCluster[name].result()})
	}
	res.Findings = regimeBacktestFindings(res)
	return res
}

func runOpportunityBacktest(observations []OpportunityBacktestObservation, runAt time.Time) OpportunityBacktestResult {
	return runOpportunityBacktestWithSlots(observations, runAt, 10)
}

func runOpportunityBacktestWithSlots(observations []OpportunityBacktestObservation, runAt time.Time, maxSlots int) OpportunityBacktestResult {
	if runAt.IsZero() {
		runAt = time.Now()
	}
	res := OpportunityBacktestResult{
		RunAt:     runAt,
		Policy:    "market-opportunity-outcome",
		NotAdvice: "Backtest diagnostic only; not investment advice or a trade recommendation.",
	}
	total := &opportunityBacktestAccumulator{}
	byCluster := map[string]*opportunityBacktestAccumulator{}
	for _, obs := range observations {
		row := runOpportunityBacktestObservation(obs)
		res.Observations = append(res.Observations, row)
		total.add(row)
		cluster := cleanBacktestCluster(row.MarketCluster)
		if byCluster[cluster] == nil {
			byCluster[cluster] = &opportunityBacktestAccumulator{}
		}
		byCluster[cluster].add(row)
	}
	res.Metrics = total.result()
	names := make([]string, 0, len(byCluster))
	for name := range byCluster {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		res.Clusters = append(res.Clusters, OpportunityBacktestClusterMetrics{Name: name, Metrics: byCluster[name].result()})
	}
	res.Diagnostics = opportunityBacktestDiagnostics(observations, res.Observations)
	res.Simulation = simulateOpportunityBacktestSlots(res.Observations, maxSlots)
	if holdoutRows := opportunityHoldoutBacktestRows(res.Observations); len(holdoutRows) > 0 {
		holdout := simulateOpportunityBacktestSlots(holdoutRows, maxSlots)
		holdout.Limitations = append(holdout.Limitations, "holdout-only out-of-sample rows")
		res.Simulation.Holdout = &holdout
	}
	res.Evidence = opportunityBacktestEvidenceWithSimulation(res.Metrics, &res.Simulation)
	res.Findings = opportunityBacktestFindings(res)
	return res
}

func runCanaryBacktestObservation(obs CanaryBacktestObservation) CanaryBacktestRowResult {
	input, asOf := canaryBacktestInput(obs)
	canary := ComputeStress(input)
	watch := canaryBacktestDefensiveAtLeast(canary, risk.SeverityWatch)
	act := canaryBacktestDefensiveAtLeast(canary, risk.SeverityAct)
	rebalance := canaryBacktestRebalanceAtLeast(canary, risk.SeverityWatch)
	dataQuality := canary.Direction == risk.DirectionDataQuality && severityRankAtLeast(canary.Severity, risk.SeverityWatch)
	blocked := canary.PlannerReadiness == risk.PlannerReadinessBlocked
	signalWatch := watch || rebalance
	regimeWatch := regimeBacktestStressWatch(input.Regime)
	regimeAct := regimeBacktestStressSignal(input.Regime)
	stage := canaryBacktestStage(canary)
	return CanaryBacktestRowResult{
		Date:               backtestDateLabel(obs.Date, asOf),
		Case:               obs.Case,
		MarketCluster:      cleanBacktestCluster(obs.MarketCluster),
		TargetStress:       obs.Target.Stress,
		TargetKind:         obs.Target.Kind,
		TargetScope:        cleanBacktestTargetScope(obs.Target.Scope),
		WindowDays:         obs.Target.WindowDays,
		DaysToStress:       obs.Target.DaysToStress,
		MaxSPYDrawdownPct:  obs.Target.MaxSPYDrawdownPct,
		VIXShockPct:        obs.Target.VIXShockPct,
		Direction:          canary.Direction,
		Action:             canary.Action,
		MarketConfirmation: canary.MarketConfirmation,
		PortfolioFit:       canary.PortfolioFit,
		InputHealth:        canary.InputHealth,
		Severity:           canary.Severity,
		PlannerMode:        canary.PlannerModeHint,
		PlannerReadiness:   canary.PlannerReadiness,
		PrimaryDrivers:     canary.PrimaryDrivers,
		LifecycleStage:     stage,
		SignalWatch:        signalWatch,
		DefensiveWatch:     watch,
		DefensiveAct:       act,
		RebalanceWatch:     rebalance,
		DataQualityWatch:   dataQuality,
		Blocked:            blocked,
		EarlyWarning:       stage == rpc.LifecycleEarlyWarning,
		ConfirmedStress:    stage == rpc.LifecycleConfirmedStress,
		Panic:              stage == rpc.LifecyclePanic,
		Stabilization:      stage == rpc.LifecycleStabilization,
		Opportunity:        stage == rpc.LifecycleOpportunity,
		RegimeOnlyWatch:    regimeWatch,
		RegimeOnlyAct:      regimeAct,
		Canary:             &canary,
	}
}

func canaryBacktestStage(canary StressResult) string {
	switch canary.Action {
	case stressActionConfirmInputs:
		return rpc.LifecycleDataQuality
	case stressActionDefend:
		if canary.Severity == risk.SeverityUrgent {
			return rpc.LifecyclePanic
		}
		return rpc.LifecycleConfirmedStress
	case stressActionWatch, stressActionRebalance:
		return rpc.LifecycleEarlyWarning
	case stressActionDeploy:
		return rpc.LifecycleOpportunity
	default:
		return rpc.LifecycleQuiet
	}
}

func runRegimeBacktestObservation(obs RegimeBacktestObservation) RegimeBacktestRowResult {
	regime, asOf := regimeBacktestInput(obs)
	market := summarizeStressMarket(regime, asOf)
	stressWatch := regimeBacktestStressWatch(regime)
	stressSignal := regimeBacktestStressSignal(regime)
	dataQuality := regimeBacktestDataQualityWatch(regime)
	stage := regime.Lifecycle.Stage
	return RegimeBacktestRowResult{
		Date:              backtestDateLabel(obs.Date, asOf),
		Case:              obs.Case,
		MarketCluster:     cleanBacktestCluster(obs.MarketCluster),
		TargetStress:      obs.Target.Stress,
		TargetKind:        obs.Target.Kind,
		TargetScope:       cleanBacktestTargetScope(obs.Target.Scope),
		Scored:            regimeBacktestScoredScope(obs.Target.Scope),
		WindowDays:        obs.Target.WindowDays,
		DaysToStress:      obs.Target.DaysToStress,
		MaxSPYDrawdownPct: obs.Target.MaxSPYDrawdownPct,
		VIXShockPct:       obs.Target.VIXShockPct,
		Verdict:           regime.Composite.Verdict,
		RedClusters:       regime.Composite.ClusterRedCount,
		YellowClusters:    regime.Composite.ClusterYellowCount,
		RankedClusters:    regime.Composite.ClusterRankedCount,
		UnrankedClusters:  regime.Composite.ClusterUnrankedCount,
		RedClusterNames:   market.RedClusterNames,
		LifecycleStage:    stage,
		StressWatch:       stressWatch,
		StressSignal:      stressSignal,
		DataQualityWatch:  dataQuality,
		EarlyWarning:      stage == rpc.LifecycleEarlyWarning,
		ConfirmedStress:   stage == rpc.LifecycleConfirmedStress,
		Panic:             stage == rpc.LifecyclePanic,
		Stabilization:     stage == rpc.LifecycleStabilization,
		Opportunity:       stage == rpc.LifecycleOpportunity,
		BaselineWatch:     legacyRegimeBacktestStressWatch(regime),
		BaselineStress:    legacyRegimeBacktestStressSignal(regime),
		Regime:            &regime,
	}
}

func runOpportunityBacktestObservation(obs OpportunityBacktestObservation) OpportunityBacktestRowResult {
	asOf := opportunityBacktestAsOf(obs)
	target := obs.Target.Opportunity
	fired := obs.Signal.Fired
	contextBlocked := opportunitySignalContextBlocked(obs.Signal.Reasons)
	split := cleanOpportunityBacktestSplit(obs.Split)
	holdout := split == "holdout" && validateOpportunityObservationSplitProvenance(obs) == nil
	if split == "holdout" && !holdout {
		split = "unknown"
	}
	retrospectiveHoldout := holdout && opportunitySplitProvenanceRetrospective(obs.SplitProvenance)
	var costPct *float64
	if fired {
		costPct = opportunityExecutionCostPct(obs.Trade)
	}
	var netExcess *float64
	var positiveNet *bool
	if costPct != nil {
		v := obs.Outcome.ExcessReturnPct - *costPct
		netExcess = &v
		b := v > 0
		positiveNet = &b
	}
	return OpportunityBacktestRowResult{
		Date:                 backtestDateLabel(obs.Date, asOf),
		Case:                 obs.Case,
		Split:                split,
		SplitProvenance:      obs.SplitProvenance,
		LabelStatus:          strings.TrimSpace(obs.LabelStatus),
		Holdout:              holdout,
		RetrospectiveHoldout: retrospectiveHoldout,
		MarketCluster:        cleanBacktestCluster(obs.MarketCluster),
		Theme:                strings.TrimSpace(obs.Theme),
		TargetOpportunity:    target,
		TargetKind:           obs.Target.Kind,
		TargetScope:          cleanBacktestTargetScope(obs.Target.Scope),
		SignalFired:          fired,
		SignalKind:           obs.Signal.Kind,
		SignalConfidence:     obs.Signal.Confidence,
		SignalSource:         obs.Signal.Source,
		SignalReasons:        append([]string(nil), obs.Signal.Reasons...),
		SignalContextBlocked: contextBlocked,
		TruePositive:         target && fired,
		FalsePositive:        !target && fired,
		Miss:                 target && !fired,
		PositiveExcess:       obs.Outcome.ExcessReturnPct > 0,
		ExecutionCostPct:     costPct,
		NetExcessReturnPct:   netExcess,
		PositiveNetExcess:    positiveNet,
		Trade:                obs.Trade,
		Outcome:              obs.Outcome,
		sourceObservation:    &obs,
	}
}

func opportunitySignalContextBlocked(reasons []string) bool {
	for _, reason := range reasons {
		reason = strings.TrimSpace(reason)
		switch reason {
		case "data_quality_not_ok",
			"data_quality_missing",
			"data_quality_quote_error",
			"data_quality_technical_error",
			"data_type_not_live",
			"data_type_missing",
			"quote_quality_missing",
			"quote_quality_prev_close",
			"quote_quality_stale",
			"quote_stale",
			"quote_indicative",
			"quote_error",
			"technical_error",
			"price_missing",
			"session_context_missing",
			"macro_context_missing",
			"macro_context_error",
			"macro_data_quality_veto",
			"macro_tone_missing",
			"macro_tone_unknown",
			"pct_above_50dma_missing",
			"pct_above_200dma_missing":
			return true
		}
		if strings.HasPrefix(reason, "quote_quality_") || strings.HasPrefix(reason, "session_") {
			return true
		}
	}
	return false
}

func opportunityExecutionCostPct(trade OpportunityBacktestTrade) *float64 {
	if trade.RoundTripCostBps == nil || math.IsNaN(*trade.RoundTripCostBps) || *trade.RoundTripCostBps < 0 {
		return nil
	}
	v := *trade.RoundTripCostBps / 100
	return &v
}

func simulateOpportunityBacktestSlots(rows []OpportunityBacktestRowResult, maxSlots int) OpportunityBacktestSimulation {
	maxSlots = normalizedOpportunityMaxSlots(maxSlots)
	out := OpportunityBacktestSimulation{
		Model:       "equal_weight_slots_v1",
		MaxSlots:    maxSlots,
		Limitations: []string{"uses scored trade windows, not daily mark-to-market equity", "simple cumulative slot returns; not annualized or compounded", "assumes equal slot size, no borrow limits, taxes, or market-impact model", "same-day fills use deterministic case order, not a ranking model"},
	}
	signals, filledTrades, skipped, maxConcurrent, windowStart, windowEnd := selectOpportunitySimulationTrades(rows, maxSlots)
	out.Signals = signals
	out.FilledSignals = len(filledTrades)
	out.SkippedSignals = skipped
	out.MaxConcurrent = maxConcurrent
	if out.FilledSignals == 0 {
		return out
	}
	var portfolioReturn, benchmarkReturn, turnover float64
	var holdDays int
	weight := 1 / float64(maxSlots)
	for _, trade := range filledTrades {
		portfolioReturn += weight * trade.netReturn
		benchmarkReturn += weight * trade.benchReturn
		turnover += weight
		holdDays += max(int(trade.exit.Sub(trade.entry).Hours()/24), 0)
	}
	out.PortfolioReturnPct = new(roundOpportunityPct(portfolioReturn))
	out.BenchmarkReturnPct = new(roundOpportunityPct(benchmarkReturn))
	out.ExcessReturnPct = new(roundOpportunityPct(portfolioReturn - benchmarkReturn))
	out.TurnoverPct = new(roundOpportunityPct(turnover * 100))
	out.AvgHoldDays = new(float64(holdDays) / float64(out.FilledSignals))
	out.WindowStart = windowStart.Format("2006-01-02")
	out.WindowEnd = windowEnd.Format("2006-01-02")
	out.InvestedExposureDays, out.CashDragDays = opportunitySimulationExposureDays(filledTrades, maxSlots, windowStart, windowEnd)
	if totalDays := int(windowEnd.Sub(windowStart).Hours()/24) + 1; totalDays > 0 {
		out.AvgConcurrent = new(float64(out.InvestedExposureDays) / float64(totalDays))
	}
	return out
}

func opportunityHoldoutBacktestRows(rows []OpportunityBacktestRowResult) []OpportunityBacktestRowResult {
	out := make([]OpportunityBacktestRowResult, 0, len(rows))
	for _, row := range rows {
		if row.Holdout {
			out = append(out, row)
		}
	}
	return out
}

func normalizedOpportunityMaxSlots(maxSlots int) int {
	if maxSlots <= 0 {
		return 10
	}
	return maxSlots
}

func selectOpportunitySimulationTrades(rows []OpportunityBacktestRowResult, maxSlots int) (int, []opportunitySimulationTrade, int, int, time.Time, time.Time) {
	maxSlots = normalizedOpportunityMaxSlots(maxSlots)
	valid := make([]opportunitySimulationTrade, 0, len(rows))
	signals := 0
	skipped := 0
	for _, row := range rows {
		if !row.SignalFired {
			continue
		}
		signals++
		if row.ExecutionCostPct == nil {
			skipped++
			continue
		}
		entry, err := parseOpportunityDate(row.Outcome.EntryDate)
		if err != nil {
			skipped++
			continue
		}
		exit, err := parseOpportunityDate(row.Outcome.ExitDate)
		if err != nil || exit.Before(entry) {
			skipped++
			continue
		}
		valid = append(valid, opportunitySimulationTrade{
			row:         row,
			entry:       entry,
			exit:        exit,
			netReturn:   row.Outcome.ForwardReturnPct - *row.ExecutionCostPct,
			benchReturn: row.Outcome.BenchmarkReturnPct,
		})
	}
	sort.SliceStable(valid, func(i, j int) bool {
		if valid[i].entry.Equal(valid[j].entry) {
			return valid[i].row.Case < valid[j].row.Case
		}
		return valid[i].entry.Before(valid[j].entry)
	})
	active := make([]opportunitySimulationTrade, 0, maxSlots)
	filled := make([]opportunitySimulationTrade, 0, len(valid))
	maxConcurrent := 0
	var windowStart, windowEnd time.Time
	for _, trade := range valid {
		active = dropExitedOpportunityTrades(active, trade.entry)
		if len(active) >= maxSlots {
			skipped++
			continue
		}
		if len(filled) == 0 || trade.entry.Before(windowStart) {
			windowStart = trade.entry
		}
		if trade.exit.After(windowEnd) {
			windowEnd = trade.exit
		}
		active = append(active, trade)
		filled = append(filled, trade)
		if len(active) > maxConcurrent {
			maxConcurrent = len(active)
		}
	}
	return signals, filled, skipped, maxConcurrent, windowStart, windowEnd
}

func dropExitedOpportunityTrades(active []opportunitySimulationTrade, now time.Time) []opportunitySimulationTrade {
	dst := active[:0]
	for _, trade := range active {
		if !trade.exit.Before(now) {
			dst = append(dst, trade)
		}
	}
	return dst
}

func opportunitySimulationExposureDays(trades []opportunitySimulationTrade, maxSlots int, start, end time.Time) (int, int) {
	if start.IsZero() || end.IsZero() || end.Before(start) {
		return 0, 0
	}
	occupied := 0
	cash := 0
	for day := start; !day.After(end); day = day.AddDate(0, 0, 1) {
		active := 0
		for _, trade := range trades {
			if !trade.entry.After(day) && !trade.exit.Before(day) {
				active++
			}
		}
		if active > maxSlots {
			active = maxSlots
		}
		occupied += active
		cash += maxSlots - active
	}
	return occupied, cash
}

func applyOpportunityMarkToMarketSimulation(res *OpportunityBacktestResult, ledger opportunityPriceBarLedger) error {
	if res == nil {
		return nil
	}
	if len(ledger.BySymbol) == 0 {
		return fmt.Errorf("bars ledger is empty")
	}
	if strings.TrimSpace(ledger.Checksum) == "" {
		return fmt.Errorf("bars ledger checksum is required")
	}
	if err := validateOpportunityOutcomeLedgerChecksum(res.Observations, ledger.Checksum); err != nil {
		return err
	}
	if err := validateOpportunityOutcomesMatchBars(res.Observations, ledger); err != nil {
		return err
	}
	maxSlots := normalizedOpportunityMaxSlots(res.Simulation.MaxSlots)
	_, filled, _, _, windowStart, windowEnd := selectOpportunitySimulationTrades(res.Observations, maxSlots)
	if len(filled) == 0 {
		return nil
	}
	mtm, err := simulateOpportunityMarkToMarket(filled, maxSlots, ledger, windowStart, windowEnd)
	if err != nil {
		return err
	}
	res.Simulation.MarkToMarket = &mtm
	if res.Simulation.Holdout != nil {
		_, holdoutFilled, _, _, holdoutWindowStart, holdoutWindowEnd := selectOpportunitySimulationTrades(opportunityHoldoutBacktestRows(res.Observations), maxSlots)
		if len(holdoutFilled) > 0 {
			holdoutMTM, err := simulateOpportunityMarkToMarket(holdoutFilled, maxSlots, ledger, holdoutWindowStart, holdoutWindowEnd)
			if err != nil {
				return err
			}
			res.Simulation.Holdout.MarkToMarket = &holdoutMTM
		}
	}
	res.Evidence = opportunityBacktestEvidenceWithSimulation(res.Metrics, &res.Simulation)
	res.Findings = opportunityBacktestFindings(*res)
	return nil
}

func validateOpportunityOutcomesMatchBars(rows []OpportunityBacktestRowResult, ledger opportunityPriceBarLedger) error {
	for i, row := range rows {
		if err := validateOpportunityOutcomeMatchesBars(row, ledger); err != nil {
			return fmt.Errorf("line %d: %w", i+1, err)
		}
	}
	return nil
}

func validateOpportunityOutcomeMatchesBars(row OpportunityBacktestRowResult, ledger opportunityPriceBarLedger) error {
	instrument := strings.ToUpper(strings.TrimSpace(row.Trade.Instrument))
	if instrument == "" {
		return fmt.Errorf("trade.instrument is required before mark-to-market reconciliation")
	}
	benchmark := strings.ToUpper(strings.TrimSpace(row.Trade.Benchmark))
	if benchmark == "" {
		benchmark = "QQQ"
	}
	if strings.TrimSpace(row.Outcome.Formula) != opportunityOutcomeFormulaCloseToClose {
		return fmt.Errorf("outcome.formula %q does not match %s; rerun score-opportunity", row.Outcome.Formula, opportunityOutcomeFormulaCloseToClose)
	}
	if strings.TrimSpace(row.Outcome.PriceBasis) != "adjusted_close" {
		return fmt.Errorf("outcome.price_basis %q does not match adjusted_close; rerun score-opportunity", row.Outcome.PriceBasis)
	}
	entryDate := strings.TrimSpace(row.Outcome.EntryDate)
	exitDate := strings.TrimSpace(row.Outcome.ExitDate)
	instrumentBars := ledger.BySymbol[instrument]
	if len(instrumentBars) == 0 {
		return fmt.Errorf("no price bars for %s", instrument)
	}
	benchmarkBars := ledger.BySymbol[benchmark]
	if len(benchmarkBars) == 0 {
		return fmt.Errorf("no benchmark bars for %s", benchmark)
	}
	if err := validateOpportunityOutcomeWindowMatchesTrade(row, instrumentBars); err != nil {
		return err
	}
	entry, ok := opportunityBarOnDate(instrumentBars, entryDate)
	if !ok {
		return fmt.Errorf("no entry bar for %s on %s", instrument, entryDate)
	}
	exit, ok := opportunityBarOnDate(instrumentBars, exitDate)
	if !ok {
		return fmt.Errorf("no exit bar for %s on %s", instrument, exitDate)
	}
	benchmarkEntry, ok := opportunityBarOnDate(benchmarkBars, entryDate)
	if !ok {
		return fmt.Errorf("no benchmark entry bar for %s on %s", benchmark, entryDate)
	}
	benchmarkExit, ok := opportunityBarOnDate(benchmarkBars, exitDate)
	if !ok {
		return fmt.Errorf("no benchmark exit bar for %s on %s", benchmark, exitDate)
	}
	entryClose := opportunityBarClose(entry)
	exitClose := opportunityBarClose(exit)
	benchmarkEntryClose := opportunityBarClose(benchmarkEntry)
	benchmarkExitClose := opportunityBarClose(benchmarkExit)
	rawForward := opportunityPctReturn(entryClose, exitClose)
	rawBenchmarkReturn := opportunityPctReturn(benchmarkEntryClose, benchmarkExitClose)
	rawExcess := rawForward - rawBenchmarkReturn
	forward := roundOpportunityPct(rawForward)
	benchmarkReturn := roundOpportunityPct(rawBenchmarkReturn)
	excess := roundOpportunityPct(rawExcess)
	adverse, favorable := opportunityExcursions(instrumentBars, entryDate, exitDate, entryClose)
	checks := []struct {
		field string
		got   float64
		want  float64
	}{
		{field: "outcome.forward_return_pct", got: row.Outcome.ForwardReturnPct, want: forward},
		{field: "outcome.benchmark_return_pct", got: row.Outcome.BenchmarkReturnPct, want: benchmarkReturn},
		{field: "outcome.excess_return_pct", got: row.Outcome.ExcessReturnPct, want: excess},
		{field: "outcome.max_adverse_excursion_pct", got: row.Outcome.MaxAdverseExcursionPct, want: roundOpportunityPct(adverse)},
		{field: "outcome.max_favorable_excursion_pct", got: row.Outcome.MaxFavorableExcursionPct, want: roundOpportunityPct(favorable)},
	}
	for _, check := range checks {
		if !opportunityFloatEqual(check.got, check.want) {
			return fmt.Errorf("%s %.4g does not match --bars ledger %.4g; rerun score-opportunity with the same bars ledger", check.field, check.got, check.want)
		}
	}
	if !opportunityFloatPtrEqual(row.Outcome.EntryPrice, entryClose) {
		return fmt.Errorf("outcome.entry_price %s does not match --bars ledger %.4g; rerun score-opportunity with the same bars ledger", opportunityFloatPtrLabel(row.Outcome.EntryPrice), entryClose)
	}
	if !opportunityFloatPtrEqual(row.Outcome.ExitPrice, exitClose) {
		return fmt.Errorf("outcome.exit_price %s does not match --bars ledger %.4g; rerun score-opportunity with the same bars ledger", opportunityFloatPtrLabel(row.Outcome.ExitPrice), exitClose)
	}
	return nil
}

func validateOpportunityOutcomeWindowMatchesTrade(row OpportunityBacktestRowResult, instrumentBars []OpportunityPriceBarRow) error {
	if row.sourceObservation == nil {
		return nil
	}
	obs := *row.sourceObservation
	expectedEntry, expectedExit, err := opportunityScoreWindow(OpportunityPointInTimeRow{
		Date:     obs.Date,
		AsOf:     obs.AsOf,
		Features: obs.Features,
		Trade:    obs.Trade,
		Outcome:  obs.Outcome,
	}, instrumentBars)
	if err != nil {
		return fmt.Errorf("outcome window does not match trade rule: %w", err)
	}
	if got := strings.TrimSpace(row.Outcome.EntryDate); got != expectedEntry.Date {
		return fmt.Errorf("outcome.entry_date %s does not match trade rule %s; rerun score-opportunity", got, expectedEntry.Date)
	}
	if got := strings.TrimSpace(row.Outcome.ExitDate); got != expectedExit.Date {
		return fmt.Errorf("outcome.exit_date %s does not match trade rule %s; rerun score-opportunity", got, expectedExit.Date)
	}
	return nil
}

func opportunityFloatEqual(got, want float64) bool {
	if math.IsNaN(got) || math.IsNaN(want) {
		return math.IsNaN(got) && math.IsNaN(want)
	}
	return math.Abs(got-want) <= 0.0001
}

func opportunityFloatPtrEqual(got *float64, want float64) bool {
	if got == nil {
		return false
	}
	return opportunityFloatEqual(*got, want)
}

func opportunityFloatPtrLabel(v *float64) string {
	if v == nil {
		return "<nil>"
	}
	return fmt.Sprintf("%.4g", *v)
}

func validateOpportunityOutcomeLedgerChecksum(rows []OpportunityBacktestRowResult, checksum string) error {
	checksum = strings.TrimSpace(checksum)
	if checksum == "" {
		return fmt.Errorf("bars ledger checksum is required")
	}
	for i, row := range rows {
		source := strings.TrimSpace(row.Outcome.SourceChecksum)
		benchmark := strings.TrimSpace(row.Outcome.BenchmarkSourceChecksum)
		if source != checksum || benchmark != checksum {
			return fmt.Errorf("line %d: outcome checksums %q/%q do not match --bars ledger %q; rerun score-opportunity with the same bars ledger before mark-to-market", i+1, source, benchmark, checksum)
		}
	}
	return nil
}

type opportunityMarkContext struct {
	trade          opportunitySimulationTrade
	instrumentBars []OpportunityPriceBarRow
	benchmarkBars  []OpportunityPriceBarRow
	entryClose     float64
	exitClose      float64
	benchmarkEntry float64
	benchmarkExit  float64
	costPct        float64
	instrument     string
	benchmark      string
}

func simulateOpportunityMarkToMarket(trades []opportunitySimulationTrade, maxSlots int, ledger opportunityPriceBarLedger, start, end time.Time) (OpportunityMarkToMarketSimulation, error) {
	sourceQuality := strings.TrimSpace(ledger.SourceQuality)
	sourceWarnings := slices.Clone(ledger.SourceWarnings)
	barSources := slices.Clone(ledger.BarSources)
	if sourceQuality == "" {
		sourceQuality, sourceWarnings, barSources = opportunityPriceBarLedgerSourceQuality(ledger.BySymbol)
	}
	out := OpportunityMarkToMarketSimulation{
		Model:                  "equal_weight_slots_mtm_v1",
		PriceSource:            strings.TrimSpace(ledger.Source),
		SourceChecksum:         ledger.Checksum,
		SourceManifest:         strings.TrimSpace(ledger.ManifestPath),
		SourceManifestChecksum: ledger.ManifestChecksum,
		SourceProvider:         strings.TrimSpace(ledger.SourceProvider),
		SourceMethod:           strings.TrimSpace(ledger.SourceMethod),
		SourceCreatedAt:        strings.TrimSpace(ledger.SourceCreatedAt),
		SourceQuality:          sourceQuality,
		SourceWarnings:         sourceWarnings,
		BarSources:             barSources,
		PriceBasis:             "adjusted_close",
		Limitations: []string{
			"close-to-close bar marks only; intraday drawdown is not measured",
			"round-trip cost is charged from the first marked point",
			"uses filled trades selected by equal_weight_slots_v1",
			"return volatility is per mark interval, not annualized",
		},
	}
	if len(trades) == 0 || start.IsZero() || end.IsZero() || end.Before(start) {
		return out, nil
	}
	contexts := make([]opportunityMarkContext, 0, len(trades))
	markDates := map[string]struct{}{}
	startDate := start.Format("2006-01-02")
	endDate := end.Format("2006-01-02")
	markDates[startDate] = struct{}{}
	markDates[endDate] = struct{}{}
	for _, trade := range trades {
		ctx, err := opportunityMarkContextForTrade(trade, ledger)
		if err != nil {
			return out, err
		}
		contexts = append(contexts, ctx)
		markDates[trade.entry.Format("2006-01-02")] = struct{}{}
		markDates[trade.exit.Format("2006-01-02")] = struct{}{}
		for _, bar := range ctx.instrumentBars {
			if bar.Date >= startDate && bar.Date <= endDate {
				markDates[bar.Date] = struct{}{}
			}
		}
		for _, bar := range ctx.benchmarkBars {
			if bar.Date >= startDate && bar.Date <= endDate {
				markDates[bar.Date] = struct{}{}
			}
		}
	}
	out.Trades = len(contexts)
	for _, ctx := range contexts {
		tradeStart := ctx.trade.entry.Format("2006-01-02")
		tradeEnd := ctx.trade.exit.Format("2006-01-02")
		instrumentMarks := opportunityBarsInWindow(ctx.instrumentBars, tradeStart, tradeEnd)
		benchmarkMarks := opportunityBarsInWindow(ctx.benchmarkBars, tradeStart, tradeEnd)
		if len(instrumentMarks) < out.MinTradeMarks || out.MinTradeMarks == 0 {
			out.MinTradeMarks = len(instrumentMarks)
		}
		if len(benchmarkMarks) < out.MinTradeMarks || out.MinTradeMarks == 0 {
			out.MinTradeMarks = len(benchmarkMarks)
		}
		out.MaxTradeMarkGapDays = max(out.MaxTradeMarkGapDays, opportunityMaxBarGapDays(instrumentMarks))
		out.MaxTradeMarkGapDays = max(out.MaxTradeMarkGapDays, opportunityMaxBarGapDays(benchmarkMarks))
	}
	dates := make([]string, 0, len(markDates))
	for date := range markDates {
		dates = append(dates, date)
	}
	slices.Sort(dates)
	weight := 1 / float64(maxSlots)
	portfolioEquity := make([]float64, 0, len(dates))
	benchmarkEquity := make([]float64, 0, len(dates))
	for _, date := range dates {
		var portfolioReturn, benchmarkReturn float64
		for _, ctx := range contexts {
			tradeStart := ctx.trade.entry.Format("2006-01-02")
			tradeEnd := ctx.trade.exit.Format("2006-01-02")
			if date < tradeStart {
				continue
			}
			instrumentReturn, err := opportunityMarkedTradeReturn(ctx.instrumentBars, date, tradeEnd, ctx.entryClose, ctx.exitClose, ctx.costPct)
			if err != nil {
				return out, fmt.Errorf("%s %s: %w", ctx.instrument, date, err)
			}
			benchmarkReturnPct, err := opportunityMarkedTradeReturn(ctx.benchmarkBars, date, tradeEnd, ctx.benchmarkEntry, ctx.benchmarkExit, 0)
			if err != nil {
				return out, fmt.Errorf("%s benchmark %s %s: %w", ctx.instrument, ctx.benchmark, date, err)
			}
			portfolioReturn += weight * instrumentReturn
			benchmarkReturn += weight * benchmarkReturnPct
		}
		portfolioEquity = append(portfolioEquity, 1+portfolioReturn/100)
		benchmarkEquity = append(benchmarkEquity, 1+benchmarkReturn/100)
	}
	out.Bars = len(dates)
	if len(portfolioEquity) == 0 {
		return out, nil
	}
	portfolioReturn := (portfolioEquity[len(portfolioEquity)-1] - 1) * 100
	benchmarkReturn := (benchmarkEquity[len(benchmarkEquity)-1] - 1) * 100
	out.PortfolioReturnPct = new(roundOpportunityPct(portfolioReturn))
	out.BenchmarkReturnPct = new(roundOpportunityPct(benchmarkReturn))
	out.ExcessReturnPct = new(roundOpportunityPct(portfolioReturn - benchmarkReturn))
	out.MaxDrawdownPct = new(roundOpportunityPct(opportunityMaxDrawdownPct(portfolioEquity)))
	out.BenchmarkMaxDrawdownPct = new(roundOpportunityPct(opportunityMaxDrawdownPct(benchmarkEquity)))
	out.EndPortfolioEquityMultiple = new(roundOpportunityMultiple(portfolioEquity[len(portfolioEquity)-1]))
	out.EndBenchmarkEquityMultiple = new(roundOpportunityMultiple(benchmarkEquity[len(benchmarkEquity)-1]))
	pointReturns := opportunityPointReturnsPct(portfolioEquity)
	benchmarkPointReturns := opportunityPointReturnsPct(benchmarkEquity)
	out.WorstBarReturnPct = minFloatPtr(pointReturns)
	out.BestBarReturnPct = maxFloatPtr(pointReturns)
	out.BarReturnVolPct = opportunityVolatilityPct(pointReturns)
	out.BenchmarkBarReturnVolPct = opportunityVolatilityPct(benchmarkPointReturns)
	return out, nil
}

func opportunityMarkContextForTrade(trade opportunitySimulationTrade, ledger opportunityPriceBarLedger) (opportunityMarkContext, error) {
	instrument := strings.ToUpper(strings.TrimSpace(trade.row.Trade.Instrument))
	if instrument == "" {
		return opportunityMarkContext{}, fmt.Errorf("trade instrument is required for %q", trade.row.Case)
	}
	benchmark := strings.ToUpper(strings.TrimSpace(trade.row.Trade.Benchmark))
	if benchmark == "" {
		benchmark = "QQQ"
	}
	instrumentBars := ledger.BySymbol[instrument]
	if len(instrumentBars) == 0 {
		return opportunityMarkContext{}, fmt.Errorf("no price bars for %s", instrument)
	}
	benchmarkBars := ledger.BySymbol[benchmark]
	if len(benchmarkBars) == 0 {
		return opportunityMarkContext{}, fmt.Errorf("no benchmark bars for %s", benchmark)
	}
	entryDate := trade.entry.Format("2006-01-02")
	exitDate := trade.exit.Format("2006-01-02")
	entryBar, ok := opportunityBarOnDate(instrumentBars, entryDate)
	if !ok {
		return opportunityMarkContext{}, fmt.Errorf("no entry bar for %s on %s", instrument, entryDate)
	}
	exitBar, ok := opportunityBarOnDate(instrumentBars, exitDate)
	if !ok {
		return opportunityMarkContext{}, fmt.Errorf("no exit bar for %s on %s", instrument, exitDate)
	}
	benchmarkEntry, ok := opportunityBarOnDate(benchmarkBars, entryDate)
	if !ok {
		return opportunityMarkContext{}, fmt.Errorf("no benchmark entry bar for %s on %s", benchmark, entryDate)
	}
	benchmarkExit, ok := opportunityBarOnDate(benchmarkBars, exitDate)
	if !ok {
		return opportunityMarkContext{}, fmt.Errorf("no benchmark exit bar for %s on %s", benchmark, exitDate)
	}
	costPct := 0.0
	if trade.row.ExecutionCostPct != nil {
		costPct = *trade.row.ExecutionCostPct
	}
	return opportunityMarkContext{
		trade:          trade,
		instrumentBars: instrumentBars,
		benchmarkBars:  benchmarkBars,
		entryClose:     opportunityBarClose(entryBar),
		exitClose:      opportunityBarClose(exitBar),
		benchmarkEntry: opportunityBarClose(benchmarkEntry),
		benchmarkExit:  opportunityBarClose(benchmarkExit),
		costPct:        costPct,
		instrument:     instrument,
		benchmark:      benchmark,
	}, nil
}

func opportunityMarkedTradeReturn(bars []OpportunityPriceBarRow, markDate, exitDate string, entryClose, exitClose, costPct float64) (float64, error) {
	if markDate >= exitDate {
		return opportunityPctReturn(entryClose, exitClose) - costPct, nil
	}
	bar, ok := opportunityBarOnOrBefore(bars, markDate)
	if !ok {
		return 0, fmt.Errorf("no bar on or before %s", markDate)
	}
	return opportunityPctReturn(entryClose, opportunityBarClose(bar)) - costPct, nil
}

func opportunityBarOnDate(bars []OpportunityPriceBarRow, date string) (OpportunityPriceBarRow, bool) {
	for _, bar := range bars {
		if bar.Date == date {
			return bar, true
		}
		if bar.Date > date {
			break
		}
	}
	return OpportunityPriceBarRow{}, false
}

func opportunityBarsInWindow(bars []OpportunityPriceBarRow, startDate, endDate string) []OpportunityPriceBarRow {
	out := make([]OpportunityPriceBarRow, 0, len(bars))
	for _, bar := range bars {
		if bar.Date < startDate {
			continue
		}
		if bar.Date > endDate {
			break
		}
		out = append(out, bar)
	}
	return out
}

func opportunityMaxBarGapDays(bars []OpportunityPriceBarRow) int {
	maxGap := 0
	var prev time.Time
	for _, bar := range bars {
		date, err := parseOpportunityDate(bar.Date)
		if err != nil {
			continue
		}
		if !prev.IsZero() {
			gap := int(date.Sub(prev).Hours() / 24)
			if gap > maxGap {
				maxGap = gap
			}
		}
		prev = date
	}
	return maxGap
}

func opportunityBarOnOrBefore(bars []OpportunityPriceBarRow, date string) (OpportunityPriceBarRow, bool) {
	var out OpportunityPriceBarRow
	ok := false
	for _, bar := range bars {
		if bar.Date > date {
			break
		}
		out = bar
		ok = true
	}
	return out, ok
}

func opportunityMaxDrawdownPct(equity []float64) float64 {
	peak := 1.0
	maxDrawdown := 0.0
	for _, v := range equity {
		if v > peak {
			peak = v
		}
		if peak <= 0 {
			continue
		}
		drawdown := (v - peak) / peak * 100
		if drawdown < maxDrawdown {
			maxDrawdown = drawdown
		}
	}
	return maxDrawdown
}

func opportunityPointReturnsPct(equity []float64) []float64 {
	out := make([]float64, 0, len(equity))
	prev := 1.0
	for _, v := range equity {
		if prev > 0 {
			out = append(out, opportunityPctReturn(prev, v))
		}
		prev = v
	}
	return out
}

func opportunityVolatilityPct(values []float64) *float64 {
	if len(values) < 2 {
		return nil
	}
	mean := meanFloat(values)
	var sum float64
	for _, v := range values {
		d := v - mean
		sum += d * d
	}
	return new(roundOpportunityPct(math.Sqrt(sum / float64(len(values)-1))))
}

func roundOpportunityMultiple(v float64) float64 {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return v
	}
	return math.Round(v*10_000) / 10_000
}

func canaryBacktestInput(obs CanaryBacktestObservation) (StressInput, time.Time) {
	asOf := canaryBacktestAsOf(obs)
	acct := obs.Account
	if acct.AsOf.IsZero() {
		acct.AsOf = asOf
	}
	pos := obs.Positions
	if pos.AsOf.IsZero() {
		pos.AsOf = asOf
	}
	regime := obs.Regime
	if regime.AsOf.IsZero() {
		regime.AsOf = asOf
	}
	backfillBacktestRegimeComposite(&regime)
	return StressInput{Account: acct, Positions: pos, Regime: regime, Now: asOf}, asOf
}

func regimeBacktestInput(obs RegimeBacktestObservation) (rpc.RegimeSnapshotResult, time.Time) {
	asOf := regimeBacktestAsOf(obs)
	regime := obs.Regime
	if regime.AsOf.IsZero() {
		regime.AsOf = asOf
	}
	// Replay stamps the tape session from the observation clock exactly like
	// the daemon does at snapshot time (session state is derived policy, not
	// raw corpus input). Date-only observations clock 15:59 ET inside their
	// observation date (backtestDateAsOf), so trading-day rows classify
	// trading_date; dates outside embedded calendar coverage stay empty and
	// the lifecycle tape terms fail open — historical corpora replay
	// unchanged.
	regime.TapeSessionState, regime.TapeSessionReason, regime.TapeNextOpen = rpc.TapeSessionFor(asOf)
	backfillBacktestRegimeComposite(&regime)
	if regime.Fingerprint.Key == "" {
		regime.Fingerprint = rpc.BuildRegimeFingerprint(&regime)
	}
	return regime, asOf
}

func canaryBacktestAsOf(obs CanaryBacktestObservation) time.Time {
	if !obs.AsOf.IsZero() {
		return obs.AsOf
	}
	return backtestDateAsOf(obs.Date)
}

func regimeBacktestAsOf(obs RegimeBacktestObservation) time.Time {
	if !obs.AsOf.IsZero() {
		return obs.AsOf
	}
	return backtestDateAsOf(obs.Date)
}

// backtestDateAsOf stamps a date-only observation inside that date's regular
// NY session (15:59 ET). Midnight UTC would land on the prior NY evening —
// often a weekend — and session-aware severity would then misread an
// end-of-date snapshot as a closed-date one.
func backtestDateAsOf(date string) time.Time {
	if date == "" {
		return time.Time{}
	}
	t, err := time.Parse("2006-01-02", date)
	if err != nil {
		return time.Time{}
	}
	if loc, lerr := time.LoadLocation("America/New_York"); lerr == nil {
		return time.Date(t.Year(), t.Month(), t.Day(), 15, 59, 0, 0, loc)
	}
	return t
}

func opportunityBacktestAsOf(obs OpportunityBacktestObservation) time.Time {
	if !obs.AsOf.IsZero() {
		return obs.AsOf
	}
	if obs.Date != "" {
		if t, err := time.Parse("2006-01-02", obs.Date); err == nil {
			return t
		}
	}
	return time.Time{}
}

func backtestDateLabel(date string, asOf time.Time) string {
	if date != "" {
		return date
	}
	if !asOf.IsZero() {
		return asOf.Format("2006-01-02")
	}
	return ""
}

func cleanBacktestCluster(cluster string) string {
	cluster = strings.TrimSpace(cluster)
	if cluster == "" {
		return "unclassified"
	}
	return cluster
}

func cleanOpportunityBacktestInstrument(instrument string) string {
	instrument = strings.ToUpper(strings.TrimSpace(instrument))
	if instrument == "" {
		return "unknown"
	}
	return instrument
}

func cleanOpportunityBacktestSplit(split string) string {
	split = strings.ToLower(strings.TrimSpace(split))
	switch split {
	case "":
		return "unknown"
	case "holdout", "out_of_sample", "out-of-sample", "oos", "validation", "walk_forward", "walk-forward":
		return "holdout"
	case "tuning", "training", "train", "in_sample", "in-sample", "research":
		return "tuning"
	default:
		return "unknown"
	}
}

func opportunityBacktestSplitUnknown(split string) bool {
	return cleanOpportunityBacktestSplit(split) == "unknown"
}

func cleanBacktestTargetScope(scope string) string {
	scope = strings.ToLower(strings.TrimSpace(scope))
	if scope == "" {
		return "market"
	}
	return scope
}

func canaryBacktestPortfolioScope(scope string) bool {
	switch cleanBacktestTargetScope(scope) {
	case "portfolio", "portfolio_only", "account", "account_only", "idiosyncratic":
		return true
	default:
		return false
	}
}

func regimeBacktestScoredScope(scope string) bool {
	switch cleanBacktestTargetScope(scope) {
	case "market", "broad_market", "cross_asset":
		return true
	default:
		return false
	}
}

func regimeBacktestStressWatch(r rpc.RegimeSnapshotResult) bool {
	stage := r.Lifecycle.Stage
	if stage != "" {
		return stage == rpc.LifecycleEarlyWarning ||
			stage == rpc.LifecycleConfirmedStress ||
			stage == rpc.LifecyclePanic
	}
	return legacyRegimeBacktestStressWatch(r)
}

func regimeBacktestStressSignal(r rpc.RegimeSnapshotResult) bool {
	stage := r.Lifecycle.Stage
	if stage != "" {
		return stage == rpc.LifecycleConfirmedStress || stage == rpc.LifecyclePanic
	}
	return legacyRegimeBacktestStressSignal(r)
}

func legacyRegimeBacktestStressWatch(r rpc.RegimeSnapshotResult) bool {
	c := r.Composite
	if c.ClusterRankedCount < verdictFloor {
		return false
	}
	return c.ClusterRedCount >= 1 || c.ClusterYellowCount >= 3
}

func legacyRegimeBacktestStressSignal(r rpc.RegimeSnapshotResult) bool {
	c := r.Composite
	return c.ClusterRankedCount >= verdictFloor && c.ClusterRedCount >= 1
}

func regimeBacktestDataQualityWatch(r rpc.RegimeSnapshotResult) bool {
	c := r.Composite
	if c.ClusterRankedCount < verdictFloor || c.ClusterUnrankedCount > 0 {
		return true
	}
	statuses := []string{
		r.VIXTermStructure.Status,
		r.VolOfVol.Status,
		r.HYGSPYDivergence.Status,
		r.CreditSpreads.Status,
		r.FundingStress.Status,
		r.USDJPY.Status,
		r.GammaZero.Status,
		r.Breadth.Status,
	}
	for _, status := range statuses {
		status = strings.ToLower(strings.TrimSpace(status))
		if status != "" && status != rpc.RegimeStatusOK {
			return true
		}
	}
	return stressGammaDegraded(r.GammaZero) || len(r.WarningDetails) > 0
}

func backfillBacktestRegimeComposite(r *rpc.RegimeSnapshotResult) {
	if r == nil {
		return
	}
	backfillBacktestRegimeSourceContract(r)
	backfillBacktestRegimeEligibility(r)
	r.Composite = rpc.RegimeComposite{}
	indicatorBands := []string{
		r.VIXTermStructure.Band,
		r.VolOfVol.Band,
		r.HYGSPYDivergence.Band,
		r.CreditSpreads.Band,
		r.FundingStress.Band,
		r.USDJPY.Band,
		r.GammaZero.Band,
		r.Breadth.Band,
	}
	for _, band := range indicatorBands {
		switch strings.ToLower(strings.TrimSpace(band)) {
		case "green":
			r.Composite.GreenCount++
			r.Composite.RankedCount++
		case "yellow":
			r.Composite.YellowCount++
			r.Composite.RankedCount++
		case "red":
			r.Composite.RedCount++
			r.Composite.RankedCount++
		default:
			r.Composite.UnrankedCount++
		}
	}
	// Cluster combination, lifecycle, and headline all come from the shared
	// rpc policy — the backtest builder was one of the four drifting copies.
	rpc.ApplyRegimeClusterTallies(&r.Composite, rpc.BuildRegimeClusterBands(r))
	r.SourceHealth = rpc.BuildRegimeSourceHealth(r, r.AsOf)
	r.Lifecycle = rpc.BuildRegimeLifecycle(r)
	r.Composite.Verdict = rpc.RegimeHeadline(r.Composite, r.Lifecycle.Stage)
}

// backfillBacktestRegimeSourceContract upgrades legacy point-in-time corpus
// rows that predate the typed freshness/source-health fields. A row declared
// status=ok is a complete observation at the corpus clock; every other or
// missing status is overdue and therefore fails closed. This compatibility
// projection is confined to offline replay — live snapshots are authored by
// the daemon and never pass through it.
func backfillBacktestRegimeSourceContract(r *rpc.RegimeSnapshotResult) {
	set := func(meta *rpc.RegimeIndicatorMeta, indicator, status string) {
		if meta.Freshness != nil {
			return
		}
		class := rpc.RegimeFreshnessOverdue
		if strings.EqualFold(strings.TrimSpace(status), rpc.RegimeStatusOK) {
			class = rpc.RegimeFreshnessFresh
		}
		meta.Freshness = &rpc.RegimeFreshness{
			Class:         class,
			MaxAgeSeconds: rpc.RegimeSourceMaxAgeSeconds(rpc.RegimeIndicatorCluster(indicator)),
		}
	}
	set(&r.VIXTermStructure.RegimeIndicatorMeta, rpc.RegimeIndicatorVIXTerm, r.VIXTermStructure.Status)
	set(&r.VolOfVol.RegimeIndicatorMeta, rpc.RegimeIndicatorVolOfVol, r.VolOfVol.Status)
	set(&r.HYGSPYDivergence.RegimeIndicatorMeta, rpc.RegimeIndicatorHYGSPY, r.HYGSPYDivergence.Status)
	set(&r.CreditSpreads.RegimeIndicatorMeta, rpc.RegimeIndicatorCredit, r.CreditSpreads.Status)
	set(&r.FundingStress.RegimeIndicatorMeta, rpc.RegimeIndicatorFunding, r.FundingStress.Status)
	set(&r.USDJPY.RegimeIndicatorMeta, rpc.RegimeIndicatorUSDJPY, r.USDJPY.Status)
	set(&r.GammaZero.RegimeIndicatorMeta, rpc.RegimeIndicatorGammaZero, r.GammaZero.Status)
	set(&r.Breadth.RegimeIndicatorMeta, rpc.RegimeIndicatorBreadth, r.Breadth.Status)
}

// backfillBacktestRegimeEligibility applies the confirmation gates to PIT
// rows. Point-in-time panels are independent daily observations, so the
// replay evaluates day-1 gates: depth and freshness bind, persistence is
// sessions=1 — streak-gated indicators confirm only through their fast-path
// depths (the crash-day escape hatch). Sequence-aware streak replay over
// chronological panels is follow-up work on the decisions journal
// (docs/docs/internals/regime-backtest.md).
func backfillBacktestRegimeEligibility(r *rpc.RegimeSnapshotResult) {
	set := func(meta *rpc.RegimeIndicatorMeta, indicator, status string, depth *float64) {
		if meta.Band != "red" {
			meta.Eligibility = nil
			return
		}
		fresh := strings.EqualFold(strings.TrimSpace(status), rpc.RegimeStatusOK) &&
			meta.Freshness != nil && meta.Freshness.Class == rpc.RegimeFreshnessFresh
		meta.Eligibility = rpc.EvaluateRegimeEligibility(rpc.RegimeEligibilityInput{
			Indicator:      indicator,
			Band:           "red",
			Depth:          depth,
			StreakSessions: 1,
			Fresh:          fresh,
		})
	}
	set(&r.VIXTermStructure.RegimeIndicatorMeta, rpc.RegimeIndicatorVIXTerm, r.VIXTermStructure.Status, r.VIXTermStructure.Ratio)
	set(&r.VolOfVol.RegimeIndicatorMeta, rpc.RegimeIndicatorVolOfVol, r.VolOfVol.Status, r.VolOfVol.Last)
	var hygDepth *float64
	if h := r.HYGSPYDivergence; h.HYGPrice != nil && h.HYG50DMA != nil && *h.HYG50DMA > 0 {
		d := (*h.HYG50DMA - *h.HYGPrice) / *h.HYG50DMA * 100
		hygDepth = &d
	}
	set(&r.HYGSPYDivergence.RegimeIndicatorMeta, rpc.RegimeIndicatorHYGSPY, r.HYGSPYDivergence.Status, hygDepth)
	set(&r.CreditSpreads.RegimeIndicatorMeta, rpc.RegimeIndicatorCredit, r.CreditSpreads.Status, nil)
	set(&r.FundingStress.RegimeIndicatorMeta, rpc.RegimeIndicatorFunding, r.FundingStress.Status, nil)
	set(&r.USDJPY.RegimeIndicatorMeta, rpc.RegimeIndicatorUSDJPY, r.USDJPY.Status, nil)
	set(&r.GammaZero.RegimeIndicatorMeta, rpc.RegimeIndicatorGammaZero, r.GammaZero.Status, rpc.RegimeGammaDepth(r.GammaZero.Envelope.Result))
	var breadthDepth *float64
	if r.Breadth.Envelope.State == rpc.BreadthStateReady {
		d := 40 - r.Breadth.Envelope.PctAbove50DMA
		breadthDepth = &d
	}
	set(&r.Breadth.RegimeIndicatorMeta, rpc.RegimeIndicatorBreadth, r.Breadth.Status, breadthDepth)
}

// Cluster combination, isolated-red rescue, and headline wording live in
// internal/rpc/regime_policy.go so offline replay uses the same semantics as
// daemon and rendering adapters.

func canaryBacktestDefensiveAtLeast(res StressResult, severity risk.SignalSeverity) bool {
	if !severityRankAtLeast(res.Severity, severity) {
		return false
	}
	return res.Direction == risk.DirectionDefensive || res.Direction == risk.DirectionMixed
}

func canaryBacktestRebalanceAtLeast(res StressResult, severity risk.SignalSeverity) bool {
	if severityRankAtLeast(res.Severity, severity) && res.Direction == risk.DirectionRebalance {
		return true
	}
	for _, row := range res.Rows {
		if row.Direction == risk.DirectionRebalance && severityRankAtLeast(row.Severity, severity) {
			return true
		}
	}
	for _, signal := range res.Signals {
		if signal.Direction == risk.DirectionRebalance && severityRankAtLeast(signal.Severity, severity) {
			return true
		}
	}
	return false
}

func (a *canaryBacktestAccumulator) add(row CanaryBacktestRowResult) {
	a.metrics.Observations++
	if row.TargetStress {
		a.metrics.TargetStress++
	} else {
		a.metrics.NonStress++
	}
	if row.SignalWatch {
		a.metrics.SignalWatch++
	}
	if row.DefensiveWatch {
		a.metrics.DefensiveWatch++
	}
	if row.DefensiveAct {
		a.metrics.DefensiveAct++
	}
	if row.RebalanceWatch {
		a.metrics.RebalanceWatch++
	}
	if row.DataQualityWatch {
		a.metrics.DataQualityWatch++
	}
	if row.Blocked {
		a.metrics.Blocked++
	}
	a.addSignal(row)
	a.addWatch(row)
	a.addAct(row)
}

func (a *canaryBacktestAccumulator) addSignal(row CanaryBacktestRowResult) {
	switch {
	case row.TargetStress && row.SignalWatch:
		a.metrics.SignalTruePositive++
		if row.DaysToStress != nil {
			a.signalLeadDays += *row.DaysToStress
			a.signalLeadCount++
		}
	case row.TargetStress:
		a.metrics.SignalMiss++
	case row.SignalWatch:
		a.metrics.SignalFalsePositive++
	}
}

func (a *canaryBacktestAccumulator) addWatch(row CanaryBacktestRowResult) {
	watch := canaryBacktestAcceptableWatch(row)
	switch {
	case row.TargetStress && watch:
		a.metrics.WatchTruePositive++
		if row.DaysToStress != nil {
			a.watchLeadDays += *row.DaysToStress
			a.watchLeadCount++
		}
	case row.TargetStress:
		a.metrics.WatchMiss++
	case row.DefensiveWatch:
		a.metrics.WatchFalsePositive++
	}
}

func canaryBacktestAcceptableWatch(row CanaryBacktestRowResult) bool {
	if row.DefensiveWatch {
		return true
	}
	return row.TargetStress && canaryBacktestPortfolioScope(row.TargetScope) && row.RebalanceWatch
}

func (a *canaryBacktestAccumulator) addAct(row CanaryBacktestRowResult) {
	switch {
	case row.TargetStress && row.DefensiveAct:
		a.metrics.ActTruePositive++
		if row.DaysToStress != nil {
			a.actLeadDays += *row.DaysToStress
			a.actLeadCount++
		}
	case row.TargetStress:
		a.metrics.ActMiss++
	case row.DefensiveAct:
		a.metrics.ActFalsePositive++
	}
}

func (a *canaryBacktestAccumulator) result() CanaryBacktestMetrics {
	m := a.metrics
	m.SignalPrecision = ratioPtr(m.SignalTruePositive, m.SignalTruePositive+m.SignalFalsePositive)
	m.SignalRecall = ratioPtr(m.SignalTruePositive, m.TargetStress)
	m.SignalFalseAlarmRate = ratioPtr(m.SignalFalsePositive, m.NonStress)
	m.SignalAvgLeadDays = avgPtr(a.signalLeadDays, a.signalLeadCount)
	m.WatchPrecision = ratioPtr(m.WatchTruePositive, m.WatchTruePositive+m.WatchFalsePositive)
	m.WatchRecall = ratioPtr(m.WatchTruePositive, m.TargetStress)
	m.WatchFalseAlarmRate = ratioPtr(m.WatchFalsePositive, m.NonStress)
	m.WatchAvgLeadDays = avgPtr(a.watchLeadDays, a.watchLeadCount)
	m.ActPrecision = ratioPtr(m.ActTruePositive, m.ActTruePositive+m.ActFalsePositive)
	m.ActRecall = ratioPtr(m.ActTruePositive, m.TargetStress)
	m.ActFalseAlarmRate = ratioPtr(m.ActFalsePositive, m.NonStress)
	m.ActAvgLeadDays = avgPtr(a.actLeadDays, a.actLeadCount)
	return m
}

func (a *regimeBacktestAccumulator) add(row RegimeBacktestRowResult) {
	a.metrics.Observations++
	if !row.Scored {
		a.metrics.OutOfScope++
		return
	}
	a.metrics.ScoredObservations++
	if row.TargetStress {
		a.metrics.TargetStress++
	} else {
		a.metrics.NonStress++
	}
	if row.StressWatch {
		a.metrics.StressWatch++
	}
	if row.StressSignal {
		a.metrics.StressSignal++
	}
	if row.DataQualityWatch {
		a.metrics.DataQualityWatch++
	}
	a.addWatch(row)
	a.addStress(row)
}

func (a *regimeBacktestAccumulator) addWatch(row RegimeBacktestRowResult) {
	switch {
	case row.TargetStress && row.StressWatch:
		a.metrics.WatchTruePositive++
		if row.DaysToStress != nil {
			a.watchLeadDays += *row.DaysToStress
			a.watchLeadCount++
		}
	case row.TargetStress:
		a.metrics.WatchMiss++
	case row.StressWatch:
		a.metrics.WatchFalsePositive++
	}
}

func (a *regimeBacktestAccumulator) addStress(row RegimeBacktestRowResult) {
	switch {
	case row.TargetStress && row.StressSignal:
		a.metrics.StressTruePositive++
		if row.DaysToStress != nil {
			a.stressLeadDays += *row.DaysToStress
			a.stressLeadCount++
		}
	case row.TargetStress:
		a.metrics.StressMiss++
	case row.StressSignal:
		a.metrics.StressFalsePositive++
	}
}

func (a *regimeBacktestAccumulator) result() RegimeBacktestMetrics {
	m := a.metrics
	m.WatchPrecision = ratioPtr(m.WatchTruePositive, m.WatchTruePositive+m.WatchFalsePositive)
	m.WatchRecall = ratioPtr(m.WatchTruePositive, m.TargetStress)
	m.WatchFalseAlarmRate = ratioPtr(m.WatchFalsePositive, m.NonStress)
	m.WatchAvgLeadDays = avgPtr(a.watchLeadDays, a.watchLeadCount)
	m.StressPrecision = ratioPtr(m.StressTruePositive, m.StressTruePositive+m.StressFalsePositive)
	m.StressRecall = ratioPtr(m.StressTruePositive, m.TargetStress)
	m.StressFalseAlarmRate = ratioPtr(m.StressFalsePositive, m.NonStress)
	m.StressAvgLeadDays = avgPtr(a.stressLeadDays, a.stressLeadCount)
	return m
}

func (a *opportunityBacktestAccumulator) add(row OpportunityBacktestRowResult) {
	a.metrics.Observations++
	if row.TargetOpportunity {
		a.metrics.TargetOpportunity++
	} else {
		a.metrics.NonOpportunity++
	}
	a.addOpportunitySplit(row)
	a.addOpportunitySignalContext(row)
	a.addOpportunityCandidate(row)
	if row.SignalFired {
		a.addOpportunitySignal(row)
	}
	a.addOpportunityClassification(row)
}

func (a *opportunityBacktestAccumulator) addOpportunitySplit(row OpportunityBacktestRowResult) {
	switch {
	case row.Holdout:
		a.metrics.HoldoutObservations++
		if row.RetrospectiveHoldout {
			a.metrics.RetrospectiveHoldoutObservations++
		}
		if row.TargetOpportunity {
			a.metrics.HoldoutTargetOpportunity++
		} else {
			a.metrics.HoldoutNonOpportunity++
		}
	case opportunityBacktestSplitUnknown(row.Split):
		a.metrics.UnknownSplitObservations++
	default:
		a.metrics.TuningObservations++
	}
}

func (a *opportunityBacktestAccumulator) addOpportunitySignalContext(row OpportunityBacktestRowResult) {
	if row.SignalContextBlocked {
		a.metrics.SignalContextBlocked++
		if row.Holdout {
			a.metrics.HoldoutSignalContextBlocked++
		}
	}
}

func (a *opportunityBacktestAccumulator) addOpportunityCandidate(row OpportunityBacktestRowResult) {
	if candidateCostPct := opportunityExecutionCostPct(row.Trade); candidateCostPct != nil {
		candidateNetExcess := row.Outcome.ExcessReturnPct - *candidateCostPct
		a.metrics.CostedCandidates++
		if candidateNetExcess > 0 {
			a.metrics.PositiveCandidateNetExcess++
		} else {
			a.metrics.NegativeCandidateNetExcess++
		}
		a.candidateNetExcessReturn += candidateNetExcess
		a.candidateNetExcessReturns = append(a.candidateNetExcessReturns, candidateNetExcess)
		if row.Holdout {
			a.metrics.HoldoutCostedCandidates++
			if candidateNetExcess > 0 {
				a.metrics.HoldoutPositiveCandidateNetExcess++
			} else {
				a.metrics.HoldoutNegativeCandidateNetExcess++
			}
			a.holdoutCandidateNetExcessReturn += candidateNetExcess
			a.holdoutCandidateNetExcessReturns = append(a.holdoutCandidateNetExcessReturns, candidateNetExcess)
		}
		if !row.SignalFired {
			a.metrics.NonFiredCostedCandidates++
			a.nonFiredCandidateNetExcessReturn += candidateNetExcess
			a.nonFiredCandidateNetExcessReturns = append(a.nonFiredCandidateNetExcessReturns, candidateNetExcess)
			if row.Holdout {
				a.metrics.HoldoutNonFiredCostedCandidates++
				a.holdoutNonFiredCandidateNetExcessReturn += candidateNetExcess
				a.holdoutNonFiredCandidateNetExcessReturns = append(a.holdoutNonFiredCandidateNetExcessReturns, candidateNetExcess)
			}
		}
	}
}

func (a *opportunityBacktestAccumulator) addOpportunitySignal(row OpportunityBacktestRowResult) {
	a.metrics.SignalFired++
	if row.Holdout {
		a.metrics.HoldoutSignalFired++
	}
	a.addOpportunitySignalConcentration(row)
	a.addOpportunityGrossSignal(row)
	a.addOpportunityNetSignal(row)
	a.outcomeCount++
}

func (a *opportunityBacktestAccumulator) addOpportunitySignalConcentration(row OpportunityBacktestRowResult) {
	if a.signalInstruments == nil {
		a.signalInstruments = map[string]int{}
	}
	if a.signalClusters == nil {
		a.signalClusters = map[string]int{}
	}
	instrument := cleanOpportunityBacktestInstrument(row.Trade.Instrument)
	cluster := cleanBacktestCluster(row.MarketCluster)
	a.signalInstruments[instrument]++
	a.signalClusters[cluster]++
	if row.Holdout {
		if a.holdoutSignalInstruments == nil {
			a.holdoutSignalInstruments = map[string]int{}
		}
		if a.holdoutSignalClusters == nil {
			a.holdoutSignalClusters = map[string]int{}
		}
		a.holdoutSignalInstruments[instrument]++
		a.holdoutSignalClusters[cluster]++
	}
}

func (a *opportunityBacktestAccumulator) addOpportunityGrossSignal(row OpportunityBacktestRowResult) {
	if row.PositiveExcess {
		a.metrics.PositiveExcess++
	} else {
		a.metrics.NegativeExcess++
	}
	a.forwardReturn += row.Outcome.ForwardReturnPct
	a.benchmarkReturn += row.Outcome.BenchmarkReturnPct
	a.excessReturn += row.Outcome.ExcessReturnPct
	a.adverseExcursion += row.Outcome.MaxAdverseExcursionPct
	a.favorableExcursion += row.Outcome.MaxFavorableExcursionPct
	a.excessReturns = append(a.excessReturns, row.Outcome.ExcessReturnPct)
}

func (a *opportunityBacktestAccumulator) addOpportunityNetSignal(row OpportunityBacktestRowResult) {
	if row.NetExcessReturnPct == nil {
		a.metrics.MissingCostSignalFired++
		if row.Holdout {
			a.metrics.HoldoutMissingCostSignalFired++
		}
		return
	}
	a.metrics.CostedSignalFired++
	if row.ExecutionCostPct != nil {
		a.executionCost += *row.ExecutionCostPct
	}
	if row.PositiveNetExcess != nil && *row.PositiveNetExcess {
		a.metrics.PositiveNetExcess++
	} else {
		a.metrics.NegativeNetExcess++
	}
	a.netExcessReturn += *row.NetExcessReturnPct
	a.netExcessReturns = append(a.netExcessReturns, *row.NetExcessReturnPct)
	if row.Holdout {
		a.metrics.HoldoutCostedSignalFired++
		if row.PositiveNetExcess != nil && *row.PositiveNetExcess {
			a.metrics.HoldoutPositiveNetExcess++
		} else {
			a.metrics.HoldoutNegativeNetExcess++
		}
		a.holdoutNetExcessReturn += *row.NetExcessReturnPct
		a.holdoutNetExcessReturns = append(a.holdoutNetExcessReturns, *row.NetExcessReturnPct)
	}
}

func (a *opportunityBacktestAccumulator) addOpportunityClassification(row OpportunityBacktestRowResult) {
	switch {
	case row.TruePositive:
		a.metrics.TruePositive++
	case row.FalsePositive:
		a.metrics.FalsePositive++
	case row.Miss:
		a.metrics.Miss++
	}
}

func (a *opportunityBacktestAccumulator) result() OpportunityBacktestMetrics {
	m := a.metrics
	m.DistinctSignalInstruments = len(a.signalInstruments)
	m.MaxSignalInstrument, m.MaxSignalInstrumentFired = maxStringCount(a.signalInstruments)
	m.MaxSignalInstrumentShare = ratioPtr(m.MaxSignalInstrumentFired, m.SignalFired)
	m.DistinctSignalClusters = len(a.signalClusters)
	m.MaxSignalCluster, m.MaxSignalClusterFired = maxStringCount(a.signalClusters)
	m.MaxSignalClusterShare = ratioPtr(m.MaxSignalClusterFired, m.SignalFired)
	m.HoldoutDistinctSignalInstruments = len(a.holdoutSignalInstruments)
	m.HoldoutMaxSignalInstrument, m.HoldoutMaxSignalInstrumentFired = maxStringCount(a.holdoutSignalInstruments)
	m.HoldoutMaxSignalInstrumentShare = ratioPtr(m.HoldoutMaxSignalInstrumentFired, m.HoldoutSignalFired)
	m.HoldoutDistinctSignalClusters = len(a.holdoutSignalClusters)
	m.HoldoutMaxSignalCluster, m.HoldoutMaxSignalClusterFired = maxStringCount(a.holdoutSignalClusters)
	m.HoldoutMaxSignalClusterShare = ratioPtr(m.HoldoutMaxSignalClusterFired, m.HoldoutSignalFired)
	m.Precision = ratioPtr(m.TruePositive, m.TruePositive+m.FalsePositive)
	m.Recall = ratioPtr(m.TruePositive, m.TargetOpportunity)
	m.FalseAlarmRate = ratioPtr(m.FalsePositive, m.NonOpportunity)
	m.ExcessHitRate = ratioPtr(m.PositiveExcess, m.SignalFired)
	m.ExcessHitRateLower95 = wilsonLowerBoundPtr(m.PositiveExcess, m.SignalFired, opportunityEvidenceConfidenceZ)
	m.NetExcessHitRate = ratioPtr(m.PositiveNetExcess, m.CostedSignalFired)
	m.NetExcessHitRateLower95 = wilsonLowerBoundPtr(m.PositiveNetExcess, m.CostedSignalFired, opportunityEvidenceConfidenceZ)
	m.HoldoutNetExcessHitRate = ratioPtr(m.HoldoutPositiveNetExcess, m.HoldoutCostedSignalFired)
	m.HoldoutNetExcessHitRateLower95 = wilsonLowerBoundPtr(m.HoldoutPositiveNetExcess, m.HoldoutCostedSignalFired, opportunityEvidenceConfidenceZ)
	m.CandidateNetExcessHitRate = ratioPtr(m.PositiveCandidateNetExcess, m.CostedCandidates)
	m.HoldoutCandidateNetExcessHitRate = ratioPtr(m.HoldoutPositiveCandidateNetExcess, m.HoldoutCostedCandidates)
	m.AvgForwardReturnPct = avgFloatPtr(a.forwardReturn, a.outcomeCount)
	m.AvgBenchmarkReturnPct = avgFloatPtr(a.benchmarkReturn, a.outcomeCount)
	m.AvgExcessReturnPct = avgFloatPtr(a.excessReturn, a.outcomeCount)
	m.AvgExcessReturnLower95Pct = meanLowerConfidencePtr(a.excessReturns, opportunityEvidenceConfidenceZ)
	m.MedianExcessReturnPct = medianFloatPtr(a.excessReturns)
	m.WorstExcessReturnPct = minFloatPtr(a.excessReturns)
	m.BestExcessReturnPct = maxFloatPtr(a.excessReturns)
	m.AvgExecutionCostPct = avgFloatPtr(a.executionCost, m.CostedSignalFired)
	m.AvgNetExcessReturnPct = avgFloatPtr(a.netExcessReturn, m.CostedSignalFired)
	m.AvgNetExcessReturnLower95Pct = meanLowerConfidencePtr(a.netExcessReturns, opportunityEvidenceConfidenceZ)
	m.HoldoutAvgNetExcessReturnPct = avgFloatPtr(a.holdoutNetExcessReturn, m.HoldoutCostedSignalFired)
	m.HoldoutAvgNetExcessReturnLower95Pct = meanLowerConfidencePtr(a.holdoutNetExcessReturns, opportunityEvidenceConfidenceZ)
	m.MedianNetExcessReturnPct = medianFloatPtr(a.netExcessReturns)
	m.WorstNetExcessReturnPct = minFloatPtr(a.netExcessReturns)
	m.BestNetExcessReturnPct = maxFloatPtr(a.netExcessReturns)
	m.AvgCandidateNetExcessPct = avgFloatPtr(a.candidateNetExcessReturn, m.CostedCandidates)
	m.MedianCandidateNetExcessPct = medianFloatPtr(a.candidateNetExcessReturns)
	m.AvgNonFiredCandidateNetPct = avgFloatPtr(a.nonFiredCandidateNetExcessReturn, m.NonFiredCostedCandidates)
	m.MedianNonFiredCandidateNetPct = medianFloatPtr(a.nonFiredCandidateNetExcessReturns)
	m.FiredVsCandidateAvgLiftPct = diffFloatPtr(m.AvgNetExcessReturnPct, m.AvgCandidateNetExcessPct)
	m.FiredVsCandidateMedianLiftPct = diffFloatPtr(m.MedianNetExcessReturnPct, m.MedianCandidateNetExcessPct)
	m.FiredVsNonFiredAvgLiftPct = diffFloatPtr(m.AvgNetExcessReturnPct, m.AvgNonFiredCandidateNetPct)
	m.FiredVsNonFiredMedianLiftPct = diffFloatPtr(m.MedianNetExcessReturnPct, m.MedianNonFiredCandidateNetPct)
	m.HoldoutAvgCandidateNetExcessPct = avgFloatPtr(a.holdoutCandidateNetExcessReturn, m.HoldoutCostedCandidates)
	m.HoldoutMedianCandidateNetExcessPct = medianFloatPtr(a.holdoutCandidateNetExcessReturns)
	m.HoldoutAvgNonFiredCandidateNetPct = avgFloatPtr(a.holdoutNonFiredCandidateNetExcessReturn, m.HoldoutNonFiredCostedCandidates)
	m.HoldoutMedianNonFiredCandidateNetPct = medianFloatPtr(a.holdoutNonFiredCandidateNetExcessReturns)
	m.HoldoutFiredVsCandidateAvgLiftPct = diffFloatPtr(m.HoldoutAvgNetExcessReturnPct, m.HoldoutAvgCandidateNetExcessPct)
	m.HoldoutFiredVsCandidateMedianLiftPct = diffFloatPtr(medianFloatPtr(a.holdoutNetExcessReturns), m.HoldoutMedianCandidateNetExcessPct)
	m.HoldoutFiredVsNonFiredAvgLiftPct = diffFloatPtr(m.HoldoutAvgNetExcessReturnPct, m.HoldoutAvgNonFiredCandidateNetPct)
	m.HoldoutFiredVsNonFiredMedianLiftPct = diffFloatPtr(medianFloatPtr(a.holdoutNetExcessReturns), m.HoldoutMedianNonFiredCandidateNetPct)
	m.AvgMaxAdverseExcursionPct = avgFloatPtr(a.adverseExcursion, a.outcomeCount)
	m.AvgMaxFavorableExcursionPct = avgFloatPtr(a.favorableExcursion, a.outcomeCount)
	return m
}

func regimeBaselineBacktestRow(row RegimeBacktestRowResult) RegimeBacktestRowResult {
	out := row
	out.StressWatch = row.BaselineWatch
	out.StressSignal = row.BaselineStress
	return out
}

func canaryRegimeOnlyBacktestRow(row CanaryBacktestRowResult) CanaryBacktestRowResult {
	out := row
	out.SignalWatch = row.RegimeOnlyWatch
	out.DefensiveWatch = row.RegimeOnlyWatch
	out.DefensiveAct = row.RegimeOnlyAct
	out.RebalanceWatch = false
	out.DataQualityWatch = false
	out.Blocked = false
	return out
}

func regimeBacktestLifecycleMetrics(rows []RegimeBacktestRowResult) BacktestLifecycleMetrics {
	acc := lifecycleMetricAccumulator{}
	for _, row := range rows {
		if !row.Scored {
			continue
		}
		acc.add(lifecycleMetricInput{
			targetStress:       row.TargetStress,
			targetKind:         row.TargetKind,
			targetScope:        row.TargetScope,
			daysToStress:       row.DaysToStress,
			earlyWarning:       row.EarlyWarning,
			confirmedStress:    row.ConfirmedStress || row.Panic,
			isPanic:            row.Panic,
			stabilization:      row.Stabilization,
			opportunity:        row.Opportunity,
			dataQualityBlocked: row.LifecycleStage == rpc.LifecycleDataQuality,
			maxSPYDrawdownPct:  row.MaxSPYDrawdownPct,
			vixShockPct:        row.VIXShockPct,
		})
	}
	return acc.result()
}

func canaryBacktestLifecycleMetrics(rows []CanaryBacktestRowResult) BacktestLifecycleMetrics {
	acc := lifecycleMetricAccumulator{}
	for _, row := range rows {
		acc.add(lifecycleMetricInput{
			targetStress:       row.TargetStress,
			targetKind:         row.TargetKind,
			targetScope:        row.TargetScope,
			daysToStress:       row.DaysToStress,
			earlyWarning:       row.EarlyWarning,
			confirmedStress:    row.ConfirmedStress || row.Panic,
			isPanic:            row.Panic,
			stabilization:      row.Stabilization,
			opportunity:        row.Opportunity,
			dataQualityBlocked: row.Blocked || row.LifecycleStage == rpc.LifecycleDataQuality,
			maxSPYDrawdownPct:  row.MaxSPYDrawdownPct,
			vixShockPct:        row.VIXShockPct,
		})
	}
	return acc.result()
}

type lifecycleMetricInput struct {
	targetStress       bool
	targetKind         string
	targetScope        string
	daysToStress       *int
	earlyWarning       bool
	confirmedStress    bool
	isPanic            bool
	stabilization      bool
	opportunity        bool
	dataQualityBlocked bool
	maxSPYDrawdownPct  *float64
	vixShockPct        *float64
}

type lifecycleMetricAccumulator struct {
	metrics       BacktestLifecycleMetrics
	earlyLeadDays []int
}

func (a *lifecycleMetricAccumulator) add(row lifecycleMetricInput) {
	a.metrics.Observations++
	if row.targetStress {
		a.metrics.TargetStress++
	} else {
		a.metrics.NonStress++
	}
	laterStress := row.targetStress && row.daysToStress != nil && *row.daysToStress > 0
	if laterStress {
		a.metrics.LaterConfirmedStress++
	}
	majorStress := row.targetStress && majorStressTarget(row.targetKind, row.maxSPYDrawdownPct, row.vixShockPct)
	if majorStress {
		a.metrics.MajorStress++
	}
	if row.earlyWarning {
		a.metrics.EarlyWarning++
	}
	if row.confirmedStress {
		a.metrics.ConfirmedStress++
	}
	if row.isPanic {
		a.metrics.PanicCount++
	}
	if row.stabilization {
		a.metrics.Stabilization++
	}
	if row.opportunity {
		a.metrics.Opportunity++
	}
	if row.dataQualityBlocked {
		a.metrics.DataQualityBlocked++
	}
	switch {
	case laterStress && row.earlyWarning:
		a.metrics.EarlyWarningTruePositive++
		a.earlyLeadDays = append(a.earlyLeadDays, *row.daysToStress)
	case laterStress:
		a.metrics.EarlyWarningMiss++
	case row.earlyWarning:
		a.metrics.EarlyWarningFalsePositive++
		if calmRallyTarget(row.targetKind) {
			a.metrics.EarlyWarningFalseCalmRally++
		}
	}
	switch {
	case row.targetStress && row.confirmedStress:
		a.metrics.ConfirmedStressTruePositive++
	case row.targetStress:
		a.metrics.ConfirmedStressMiss++
	case row.confirmedStress:
		a.metrics.ConfirmedStressFalsePositive++
	}
	switch {
	case majorStress && row.isPanic:
		a.metrics.PanicTruePositive++
	case majorStress:
		a.metrics.PanicMiss++
	}
	if row.targetStress && (row.stabilization || row.opportunity) {
		a.metrics.StabilizationOpportunityFalseStarts++
	}
}

func (a *lifecycleMetricAccumulator) result() BacktestLifecycleMetrics {
	m := a.metrics
	m.EarlyWarningPrecision = ratioPtr(m.EarlyWarningTruePositive, m.EarlyWarningTruePositive+m.EarlyWarningFalsePositive)
	m.EarlyWarningRecall = ratioPtr(m.EarlyWarningTruePositive, m.LaterConfirmedStress)
	m.EarlyWarningMedianLeadDays = medianIntPtr(a.earlyLeadDays)
	m.ConfirmedStressPrecision = ratioPtr(m.ConfirmedStressTruePositive, m.ConfirmedStressTruePositive+m.ConfirmedStressFalsePositive)
	m.ConfirmedStressRecall = ratioPtr(m.ConfirmedStressTruePositive, m.TargetStress)
	m.PanicRecall = ratioPtr(m.PanicTruePositive, m.MajorStress)
	return m
}

func regimeBacktestEventMetrics(rows []RegimeBacktestRowResult) BacktestEventMetrics {
	events := map[string]*eventMetricAccumulator{}
	for _, row := range rows {
		if !row.Scored {
			continue
		}
		key := backtestEventKey(row.MarketCluster, row.Case)
		if events[key] == nil {
			events[key] = &eventMetricAccumulator{}
		}
		events[key].add(row.TargetStress, row.EarlyWarning || row.ConfirmedStress || row.Panic, row.ConfirmedStress || row.Panic, row.Panic, majorStressTarget(row.TargetKind, row.MaxSPYDrawdownPct, row.VIXShockPct))
	}
	return eventMetricsResult(events)
}

func canaryBacktestEventMetrics(rows []CanaryBacktestRowResult) BacktestEventMetrics {
	events := map[string]*eventMetricAccumulator{}
	for _, row := range rows {
		key := backtestEventKey(row.MarketCluster, row.Case)
		if events[key] == nil {
			events[key] = &eventMetricAccumulator{}
		}
		watch := row.EarlyWarning || row.ConfirmedStress || row.Panic || row.DefensiveWatch || row.RebalanceWatch
		confirmed := row.ConfirmedStress || row.Panic
		events[key].add(row.TargetStress, watch, confirmed, row.Panic, majorStressTarget(row.TargetKind, row.MaxSPYDrawdownPct, row.VIXShockPct))
	}
	return eventMetricsResult(events)
}

type eventMetricAccumulator struct {
	targetStress    bool
	watch           bool
	confirmedStress bool
	isPanic         bool
	majorStress     bool
}

func (e *eventMetricAccumulator) add(targetStress, watch, confirmedStress, isPanic, majorStress bool) {
	e.targetStress = e.targetStress || targetStress
	e.watch = e.watch || watch
	e.confirmedStress = e.confirmedStress || confirmedStress
	e.isPanic = e.isPanic || isPanic
	e.majorStress = e.majorStress || majorStress
}

func eventMetricsResult(events map[string]*eventMetricAccumulator) BacktestEventMetrics {
	var m BacktestEventMetrics
	for _, e := range events {
		m.Events++
		if e.targetStress {
			m.TargetStressEvents++
		} else {
			m.NonStressEvents++
		}
		if e.watch {
			m.WatchEvents++
		}
		if e.confirmedStress {
			m.ConfirmedStressEvents++
		}
		if e.isPanic {
			m.PanicEvents++
		}
		switch {
		case e.targetStress && e.watch:
			m.WatchTruePositiveEvents++
		case e.targetStress:
			m.WatchMissEvents++
		case e.watch:
			m.WatchFalsePositiveEvents++
		}
		switch {
		case e.targetStress && e.confirmedStress:
			m.ConfirmedStressTruePositive++
		case e.targetStress:
			m.ConfirmedStressMiss++
		case e.confirmedStress:
			m.ConfirmedStressFalsePositive++
		}
	}
	m.WatchPrecision = ratioPtr(m.WatchTruePositiveEvents, m.WatchTruePositiveEvents+m.WatchFalsePositiveEvents)
	m.WatchRecall = ratioPtr(m.WatchTruePositiveEvents, m.TargetStressEvents)
	m.ConfirmedStressPrecision = ratioPtr(m.ConfirmedStressTruePositive, m.ConfirmedStressTruePositive+m.ConfirmedStressFalsePositive)
	m.ConfirmedStressRecall = ratioPtr(m.ConfirmedStressTruePositive, m.TargetStressEvents)
	panicTP := 0
	majorEvents := 0
	for _, e := range events {
		if !e.targetStress || !e.majorStress {
			continue
		}
		majorEvents++
		if e.isPanic {
			panicTP++
		}
	}
	m.PanicRecall = ratioPtr(panicTP, majorEvents)
	return m
}

func backtestEventKey(cluster, fallback string) string {
	cluster = cleanBacktestCluster(cluster)
	if cluster != "unclassified" {
		return cluster
	}
	if strings.TrimSpace(fallback) != "" {
		return fallback
	}
	return cluster
}

func canaryBacktestCategories(row CanaryBacktestRowResult) []string {
	categories := []string{}
	scope := cleanBacktestTargetScope(row.TargetScope)
	kind := strings.ToLower(row.TargetKind + " " + row.Case + " " + row.MarketCluster)
	if scope == "market" || strings.Contains(kind, "market") || strings.Contains(kind, "shock") || strings.Contains(kind, "selloff") || strings.Contains(kind, "crash") || strings.Contains(kind, "rates") || strings.Contains(kind, "carry") || strings.Contains(kind, "bank") {
		categories = append(categories, "market-driven")
	}
	if canaryBacktestPortfolioScope(scope) {
		categories = append(categories, "portfolio-driven")
	}
	if strings.Contains(kind, "concentration") || strings.Contains(kind, "squeeze") || hasAnySignal(row.PrimaryDrivers, risk.SignalSingleNameExposureHigh, risk.SignalSingleNameDeltaHigh, risk.SignalHeldUnderlyingPnLShock) {
		categories = append(categories, "concentration-driven")
	}
	if strings.Contains(kind, "margin") || hasAnySignal(row.PrimaryDrivers, risk.SignalMarginCushionLow, risk.SignalLookAheadCushionLow) {
		categories = append(categories, "margin-driven")
	}
	if strings.Contains(kind, "option") || strings.Contains(kind, "convex") || hasAnySignal(row.PrimaryDrivers, risk.SignalShortConvexityHigh, risk.SignalOptionGreeksDegraded, risk.SignalHeldOptionExpiryConcentration) {
		categories = append(categories, "options-driven")
	}
	if strings.Contains(kind, "coverage") || strings.Contains(kind, "data-quality") || row.DataQualityWatch || row.Blocked || row.LifecycleStage == rpc.LifecycleDataQuality || hasAnySignal(row.PrimaryDrivers, risk.SignalHeldLiquidityDegraded) {
		categories = append(categories, "data-quality")
	}
	if len(categories) == 0 {
		categories = append(categories, "other")
	}
	slices.Sort(categories)
	return slices.Compact(categories)
}

func canaryBacktestRegimeLift(rows []CanaryBacktestRowResult) CanaryBacktestRegimeLift {
	var out CanaryBacktestRegimeLift
	for _, row := range rows {
		if !row.TargetStress || !canaryBacktestPortfolioScope(row.TargetScope) {
			continue
		}
		out.PortfolioStressRows++
		if row.RegimeOnlyWatch {
			out.RegimeOnlyWatchTruePositive++
		}
		if canaryBacktestAcceptableWatch(row) {
			out.CanaryWatchTruePositive++
		}
		if !row.RegimeOnlyWatch && canaryBacktestAcceptableWatch(row) {
			out.CanaryAddedTruePositive++
		}
	}
	out.RegimeOnlyRecall = ratioPtr(out.RegimeOnlyWatchTruePositive, out.PortfolioStressRows)
	out.CanaryRecall = ratioPtr(out.CanaryWatchTruePositive, out.PortfolioStressRows)
	return out
}

func majorStressTarget(kind string, maxSPYDrawdownPct, vixShockPct *float64) bool {
	k := strings.ToLower(kind)
	if maxSPYDrawdownPct != nil && *maxSPYDrawdownPct <= -4 {
		return true
	}
	if vixShockPct != nil && *vixShockPct >= 20 {
		return true
	}
	for _, token := range []string{"crash", "liquidity", "shock", "selloff", "volmageddon", "rates", "bank", "funding", "tariff", "carry"} {
		if strings.Contains(k, token) {
			return true
		}
	}
	return false
}

func calmRallyTarget(kind string) bool {
	k := strings.ToLower(kind)
	return strings.Contains(k, "calm") || strings.Contains(k, "rally") || strings.Contains(k, "control") || strings.Contains(k, "recovered")
}

func hasAnySignal(got []risk.SignalID, wants ...risk.SignalID) bool {
	for _, id := range got {
		if slices.Contains(wants, id) {
			return true
		}
	}
	return false
}

func ratioPtr(num, denom int) *float64 {
	if denom == 0 {
		return nil
	}
	v := float64(num) / float64(denom)
	return &v
}

func avgPtr(sum, count int) *float64 {
	if count == 0 {
		return nil
	}
	v := float64(sum) / float64(count)
	return &v
}

func avgFloatPtr(sum float64, count int) *float64 {
	if count == 0 {
		return nil
	}
	v := sum / float64(count)
	return &v
}

func diffFloatPtr(left, right *float64) *float64 {
	if left == nil || right == nil || math.IsNaN(*left) || math.IsNaN(*right) {
		return nil
	}
	v := *left - *right
	return &v
}

func wilsonLowerBoundPtr(successes, total int, z float64) *float64 {
	if total <= 0 || z <= 0 {
		return nil
	}
	n := float64(total)
	p := float64(successes) / n
	z2 := z * z
	denom := 1 + z2/n
	center := p + z2/(2*n)
	margin := z * math.Sqrt((p*(1-p)+z2/(4*n))/n)
	v := (center - margin) / denom
	v = max(0, min(1, v))
	return &v
}

func meanLowerConfidencePtr(values []float64, z float64) *float64 {
	if len(values) == 0 || z <= 0 {
		return nil
	}
	mean := meanFloat(values)
	if len(values) == 1 {
		return &mean
	}
	var sumSquares float64
	for _, value := range values {
		delta := value - mean
		sumSquares += delta * delta
	}
	variance := sumSquares / float64(len(values)-1)
	v := mean - z*math.Sqrt(variance/float64(len(values)))
	return &v
}

func meanFloat(values []float64) float64 {
	var sum float64
	for _, value := range values {
		sum += value
	}
	return sum / float64(len(values))
}

func maxStringCount(counts map[string]int) (string, int) {
	if len(counts) == 0 {
		return "", 0
	}
	keys := make([]string, 0, len(counts))
	for key := range counts {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	bestKey := keys[0]
	bestCount := counts[bestKey]
	for _, key := range keys[1:] {
		count := counts[key]
		if count > bestCount {
			bestKey, bestCount = key, count
		}
	}
	return bestKey, bestCount
}

func medianFloatPtr(values []float64) *float64 {
	if len(values) == 0 {
		return nil
	}
	sorted := slices.Clone(values)
	slices.Sort(sorted)
	mid := len(sorted) / 2
	var v float64
	if len(sorted)%2 == 1 {
		v = sorted[mid]
	} else {
		v = (sorted[mid-1] + sorted[mid]) / 2
	}
	return &v
}

func minFloatPtr(values []float64) *float64 {
	if len(values) == 0 {
		return nil
	}
	v := values[0]
	for _, value := range values[1:] {
		v = min(v, value)
	}
	return &v
}

func maxFloatPtr(values []float64) *float64 {
	if len(values) == 0 {
		return nil
	}
	v := values[0]
	for _, value := range values[1:] {
		v = max(v, value)
	}
	return &v
}

func medianIntPtr(values []int) *float64 {
	if len(values) == 0 {
		return nil
	}
	sorted := slices.Clone(values)
	slices.Sort(sorted)
	mid := len(sorted) / 2
	var v float64
	if len(sorted)%2 == 1 {
		v = float64(sorted[mid])
	} else {
		v = float64(sorted[mid-1]+sorted[mid]) / 2
	}
	return &v
}

const (
	opportunityEvidenceMinObservations             = 100
	opportunityEvidenceMinSignalFired              = 30
	opportunityEvidenceMinTargetOpportunity        = 30
	opportunityEvidenceMinNonOpportunity           = 30
	opportunityEvidenceMinSignalInstruments        = 10
	opportunityEvidenceMinSignalClusters           = 3
	opportunityEvidenceMinHoldoutObservations      = 30
	opportunityEvidenceMinHoldoutSignalFired       = 10
	opportunityEvidenceMinHoldoutTargetOpportunity = 10
	opportunityEvidenceMinHoldoutNonOpportunity    = 10
	opportunityEvidenceMinHoldoutSignalInstruments = 5
	opportunityEvidenceMinHoldoutSignalClusters    = 2
	opportunityEvidenceMaxInstrumentShare          = 0.25
	opportunityEvidenceMaxClusterShare             = 0.60
	opportunityEvidenceMaxHoldoutInstrumentShare   = 0.40
	opportunityEvidenceMaxHoldoutClusterShare      = 0.75
	opportunityEvidenceMaxMTMDrawdownPct           = -25.0
	opportunityEvidenceMaxMTMGapDays               = 7
	opportunityEvidenceMinMTMExcessToDrawdown      = 1.0
	opportunityEvidenceConfidenceZ                 = 1.96
)

func opportunityBacktestEvidence(m OpportunityBacktestMetrics) OpportunityBacktestEvidence {
	return opportunityBacktestEvidenceWithSimulation(m, nil)
}

func opportunityBacktestEvidenceWithSimulation(m OpportunityBacktestMetrics, sim *OpportunityBacktestSimulation) OpportunityBacktestEvidence {
	out := OpportunityBacktestEvidence{
		MinObservations:                 opportunityEvidenceMinObservations,
		MinSignalFired:                  opportunityEvidenceMinSignalFired,
		MinTargetOpportunity:            opportunityEvidenceMinTargetOpportunity,
		MinNonOpportunity:               opportunityEvidenceMinNonOpportunity,
		MinSignalInstruments:            opportunityEvidenceMinSignalInstruments,
		MinSignalClusters:               opportunityEvidenceMinSignalClusters,
		MinHoldoutObservations:          opportunityEvidenceMinHoldoutObservations,
		MinHoldoutSignalFired:           opportunityEvidenceMinHoldoutSignalFired,
		MinHoldoutTargetOpportunity:     opportunityEvidenceMinHoldoutTargetOpportunity,
		MinHoldoutNonOpportunity:        opportunityEvidenceMinHoldoutNonOpportunity,
		MinHoldoutSignalInstruments:     opportunityEvidenceMinHoldoutSignalInstruments,
		MinHoldoutSignalClusters:        opportunityEvidenceMinHoldoutSignalClusters,
		MinPortfolioFilledSignals:       opportunityEvidenceMinSignalFired,
		MaxSignalInstrumentShare:        opportunityEvidenceMaxInstrumentShare,
		MaxSignalClusterShare:           opportunityEvidenceMaxClusterShare,
		MaxHoldoutSignalInstrumentShare: opportunityEvidenceMaxHoldoutInstrumentShare,
		MaxHoldoutSignalClusterShare:    opportunityEvidenceMaxHoldoutClusterShare,
		MaxMarkToMarketDrawdownPct:      opportunityEvidenceMaxMTMDrawdownPct,
		MaxMarkToMarketGapDays:          opportunityEvidenceMaxMTMGapDays,
		MinMarkToMarketExcessToDrawdown: opportunityEvidenceMinMTMExcessToDrawdown,
		Needs:                           opportunityBacktestEvidenceNeeds(m),
	}
	switch {
	case m.Observations == 0:
		out.Status = "no_data"
		out.Reasons = append(out.Reasons, "no observations were loaded")
	case m.SignalContextBlocked > 0:
		out.Status = "dirty_sample"
		out.Reasons = append(out.Reasons, fmt.Sprintf("%d observation(s) have blocked signal context; remove stale/missing/non-live rows before reading the evidence gate", m.SignalContextBlocked))
	case m.SignalFired == 0:
		out.Status = "no_signals"
		out.Reasons = append(out.Reasons, "no fired signal rows were present")
	case m.Observations < opportunityEvidenceMinObservations ||
		m.SignalFired < opportunityEvidenceMinSignalFired ||
		m.TargetOpportunity < opportunityEvidenceMinTargetOpportunity ||
		m.NonOpportunity < opportunityEvidenceMinNonOpportunity:
		out.Status = "insufficient_sample"
		if m.Observations < opportunityEvidenceMinObservations {
			out.Reasons = append(out.Reasons, fmt.Sprintf("observations %d < %d", m.Observations, opportunityEvidenceMinObservations))
		}
		if m.SignalFired < opportunityEvidenceMinSignalFired {
			out.Reasons = append(out.Reasons, fmt.Sprintf("fired signals %d < %d", m.SignalFired, opportunityEvidenceMinSignalFired))
		}
		if m.TargetOpportunity < opportunityEvidenceMinTargetOpportunity {
			out.Reasons = append(out.Reasons, fmt.Sprintf("target opportunities %d < %d", m.TargetOpportunity, opportunityEvidenceMinTargetOpportunity))
		}
		if m.NonOpportunity < opportunityEvidenceMinNonOpportunity {
			out.Reasons = append(out.Reasons, fmt.Sprintf("non-opportunity controls %d < %d", m.NonOpportunity, opportunityEvidenceMinNonOpportunity))
		}
		if m.MissingCostSignalFired > 0 {
			out.Reasons = append(out.Reasons, fmt.Sprintf("missing round-trip cost for %d/%d fired signals", m.MissingCostSignalFired, m.SignalFired))
		}
	case m.UnknownSplitObservations > 0:
		out.Status = "unknown_split"
		out.Reasons = append(out.Reasons, fmt.Sprintf("%d observation(s) have unknown split; assign every row to tuning or holdout before reading the evidence gate", m.UnknownSplitObservations))
	case m.RetrospectiveHoldoutObservations > 0:
		out.Status = "retrospective_holdout"
		out.Reasons = append(out.Reasons, fmt.Sprintf("%d holdout observation(s) came from a retrospective historical date split; use it for diagnostics, not pre-registered alpha evidence", m.RetrospectiveHoldoutObservations))
	case m.ExcessHitRate == nil || *m.ExcessHitRate <= 0.5:
		out.Status = "unfavorable"
		out.Reasons = append(out.Reasons, "excess hit rate is not above 50%")
	case m.MedianExcessReturnPct == nil || *m.MedianExcessReturnPct <= 0:
		out.Status = "unfavorable"
		out.Reasons = append(out.Reasons, "median excess return is not positive")
	case m.AvgExcessReturnPct == nil || *m.AvgExcessReturnPct <= 0:
		out.Status = "unfavorable"
		out.Reasons = append(out.Reasons, "average excess return is not positive")
	case m.MissingCostSignalFired > 0:
		out.Status = "missing_costs"
		out.Reasons = append(out.Reasons, fmt.Sprintf("missing round-trip cost for %d/%d fired signals", m.MissingCostSignalFired, m.SignalFired))
	case m.NetExcessHitRate == nil || *m.NetExcessHitRate <= 0.5:
		out.Status = "unfavorable"
		out.Reasons = append(out.Reasons, "net excess hit rate is not above 50% after costs")
	case m.MedianNetExcessReturnPct == nil || *m.MedianNetExcessReturnPct <= 0:
		out.Status = "unfavorable"
		out.Reasons = append(out.Reasons, "median net excess return is not positive after costs")
	case m.AvgNetExcessReturnPct == nil || *m.AvgNetExcessReturnPct <= 0:
		out.Status = "unfavorable"
		out.Reasons = append(out.Reasons, "average net excess return is not positive after costs")
	case m.DistinctSignalInstruments < opportunityEvidenceMinSignalInstruments:
		out.Status = "concentrated_sample"
		out.Reasons = append(out.Reasons, fmt.Sprintf("distinct fired instruments %d < %d", m.DistinctSignalInstruments, opportunityEvidenceMinSignalInstruments))
	case m.MaxSignalInstrumentShare != nil && *m.MaxSignalInstrumentShare > opportunityEvidenceMaxInstrumentShare:
		out.Status = "concentrated_sample"
		out.Reasons = append(out.Reasons, fmt.Sprintf("largest fired instrument %s is %d/%d (%s), above max %s",
			m.MaxSignalInstrument,
			m.MaxSignalInstrumentFired,
			m.SignalFired,
			formatBacktestRate(m.MaxSignalInstrumentShare),
			formatBacktestRateValue(opportunityEvidenceMaxInstrumentShare)))
	case m.DistinctSignalClusters < opportunityEvidenceMinSignalClusters:
		out.Status = "concentrated_sample"
		out.Reasons = append(out.Reasons, fmt.Sprintf("distinct fired clusters %d < %d", m.DistinctSignalClusters, opportunityEvidenceMinSignalClusters))
	case m.MaxSignalClusterShare != nil && *m.MaxSignalClusterShare > opportunityEvidenceMaxClusterShare:
		out.Status = "concentrated_sample"
		out.Reasons = append(out.Reasons, fmt.Sprintf("largest fired cluster %s is %d/%d (%s), above max %s",
			m.MaxSignalCluster,
			m.MaxSignalClusterFired,
			m.SignalFired,
			formatBacktestRate(m.MaxSignalClusterShare),
			formatBacktestRateValue(opportunityEvidenceMaxClusterShare)))
	case m.ExcessHitRateLower95 == nil:
		out.Status = "weak_edge"
		out.Reasons = append(out.Reasons, "gross excess hit-rate 95% lower bound is unavailable")
	case *m.ExcessHitRateLower95 <= 0.5:
		out.Status = "weak_edge"
		out.Reasons = append(out.Reasons, fmt.Sprintf("gross excess hit-rate 95%% lower bound %s is not above 50%%", formatBacktestRate(m.ExcessHitRateLower95)))
	case m.AvgExcessReturnLower95Pct == nil:
		out.Status = "weak_edge"
		out.Reasons = append(out.Reasons, "average gross excess 95% lower bound is unavailable")
	case *m.AvgExcessReturnLower95Pct <= 0:
		out.Status = "weak_edge"
		out.Reasons = append(out.Reasons, fmt.Sprintf("average gross excess 95%% lower bound %s is not positive", formatBacktestPercent(m.AvgExcessReturnLower95Pct)))
	case m.NetExcessHitRateLower95 == nil:
		out.Status = "weak_edge"
		out.Reasons = append(out.Reasons, "net excess hit-rate 95% lower bound is unavailable")
	case *m.NetExcessHitRateLower95 <= 0.5:
		out.Status = "weak_edge"
		out.Reasons = append(out.Reasons, fmt.Sprintf("net excess hit-rate 95%% lower bound %s is not above 50%%", formatBacktestRate(m.NetExcessHitRateLower95)))
	case m.AvgNetExcessReturnLower95Pct == nil:
		out.Status = "weak_edge"
		out.Reasons = append(out.Reasons, "average net excess 95% lower bound is unavailable")
	case *m.AvgNetExcessReturnLower95Pct <= 0:
		out.Status = "weak_edge"
		out.Reasons = append(out.Reasons, fmt.Sprintf("average net excess 95%% lower bound %s is not positive", formatBacktestPercent(m.AvgNetExcessReturnLower95Pct)))
	case m.CostedCandidates == 0:
		out.Status = "missing_candidate_baseline"
		out.Reasons = append(out.Reasons, "candidate-universe baseline is unavailable; no candidate rows have round-trip cost assumptions")
	case m.CostedCandidates < m.Observations:
		out.Status = "missing_candidate_costs"
		out.Reasons = append(out.Reasons, fmt.Sprintf("missing round-trip cost for %d/%d candidate rows; candidate-universe baseline is incomplete", m.Observations-m.CostedCandidates, m.Observations))
	case m.FiredVsCandidateAvgLiftPct == nil || *m.FiredVsCandidateAvgLiftPct <= 0:
		out.Status = "weak_candidate_baseline"
		out.Reasons = append(out.Reasons, fmt.Sprintf("fired average net excess does not beat all-candidate baseline after costs; lift %s", formatBacktestPercent(m.FiredVsCandidateAvgLiftPct)))
	case m.NonFiredCostedCandidates == 0:
		out.Status = "weak_candidate_baseline"
		out.Reasons = append(out.Reasons, "no costed non-fired candidate rows were present; selection lift cannot be measured")
	case m.FiredVsNonFiredAvgLiftPct == nil || *m.FiredVsNonFiredAvgLiftPct <= 0:
		out.Status = "weak_candidate_baseline"
		out.Reasons = append(out.Reasons, fmt.Sprintf("fired average net excess does not beat non-fired candidate baseline after costs; lift %s", formatBacktestPercent(m.FiredVsNonFiredAvgLiftPct)))
	case m.HoldoutObservations == 0:
		out.Status = "no_holdout"
		out.Reasons = append(out.Reasons, "no holdout/out-of-sample rows were present")
	case m.HoldoutObservations < opportunityEvidenceMinHoldoutObservations ||
		m.HoldoutSignalFired < opportunityEvidenceMinHoldoutSignalFired ||
		m.HoldoutTargetOpportunity < opportunityEvidenceMinHoldoutTargetOpportunity ||
		m.HoldoutNonOpportunity < opportunityEvidenceMinHoldoutNonOpportunity:
		out.Status = "insufficient_holdout"
		if m.HoldoutObservations < opportunityEvidenceMinHoldoutObservations {
			out.Reasons = append(out.Reasons, fmt.Sprintf("holdout observations %d < %d", m.HoldoutObservations, opportunityEvidenceMinHoldoutObservations))
		}
		if m.HoldoutSignalFired < opportunityEvidenceMinHoldoutSignalFired {
			out.Reasons = append(out.Reasons, fmt.Sprintf("holdout fired signals %d < %d", m.HoldoutSignalFired, opportunityEvidenceMinHoldoutSignalFired))
		}
		if m.HoldoutTargetOpportunity < opportunityEvidenceMinHoldoutTargetOpportunity {
			out.Reasons = append(out.Reasons, fmt.Sprintf("holdout target opportunities %d < %d", m.HoldoutTargetOpportunity, opportunityEvidenceMinHoldoutTargetOpportunity))
		}
		if m.HoldoutNonOpportunity < opportunityEvidenceMinHoldoutNonOpportunity {
			out.Reasons = append(out.Reasons, fmt.Sprintf("holdout non-opportunity controls %d < %d", m.HoldoutNonOpportunity, opportunityEvidenceMinHoldoutNonOpportunity))
		}
	case m.HoldoutDistinctSignalInstruments < opportunityEvidenceMinHoldoutSignalInstruments:
		out.Status = "concentrated_holdout"
		out.Reasons = append(out.Reasons, fmt.Sprintf("holdout distinct fired instruments %d < %d", m.HoldoutDistinctSignalInstruments, opportunityEvidenceMinHoldoutSignalInstruments))
	case m.HoldoutMaxSignalInstrumentShare != nil && *m.HoldoutMaxSignalInstrumentShare > opportunityEvidenceMaxHoldoutInstrumentShare:
		out.Status = "concentrated_holdout"
		out.Reasons = append(out.Reasons, fmt.Sprintf("holdout largest fired instrument %s is %d/%d (%s), above max %s",
			m.HoldoutMaxSignalInstrument,
			m.HoldoutMaxSignalInstrumentFired,
			m.HoldoutSignalFired,
			formatBacktestRate(m.HoldoutMaxSignalInstrumentShare),
			formatBacktestRateValue(opportunityEvidenceMaxHoldoutInstrumentShare)))
	case m.HoldoutDistinctSignalClusters < opportunityEvidenceMinHoldoutSignalClusters:
		out.Status = "concentrated_holdout"
		out.Reasons = append(out.Reasons, fmt.Sprintf("holdout distinct fired clusters %d < %d", m.HoldoutDistinctSignalClusters, opportunityEvidenceMinHoldoutSignalClusters))
	case m.HoldoutMaxSignalClusterShare != nil && *m.HoldoutMaxSignalClusterShare > opportunityEvidenceMaxHoldoutClusterShare:
		out.Status = "concentrated_holdout"
		out.Reasons = append(out.Reasons, fmt.Sprintf("holdout largest fired cluster %s is %d/%d (%s), above max %s",
			m.HoldoutMaxSignalCluster,
			m.HoldoutMaxSignalClusterFired,
			m.HoldoutSignalFired,
			formatBacktestRate(m.HoldoutMaxSignalClusterShare),
			formatBacktestRateValue(opportunityEvidenceMaxHoldoutClusterShare)))
	case m.HoldoutNetExcessHitRate == nil || *m.HoldoutNetExcessHitRate <= 0.5:
		out.Status = "weak_holdout"
		out.Reasons = append(out.Reasons, "holdout net excess hit rate is not above 50% after costs")
	case m.HoldoutAvgNetExcessReturnPct == nil || *m.HoldoutAvgNetExcessReturnPct <= 0:
		out.Status = "weak_holdout"
		out.Reasons = append(out.Reasons, "holdout average net excess return is not positive after costs")
	case m.HoldoutNetExcessHitRateLower95 == nil || *m.HoldoutNetExcessHitRateLower95 <= 0.5:
		out.Status = "weak_holdout"
		out.Reasons = append(out.Reasons, fmt.Sprintf("holdout net excess hit-rate 95%% lower bound %s is not above 50%%", formatBacktestRate(m.HoldoutNetExcessHitRateLower95)))
	case m.HoldoutAvgNetExcessReturnLower95Pct == nil || *m.HoldoutAvgNetExcessReturnLower95Pct <= 0:
		out.Status = "weak_holdout"
		out.Reasons = append(out.Reasons, fmt.Sprintf("holdout average net excess 95%% lower bound %s is not positive", formatBacktestPercent(m.HoldoutAvgNetExcessReturnLower95Pct)))
	case m.HoldoutCostedCandidates == 0:
		out.Status = "missing_holdout_candidate_baseline"
		out.Reasons = append(out.Reasons, "holdout candidate-universe baseline is unavailable; no holdout candidate rows have round-trip cost assumptions")
	case m.HoldoutCostedCandidates < m.HoldoutObservations:
		out.Status = "missing_holdout_candidate_costs"
		out.Reasons = append(out.Reasons, fmt.Sprintf("missing round-trip cost for %d/%d holdout candidate rows; holdout candidate-universe baseline is incomplete", m.HoldoutObservations-m.HoldoutCostedCandidates, m.HoldoutObservations))
	case m.HoldoutFiredVsCandidateAvgLiftPct == nil || *m.HoldoutFiredVsCandidateAvgLiftPct <= 0:
		out.Status = "weak_holdout_baseline"
		out.Reasons = append(out.Reasons, fmt.Sprintf("holdout fired average net excess does not beat holdout all-candidate baseline after costs; lift %s", formatBacktestPercent(m.HoldoutFiredVsCandidateAvgLiftPct)))
	case m.HoldoutNonFiredCostedCandidates == 0:
		out.Status = "weak_holdout_baseline"
		out.Reasons = append(out.Reasons, "no costed holdout non-fired candidate rows were present; holdout selection lift cannot be measured")
	case m.HoldoutFiredVsNonFiredAvgLiftPct == nil || *m.HoldoutFiredVsNonFiredAvgLiftPct <= 0:
		out.Status = "weak_holdout_baseline"
		out.Reasons = append(out.Reasons, fmt.Sprintf("holdout fired average net excess does not beat holdout non-fired candidate baseline after costs; lift %s", formatBacktestPercent(m.HoldoutFiredVsNonFiredAvgLiftPct)))
	case sim == nil || strings.TrimSpace(sim.Model) == "":
		out.Status = "missing_portfolio"
		out.Reasons = append(out.Reasons, "portfolio simulation is required before reading the evidence gate")
	case sim.FilledSignals < opportunityEvidenceMinSignalFired:
		out.Status = "insufficient_portfolio"
		out.Reasons = append(out.Reasons, fmt.Sprintf("portfolio simulation filled %d/%d fired signals; need at least %d filled trades after capacity rules", sim.FilledSignals, sim.Signals, opportunityEvidenceMinSignalFired))
	case sim.PortfolioReturnPct == nil || *sim.PortfolioReturnPct <= 0:
		out.Status = "weak_portfolio"
		out.Reasons = append(out.Reasons, "portfolio simulation return is not positive after costs")
	case sim.ExcessReturnPct == nil || *sim.ExcessReturnPct <= 0:
		out.Status = "weak_portfolio"
		out.Reasons = append(out.Reasons, "portfolio simulation did not beat the benchmark after costs")
	case sim.MarkToMarket == nil || strings.TrimSpace(sim.MarkToMarket.Model) == "":
		out.Status = "missing_mtm"
		out.Reasons = append(out.Reasons, "mark-to-market bar simulation is required before reading the alpha evidence gate")
	case opportunityMTMSourceQuality(sim.MarkToMarket) != "ok":
		out.Status = "untrusted_bars"
		out.Reasons = append(out.Reasons, opportunityMTMSourceQualityReason("mark-to-market", sim.MarkToMarket))
	case sim.MarkToMarket.PortfolioReturnPct == nil || *sim.MarkToMarket.PortfolioReturnPct <= 0:
		out.Status = "weak_mtm"
		out.Reasons = append(out.Reasons, "mark-to-market portfolio return is not positive after costs")
	case sim.MarkToMarket.ExcessReturnPct == nil || *sim.MarkToMarket.ExcessReturnPct <= 0:
		out.Status = "weak_mtm"
		out.Reasons = append(out.Reasons, "mark-to-market portfolio did not beat the benchmark after costs")
	case sim.MarkToMarket.MaxDrawdownPct == nil:
		out.Status = "weak_mtm"
		out.Reasons = append(out.Reasons, "mark-to-market max drawdown is unavailable")
	case *sim.MarkToMarket.MaxDrawdownPct < opportunityEvidenceMaxMTMDrawdownPct:
		out.Status = "weak_mtm"
		out.Reasons = append(out.Reasons, fmt.Sprintf("mark-to-market max drawdown %s is worse than limit %s", formatBacktestPercent(sim.MarkToMarket.MaxDrawdownPct), formatBacktestPercentValue(opportunityEvidenceMaxMTMDrawdownPct)))
	case sim.MarkToMarket.MaxTradeMarkGapDays > opportunityEvidenceMaxMTMGapDays:
		out.Status = "weak_mtm"
		out.Reasons = append(out.Reasons, fmt.Sprintf("mark-to-market bar coverage max gap %dd exceeds %dd; use denser daily bars before reading the alpha evidence gate", sim.MarkToMarket.MaxTradeMarkGapDays, opportunityEvidenceMaxMTMGapDays))
	case opportunityMTMExcessToDrawdown(sim.MarkToMarket) < opportunityEvidenceMinMTMExcessToDrawdown:
		out.Status = "weak_mtm"
		out.Reasons = append(out.Reasons, fmt.Sprintf("mark-to-market excess/drawdown %.2f is below %.2f", opportunityMTMExcessToDrawdown(sim.MarkToMarket), opportunityEvidenceMinMTMExcessToDrawdown))
	case sim.Holdout == nil || strings.TrimSpace(sim.Holdout.Model) == "":
		out.Status = "missing_holdout_portfolio"
		out.Reasons = append(out.Reasons, "holdout-only portfolio simulation is required before reading the evidence gate")
	case sim.Holdout.FilledSignals < opportunityEvidenceMinHoldoutSignalFired:
		out.Status = "insufficient_holdout_portfolio"
		out.Reasons = append(out.Reasons, fmt.Sprintf("holdout portfolio simulation filled %d/%d fired signals; need at least %d filled holdout trades after capacity rules", sim.Holdout.FilledSignals, sim.Holdout.Signals, opportunityEvidenceMinHoldoutSignalFired))
	case sim.Holdout.PortfolioReturnPct == nil || *sim.Holdout.PortfolioReturnPct <= 0:
		out.Status = "weak_holdout_portfolio"
		out.Reasons = append(out.Reasons, "holdout-only portfolio simulation return is not positive after costs")
	case sim.Holdout.ExcessReturnPct == nil || *sim.Holdout.ExcessReturnPct <= 0:
		out.Status = "weak_holdout_portfolio"
		out.Reasons = append(out.Reasons, "holdout-only portfolio simulation did not beat the benchmark after costs")
	case sim.Holdout.MarkToMarket == nil || strings.TrimSpace(sim.Holdout.MarkToMarket.Model) == "":
		out.Status = "missing_holdout_mtm"
		out.Reasons = append(out.Reasons, "holdout-only mark-to-market bar simulation is required before reading the alpha evidence gate")
	case opportunityMTMSourceQuality(sim.Holdout.MarkToMarket) != "ok":
		out.Status = "untrusted_holdout_bars"
		out.Reasons = append(out.Reasons, opportunityMTMSourceQualityReason("holdout-only mark-to-market", sim.Holdout.MarkToMarket))
	case sim.Holdout.MarkToMarket.PortfolioReturnPct == nil || *sim.Holdout.MarkToMarket.PortfolioReturnPct <= 0:
		out.Status = "weak_holdout_mtm"
		out.Reasons = append(out.Reasons, "holdout-only mark-to-market portfolio return is not positive after costs")
	case sim.Holdout.MarkToMarket.ExcessReturnPct == nil || *sim.Holdout.MarkToMarket.ExcessReturnPct <= 0:
		out.Status = "weak_holdout_mtm"
		out.Reasons = append(out.Reasons, "holdout-only mark-to-market portfolio did not beat the benchmark after costs")
	case sim.Holdout.MarkToMarket.MaxDrawdownPct == nil:
		out.Status = "weak_holdout_mtm"
		out.Reasons = append(out.Reasons, "holdout-only mark-to-market max drawdown is unavailable")
	case *sim.Holdout.MarkToMarket.MaxDrawdownPct < opportunityEvidenceMaxMTMDrawdownPct:
		out.Status = "weak_holdout_mtm"
		out.Reasons = append(out.Reasons, fmt.Sprintf("holdout-only mark-to-market max drawdown %s is worse than limit %s", formatBacktestPercent(sim.Holdout.MarkToMarket.MaxDrawdownPct), formatBacktestPercentValue(opportunityEvidenceMaxMTMDrawdownPct)))
	case sim.Holdout.MarkToMarket.MaxTradeMarkGapDays > opportunityEvidenceMaxMTMGapDays:
		out.Status = "weak_holdout_mtm"
		out.Reasons = append(out.Reasons, fmt.Sprintf("holdout-only mark-to-market bar coverage max gap %dd exceeds %dd; use denser daily bars before reading the alpha evidence gate", sim.Holdout.MarkToMarket.MaxTradeMarkGapDays, opportunityEvidenceMaxMTMGapDays))
	case opportunityMTMExcessToDrawdown(sim.Holdout.MarkToMarket) < opportunityEvidenceMinMTMExcessToDrawdown:
		out.Status = "weak_holdout_mtm"
		out.Reasons = append(out.Reasons, fmt.Sprintf("holdout-only mark-to-market excess/drawdown %.2f is below %.2f", opportunityMTMExcessToDrawdown(sim.Holdout.MarkToMarket), opportunityEvidenceMinMTMExcessToDrawdown))
	default:
		out.Status = "promising_diagnostic"
		out.Reasons = append(out.Reasons, "sample clears minimum size, labelled opportunity/control, concentration, aggregate lower-bound, candidate-baseline lift, holdout, holdout-baseline lift, holdout-only portfolio, portfolio capacity, and mark-to-market risk gates after costs")
	}
	return out
}

func opportunityMTMSourceQuality(mtm *OpportunityMarkToMarketSimulation) string {
	if mtm == nil {
		return ""
	}
	quality := strings.TrimSpace(mtm.SourceQuality)
	if quality == "" {
		return "missing_source"
	}
	if quality != "ok" {
		return quality
	}
	if !opportunityIsSHA256Checksum(mtm.SourceChecksum) {
		return "missing_checksum"
	}
	if strings.TrimSpace(mtm.SourceManifest) == "" || !opportunityIsSHA256Checksum(mtm.SourceManifestChecksum) {
		return "missing_manifest"
	}
	if strings.TrimSpace(mtm.PriceBasis) != "adjusted_close" {
		return "invalid_price_basis"
	}
	if strings.TrimSpace(mtm.PriceSource) == "" || len(mtm.BarSources) == 0 {
		return "missing_source"
	}
	for _, source := range mtm.BarSources {
		switch opportunityPriceBarSourceClass(source) {
		case "trusted":
			continue
		case "missing":
			return "missing_source"
		case "fixture":
			return "fixture_source"
		default:
			return "untrusted_source"
		}
	}
	return quality
}

func opportunityMTMSourceQualityReason(label string, mtm *OpportunityMarkToMarketSimulation) string {
	quality := opportunityMTMSourceQuality(mtm)
	var suffix string
	if mtm != nil && len(mtm.SourceWarnings) > 0 {
		suffix = ": " + strings.Join(mtm.SourceWarnings, "; ")
	}
	switch quality {
	case "missing_source":
		return fmt.Sprintf("%s bars are missing source provenance%s", label, suffix)
	case "fixture_source":
		return fmt.Sprintf("%s bars use fixture/test source provenance%s", label, suffix)
	case "untrusted_source":
		return fmt.Sprintf("%s bars use unrecognized source provenance%s", label, suffix)
	case "unattested_source":
		return fmt.Sprintf("%s bars use trusted-looking source rows without a matching bars manifest%s", label, suffix)
	case "missing_checksum":
		return fmt.Sprintf("%s bars are missing a sha256 source checksum%s", label, suffix)
	case "missing_manifest":
		return fmt.Sprintf("%s bars are missing a matching source manifest checksum%s", label, suffix)
	case "invalid_price_basis":
		basis := ""
		if mtm != nil {
			basis = strings.TrimSpace(mtm.PriceBasis)
		}
		return fmt.Sprintf("%s bars use price_basis %q; expected adjusted_close%s", label, basis, suffix)
	default:
		return fmt.Sprintf("%s bars source quality %q is not alpha-grade%s", label, quality, suffix)
	}
}

func opportunityIsSHA256Checksum(checksum string) bool {
	checksum = strings.TrimSpace(checksum)
	if !strings.HasPrefix(checksum, "sha256:") || len(checksum) != len("sha256:")+64 {
		return false
	}
	for _, r := range checksum[len("sha256:"):] {
		switch {
		case r >= '0' && r <= '9':
		case r >= 'a' && r <= 'f':
		case r >= 'A' && r <= 'F':
		default:
			return false
		}
	}
	return true
}

func opportunityMTMExcessToDrawdown(mtm *OpportunityMarkToMarketSimulation) float64 {
	if mtm == nil || mtm.ExcessReturnPct == nil || mtm.MaxDrawdownPct == nil {
		return 0
	}
	drawdown := math.Abs(*mtm.MaxDrawdownPct)
	if drawdown == 0 {
		if *mtm.ExcessReturnPct > 0 {
			return math.Inf(1)
		}
		return 0
	}
	return *mtm.ExcessReturnPct / drawdown
}

func opportunityBacktestEvidenceNeeds(m OpportunityBacktestMetrics) OpportunityBacktestEvidenceNeeds {
	return OpportunityBacktestEvidenceNeeds{
		AdditionalObservations:             positiveBacktestNeed(opportunityEvidenceMinObservations, m.Observations),
		AdditionalSignalFired:              positiveBacktestNeed(opportunityEvidenceMinSignalFired, m.SignalFired),
		AdditionalTargetOpportunity:        positiveBacktestNeed(opportunityEvidenceMinTargetOpportunity, m.TargetOpportunity),
		AdditionalNonOpportunity:           positiveBacktestNeed(opportunityEvidenceMinNonOpportunity, m.NonOpportunity),
		AdditionalSignalInstruments:        positiveBacktestNeed(opportunityEvidenceMinSignalInstruments, m.DistinctSignalInstruments),
		AdditionalSignalClusters:           positiveBacktestNeed(opportunityEvidenceMinSignalClusters, m.DistinctSignalClusters),
		AdditionalHoldoutObservations:      positiveBacktestNeed(opportunityEvidenceMinHoldoutObservations, m.HoldoutObservations),
		AdditionalHoldoutSignalFired:       positiveBacktestNeed(opportunityEvidenceMinHoldoutSignalFired, m.HoldoutSignalFired),
		AdditionalHoldoutTargetOpportunity: positiveBacktestNeed(opportunityEvidenceMinHoldoutTargetOpportunity, m.HoldoutTargetOpportunity),
		AdditionalHoldoutNonOpportunity:    positiveBacktestNeed(opportunityEvidenceMinHoldoutNonOpportunity, m.HoldoutNonOpportunity),
		AdditionalHoldoutSignalInstruments: positiveBacktestNeed(opportunityEvidenceMinHoldoutSignalInstruments, m.HoldoutDistinctSignalInstruments),
		AdditionalHoldoutSignalClusters:    positiveBacktestNeed(opportunityEvidenceMinHoldoutSignalClusters, m.HoldoutDistinctSignalClusters),
		UnknownSplitObservations:           m.UnknownSplitObservations,
		MissingCostSignalFired:             m.MissingCostSignalFired,
		SignalContextBlocked:               m.SignalContextBlocked,
	}
}

func positiveBacktestNeed(minimum, observed int) int {
	if observed >= minimum {
		return 0
	}
	return minimum - observed
}

func canaryBacktestFindings(res CanaryBacktestResult) []string {
	m := res.Metrics
	if m.Observations == 0 {
		return []string{"No observations were loaded."}
	}
	var findings []string
	if m.TargetStress == 0 {
		findings = append(findings, "No target stress rows were present; add labelled forward windows before reading precision or recall.")
	} else if m.SignalMiss == 0 {
		findings = append(findings, "Watch-level canary signals caught every labelled stress row in this panel.")
	} else {
		findings = append(findings, fmt.Sprintf("Watch-level canary signals missed %d labelled stress row(s).", m.SignalMiss))
	}
	if m.SignalFalsePositive > 0 {
		findings = append(findings, fmt.Sprintf("Watch-level canary signals fired on %d non-stress row(s); inspect risk-budget false positives before tightening policy.", m.SignalFalsePositive))
	}
	if m.TargetStress == 0 {
		// Already covered above; keep the defensive-specific finding quiet when
		// there is no stress label base rate.
	} else if m.WatchMiss == 0 {
		findings = append(findings, "Portfolio-aware watch alerts caught every labelled stress row in this panel.")
	} else {
		findings = append(findings, fmt.Sprintf("Portfolio-aware watch alerts missed %d labelled stress row(s).", m.WatchMiss))
	}
	if m.WatchFalsePositive > 0 {
		findings = append(findings, fmt.Sprintf("Watch-level defensive alerts fired on %d non-stress row(s); inspect cluster false positives before tightening policy.", m.WatchFalsePositive))
	}
	if m.ActMiss > 0 && m.TargetStress > 0 {
		findings = append(findings, fmt.Sprintf("Act-level alerts caught %d/%d stress rows; treat act recall as a severity filter, not the only success gate.", m.ActTruePositive, m.TargetStress))
	}
	if res.RegimeLift.PortfolioStressRows > 0 {
		findings = append(findings, fmt.Sprintf("Portfolio lift: canary watch caught %d/%d portfolio stress row(s) vs %d/%d for regime-only.",
			res.RegimeLift.CanaryWatchTruePositive, res.RegimeLift.PortfolioStressRows,
			res.RegimeLift.RegimeOnlyWatchTruePositive, res.RegimeLift.PortfolioStressRows))
	}
	if res.Lifecycle.EarlyWarning > 0 {
		findings = append(findings, fmt.Sprintf("Early-warning lifecycle rows: %d total, %d calm/rally false episode(s), median lead %s.",
			res.Lifecycle.EarlyWarning, res.Lifecycle.EarlyWarningFalseCalmRally, formatBacktestNumber(res.Lifecycle.EarlyWarningMedianLeadDays)))
	}
	if m.DataQualityWatch > 0 {
		findings = append(findings, fmt.Sprintf("%d data-quality watch row(s) were tracked separately from defensive false positives.", m.DataQualityWatch))
	}
	if m.RebalanceWatch > 0 {
		findings = append(findings, fmt.Sprintf("%d rebalance watch row(s) were tracked separately from defensive alerts.", m.RebalanceWatch))
	}
	for _, cluster := range res.Clusters {
		cm := cluster.Metrics
		if cm.WatchMiss > 0 {
			findings = append(findings, fmt.Sprintf("%s: %d watch miss(es).", cluster.Name, cm.WatchMiss))
		}
		if cm.WatchFalsePositive > 0 {
			findings = append(findings, fmt.Sprintf("%s: %d watch false positive(s).", cluster.Name, cm.WatchFalsePositive))
		}
	}
	return findings
}

func regimeBacktestFindings(res RegimeBacktestResult) []string {
	m := res.Metrics
	if m.Observations == 0 {
		return []string{"No observations were loaded."}
	}
	var findings []string
	if m.OutOfScope > 0 {
		findings = append(findings, fmt.Sprintf("%d out-of-scope row(s) were excluded from regime precision/recall.", m.OutOfScope))
	}
	if m.TargetStress == 0 {
		findings = append(findings, "No scored market-stress rows were present; add labelled forward windows before reading precision or recall.")
	} else if m.WatchMiss == 0 {
		findings = append(findings, "Regime watch caught every scored market-stress row in this panel.")
	} else {
		findings = append(findings, fmt.Sprintf("Regime watch missed %d scored market-stress row(s).", m.WatchMiss))
	}
	if m.WatchFalsePositive > 0 {
		findings = append(findings, fmt.Sprintf("Regime watch fired on %d scored non-stress row(s); inspect yellow-cluster false positives before tuning.", m.WatchFalsePositive))
	}
	if m.TargetStress > 0 && m.StressMiss > 0 {
		findings = append(findings, fmt.Sprintf("Red-cluster stress signals caught %d/%d scored market-stress row(s).", m.StressTruePositive, m.TargetStress))
	}
	if m.StressFalsePositive > 0 {
		findings = append(findings, fmt.Sprintf("Red-cluster stress signals fired on %d scored non-stress row(s).", m.StressFalsePositive))
	}
	if res.Baseline.StressPrecision != nil && m.StressPrecision != nil {
		findings = append(findings, fmt.Sprintf("Confirmed-stress precision moved from %s before to %s after lifecycle confirmation.",
			formatBacktestRate(res.Baseline.StressPrecision), formatBacktestRate(m.StressPrecision)))
	}
	if res.Lifecycle.EarlyWarning > 0 {
		findings = append(findings, fmt.Sprintf("Early-warning lifecycle rows: %d total, %d calm/rally false episode(s), median lead %s.",
			res.Lifecycle.EarlyWarning, res.Lifecycle.EarlyWarningFalseCalmRally, formatBacktestNumber(res.Lifecycle.EarlyWarningMedianLeadDays)))
	}
	if m.DataQualityWatch > 0 {
		findings = append(findings, fmt.Sprintf("%d scored data-quality watch row(s) were tracked separately from stress false positives.", m.DataQualityWatch))
	}
	for _, cluster := range res.Clusters {
		cm := cluster.Metrics
		if cm.WatchMiss > 0 {
			findings = append(findings, fmt.Sprintf("%s: %d watch miss(es).", cluster.Name, cm.WatchMiss))
		}
		if cm.WatchFalsePositive > 0 {
			findings = append(findings, fmt.Sprintf("%s: %d watch false positive(s).", cluster.Name, cm.WatchFalsePositive))
		}
	}
	return findings
}

func opportunityBacktestFindings(res OpportunityBacktestResult) []string {
	m := res.Metrics
	if m.Observations == 0 {
		return []string{"No observations were loaded."}
	}
	var findings []string
	if res.Evidence.Status != "" {
		reason := strings.Join(res.Evidence.Reasons, "; ")
		if reason == "" {
			reason = "no reason recorded"
		}
		findings = append(findings, fmt.Sprintf("Evidence status %s: %s.", res.Evidence.Status, reason))
	}
	if opportunityBacktestEvidenceNeedsWork(res.Evidence.Needs) {
		findings = append(findings, fmt.Sprintf("Evidence needed before alpha claims: +%d observations, +%d fired signals, +%d opportunity labels, +%d control labels; holdout needs +%d observations and +%d fired signals.",
			res.Evidence.Needs.AdditionalObservations,
			res.Evidence.Needs.AdditionalSignalFired,
			res.Evidence.Needs.AdditionalTargetOpportunity,
			res.Evidence.Needs.AdditionalNonOpportunity,
			res.Evidence.Needs.AdditionalHoldoutObservations,
			res.Evidence.Needs.AdditionalHoldoutSignalFired))
	}
	if m.SignalContextBlocked > 0 {
		findings = append(findings, fmt.Sprintf("%d observation(s) had stale, missing, or non-live signal context and are blocking alpha evidence.", m.SignalContextBlocked))
	}
	if m.TargetOpportunity == 0 {
		findings = append(findings, "No target opportunity rows were present; add labelled windows before reading precision or recall.")
	} else if m.Miss == 0 {
		findings = append(findings, "Opportunity signals caught every labelled opportunity row in this panel.")
	} else {
		findings = append(findings, fmt.Sprintf("Opportunity signals missed %d labelled opportunity row(s).", m.Miss))
	}
	if m.FalsePositive > 0 {
		findings = append(findings, fmt.Sprintf("Opportunity signals fired on %d non-opportunity row(s); inspect chase-risk before tuning.", m.FalsePositive))
	}
	if m.SignalFired > 0 {
		findings = append(findings, fmt.Sprintf("%d/%d fired signal row(s) had positive excess return versus benchmark.", m.PositiveExcess, m.SignalFired))
	}
	if m.MedianExcessReturnPct != nil && m.WorstExcessReturnPct != nil && m.BestExcessReturnPct != nil {
		findings = append(findings, fmt.Sprintf("Fired-signal excess distribution: median %s, worst %s, best %s.",
			formatBacktestPercentValue(*m.MedianExcessReturnPct),
			formatBacktestPercentValue(*m.WorstExcessReturnPct),
			formatBacktestPercentValue(*m.BestExcessReturnPct)))
	}
	if m.MissingCostSignalFired > 0 {
		findings = append(findings, fmt.Sprintf("%d/%d fired signal row(s) are missing round-trip cost assumptions; net alpha metrics are incomplete.", m.MissingCostSignalFired, m.SignalFired))
	}
	if m.MedianNetExcessReturnPct != nil && m.WorstNetExcessReturnPct != nil && m.BestNetExcessReturnPct != nil {
		findings = append(findings, fmt.Sprintf("Cost-adjusted net excess distribution: median %s, worst %s, best %s.",
			formatBacktestPercentValue(*m.MedianNetExcessReturnPct),
			formatBacktestPercentValue(*m.WorstNetExcessReturnPct),
			formatBacktestPercentValue(*m.BestNetExcessReturnPct)))
	}
	if m.CostedCandidates > 0 {
		findings = append(findings, fmt.Sprintf("Candidate-universe baseline after costs: all candidates %d/%d costed, hit %s, avg %s, median %s; fired lift vs all avg %s, median %s.",
			m.CostedCandidates,
			m.Observations,
			formatBacktestRate(m.CandidateNetExcessHitRate),
			formatBacktestPercent(m.AvgCandidateNetExcessPct),
			formatBacktestPercent(m.MedianCandidateNetExcessPct),
			formatBacktestPercent(m.FiredVsCandidateAvgLiftPct),
			formatBacktestPercent(m.FiredVsCandidateMedianLiftPct)))
	}
	if m.NonFiredCostedCandidates > 0 {
		findings = append(findings, fmt.Sprintf("Selection lift after costs: non-fired candidates %d costed, avg %s, median %s; fired lift vs non-fired avg %s, median %s.",
			m.NonFiredCostedCandidates,
			formatBacktestPercent(m.AvgNonFiredCandidateNetPct),
			formatBacktestPercent(m.MedianNonFiredCandidateNetPct),
			formatBacktestPercent(m.FiredVsNonFiredAvgLiftPct),
			formatBacktestPercent(m.FiredVsNonFiredMedianLiftPct)))
	}
	if m.HoldoutCostedCandidates > 0 {
		findings = append(findings, fmt.Sprintf("Holdout candidate baseline after costs: all candidates %d/%d costed, avg %s, median %s; fired lift vs all avg %s, median %s.",
			m.HoldoutCostedCandidates,
			m.HoldoutObservations,
			formatBacktestPercent(m.HoldoutAvgCandidateNetExcessPct),
			formatBacktestPercent(m.HoldoutMedianCandidateNetExcessPct),
			formatBacktestPercent(m.HoldoutFiredVsCandidateAvgLiftPct),
			formatBacktestPercent(m.HoldoutFiredVsCandidateMedianLiftPct)))
	}
	if m.SignalFired > 0 {
		findings = append(findings, fmt.Sprintf("Fired-signal concentration: %d instrument(s), max %s %d/%d (%s); %d cluster(s), max %s %d/%d (%s).",
			m.DistinctSignalInstruments,
			emptyDash(m.MaxSignalInstrument),
			m.MaxSignalInstrumentFired,
			m.SignalFired,
			formatBacktestRate(m.MaxSignalInstrumentShare),
			m.DistinctSignalClusters,
			emptyDash(m.MaxSignalCluster),
			m.MaxSignalClusterFired,
			m.SignalFired,
			formatBacktestRate(m.MaxSignalClusterShare)))
	}
	findings = append(findings, fmt.Sprintf("Validation split: %d holdout row(s), %d tuning row(s), %d unknown split row(s); holdout fired %d/%d.",
		m.HoldoutObservations,
		m.TuningObservations,
		m.UnknownSplitObservations,
		m.HoldoutSignalFired,
		m.SignalFired))
	if m.HoldoutSignalFired > 0 {
		findings = append(findings, fmt.Sprintf("Holdout concentration: %d instrument(s), max %s %d/%d (%s); %d cluster(s), max %s %d/%d (%s).",
			m.HoldoutDistinctSignalInstruments,
			emptyDash(m.HoldoutMaxSignalInstrument),
			m.HoldoutMaxSignalInstrumentFired,
			m.HoldoutSignalFired,
			formatBacktestRate(m.HoldoutMaxSignalInstrumentShare),
			m.HoldoutDistinctSignalClusters,
			emptyDash(m.HoldoutMaxSignalCluster),
			m.HoldoutMaxSignalClusterFired,
			m.HoldoutSignalFired,
			formatBacktestRate(m.HoldoutMaxSignalClusterShare)))
	}
	if m.ExcessHitRateLower95 != nil || m.AvgExcessReturnLower95Pct != nil || m.NetExcessHitRateLower95 != nil || m.AvgNetExcessReturnLower95Pct != nil {
		findings = append(findings, fmt.Sprintf("95%% lower-bound check: gross hit %s, avg gross %s, net hit %s, avg net %s.",
			formatBacktestRate(m.ExcessHitRateLower95),
			formatBacktestPercent(m.AvgExcessReturnLower95Pct),
			formatBacktestRate(m.NetExcessHitRateLower95),
			formatBacktestPercent(m.AvgNetExcessReturnLower95Pct)))
	}
	if m.AvgMaxAdverseExcursionPct != nil {
		findings = append(findings, fmt.Sprintf("Average fired-signal adverse excursion was %s before the horizon exit.", formatBacktestPercentValue(*m.AvgMaxAdverseExcursionPct)))
	}
	for _, cluster := range res.Clusters {
		cm := cluster.Metrics
		if cm.Miss > 0 {
			findings = append(findings, fmt.Sprintf("%s: %d missed opportunity row(s).", cluster.Name, cm.Miss))
		}
		if cm.FalsePositive > 0 {
			findings = append(findings, fmt.Sprintf("%s: %d false opportunity row(s).", cluster.Name, cm.FalsePositive))
		}
	}
	return findings
}

func opportunityBacktestEvidenceNeedsWork(n OpportunityBacktestEvidenceNeeds) bool {
	return n.AdditionalObservations > 0 ||
		n.AdditionalSignalFired > 0 ||
		n.AdditionalTargetOpportunity > 0 ||
		n.AdditionalNonOpportunity > 0 ||
		n.AdditionalSignalInstruments > 0 ||
		n.AdditionalSignalClusters > 0 ||
		n.AdditionalHoldoutObservations > 0 ||
		n.AdditionalHoldoutSignalFired > 0 ||
		n.AdditionalHoldoutTargetOpportunity > 0 ||
		n.AdditionalHoldoutNonOpportunity > 0 ||
		n.AdditionalHoldoutSignalInstruments > 0 ||
		n.AdditionalHoldoutSignalClusters > 0 ||
		n.UnknownSplitObservations > 0 ||
		n.MissingCostSignalFired > 0 ||
		n.SignalContextBlocked > 0
}

func renderCanaryBacktestText(env *Env, out io.Writer, r *CanaryBacktestResult) int {
	fmt.Fprintln(out)
	fmt.Fprintf(out, "Canary Backtest  ·  %d observations  ·  policy %s\n", r.Metrics.Observations, r.Policy)
	fmt.Fprintln(out)
	fmt.Fprintf(out, "  %-12s %d stress / %d non-stress\n", "Targets", r.Metrics.TargetStress, r.Metrics.NonStress)
	fmt.Fprintf(out, "  %-12s precision %s · recall %s · false alarms %s · avg lead %s\n",
		"Signal",
		formatBacktestRate(r.Metrics.SignalPrecision),
		formatBacktestRate(r.Metrics.SignalRecall),
		formatBacktestRate(r.Metrics.SignalFalseAlarmRate),
		formatBacktestNumber(r.Metrics.SignalAvgLeadDays),
	)
	fmt.Fprintf(out, "  %-12s precision %s · recall %s · false alarms %s · avg lead %s\n",
		"Watch",
		formatBacktestRate(r.Metrics.WatchPrecision),
		formatBacktestRate(r.Metrics.WatchRecall),
		formatBacktestRate(r.Metrics.WatchFalseAlarmRate),
		formatBacktestNumber(r.Metrics.WatchAvgLeadDays),
	)
	fmt.Fprintf(out, "  %-12s precision %s · recall %s · false alarms %s · avg lead %s\n",
		"Act",
		formatBacktestRate(r.Metrics.ActPrecision),
		formatBacktestRate(r.Metrics.ActRecall),
		formatBacktestRate(r.Metrics.ActFalseAlarmRate),
		formatBacktestNumber(r.Metrics.ActAvgLeadDays),
	)
	fmt.Fprintf(out, "  %-12s precision %s · recall %s · false calm/rally %d · median lead %s\n",
		"Early",
		formatBacktestRate(r.Lifecycle.EarlyWarningPrecision),
		formatBacktestRate(r.Lifecycle.EarlyWarningRecall),
		r.Lifecycle.EarlyWarningFalseCalmRally,
		formatBacktestNumber(r.Lifecycle.EarlyWarningMedianLeadDays),
	)
	fmt.Fprintf(out, "  %-12s precision %s · recall %s · panic/forced recall %s · false starts %d\n",
		"Lifecycle",
		formatBacktestRate(r.Lifecycle.ConfirmedStressPrecision),
		formatBacktestRate(r.Lifecycle.ConfirmedStressRecall),
		formatBacktestRate(r.Lifecycle.PanicRecall),
		r.Lifecycle.StabilizationOpportunityFalseStarts,
	)
	fmt.Fprintf(out, "  %-12s watch precision %s · recall %s · confirmed precision %s · recall %s\n",
		"Events",
		formatBacktestRate(r.Events.WatchPrecision),
		formatBacktestRate(r.Events.WatchRecall),
		formatBacktestRate(r.Events.ConfirmedStressPrecision),
		formatBacktestRate(r.Events.ConfirmedStressRecall),
	)
	fmt.Fprintf(out, "  %-12s regime-only recall %s · canary recall %s · added portfolio TP %d\n",
		"Lift",
		formatBacktestRate(r.RegimeLift.RegimeOnlyRecall),
		formatBacktestRate(r.RegimeLift.CanaryRecall),
		r.RegimeLift.CanaryAddedTruePositive,
	)
	fmt.Fprintf(out, "  %-12s %d rebalance watch row(s)\n", "Risk budget", r.Metrics.RebalanceWatch)
	fmt.Fprintf(out, "  %-12s %d data-quality watch · %d blocked planner row(s)\n", "Quality", r.Metrics.DataQualityWatch, r.Metrics.Blocked)
	fmt.Fprintln(out)

	if len(r.Clusters) > 0 {
		header := fmt.Sprintf("  %-28s %4s %6s %6s %6s %6s %6s %7s",
			"CLUSTER", "OBS", "STRESS", "WAT TP", "DEF FP", "REBAL", "ACT TP", "BLOCKED")
		fmt.Fprintln(out, env.dim(header))
		fmt.Fprintln(out, env.dim(strings.Repeat("-", visibleLen(header))))
		for _, cluster := range r.Clusters {
			m := cluster.Metrics
			fmt.Fprintf(out, "  %-28s %4d %6d %6d %6d %6d %6d %7d\n",
				truncateVisible(cluster.Name, 28),
				m.Observations,
				m.TargetStress,
				m.WatchTruePositive,
				m.WatchFalsePositive,
				m.RebalanceWatch,
				m.ActTruePositive,
				m.Blocked,
			)
		}
		fmt.Fprintln(out)
	}
	if len(r.Categories) > 0 {
		header := fmt.Sprintf("  %-28s %4s %6s %6s %6s %6s %6s %7s",
			"CATEGORY", "OBS", "STRESS", "WAT TP", "DEF FP", "REBAL", "ACT TP", "BLOCKED")
		fmt.Fprintln(out, env.dim(header))
		fmt.Fprintln(out, env.dim(strings.Repeat("-", visibleLen(header))))
		for _, category := range r.Categories {
			m := category.Metrics
			fmt.Fprintf(out, "  %-28s %4d %6d %6d %6d %6d %6d %7d\n",
				truncateVisible(category.Name, 28),
				m.Observations,
				m.TargetStress,
				m.WatchTruePositive,
				m.WatchFalsePositive,
				m.RebalanceWatch,
				m.ActTruePositive,
				m.Blocked,
			)
		}
		fmt.Fprintln(out)
	}
	if len(r.Findings) > 0 {
		fmt.Fprintln(out, "Findings")
		for _, finding := range r.Findings {
			fmt.Fprintf(out, "  - %s\n", finding)
		}
		fmt.Fprintln(out)
	}
	if r.NotAdvice != "" {
		fmt.Fprintln(out, env.dim("  "+r.NotAdvice))
		fmt.Fprintln(out)
	}
	return 0
}

func renderRegimeBacktestText(env *Env, out io.Writer, r *RegimeBacktestResult) int {
	fmt.Fprintln(out)
	fmt.Fprintf(out, "Regime Backtest  ·  %d observations  ·  policy %s\n", r.Metrics.Observations, r.Policy)
	fmt.Fprintln(out)
	fmt.Fprintf(out, "  %-12s %d stress / %d non-stress / %d out-of-scope\n", "Targets", r.Metrics.TargetStress, r.Metrics.NonStress, r.Metrics.OutOfScope)
	fmt.Fprintf(out, "  %-12s precision %s · recall %s · false alarms %s · avg lead %s\n",
		"Watch",
		formatBacktestRate(r.Metrics.WatchPrecision),
		formatBacktestRate(r.Metrics.WatchRecall),
		formatBacktestRate(r.Metrics.WatchFalseAlarmRate),
		formatBacktestNumber(r.Metrics.WatchAvgLeadDays),
	)
	fmt.Fprintf(out, "  %-12s precision %s · recall %s · false alarms %s · avg lead %s\n",
		"Stress",
		formatBacktestRate(r.Metrics.StressPrecision),
		formatBacktestRate(r.Metrics.StressRecall),
		formatBacktestRate(r.Metrics.StressFalseAlarmRate),
		formatBacktestNumber(r.Metrics.StressAvgLeadDays),
	)
	fmt.Fprintf(out, "  %-12s stress precision %s · recall %s · false alarms %s\n",
		"Before",
		formatBacktestRate(r.Baseline.StressPrecision),
		formatBacktestRate(r.Baseline.StressRecall),
		formatBacktestRate(r.Baseline.StressFalseAlarmRate),
	)
	fmt.Fprintf(out, "  %-12s precision %s · recall %s · false calm/rally %d · median lead %s\n",
		"Early",
		formatBacktestRate(r.Lifecycle.EarlyWarningPrecision),
		formatBacktestRate(r.Lifecycle.EarlyWarningRecall),
		r.Lifecycle.EarlyWarningFalseCalmRally,
		formatBacktestNumber(r.Lifecycle.EarlyWarningMedianLeadDays),
	)
	fmt.Fprintf(out, "  %-12s panic recall %s · stabilization/opportunity false starts %d\n",
		"Lifecycle",
		formatBacktestRate(r.Lifecycle.PanicRecall),
		r.Lifecycle.StabilizationOpportunityFalseStarts,
	)
	fmt.Fprintf(out, "  %-12s watch precision %s · recall %s · confirmed precision %s · recall %s\n",
		"Events",
		formatBacktestRate(r.Events.WatchPrecision),
		formatBacktestRate(r.Events.WatchRecall),
		formatBacktestRate(r.Events.ConfirmedStressPrecision),
		formatBacktestRate(r.Events.ConfirmedStressRecall),
	)
	fmt.Fprintf(out, "  %-12s %d data-quality watch row(s)\n", "Quality", r.Metrics.DataQualityWatch)
	fmt.Fprintln(out)

	if len(r.Clusters) > 0 {
		header := fmt.Sprintf("  %-28s %4s %6s %6s %6s %6s %6s %3s %3s",
			"CLUSTER", "OBS", "SCORED", "STRESS", "WAT TP", "WAT FP", "STR TP", "DQ", "OOS")
		fmt.Fprintln(out, env.dim(header))
		fmt.Fprintln(out, env.dim(strings.Repeat("-", visibleLen(header))))
		for _, cluster := range r.Clusters {
			m := cluster.Metrics
			fmt.Fprintf(out, "  %-28s %4d %6d %6d %6d %6d %6d %3d %3d\n",
				truncateVisible(cluster.Name, 28),
				m.Observations,
				m.ScoredObservations,
				m.TargetStress,
				m.WatchTruePositive,
				m.WatchFalsePositive,
				m.StressTruePositive,
				m.DataQualityWatch,
				m.OutOfScope,
			)
		}
		fmt.Fprintln(out)
	}
	if len(r.Findings) > 0 {
		fmt.Fprintln(out, "Findings")
		for _, finding := range r.Findings {
			fmt.Fprintf(out, "  - %s\n", finding)
		}
		fmt.Fprintln(out)
	}
	if r.NotAdvice != "" {
		fmt.Fprintln(out, env.dim("  "+r.NotAdvice))
		fmt.Fprintln(out)
	}
	return 0
}

func renderOpportunityBacktestText(env *Env, out io.Writer, r *OpportunityBacktestResult) int {
	fmt.Fprintln(out)
	fmt.Fprintf(out, "Opportunity Backtest  ·  %d observations  ·  policy %s\n", r.Metrics.Observations, r.Policy)
	fmt.Fprintln(out)
	fmt.Fprintf(out, "  %-12s %d opportunity / %d non-opportunity\n", "Targets", r.Metrics.TargetOpportunity, r.Metrics.NonOpportunity)
	if r.Evidence.Status != "" {
		fmt.Fprintf(out, "  %-12s %s · min %d obs / %d fires / %d opp / %d controls\n",
			"Evidence",
			r.Evidence.Status,
			r.Evidence.MinObservations,
			r.Evidence.MinSignalFired,
			r.Evidence.MinTargetOpportunity,
			r.Evidence.MinNonOpportunity,
		)
		fmt.Fprintf(out, "  %-12s %s\n", "Verdict", opportunityBacktestEvidenceVerdict(r.Evidence.Status))
	}
	fmt.Fprintf(out, "  %-12s %d instruments (min %d) · max %s %s (limit %s) · %d clusters (min %d) · max %s %s (limit %s)\n",
		"Diversity",
		r.Metrics.DistinctSignalInstruments,
		r.Evidence.MinSignalInstruments,
		emptyDash(r.Metrics.MaxSignalInstrument),
		formatBacktestRate(r.Metrics.MaxSignalInstrumentShare),
		formatBacktestRateValue(r.Evidence.MaxSignalInstrumentShare),
		r.Metrics.DistinctSignalClusters,
		r.Evidence.MinSignalClusters,
		emptyDash(r.Metrics.MaxSignalCluster),
		formatBacktestRate(r.Metrics.MaxSignalClusterShare),
		formatBacktestRateValue(r.Evidence.MaxSignalClusterShare),
	)
	fmt.Fprintf(out, "  %-12s holdout %d obs / %d fires / %d opp / %d controls · unknown split %d\n",
		"Validation",
		r.Metrics.HoldoutObservations,
		r.Metrics.HoldoutSignalFired,
		r.Metrics.HoldoutTargetOpportunity,
		r.Metrics.HoldoutNonOpportunity,
		r.Metrics.UnknownSplitObservations,
	)
	if r.Metrics.SignalContextBlocked > 0 {
		fmt.Fprintf(out, "  %-12s %d signal-context blocked row(s) · holdout %d\n",
			"Quality",
			r.Metrics.SignalContextBlocked,
			r.Metrics.HoldoutSignalContextBlocked,
		)
	}
	fmt.Fprintf(out, "  %-12s +%d obs / +%d fires / +%d opp / +%d controls · +%d inst / +%d clusters\n",
		"Need",
		r.Evidence.Needs.AdditionalObservations,
		r.Evidence.Needs.AdditionalSignalFired,
		r.Evidence.Needs.AdditionalTargetOpportunity,
		r.Evidence.Needs.AdditionalNonOpportunity,
		r.Evidence.Needs.AdditionalSignalInstruments,
		r.Evidence.Needs.AdditionalSignalClusters,
	)
	fmt.Fprintf(out, "  %-12s +%d obs / +%d fires / +%d opp / +%d controls · +%d inst / +%d clusters\n",
		"Holdout need",
		r.Evidence.Needs.AdditionalHoldoutObservations,
		r.Evidence.Needs.AdditionalHoldoutSignalFired,
		r.Evidence.Needs.AdditionalHoldoutTargetOpportunity,
		r.Evidence.Needs.AdditionalHoldoutNonOpportunity,
		r.Evidence.Needs.AdditionalHoldoutSignalInstruments,
		r.Evidence.Needs.AdditionalHoldoutSignalClusters,
	)
	fmt.Fprintf(out, "  %-12s %d instruments (min %d) · max %s %s (limit %s) · %d clusters (min %d) · max %s %s (limit %s)\n",
		"Holdout div",
		r.Metrics.HoldoutDistinctSignalInstruments,
		r.Evidence.MinHoldoutSignalInstruments,
		emptyDash(r.Metrics.HoldoutMaxSignalInstrument),
		formatBacktestRate(r.Metrics.HoldoutMaxSignalInstrumentShare),
		formatBacktestRateValue(r.Evidence.MaxHoldoutSignalInstrumentShare),
		r.Metrics.HoldoutDistinctSignalClusters,
		r.Evidence.MinHoldoutSignalClusters,
		emptyDash(r.Metrics.HoldoutMaxSignalCluster),
		formatBacktestRate(r.Metrics.HoldoutMaxSignalClusterShare),
		formatBacktestRateValue(r.Evidence.MaxHoldoutSignalClusterShare),
	)
	fmt.Fprintf(out, "  %-12s filled %d/%d (min %d) · MTM %s · gap %s (limit %dd) · max DD %s (limit %s) · ex/DD %s (min %.2f)\n",
		"Risk gate",
		r.Simulation.FilledSignals,
		r.Simulation.Signals,
		r.Evidence.MinPortfolioFilledSignals,
		opportunityBacktestMTMPresentLabel(r.Simulation.MarkToMarket),
		opportunityBacktestMTMGapLabel(r.Simulation.MarkToMarket),
		r.Evidence.MaxMarkToMarketGapDays,
		opportunityBacktestMTMDrawdownLabel(r.Simulation.MarkToMarket),
		formatBacktestPercentValue(r.Evidence.MaxMarkToMarketDrawdownPct),
		opportunityBacktestMTMExcessDrawdownLabel(r.Simulation.MarkToMarket),
		r.Evidence.MinMarkToMarketExcessToDrawdown,
	)
	if holdout := r.Simulation.Holdout; holdout != nil {
		fmt.Fprintf(out, "  %-12s filled %d/%d (min %d) · MTM %s · gap %s (limit %dd) · max DD %s (limit %s) · ex/DD %s (min %.2f)\n",
			"Holdout risk",
			holdout.FilledSignals,
			holdout.Signals,
			r.Evidence.MinHoldoutSignalFired,
			opportunityBacktestMTMPresentLabel(holdout.MarkToMarket),
			opportunityBacktestMTMGapLabel(holdout.MarkToMarket),
			r.Evidence.MaxMarkToMarketGapDays,
			opportunityBacktestMTMDrawdownLabel(holdout.MarkToMarket),
			formatBacktestPercentValue(r.Evidence.MaxMarkToMarketDrawdownPct),
			opportunityBacktestMTMExcessDrawdownLabel(holdout.MarkToMarket),
			r.Evidence.MinMarkToMarketExcessToDrawdown,
		)
	}
	fmt.Fprintf(out, "  %-12s precision %s · recall %s · false alarms %s\n",
		"Signal",
		formatBacktestRate(r.Metrics.Precision),
		formatBacktestRate(r.Metrics.Recall),
		formatBacktestRate(r.Metrics.FalseAlarmRate),
	)
	fmt.Fprintf(out, "  %-12s hit %s · avg fwd %s · avg excess %s · median excess %s · avg adverse %s · avg favorable %s\n",
		"Outcome",
		formatBacktestRate(r.Metrics.ExcessHitRate),
		formatBacktestPercent(r.Metrics.AvgForwardReturnPct),
		formatBacktestPercent(r.Metrics.AvgExcessReturnPct),
		formatBacktestPercent(r.Metrics.MedianExcessReturnPct),
		formatBacktestPercent(r.Metrics.AvgMaxAdverseExcursionPct),
		formatBacktestPercent(r.Metrics.AvgMaxFavorableExcursionPct),
	)
	fmt.Fprintf(out, "  %-12s costed %d/%d · hit %s · avg cost %s · avg net %s · median net %s\n",
		"Net outcome",
		r.Metrics.CostedSignalFired,
		r.Metrics.SignalFired,
		formatBacktestRate(r.Metrics.NetExcessHitRate),
		formatBacktestPercent(r.Metrics.AvgExecutionCostPct),
		formatBacktestPercent(r.Metrics.AvgNetExcessReturnPct),
		formatBacktestPercent(r.Metrics.MedianNetExcessReturnPct),
	)
	fmt.Fprintf(out, "  %-12s all %d/%d · hit %s · avg %s · median %s · non-fired %d avg %s median %s\n",
		"Baseline",
		r.Metrics.CostedCandidates,
		r.Metrics.Observations,
		formatBacktestRate(r.Metrics.CandidateNetExcessHitRate),
		formatBacktestPercent(r.Metrics.AvgCandidateNetExcessPct),
		formatBacktestPercent(r.Metrics.MedianCandidateNetExcessPct),
		r.Metrics.NonFiredCostedCandidates,
		formatBacktestPercent(r.Metrics.AvgNonFiredCandidateNetPct),
		formatBacktestPercent(r.Metrics.MedianNonFiredCandidateNetPct),
	)
	fmt.Fprintf(out, "  %-12s vs all avg %s median %s · vs non-fired avg %s median %s\n",
		"Lift",
		formatBacktestPercent(r.Metrics.FiredVsCandidateAvgLiftPct),
		formatBacktestPercent(r.Metrics.FiredVsCandidateMedianLiftPct),
		formatBacktestPercent(r.Metrics.FiredVsNonFiredAvgLiftPct),
		formatBacktestPercent(r.Metrics.FiredVsNonFiredMedianLiftPct),
	)
	if len(r.Diagnostics.Features) > 0 {
		for _, bucket := range topOpportunityDiagnosticBuckets(r.Diagnostics.Features, 4) {
			m := bucket.Metrics
			fmt.Fprintf(out, "  %-12s %-24s fires %d · avg net %s · lift %s · holdout lift %s\n",
				"Feature",
				truncateVisible(bucket.Name, 24),
				m.SignalFired,
				formatBacktestPercent(m.AvgNetExcessReturnPct),
				formatBacktestPercent(m.FiredVsCandidateAvgLiftPct),
				formatBacktestPercent(m.HoldoutFiredVsCandidateAvgLiftPct),
			)
		}
	}
	if len(r.Diagnostics.Reasons) > 0 {
		for _, bucket := range topOpportunityDiagnosticBuckets(r.Diagnostics.Reasons, 4) {
			m := bucket.Metrics
			fmt.Fprintf(out, "  %-12s %-24s rows %d · class %s · avg cand %s\n",
				"Reason",
				truncateVisible(bucket.Name, 24),
				m.Observations,
				bucket.Class,
				formatBacktestPercent(m.AvgCandidateNetExcessPct),
			)
		}
	}
	if r.Metrics.HoldoutObservations > 0 {
		fmt.Fprintf(out, "  %-12s all %d/%d avg %s median %s · lift avg %s median %s\n",
			"Holdout base",
			r.Metrics.HoldoutCostedCandidates,
			r.Metrics.HoldoutObservations,
			formatBacktestPercent(r.Metrics.HoldoutAvgCandidateNetExcessPct),
			formatBacktestPercent(r.Metrics.HoldoutMedianCandidateNetExcessPct),
			formatBacktestPercent(r.Metrics.HoldoutFiredVsCandidateAvgLiftPct),
			formatBacktestPercent(r.Metrics.HoldoutFiredVsCandidateMedianLiftPct),
		)
		fmt.Fprintf(out, "  %-12s non-fired %d avg %s median %s · lift avg %s median %s\n",
			"Holdout sel",
			r.Metrics.HoldoutNonFiredCostedCandidates,
			formatBacktestPercent(r.Metrics.HoldoutAvgNonFiredCandidateNetPct),
			formatBacktestPercent(r.Metrics.HoldoutMedianNonFiredCandidateNetPct),
			formatBacktestPercent(r.Metrics.HoldoutFiredVsNonFiredAvgLiftPct),
			formatBacktestPercent(r.Metrics.HoldoutFiredVsNonFiredMedianLiftPct),
		)
	}
	fmt.Fprintf(out, "  %-12s gross hit lb95 %s · avg gross lb95 %s · net hit lb95 %s · avg net lb95 %s\n",
		"Confidence",
		formatBacktestRate(r.Metrics.ExcessHitRateLower95),
		formatBacktestPercent(r.Metrics.AvgExcessReturnLower95Pct),
		formatBacktestRate(r.Metrics.NetExcessHitRateLower95),
		formatBacktestPercent(r.Metrics.AvgNetExcessReturnLower95Pct),
	)
	if r.Simulation.Model != "" {
		fmt.Fprintf(out, "  %-12s %d/%d filled · return %s vs bench %s · excess %s · max open %d · turnover %s\n",
			"Portfolio",
			r.Simulation.FilledSignals,
			r.Simulation.Signals,
			formatBacktestPercent(r.Simulation.PortfolioReturnPct),
			formatBacktestPercent(r.Simulation.BenchmarkReturnPct),
			formatBacktestPercent(r.Simulation.ExcessReturnPct),
			r.Simulation.MaxConcurrent,
			formatBacktestPercent(r.Simulation.TurnoverPct),
		)
	}
	if mtm := r.Simulation.MarkToMarket; mtm != nil && mtm.Model != "" {
		fmt.Fprintf(out, "  %-12s %d bars · return %s vs bench %s · max DD %s vs bench %s · vol %s\n",
			"MTM",
			mtm.Bars,
			formatBacktestPercent(mtm.PortfolioReturnPct),
			formatBacktestPercent(mtm.BenchmarkReturnPct),
			formatBacktestPercent(mtm.MaxDrawdownPct),
			formatBacktestPercent(mtm.BenchmarkMaxDrawdownPct),
			formatBacktestPercent(mtm.BarReturnVolPct),
		)
		fmt.Fprintf(out, "  %-12s %s\n", "Bar source", opportunityBacktestMTMSourceLabel(mtm))
	}
	if r.Simulation.Holdout != nil {
		if mtm := r.Simulation.Holdout.MarkToMarket; mtm != nil && mtm.Model != "" {
			fmt.Fprintf(out, "  %-12s %d bars · return %s vs bench %s · max DD %s vs bench %s · vol %s\n",
				"Holdout MTM",
				mtm.Bars,
				formatBacktestPercent(mtm.PortfolioReturnPct),
				formatBacktestPercent(mtm.BenchmarkReturnPct),
				formatBacktestPercent(mtm.MaxDrawdownPct),
				formatBacktestPercent(mtm.BenchmarkMaxDrawdownPct),
				formatBacktestPercent(mtm.BarReturnVolPct),
			)
			fmt.Fprintf(out, "  %-12s %s\n", "Hold source", opportunityBacktestMTMSourceLabel(mtm))
		}
	}
	costModels := opportunityBacktestCostModelLabels(r)
	if len(costModels) > 0 {
		fmt.Fprintf(out, "  %-12s %s\n", "Cost model", strings.Join(costModels, ", "))
	}
	fmt.Fprintln(out)

	if len(r.Clusters) > 0 {
		header := fmt.Sprintf("  %-28s %4s %4s %5s %4s %4s %4s %4s %8s",
			"CLUSTER", "OBS", "OPP", "FIRED", "TP", "FP", "MISS", "HIT", "AVG EX")
		fmt.Fprintln(out, env.dim(header))
		fmt.Fprintln(out, env.dim(strings.Repeat("-", visibleLen(header))))
		for _, cluster := range r.Clusters {
			m := cluster.Metrics
			fmt.Fprintf(out, "  %-28s %4d %4d %5d %4d %4d %4d %4d %8s\n",
				truncateVisible(cluster.Name, 28),
				m.Observations,
				m.TargetOpportunity,
				m.SignalFired,
				m.TruePositive,
				m.FalsePositive,
				m.Miss,
				m.PositiveExcess,
				formatBacktestPercent(m.AvgExcessReturnPct),
			)
		}
		fmt.Fprintln(out)
	}
	if len(r.Findings) > 0 {
		fmt.Fprintln(out, "Findings")
		for _, finding := range r.Findings {
			fmt.Fprintf(out, "  - %s\n", finding)
		}
		fmt.Fprintln(out)
	}
	if r.NotAdvice != "" {
		fmt.Fprintln(out, env.dim("  "+r.NotAdvice))
		fmt.Fprintln(out)
	}
	return 0
}

func formatBacktestRate(v *float64) string {
	if v == nil || math.IsNaN(*v) {
		return "--"
	}
	return formatBacktestRateValue(*v)
}

func formatBacktestRateValue(v float64) string {
	return fmt.Sprintf("%.0f%%", v*100)
}

func formatBacktestPercent(v *float64) string {
	if v == nil || math.IsNaN(*v) {
		return "--"
	}
	return formatBacktestPercentValue(*v)
}

func formatBacktestPercentValue(v float64) string {
	return fmt.Sprintf("%+.1f%%", v)
}

func formatBacktestNumber(v *float64) string {
	if v == nil || math.IsNaN(*v) {
		return "--"
	}
	return fmt.Sprintf("%.1fd", *v)
}

func emptyDash(s string) string {
	if strings.TrimSpace(s) == "" {
		return "--"
	}
	return s
}

func opportunityBacktestEvidenceVerdict(status string) string {
	switch strings.TrimSpace(status) {
	case "promising_diagnostic":
		return "promising diagnostic only; not alpha proof without locked walk-forward/live paper evidence"
	case "":
		return "--"
	default:
		return "not alpha evidence; treat return metrics below as diagnostics until gates pass"
	}
}

func opportunityBacktestMTMPresentLabel(mtm *OpportunityMarkToMarketSimulation) string {
	if mtm == nil || strings.TrimSpace(mtm.Model) == "" {
		return "no"
	}
	return "yes"
}

func opportunityBacktestMTMGapLabel(mtm *OpportunityMarkToMarketSimulation) string {
	if mtm == nil || strings.TrimSpace(mtm.Model) == "" {
		return "--"
	}
	return fmt.Sprintf("%dd", mtm.MaxTradeMarkGapDays)
}

func opportunityBacktestMTMDrawdownLabel(mtm *OpportunityMarkToMarketSimulation) string {
	if mtm == nil {
		return "--"
	}
	return formatBacktestPercent(mtm.MaxDrawdownPct)
}

func opportunityBacktestMTMExcessDrawdownLabel(mtm *OpportunityMarkToMarketSimulation) string {
	if mtm == nil || mtm.ExcessReturnPct == nil || mtm.MaxDrawdownPct == nil {
		return "--"
	}
	ratio := opportunityMTMExcessToDrawdown(mtm)
	if math.IsInf(ratio, 1) {
		return "+Inf"
	}
	return fmt.Sprintf("%.2f", ratio)
}

func opportunityBacktestMTMSourceLabel(mtm *OpportunityMarkToMarketSimulation) string {
	if mtm == nil || strings.TrimSpace(mtm.Model) == "" {
		return "--"
	}
	quality := opportunityMTMSourceQuality(mtm)
	parts := []string{quality}
	if len(mtm.SourceWarnings) > 0 {
		parts = append(parts, strings.Join(mtm.SourceWarnings, "; "))
	} else if len(mtm.BarSources) > 0 {
		parts = append(parts, strings.Join(mtm.BarSources, ", "))
	} else if strings.TrimSpace(mtm.PriceSource) != "" {
		parts = append(parts, strings.TrimSpace(mtm.PriceSource))
	}
	if checksum := opportunityBacktestShortChecksum(mtm.SourceChecksum); checksum != "" {
		parts = append(parts, "checksum "+checksum)
	}
	if manifest := opportunityBacktestShortChecksum(mtm.SourceManifestChecksum); manifest != "" {
		parts = append(parts, "manifest "+manifest)
	}
	return strings.Join(parts, " · ")
}

func opportunityBacktestShortChecksum(checksum string) string {
	checksum = strings.TrimSpace(checksum)
	const prefix = "sha256:"
	if strings.HasPrefix(checksum, prefix) && len(checksum) >= len(prefix)+12 {
		return prefix + checksum[len(prefix):len(prefix)+12]
	}
	return checksum
}

func opportunityBacktestCostModelLabels(r *OpportunityBacktestResult) []string {
	if r == nil {
		return nil
	}
	seen := map[string]struct{}{}
	var labels []string
	for _, row := range r.Observations {
		if !row.SignalFired || row.ExecutionCostPct == nil {
			continue
		}
		label := strings.TrimSpace(row.Trade.CostModel)
		if label == "" && row.Trade.RoundTripCostBps != nil {
			label = fmt.Sprintf("round_trip_cost_bps=%.1f", *row.Trade.RoundTripCostBps)
		}
		if label == "" {
			label = "unspecified"
		}
		if _, ok := seen[label]; ok {
			continue
		}
		seen[label] = struct{}{}
		labels = append(labels, label)
	}
	slices.Sort(labels)
	return labels
}

func truncateVisible(s string, width int) string {
	if visibleLen(s) <= width {
		return s
	}
	if width <= 1 {
		return s[:0]
	}
	runes := []rune(s)
	if len(runes) <= width {
		return s
	}
	return string(runes[:width-1]) + "."
}
