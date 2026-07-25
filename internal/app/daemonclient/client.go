package daemonclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/osauer/ibkr/v2/internal/canary"
	"github.com/osauer/ibkr/v2/internal/dial"
	"github.com/osauer/ibkr/v2/internal/rpc"
)

// Client is the app host's typed daemon capability surface. Implementations
// transport requests only; callers and the daemon retain their respective
// authentication, confirmation, policy, and broker-write gates.
type Client interface {
	Status(context.Context) (*rpc.HealthResult, error)
	MarketCalendar(context.Context) (*rpc.MarketCalendarResult, error)
	MarketCalendarFor(context.Context, string) (*rpc.MarketCalendarResult, error)
	Account(context.Context) (*rpc.AccountResult, error)
	Positions(context.Context) (*rpc.PositionsResult, error)
	Quote(context.Context, rpc.ContractParams) (*rpc.Quote, error)
	StreamQuote(context.Context, rpc.ContractParams, func(rpc.Frame) error) error
	MarketEvents(context.Context, rpc.MarketEventsParams) (*rpc.MarketEventsResult, error)
	Canary(context.Context) (*rpc.StressResult, error)
	CanaryWithRegime(context.Context) (*rpc.StressResult, *rpc.RegimeMonitorResult, error)
	Rules(context.Context) (*rpc.RulesResult, error)
	Brief(context.Context) (*rpc.BriefResult, error)
	NudgesSnapshot(context.Context) (*rpc.NudgesSnapshotResult, error)
	NudgesCutoverReview(context.Context, rpc.NudgesCutoverReviewParams) (*rpc.NudgesCutoverReviewResult, error)
	BriefAck(context.Context, rpc.BriefAckParams) (*rpc.BriefAckResult, error)
	ReconcileSignoff(context.Context, rpc.CapitalEventParams) (*rpc.RiskPolicyWriteResult, error)
	TradingStatus(context.Context) (*rpc.TradingStatus, error)
	AutoTradeStatus(context.Context) (*rpc.AutoTradeStatus, error)
	OpportunitiesStatus(context.Context) (*rpc.OpportunityStatus, error)
	OpportunitiesSnapshot(context.Context, rpc.OpportunitySnapshotParams) (*rpc.OpportunitySnapshot, error)
	OpportunitiesRefresh(context.Context, rpc.OpportunityRefreshParams) (*rpc.OpportunitySnapshot, error)
	OpportunitiesPreviewExercise(context.Context, rpc.OpportunityExercisePreviewParams) (*rpc.OpportunityExercisePreviewResult, error)
	OpportunitiesSubmitExercise(context.Context, rpc.OpportunityExerciseSubmitParams) (*rpc.OpportunityExerciseSubmitResult, error)
	OpportunitiesIgnore(context.Context, rpc.OpportunityIgnoreParams) (*rpc.OpportunityIgnoreResult, error)
	TradeProposalsSnapshot(context.Context, rpc.TradeProposalSnapshotParams) (*rpc.TradeProposalSnapshot, error)
	TradeProposalsRefresh(context.Context, rpc.TradeProposalRefreshParams) (*rpc.TradeProposalSnapshot, error)
	TradeProposalsPreview(context.Context, rpc.TradeProposalPreviewParams) (*rpc.TradeProposalPreviewResult, error)
	TradeProposalsSubmit(context.Context, rpc.TradeProposalSubmitParams) (*rpc.TradeProposalSubmitResult, error)
	TradeProposalsReducePreview(context.Context, rpc.TradeProposalReduceParams) (*rpc.TradeProposalReduceResult, error)
	TradeProposalsReduceSubmit(context.Context, rpc.TradeProposalReduceParams) (*rpc.TradeProposalReduceResult, error)
	TradeProposalsReducePortfolioPreview(context.Context, rpc.TradeProposalReducePortfolioParams) (*rpc.TradeProposalReducePortfolioResult, error)
	TradeProposalsReducePortfolioSubmit(context.Context, rpc.TradeProposalReducePortfolioParams) (*rpc.TradeProposalReducePortfolioResult, error)
	TradeProposalsIgnore(context.Context, rpc.TradeProposalIgnoreParams) (*rpc.TradeProposalIgnoreResult, error)
	Settings(context.Context) (*rpc.PlatformSettings, error)
	UpdateSettings(context.Context, json.RawMessage) (*rpc.PlatformSettings, error)
	OrderPreview(context.Context, rpc.OrderPreviewParams) (*rpc.OrderPreviewResult, error)
	OrderPlace(context.Context, rpc.OrderPlaceParams) (*rpc.OrderPlaceResult, error)
	OrderModify(context.Context, rpc.OrderModifyParams) (*rpc.OrderModifyResult, error)
	OrderCancel(context.Context, rpc.OrderCancelParams) (*rpc.OrderCancelResult, error)
	OrdersOpen(context.Context, rpc.OrdersOpenParams) (*rpc.OrdersOpenResult, error)
	OrderStatus(context.Context, rpc.OrderStatusParams) (*rpc.OrderStatusResult, error)
	PurgeStatus(context.Context, rpc.PurgeStatusParams) (*rpc.PurgeStatusResult, error)
	PurgeExecute(context.Context, rpc.PurgeExecuteParams) (*rpc.PurgeExecuteResult, error)
	PurgeRestorePreview(context.Context, rpc.PurgeRestoreParams) (*rpc.PurgeRestoreResult, error)
	PurgeRestoreExecute(context.Context, rpc.PurgeRestoreParams) (*rpc.PurgeRestoreResult, error)
}

// ReconciliationClient is an optional paired-app capability. Keeping it
// separate from Client lets older adapters remain read-compatible while the
// HTTP layer fails closed when the daemon does not support the new surface.
type ReconciliationClient interface {
	ReconcileStatus(context.Context) (*rpc.ReconStatusResult, error)
	ReconcileCheck(context.Context) (*rpc.ReconCheckResult, error)
}

// Real opens a short-lived daemon connection for each typed call and can
// optionally autospawn the daemon when its socket is absent.
type Real struct {
	SocketPath string
	AutoSpawn  bool
}

const appQuoteSnapshotTimeout = 2500 * time.Millisecond

// Result-validation errors identify daemon responses rejected at the app RPC
// boundary without including private payload data.
var (
	ErrInvalidNudgesCutoverReviewResult = errors.New("invalid nudges cutover-review result")
	ErrInvalidAlertCandidateSnapshot    = errors.New("invalid alert candidate snapshot")
)

// Status returns the daemon health snapshot.
func (c Real) Status(ctx context.Context) (*rpc.HealthResult, error) {
	var out rpc.HealthResult
	if err := c.call(ctx, rpc.MethodStatusHealth, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// MarketCalendar returns the default US market calendar window.
func (c Real) MarketCalendar(ctx context.Context) (*rpc.MarketCalendarResult, error) {
	return c.MarketCalendarFor(ctx, "us")
}

// MarketCalendarFor returns a three-day daemon calendar window for market.
func (c Real) MarketCalendarFor(ctx context.Context, market string) (*rpc.MarketCalendarResult, error) {
	var out rpc.MarketCalendarResult
	params := rpc.MarketCalendarParams{Market: market, At: time.Now().UTC(), Days: 3}
	if err := c.call(ctx, rpc.MethodMarketCalendar, params, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Account returns the daemon's typed account-summary result.
func (c Real) Account(ctx context.Context) (*rpc.AccountResult, error) {
	var out rpc.AccountResult
	if err := c.call(ctx, rpc.MethodAccountSummary, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Positions returns the daemon's current typed positions view.
func (c Real) Positions(ctx context.Context) (*rpc.PositionsResult, error) {
	var out rpc.PositionsResult
	if err := c.call(ctx, rpc.MethodPositionsList, rpc.PositionsListParams{}, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Quote requests a bounded daemon snapshot for contract.
func (c Real) Quote(ctx context.Context, contract rpc.ContractParams) (*rpc.Quote, error) {
	var out rpc.Quote
	params := rpc.QuoteSnapshotParams{
		Contract:  contract,
		TimeoutMs: int(appQuoteSnapshotTimeout.Milliseconds()),
	}
	if err := c.call(ctx, rpc.MethodQuoteSnapshot, params, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// StreamQuote forwards decoded quote frames until ctx, the daemon stream, or
// onFrame terminates. A nil onFrame consumes frames without a callback.
func (c Real) StreamQuote(ctx context.Context, contract rpc.ContractParams, onFrame func(rpc.Frame) error) error {
	conn, err := c.connect(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	params := rpc.QuoteSubscribeParams{Contract: contract}
	return conn.Stream(ctx, rpc.MethodQuoteSubscribe, params, func(raw json.RawMessage) error {
		var frame rpc.Frame
		if err := json.Unmarshal(raw, &frame); err != nil {
			return err
		}
		if onFrame != nil {
			return onFrame(frame)
		}
		return nil
	})
}

// MarketEvents returns the daemon-classified market-event snapshot.
func (c Real) MarketEvents(ctx context.Context, params rpc.MarketEventsParams) (*rpc.MarketEventsResult, error) {
	var out rpc.MarketEventsResult
	if err := c.call(ctx, rpc.MethodMarketEventsSnapshot, params, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Canary fetches the daemon-authored Canary result over one connection.
func (c Real) Canary(ctx context.Context) (*rpc.StressResult, error) {
	conn, err := c.connect(ctx)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	out, err := canary.FetchStress(ctx, conn)
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// CanaryWithRegime fetches one coordinated Canary/regime snapshot and compacts
// the regime result for app consumption.
func (c Real) CanaryWithRegime(ctx context.Context) (*rpc.StressResult, *rpc.RegimeMonitorResult, error) {
	conn, err := c.connect(ctx)
	if err != nil {
		return nil, nil, err
	}
	defer conn.Close()
	canaryResult, _, regime, err := canary.FetchStressSnapshotWithRegime(ctx, conn)
	if err != nil {
		return nil, nil, err
	}
	monitor := rpc.CompactRegimeMonitor(&regime)
	return &canaryResult, &monitor, nil
}

// AlertCandidates is an optional capability rather than part of Client so
// app adapters compiled against older daemon surfaces continue to work. The
// live service discovers it explicitly and keeps failures fail-closed.
func (c Real) AlertCandidates(ctx context.Context) (*rpc.AlertCandidateSnapshot, error) {
	return alertCandidates(ctx, c.call)
}

func alertCandidates(ctx context.Context, call func(context.Context, string, any, any) error) (*rpc.AlertCandidateSnapshot, error) {
	var out rpc.AlertCandidateSnapshot
	if err := call(ctx, rpc.MethodAlertCandidates, rpc.AlertCandidatesParams{}, &out); err != nil {
		return nil, err
	}
	if err := rpc.ValidateAlertCandidateSnapshot(out); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidAlertCandidateSnapshot, err)
	}
	return &out, nil
}

// Rules returns the daemon-authored rulebook snapshot.
func (c Real) Rules(ctx context.Context) (*rpc.RulesResult, error) {
	var out rpc.RulesResult
	if err := c.call(ctx, rpc.MethodRulesSnapshot, rpc.RulesSnapshotParams{}, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Brief returns the daemon-authored trading brief snapshot.
func (c Real) Brief(ctx context.Context) (*rpc.BriefResult, error) {
	var out rpc.BriefResult
	if err := c.call(ctx, rpc.MethodBriefSnapshot, rpc.BriefSnapshotParams{}, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// NudgesSnapshot returns the daemon-authored governance candidates and source
// health without granting app-side evaluation authority.
func (c Real) NudgesSnapshot(ctx context.Context) (*rpc.NudgesSnapshotResult, error) {
	var out rpc.NudgesSnapshotResult
	if err := c.call(ctx, rpc.MethodNudgesSnapshot, rpc.NudgesSnapshotParams{}, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ReconcileStatus returns and validates the daemon's reconciliation automation
// status.
func (c Real) ReconcileStatus(ctx context.Context) (*rpc.ReconStatusResult, error) {
	var out rpc.ReconStatusResult
	if err := c.call(ctx, rpc.MethodReconStatus, rpc.ReconStatusParams{}, &out); err != nil {
		return nil, err
	}
	if err := rpc.ValidateReconAutomationStatus(out.Status); err != nil {
		return nil, fmt.Errorf("invalid reconciliation status result: %w", err)
	}
	return &out, nil
}

// ReconcileCheck requests a daemon reconciliation check and validates the
// typed result.
func (c Real) ReconcileCheck(ctx context.Context) (*rpc.ReconCheckResult, error) {
	var out rpc.ReconCheckResult
	if err := c.call(ctx, rpc.MethodReconCheck, rpc.ReconCheckParams{}, &out); err != nil {
		return nil, err
	}
	if err := rpc.ValidateReconCheckResult(out); err != nil {
		return nil, fmt.Errorf("invalid reconciliation check result: %w", err)
	}
	return &out, nil
}

// NudgesCutoverReview forwards a typed cutover-review request and validates
// that the result remains JSON-encodable at the app boundary.
func (c Real) NudgesCutoverReview(ctx context.Context, params rpc.NudgesCutoverReviewParams) (*rpc.NudgesCutoverReviewResult, error) {
	return nudgesCutoverReview(ctx, params, c.call)
}

func nudgesCutoverReview(ctx context.Context, params rpc.NudgesCutoverReviewParams, call func(context.Context, string, any, any) error) (*rpc.NudgesCutoverReviewResult, error) {
	var out rpc.NudgesCutoverReviewResult
	if err := call(ctx, rpc.MethodNudgesCutoverReview, params, &out); err != nil {
		return nil, err
	}
	if _, err := json.Marshal(out); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidNudgesCutoverReviewResult, err)
	}
	return &out, nil
}

// BriefAck forwards a paired-app acknowledgement to the daemon.
func (c Real) BriefAck(ctx context.Context, params rpc.BriefAckParams) (*rpc.BriefAckResult, error) {
	var out rpc.BriefAckResult
	if err := c.call(ctx, rpc.MethodBriefAck, params, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ReconcileSignoff records a typed reconciliation capital event through the
// daemon; it forces the event type to reconcile.
func (c Real) ReconcileSignoff(ctx context.Context, params rpc.CapitalEventParams) (*rpc.RiskPolicyWriteResult, error) {
	params.Type = "reconcile"
	var out rpc.RiskPolicyWriteResult
	if err := c.call(ctx, rpc.MethodRiskPolicyCapitalEvent, params, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// TradingStatus returns the daemon's current broker-write readiness surface.
func (c Real) TradingStatus(ctx context.Context) (*rpc.TradingStatus, error) {
	var out rpc.TradingStatus
	if err := c.call(ctx, rpc.MethodTradingStatus, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// AutoTradeStatus returns the daemon-owned automation readiness surface.
func (c Real) AutoTradeStatus(ctx context.Context) (*rpc.AutoTradeStatus, error) {
	var out rpc.AutoTradeStatus
	if err := c.call(ctx, rpc.MethodAutoTradeStatus, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// OpportunitiesStatus returns the daemon-owned opportunity-engine status.
func (c Real) OpportunitiesStatus(ctx context.Context) (*rpc.OpportunityStatus, error) {
	var out rpc.OpportunityStatus
	if err := c.call(ctx, rpc.MethodOpportunitiesStatus, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// OpportunitiesSnapshot returns the daemon-authored opportunity snapshot.
func (c Real) OpportunitiesSnapshot(ctx context.Context, params rpc.OpportunitySnapshotParams) (*rpc.OpportunitySnapshot, error) {
	var out rpc.OpportunitySnapshot
	if err := c.call(ctx, rpc.MethodOpportunitiesSnapshot, params, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// OpportunitiesRefresh asks the daemon to refresh its opportunity snapshot.
func (c Real) OpportunitiesRefresh(ctx context.Context, params rpc.OpportunityRefreshParams) (*rpc.OpportunitySnapshot, error) {
	var out rpc.OpportunitySnapshot
	if err := c.call(ctx, rpc.MethodOpportunitiesRefresh, params, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// OpportunitiesPreviewExercise requests the daemon's guarded exercise preview.
func (c Real) OpportunitiesPreviewExercise(ctx context.Context, params rpc.OpportunityExercisePreviewParams) (*rpc.OpportunityExercisePreviewResult, error) {
	var out rpc.OpportunityExercisePreviewResult
	if err := c.call(ctx, rpc.MethodOpportunitiesPreviewExercise, params, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// OpportunitiesSubmitExercise transports a paired-device submit request; the
// daemon still enforces preview, eligibility, account, mode, and write policy.
func (c Real) OpportunitiesSubmitExercise(ctx context.Context, params rpc.OpportunityExerciseSubmitParams) (*rpc.OpportunityExerciseSubmitResult, error) {
	var out rpc.OpportunityExerciseSubmitResult
	if err := c.call(ctx, rpc.MethodOpportunitiesSubmitExercise, params, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// OpportunitiesIgnore asks the daemon to apply an opportunity-ignore action.
func (c Real) OpportunitiesIgnore(ctx context.Context, params rpc.OpportunityIgnoreParams) (*rpc.OpportunityIgnoreResult, error) {
	var out rpc.OpportunityIgnoreResult
	if err := c.call(ctx, rpc.MethodOpportunitiesIgnore, params, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// TradeProposalsSnapshot returns the daemon-authored proposal snapshot.
func (c Real) TradeProposalsSnapshot(ctx context.Context, params rpc.TradeProposalSnapshotParams) (*rpc.TradeProposalSnapshot, error) {
	var out rpc.TradeProposalSnapshot
	if err := c.call(ctx, rpc.MethodTradeProposalsSnapshot, params, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// TradeProposalsRefresh asks the daemon to refresh its proposal snapshot.
func (c Real) TradeProposalsRefresh(ctx context.Context, params rpc.TradeProposalRefreshParams) (*rpc.TradeProposalSnapshot, error) {
	var out rpc.TradeProposalSnapshot
	if err := c.call(ctx, rpc.MethodTradeProposalsRefresh, params, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// TradeProposalsPreview requests the daemon's guarded proposal preview.
func (c Real) TradeProposalsPreview(ctx context.Context, params rpc.TradeProposalPreviewParams) (*rpc.TradeProposalPreviewResult, error) {
	var out rpc.TradeProposalPreviewResult
	if err := c.call(ctx, rpc.MethodTradeProposalsPreview, params, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// TradeProposalsSubmit transports a paired-device submit request; all broker
// authorization and preview-token checks remain daemon-owned.
func (c Real) TradeProposalsSubmit(ctx context.Context, params rpc.TradeProposalSubmitParams) (*rpc.TradeProposalSubmitResult, error) {
	var out rpc.TradeProposalSubmitResult
	if err := c.call(ctx, rpc.MethodTradeProposalsSubmit, params, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// TradeProposalsReducePreview requests the daemon's guarded single-proposal
// reduction preview.
func (c Real) TradeProposalsReducePreview(ctx context.Context, params rpc.TradeProposalReduceParams) (*rpc.TradeProposalReduceResult, error) {
	var out rpc.TradeProposalReduceResult
	if err := c.call(ctx, rpc.MethodTradeProposalsReducePreview, params, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// TradeProposalsReduceSubmit transports a paired-device reduction request;
// daemon broker-write gates remain binding.
func (c Real) TradeProposalsReduceSubmit(ctx context.Context, params rpc.TradeProposalReduceParams) (*rpc.TradeProposalReduceResult, error) {
	var out rpc.TradeProposalReduceResult
	if err := c.call(ctx, rpc.MethodTradeProposalsReduceSubmit, params, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// TradeProposalsReducePortfolioPreview requests the daemon's guarded portfolio
// reduction preview.
func (c Real) TradeProposalsReducePortfolioPreview(ctx context.Context, params rpc.TradeProposalReducePortfolioParams) (*rpc.TradeProposalReducePortfolioResult, error) {
	var out rpc.TradeProposalReducePortfolioResult
	if err := c.call(ctx, rpc.MethodTradeProposalsReducePortfolioPreview, params, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// TradeProposalsReducePortfolioSubmit transports a paired-device portfolio
// reduction request; daemon broker-write gates remain binding.
func (c Real) TradeProposalsReducePortfolioSubmit(ctx context.Context, params rpc.TradeProposalReducePortfolioParams) (*rpc.TradeProposalReducePortfolioResult, error) {
	var out rpc.TradeProposalReducePortfolioResult
	if err := c.call(ctx, rpc.MethodTradeProposalsReducePortfolioSubmit, params, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// TradeProposalsIgnore asks the daemon to apply a proposal-ignore action.
func (c Real) TradeProposalsIgnore(ctx context.Context, params rpc.TradeProposalIgnoreParams) (*rpc.TradeProposalIgnoreResult, error) {
	var out rpc.TradeProposalIgnoreResult
	if err := c.call(ctx, rpc.MethodTradeProposalsIgnore, params, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Settings returns the daemon-owned platform settings snapshot.
func (c Real) Settings(ctx context.Context) (*rpc.PlatformSettings, error) {
	var out rpc.PlatformSettings
	if err := c.call(ctx, rpc.MethodSettingsGet, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// UpdateSettings transports an opaque JSON merge patch to the daemon's typed
// settings handler; this adapter does not reinterpret or authorize fields.
func (c Real) UpdateSettings(ctx context.Context, patch json.RawMessage) (*rpc.PlatformSettings, error) {
	var out rpc.PlatformSettings
	if err := c.call(ctx, rpc.MethodSettingsUpdate, patch, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// OrderPreview requests the daemon's broker WhatIf and policy-gated preview.
func (c Real) OrderPreview(ctx context.Context, params rpc.OrderPreviewParams) (*rpc.OrderPreviewResult, error) {
	var out rpc.OrderPreviewResult
	if err := c.call(ctx, rpc.MethodOrderPreview, params, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// OrderPlace transports a paired-device place request; the daemon retains all
// preview-token, account, mode, freeze, eligibility, and journaling gates.
func (c Real) OrderPlace(ctx context.Context, params rpc.OrderPlaceParams) (*rpc.OrderPlaceResult, error) {
	var out rpc.OrderPlaceResult
	if err := c.call(ctx, rpc.MethodOrderPlace, params, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// OrderModify transports a paired-device modification request; the daemon
// retains preview-token and broker-write authority.
func (c Real) OrderModify(ctx context.Context, params rpc.OrderModifyParams) (*rpc.OrderModifyResult, error) {
	var out rpc.OrderModifyResult
	if err := c.call(ctx, rpc.MethodOrderModify, params, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// OrderCancel transports a paired-device cancellation request; cancellation
// policy and broker authority remain daemon-owned.
func (c Real) OrderCancel(ctx context.Context, params rpc.OrderCancelParams) (*rpc.OrderCancelResult, error) {
	var out rpc.OrderCancelResult
	if err := c.call(ctx, rpc.MethodOrderCancel, params, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// OrdersOpen returns the daemon's open-order journal view.
func (c Real) OrdersOpen(ctx context.Context, params rpc.OrdersOpenParams) (*rpc.OrdersOpenResult, error) {
	var out rpc.OrdersOpenResult
	if err := c.call(ctx, rpc.MethodOrdersOpen, params, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// OrderStatus returns the daemon's status for one locally journaled order.
func (c Real) OrderStatus(ctx context.Context, params rpc.OrderStatusParams) (*rpc.OrderStatusResult, error) {
	var out rpc.OrderStatusResult
	if err := c.call(ctx, rpc.MethodOrderStatus, params, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// PurgeStatus returns the daemon-owned purge state projection.
func (c Real) PurgeStatus(ctx context.Context, params rpc.PurgeStatusParams) (*rpc.PurgeStatusResult, error) {
	var out rpc.PurgeStatusResult
	if err := c.call(ctx, rpc.MethodPurgeStatus, params, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// PurgeExecute transports a paired-device purge request; the daemon retains
// target validation, confirmation, policy, and execution authority.
func (c Real) PurgeExecute(ctx context.Context, params rpc.PurgeExecuteParams) (*rpc.PurgeExecuteResult, error) {
	var out rpc.PurgeExecuteResult
	if err := c.call(ctx, rpc.MethodPurgeExecute, params, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// PurgeRestorePreview requests the daemon's restore preview without executing
// the restore.
func (c Real) PurgeRestorePreview(ctx context.Context, params rpc.PurgeRestoreParams) (*rpc.PurgeRestoreResult, error) {
	var out rpc.PurgeRestoreResult
	if err := c.call(ctx, rpc.MethodPurgeRestorePreview, params, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// PurgeRestoreExecute transports a paired-device restore request; execution
// authority and safety checks remain daemon-owned.
func (c Real) PurgeRestoreExecute(ctx context.Context, params rpc.PurgeRestoreParams) (*rpc.PurgeRestoreResult, error) {
	var out rpc.PurgeRestoreResult
	if err := c.call(ctx, rpc.MethodPurgeRestoreExecute, params, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c Real) call(ctx context.Context, method string, params any, out any) error {
	conn, err := c.connect(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	if err := conn.Call(ctx, method, params, out); err != nil {
		return fmt.Errorf("%s: %w", method, err)
	}
	return nil
}

func (c Real) connect(ctx context.Context) (*dial.Conn, error) {
	path := c.SocketPath
	if path == "" {
		path = dial.DefaultSocketPath()
	}
	conn, err := dial.Connect(path)
	if errors.Is(err, dial.ErrSocketMissing) && c.AutoSpawn {
		conn, err = dial.AutospawnAndConnectContext(ctx, path)
	}
	if err != nil {
		return nil, err
	}
	return conn, nil
}
