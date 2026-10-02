package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/osauer/canary/v2/internal/daemon/corestore"
	"github.com/osauer/canary/v2/internal/risk"
	"github.com/osauer/canary/v2/internal/rpc"
)

const cashSweepTraceKind = "cash_sweep_decision_current"
const cashSweepTraceEventType = "cash_sweep_decision"

type cashSweepTraceState struct {
	Digest string                       `json:"digest"`
	Recent []rpc.CashSweepDecisionTrace `json:"recent"`
}

func (e *proposalEngine) persistCashSweepTrace(ctx context.Context, policy protectionPolicy, sources rpc.TradeProposalSourceFingerprints, scope brokerStateScope, now time.Time, st *rpc.TradeProposalCashSweepStatus) error {
	if e == nil || e.store == nil {
		return errors.New("sweep SQLite authority unavailable")
	}
	e.store.mu.Lock()
	defer e.store.mu.Unlock()
	if e.store.core == nil {
		return errors.New("sweep SQLite authority unavailable")
	}
	trace := rpc.CashSweepDecisionTrace{At: now, PolicyID: policy.PolicyID, PolicyVersion: policy.PolicyVersion,
		OperationalFunding: risk.CloneCashSweepOperationalObservation(st.OperationalFunding), CalibrationStudies: risk.CloneCashSweepCalibrationStudies(st.CalibrationStudies),
		AccountReceiptAt: st.AccountReceiptAt, PositionsReceiptAt: st.PositionsReceiptAt, FundingAsOf: st.FundingAsOf,
		FundingValidUntil: st.FundingValidUntil, ScenarioFingerprint: st.ScenarioFingerprint,
		PlanningSessionEpoch: st.PlanningSessionEpoch, PlanningDaemonStartedAt: st.PlanningDaemonStartedAt,
		PolicyFingerprint: fingerprintProtectionPolicy(policy).Key, Mode: st.Mode, CurrencyPriority: st.CurrencyPriority, CurrencyPrioritySource: st.CurrencyPrioritySource,
		ReserveCushionEUR: st.ReserveCushionEUR, ReserveState: st.ReserveState, ReserveReason: st.ReserveReason}
	if sources.Account != nil {
		trace.AccountFingerprint = sources.Account.Key
	}
	if sources.Positions != nil {
		trace.PositionsFingerprint = sources.Positions.Key
	}
	for _, c := range st.Currencies {
		trace.Currencies = append(trace.Currencies, rpc.CashSweepDecisionCurrency{Currency: c.Currency, Action: c.State, Reason: c.Reason,
			WebCashOriginalAsOf: c.WebCashOriginalAsOf, SettledSourceKind: c.SettledSourceKind,
			PriorityRank: c.PriorityRank, Cash: c.Cash, Committed: c.Committed, FundingNeed: c.FundingNeed,
			BufferAllocation: c.BufferAllocation, EffectiveReserve: c.EffectiveReserve, Free: c.Free})
	}
	semantic := trace
	// Poll clocks and refreshed provenance alone do not create new decisions.
	// A changed policy, money calculation, posture or reason always does.
	semantic.At = time.Time{}
	semantic.AccountReceiptAt, semantic.PositionsReceiptAt, semantic.FundingAsOf, semantic.FundingValidUntil = time.Time{}, time.Time{}, time.Time{}, time.Time{}
	semantic.PlanningSessionEpoch, semantic.PlanningDaemonStartedAt = 0, time.Time{}
	semantic.OperationalFunding = risk.CloneCashSweepOperationalObservation(trace.OperationalFunding)
	if o := semantic.OperationalFunding; o != nil {
		o.AsOf, o.AccountReceiptAt, o.PositionsReceiptAt = time.Time{}, time.Time{}, time.Time{}
		for i := range o.Obligations {
			o.Obligations[i].QuoteOriginalAt, o.Obligations[i].DeliverableOriginalAt = time.Time{}, time.Time{}
		}
	}
	semantic.CalibrationStudies = risk.CloneCashSweepCalibrationStudies(trace.CalibrationStudies)
	for i := range semantic.CalibrationStudies {
		semantic.CalibrationStudies[i].AsOf = time.Time{}
	}
	// Deep-copy before omitting refresh-only original clocks from the digest.
	semantic.Currencies = append([]rpc.CashSweepDecisionCurrency(nil), trace.Currencies...)
	for i := range semantic.Currencies {
		semantic.Currencies[i].WebCashOriginalAsOf = time.Time{}
	}
	semantic.AccountFingerprint, semantic.PositionsFingerprint = "", ""
	raw, err := json.Marshal(semantic)
	if err != nil {
		return err
	}
	digestBytes := sha256.Sum256(raw)
	digest := hex.EncodeToString(digestBytes[:])
	scopeHash := sha256.Sum256([]byte(scope.Account + "|" + scope.Mode))
	scopeKey := "cash_sweep:" + hex.EncodeToString(scopeHash[:])
	for range 3 {
		doc, exists, err := e.store.core.GetStateDocument(ctx, scopeKey, cashSweepTraceKind)
		if err != nil {
			return err
		}
		state := cashSweepTraceState{}
		if exists {
			if err := json.Unmarshal(doc.JSON, &state); err != nil {
				return err
			}
		}
		if state.Digest == digest {
			st.DecisionTrace, st.TraceState = rpc.CloneCashSweepDecisionTrace(state.Recent), "recorded"
			return nil
		}
		state.Digest = digest
		state.Recent = append([]rpc.CashSweepDecisionTrace{trace}, state.Recent...)
		if len(state.Recent) > 20 {
			state.Recent = state.Recent[:20]
		}
		stateRaw, err := json.Marshal(state)
		if err != nil {
			return err
		}
		eventRaw, err := json.Marshal(trace)
		if err != nil {
			return err
		}
		_, _, err = e.store.core.CompareAndSwapStateDocumentWithEvents(ctx,
			corestore.StateDocumentCAS{ScopeKey: scopeKey, Kind: cashSweepTraceKind, ExpectedRevision: doc.Revision, JSON: stateRaw},
			[]corestore.EventInput{{ScopeKey: scopeKey, EventKey: fmt.Sprintf("cash-sweep-%d", doc.Revision+1), Type: cashSweepTraceEventType,
				Action: "planned", Origin: "daemon", OccurredAt: now, PayloadJSON: eventRaw}})
		if err != nil {
			if _, ok := errors.AsType[*corestore.RevisionConflictError](err); ok {
				continue
			}
			return err
		}
		st.DecisionTrace, st.TraceState = rpc.CloneCashSweepDecisionTrace(state.Recent), "recorded"
		return nil
	}
	return errors.New("sweep decision trace revision conflict")
}
