// Package daemonclient adapts the app host to the daemon's typed RPC surface.
// It can carry read, preview, and paired-device action requests, but capability
// to call a method is not authority: the daemon retains policy, preview-token,
// account, mode, freeze, and broker-write enforcement.
package daemonclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/osauer/canary/v2/internal/dial"
	"github.com/osauer/canary/v2/internal/rpc"
	"github.com/osauer/canary/v2/internal/rpc/api"
	"github.com/osauer/canary/v2/internal/stress"
	"time"
)

// Client is the app host's typed daemon capability surface. Implementations
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
	Stress(context.Context) (*rpc.StressResult, error)
	StressWithRegime(context.Context) (*rpc.StressResult, *rpc.RegimeMonitorResult, error)
	Rules(context.Context) (*rpc.RulesResult, error)
	Brief(context.Context) (*rpc.BriefResult, error)
	NudgesSnapshot(context.Context) (*rpc.NudgesSnapshotResult, error)
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
	TradeProposalsRequestStop(context.Context, rpc.TradeProposalRequestStopParams) (*rpc.TradeProposalRequestStopResult, error)
	TradeProposalsIgnore(context.Context, rpc.TradeProposalIgnoreParams) (*rpc.TradeProposalIgnoreResult, error)
	TradeProposalsVeto(context.Context, rpc.TradeProposalVetoParams) (*rpc.TradeProposalVetoResult, error)
	Settings(context.Context) (*rpc.PlatformSettings, error)
	UpdateSettings(context.Context, json.RawMessage) (*rpc.PlatformSettings, error)
	OrderPreview(context.Context, rpc.OrderPreviewParams) (*rpc.OrderPreviewResult, error)
	StrategyPreview(context.Context, rpc.StrategyPreviewParams) (*rpc.OrderPreviewResult, error)
	OrderPlace(context.Context, rpc.OrderPlaceParams) (*rpc.OrderPlaceResult, error)
	OrderModify(context.Context, rpc.OrderModifyParams) (*rpc.OrderModifyResult, error)
	OrderCancel(context.Context, rpc.OrderCancelParams) (*rpc.OrderCancelResult, error)
	OrdersOpen(context.Context, rpc.OrdersOpenParams) (*rpc.OrdersOpenResult, error)
	OrderStatus(context.Context, rpc.OrderStatusParams) (*rpc.OrderStatusResult, error)
}

// ReconciliationClient is an optional paired-app capability. Keeping it
// separate from Client lets older adapters remain read-compatible while the
// HTTP layer fails closed when the daemon does not support the new surface.
type ReconciliationClient interface {
	ReconcileStatus(context.Context) (*rpc.ReconStatusResult, error)
	ReconcileCheck(context.Context) (*rpc.ReconCheckResult, error)
}

// EdgeClient is the optional, read-only retrospective review capability. It is
// separate from Client so older app-host adapters fail closed without gaining
// a new method implicitly.
type EdgeClient interface {
	EdgeSnapshot(context.Context, rpc.EdgeSnapshotParams) (*rpc.EdgeResult, error)
}

// FinancingClient is the optional read-only earned stock-lending fee history.
type FinancingClient interface {
	FinancingFees(context.Context, rpc.FinancingFeesParams) (*rpc.FinancingFeesResult, error)
}

// FinancingFees reads retained statement fees without contacting the broker.
func (c Real) FinancingFees(ctx context.Context, params rpc.FinancingFeesParams) (*rpc.FinancingFeesResult, error) {
	var out rpc.FinancingFeesResult
	if err := rpc.Call(ctx, rpc.CallerFunc(c.call), api.FinancingFees, params, &out); err != nil {
		return nil, err
	}
	if err := rpc.ValidateFinancingFeesResponse(out, params); err != nil {
		return nil, fmt.Errorf("invalid lending fees: %w", err)
	}
	return &out, nil
}

// PushDeliveryProofClient is the optional capability that relays the app's
// redacted Web Push delivery proof to the daemon. It records evidence only;
// it grants no delivery, alert, or broker authority.
type PushDeliveryProofClient interface {
	ReportPushDeliveryProof(context.Context, rpc.PushDeliveryProof) (*rpc.AlertDeliveryProofResult, error)
}

// ReportPushDeliveryProof relays one proof. It never autospawns the daemon:
// evidence reporting must not start a broker-connected process.
func (c Real) ReportPushDeliveryProof(ctx context.Context, proof rpc.PushDeliveryProof) (*rpc.AlertDeliveryProofResult, error) {
	var out rpc.AlertDeliveryProofResult
	if err := rpc.Call(ctx, rpc.CallerFunc(Real{SocketPath: c.SocketPath}.call), api.AlertDeliveryProof, proof, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// MarketTapeClient is the optional read-only broad-market history capability.
type MarketTapeClient interface {
	MarketTape(context.Context, rpc.MarketTapeParams) (*rpc.MarketTapeResult, error)
}

// MarketTape composes a bounded retrospective tape in the daemon.
func (c Real) MarketTape(ctx context.Context, params rpc.MarketTapeParams) (*rpc.MarketTapeResult, error) {
	var out rpc.MarketTapeResult
	if err := rpc.Call(ctx, rpc.CallerFunc(c.call), api.MarketTape, params, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Real opens a short-lived daemon connection for each typed call and can
// optionally autospawn the daemon when its socket is absent.
type Real struct {
	SocketPath string
	AutoSpawn  bool
}

const appQuoteSnapshotTimeout = 2500 * time.Millisecond

// Result-validation errors identify daemon responses rejected at the app RPC
var (
	ErrInvalidAlertCandidateSnapshot = errors.New("invalid alert candidate snapshot")
)

// Status returns the daemon health snapshot.
func (c Real) Status(ctx context.Context) (*rpc.HealthResult, error) {
	var out rpc.HealthResult
	if err := rpc.Call(ctx, rpc.CallerFunc(c.call), api.StatusHealth, rpc.HealthParams{}, &out); err != nil {
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
	if err := rpc.Call(ctx, rpc.CallerFunc(c.call), api.MarketCalendar, params, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Account returns the daemon's typed account-summary result.
func (c Real) Account(ctx context.Context) (*rpc.AccountResult, error) {
	var out rpc.AccountResult
	if err := rpc.Call(ctx, rpc.CallerFunc(c.call), api.AccountSummary, rpc.AccountSummaryParams{}, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Positions returns the daemon's current typed positions view.
func (c Real) Positions(ctx context.Context) (*rpc.PositionsResult, error) {
	var out rpc.PositionsResult
	if err := rpc.Call(ctx, rpc.CallerFunc(c.call), api.PositionsList, rpc.PositionsListParams{}, &out); err != nil {
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
	if err := rpc.Call(ctx, rpc.CallerFunc(c.call), api.QuoteSnapshot, params, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// StreamQuote forwards decoded quote frames until ctx, the daemon stream, or
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
	if err := rpc.Call(ctx, rpc.CallerFunc(c.call), api.MarketEventsSnapshot, params, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Stress reads the daemon-composed portfolio-stress result.
func (c Real) Stress(ctx context.Context) (*rpc.StressResult, error) {
	conn, err := c.connect(ctx)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	out, err := stress.FetchStress(ctx, conn)
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// StressWithRegime reads the daemon's stress result with the compact regime
// monitor read it was composed from.
func (c Real) StressWithRegime(ctx context.Context) (*rpc.StressResult, *rpc.RegimeMonitorResult, error) {
	conn, err := c.connect(ctx)
	if err != nil {
		return nil, nil, err
	}
	defer conn.Close()
	stressResult, monitor, err := stress.FetchStressWithRegimeMonitor(ctx, conn)
	if err != nil {
		return nil, nil, err
	}
	return &stressResult, monitor, nil
}

// AlertCandidates is an optional capability rather than part of Client so
// live service discovers it explicitly and keeps failures fail-closed.
func (c Real) AlertCandidates(ctx context.Context) (*rpc.AlertCandidateSnapshot, error) {
	return alertCandidates(ctx, c.call)
}

func alertCandidates(ctx context.Context, call func(context.Context, string, any, any) error) (*rpc.AlertCandidateSnapshot, error) {
	var out rpc.AlertCandidateSnapshot
	if err := rpc.Call(ctx, rpc.CallerFunc(call), api.AlertCandidates, rpc.AlertCandidatesParams{}, &out); err != nil {
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
	if err := rpc.Call(ctx, rpc.CallerFunc(c.call), api.RulesSnapshot, rpc.RulesSnapshotParams{}, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Brief returns the daemon-authored trading brief snapshot.
func (c Real) Brief(ctx context.Context) (*rpc.BriefResult, error) {
	var out rpc.BriefResult
	if err := rpc.Call(ctx, rpc.CallerFunc(c.call), api.BriefSnapshot, rpc.BriefSnapshotParams{}, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// EdgeSnapshot returns an already-published daemon result. The RPC read cannot
// start Flex or HMDS work.
func (c Real) EdgeSnapshot(ctx context.Context, params rpc.EdgeSnapshotParams) (*rpc.EdgeResult, error) {
	var out rpc.EdgeResult
	if err := rpc.Call(ctx, rpc.CallerFunc(c.call), api.EdgeSnapshot, params, &out); err != nil {
		return nil, err
	}
	if err := rpc.ValidateEdgeResult(out); err != nil {
		return nil, fmt.Errorf("invalid Edge result: %w", err)
	}
	return &out, nil
}

// NudgesSnapshot returns the daemon-authored governance candidates and source
// health without granting app-side evaluation authority.
func (c Real) NudgesSnapshot(ctx context.Context) (*rpc.NudgesSnapshotResult, error) {
	var out rpc.NudgesSnapshotResult
	if err := rpc.Call(ctx, rpc.CallerFunc(c.call), api.NudgesSnapshot, rpc.NudgesSnapshotParams{}, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ReconcileStatus returns and validates the daemon's reconciliation automation
func (c Real) ReconcileStatus(ctx context.Context) (*rpc.ReconStatusResult, error) {
	var out rpc.ReconStatusResult
	if err := rpc.Call(ctx, rpc.CallerFunc(c.call), api.ReconStatus, rpc.ReconStatusParams{}, &out); err != nil {
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
	if err := rpc.Call(ctx, rpc.CallerFunc(c.call), api.ReconCheck, rpc.ReconCheckParams{}, &out); err != nil {
		return nil, err
	}
	if err := rpc.ValidateReconCheckResult(out); err != nil {
		return nil, fmt.Errorf("invalid reconciliation check result: %w", err)
	}
	return &out, nil
}

// TradingStatus returns the daemon's current broker-write readiness surface.
func (c Real) TradingStatus(ctx context.Context) (*rpc.TradingStatus, error) {
	var out rpc.TradingStatus
	if err := rpc.Call(ctx, rpc.CallerFunc(c.call), api.TradingStatus, rpc.NoParams{}, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// AutoTradeStatus returns the daemon-owned automation readiness surface.
func (c Real) AutoTradeStatus(ctx context.Context) (*rpc.AutoTradeStatus, error) {
	var out rpc.AutoTradeStatus
	if err := rpc.Call(ctx, rpc.CallerFunc(c.call), api.AutoTradeStatus, rpc.NoParams{}, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// OpportunitiesStatus returns the daemon-owned opportunity-engine status.
func (c Real) OpportunitiesStatus(ctx context.Context) (*rpc.OpportunityStatus, error) {
	var out rpc.OpportunityStatus
	if err := rpc.Call(ctx, rpc.CallerFunc(c.call), api.OpportunitiesStatus, rpc.NoParams{}, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// OpportunitiesSnapshot returns the daemon-authored opportunity snapshot.
func (c Real) OpportunitiesSnapshot(ctx context.Context, params rpc.OpportunitySnapshotParams) (*rpc.OpportunitySnapshot, error) {
	var out rpc.OpportunitySnapshot
	if err := rpc.Call(ctx, rpc.CallerFunc(c.call), api.OpportunitiesSnapshot, params, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// OpportunitiesRefresh asks the daemon to refresh its opportunity snapshot.
func (c Real) OpportunitiesRefresh(ctx context.Context, params rpc.OpportunityRefreshParams) (*rpc.OpportunitySnapshot, error) {
	var out rpc.OpportunitySnapshot
	if err := rpc.Call(ctx, rpc.CallerFunc(c.call), api.OpportunitiesRefresh, params, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// OpportunitiesPreviewExercise requests the daemon's guarded exercise preview.
func (c Real) OpportunitiesPreviewExercise(ctx context.Context, params rpc.OpportunityExercisePreviewParams) (*rpc.OpportunityExercisePreviewResult, error) {
	var out rpc.OpportunityExercisePreviewResult
	if err := rpc.Call(ctx, rpc.CallerFunc(c.call), api.OpportunitiesPreviewExercise, params, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// OpportunitiesSubmitExercise transports a paired-device submit request; the
// daemon still enforces preview, eligibility, account, mode, and write policy.
func (c Real) OpportunitiesSubmitExercise(ctx context.Context, params rpc.OpportunityExerciseSubmitParams) (*rpc.OpportunityExerciseSubmitResult, error) {
	var out rpc.OpportunityExerciseSubmitResult
	if err := rpc.Call(ctx, rpc.CallerFunc(c.call), api.OpportunitiesSubmitExercise, params, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// OpportunitiesIgnore asks the daemon to apply an opportunity-ignore action.
func (c Real) OpportunitiesIgnore(ctx context.Context, params rpc.OpportunityIgnoreParams) (*rpc.OpportunityIgnoreResult, error) {
	var out rpc.OpportunityIgnoreResult
	if err := rpc.Call(ctx, rpc.CallerFunc(c.call), api.OpportunitiesIgnore, params, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// TradeProposalsSnapshot returns the daemon-authored proposal snapshot.
func (c Real) TradeProposalsSnapshot(ctx context.Context, params rpc.TradeProposalSnapshotParams) (*rpc.TradeProposalSnapshot, error) {
	var out rpc.TradeProposalSnapshot
	if err := rpc.Call(ctx, rpc.CallerFunc(c.call), api.TradeProposalsSnapshot, params, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// TradeProposalsRefresh asks the daemon to refresh its proposal snapshot.
func (c Real) TradeProposalsRefresh(ctx context.Context, params rpc.TradeProposalRefreshParams) (*rpc.TradeProposalSnapshot, error) {
	var out rpc.TradeProposalSnapshot
	if err := rpc.Call(ctx, rpc.CallerFunc(c.call), api.TradeProposalsRefresh, params, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// TradeProposalsPreview requests the daemon's guarded proposal preview.
func (c Real) TradeProposalsPreview(ctx context.Context, params rpc.TradeProposalPreviewParams) (*rpc.TradeProposalPreviewResult, error) {
	var out rpc.TradeProposalPreviewResult
	if err := rpc.Call(ctx, rpc.CallerFunc(c.call), api.TradeProposalsPreview, params, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// TradeProposalsSubmit transports a paired-device submit request; all broker
// authorization and preview-token checks remain daemon-owned.
func (c Real) TradeProposalsSubmit(ctx context.Context, params rpc.TradeProposalSubmitParams) (*rpc.TradeProposalSubmitResult, error) {
	var out rpc.TradeProposalSubmitResult
	if err := rpc.Call(ctx, rpc.CallerFunc(c.call), api.TradeProposalsSubmit, params, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// TradeProposalsReducePreview requests the daemon's guarded single-proposal
func (c Real) TradeProposalsReducePreview(ctx context.Context, params rpc.TradeProposalReduceParams) (*rpc.TradeProposalReduceResult, error) {
	var out rpc.TradeProposalReduceResult
	if err := rpc.Call(ctx, rpc.CallerFunc(c.call), api.TradeProposalsReducePreview, params, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// TradeProposalsReduceSubmit transports a paired-device reduction request;
// daemon broker-write gates remain binding.
func (c Real) TradeProposalsReduceSubmit(ctx context.Context, params rpc.TradeProposalReduceParams) (*rpc.TradeProposalReduceResult, error) {
	var out rpc.TradeProposalReduceResult
	if err := rpc.Call(ctx, rpc.CallerFunc(c.call), api.TradeProposalsReduceSubmit, params, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// TradeProposalsReducePortfolioPreview requests the daemon's guarded portfolio
func (c Real) TradeProposalsReducePortfolioPreview(ctx context.Context, params rpc.TradeProposalReducePortfolioParams) (*rpc.TradeProposalReducePortfolioResult, error) {
	var out rpc.TradeProposalReducePortfolioResult
	if err := rpc.Call(ctx, rpc.CallerFunc(c.call), api.TradeProposalsReducePortfolioPreview, params, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// TradeProposalsReducePortfolioSubmit transports a paired-device portfolio
// reduction request; daemon broker-write gates remain binding.
func (c Real) TradeProposalsReducePortfolioSubmit(ctx context.Context, params rpc.TradeProposalReducePortfolioParams) (*rpc.TradeProposalReducePortfolioResult, error) {
	var out rpc.TradeProposalReducePortfolioResult
	if err := rpc.Call(ctx, rpc.CallerFunc(c.call), api.TradeProposalsReducePortfolioSubmit, params, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// TradeProposalsRequestStop asks the daemon to generate a protective
// trailing-stop proposal for one named position now. Generation only; the
// result flows into the ordinary preview/submit gates.
func (c Real) TradeProposalsRequestStop(ctx context.Context, params rpc.TradeProposalRequestStopParams) (*rpc.TradeProposalRequestStopResult, error) {
	var out rpc.TradeProposalRequestStopResult
	if err := rpc.Call(ctx, rpc.CallerFunc(c.call), api.TradeProposalsRequestStop, params, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// TradeProposalsIgnore asks the daemon to apply a proposal-ignore action.
func (c Real) TradeProposalsIgnore(ctx context.Context, params rpc.TradeProposalIgnoreParams) (*rpc.TradeProposalIgnoreResult, error) {
	var out rpc.TradeProposalIgnoreResult
	if err := rpc.Call(ctx, rpc.CallerFunc(c.call), api.TradeProposalsIgnore, params, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// TradeProposalsVeto stops the pending pre-authorised submission for one
// proposal key. The app stamps the paired-device origin; the daemon refuses
// every other origin.
func (c Real) TradeProposalsVeto(ctx context.Context, params rpc.TradeProposalVetoParams) (*rpc.TradeProposalVetoResult, error) {
	var out rpc.TradeProposalVetoResult
	if err := rpc.Call(ctx, rpc.CallerFunc(c.call), api.TradeProposalsVeto, params, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Settings returns the daemon-owned platform settings snapshot.
func (c Real) Settings(ctx context.Context) (*rpc.PlatformSettings, error) {
	var out rpc.PlatformSettings
	if err := rpc.Call(ctx, rpc.CallerFunc(c.call), api.SettingsGet, rpc.NoParams{}, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// UpdateSettings transports an opaque JSON merge patch to the daemon's typed
func (c Real) UpdateSettings(ctx context.Context, patch json.RawMessage) (*rpc.PlatformSettings, error) {
	var out rpc.PlatformSettings
	if err := rpc.Call(ctx, rpc.CallerFunc(c.call), api.SettingsUpdate, patch, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// OrderPreview requests the daemon's broker WhatIf and policy-gated preview.
func (c Real) OrderPreview(ctx context.Context, params rpc.OrderPreviewParams) (*rpc.OrderPreviewResult, error) {
	var out rpc.OrderPreviewResult
	if err := rpc.Call(ctx, rpc.CallerFunc(c.call), api.OrderPreview, params, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// StrategyPreview asks the daemon to resolve one current strategy and build a
// proportional, broker-guaranteed combo preview. The app never authors legs.
func (c Real) StrategyPreview(ctx context.Context, params rpc.StrategyPreviewParams) (*rpc.OrderPreviewResult, error) {
	var out rpc.OrderPreviewResult
	if err := rpc.Call(ctx, rpc.CallerFunc(c.call), api.StrategyPreview, params, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// OrderPlace transports a paired-device place request; the daemon retains all
// preview-token, account, mode, freeze, eligibility, and journaling gates.
func (c Real) OrderPlace(ctx context.Context, params rpc.OrderPlaceParams) (*rpc.OrderPlaceResult, error) {
	var out rpc.OrderPlaceResult
	if err := rpc.Call(ctx, rpc.CallerFunc(c.call), api.OrderPlace, params, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// OrderModify transports a paired-device modification request; the daemon
// retains preview-token and broker-write authority.
func (c Real) OrderModify(ctx context.Context, params rpc.OrderModifyParams) (*rpc.OrderModifyResult, error) {
	var out rpc.OrderModifyResult
	if err := rpc.Call(ctx, rpc.CallerFunc(c.call), api.OrderModify, params, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// OrderCancel transports a paired-device cancellation request; cancellation
// policy and broker authority remain daemon-owned.
func (c Real) OrderCancel(ctx context.Context, params rpc.OrderCancelParams) (*rpc.OrderCancelResult, error) {
	var out rpc.OrderCancelResult
	if err := rpc.Call(ctx, rpc.CallerFunc(c.call), api.OrderCancel, params, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// OrdersOpen returns the daemon's open-order journal view.
func (c Real) OrdersOpen(ctx context.Context, params rpc.OrdersOpenParams) (*rpc.OrdersOpenResult, error) {
	var out rpc.OrdersOpenResult
	if err := rpc.Call(ctx, rpc.CallerFunc(c.call), api.OrdersOpen, params, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// OrderStatus returns the daemon's status for one locally journaled order.
func (c Real) OrderStatus(ctx context.Context, params rpc.OrderStatusParams) (*rpc.OrderStatusResult, error) {
	var out rpc.OrderStatusResult
	if err := rpc.Call(ctx, rpc.CallerFunc(c.call), api.OrderStatus, params, &out); err != nil {
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
