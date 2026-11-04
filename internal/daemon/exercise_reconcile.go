package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"slices"
	"strings"
	"time"

	"github.com/osauer/canary/v2/internal/daemon/corestore"
	"github.com/osauer/canary/v2/internal/flexstmt"
	"github.com/osauer/canary/v2/internal/rpc"
)

const exerciseHumanEvidenceProvenance = "human-attested broker evidence"

type exerciseEvidence struct {
	ID  string
	Key string
	At  time.Time
}

func (s *Server) exerciseEvidenceSnapshot(ctx context.Context) (corestore.StatementProjectionSnapshot, error) {
	if s.coreStore == nil || !validFlexQueryFingerprint(s.configuredFlexQueryFingerprint()) {
		return corestore.StatementProjectionSnapshot{}, fmt.Errorf("authenticated active-query IBKR Flex evidence is unavailable")
	}
	snapshot, err := s.coreStore.LoadStatementProjectionSnapshot(ctx, s.activeStatementProjectionScope(), statementProjectionMaxRows*25, statementProjectionMaxRows)
	if err != nil {
		return snapshot, err
	}
	if len(snapshot.Records) >= statementProjectionMaxRows*25 {
		return snapshot, fmt.Errorf("exercise evidence inventory is incomplete")
	}
	return snapshot, nil
}

func matchingExerciseEvidence(record pendingExerciseRecord, snapshot corestore.StatementProjectionSnapshot, resolutions exerciseResolutionDocument) []exerciseEvidence {
	e, r := record.Event, record.Event.ExerciseReservation
	if r == nil || r.Quantity <= 0 || r.Option.ConID <= 0 || r.Underlying.ConID <= 0 || r.OptionBefore < float64(r.Quantity) {
		return nil
	}
	used := map[string]bool{}
	for _, resolution := range resolutions.Resolutions {
		used[resolution.EvidenceKey] = true
	}
	var out []exerciseEvidence
	for _, item := range snapshot.Records {
		if item.Kind != corestore.StatementRecordOptionEvent || !strings.EqualFold(item.AccountKey, e.Account) || item.StatementFileSHA256 == ([sha256.Size]byte{}) || !item.GeneratedAt.After(e.At) {
			continue
		}
		var option flexstmt.OptionEvent
		if json.Unmarshal(item.RawJSON, &option) != nil || option.TransactionType != "EXERCISE" || option.AssetClass != "OPT" || !strings.EqualFold(option.AccountID, e.Account) || option.ConID != int64(r.Option.ConID) || option.UnderlyingConID != int64(r.Underlying.ConID) || option.Quantity == nil || math.Abs(*option.Quantity) != float64(r.Quantity) || option.TradeID == "" {
			continue
		}
		var matches []corestore.StatementRecord
		var trade flexstmt.Trade
		for _, candidate := range snapshot.Records {
			if candidate.Kind != corestore.StatementRecordTrade || !strings.EqualFold(candidate.AccountKey, e.Account) || candidate.StatementFileSHA256 == ([sha256.Size]byte{}) {
				continue
			}
			var row flexstmt.Trade
			if json.Unmarshal(candidate.RawJSON, &row) != nil || row.TradeID != option.TradeID || !strings.EqualFold(row.AccountID, e.Account) {
				continue
			}
			// Date-only trades cannot establish ordering against a timed request.
			local := row.ExecutedAt.In(flexstmt.StatementLocation())
			if row.ConID != int64(r.Option.ConID) || row.UnderlyingConID != int64(r.Underlying.ConID) || row.AssetClass != "OPT" || row.Quantity == nil || math.Abs(*row.Quantity) != float64(r.Quantity) || row.Side != "SELL" || row.OpenClose != "C" || row.ExecutionID == "" || !row.ExecutedAt.After(e.At) || row.ExecutedAt.After(candidate.GeneratedAt) || local.Hour() == 0 && local.Minute() == 0 && local.Second() == 0 {
				continue
			}
			matches = append(matches, candidate)
			trade = row
		}
		if len(matches) != 1 {
			continue
		}
		key := strings.ToUpper(e.Account) + "|" + trade.ExecutionID + "|" + option.TradeID
		if used[key] {
			continue
		}
		digest := sha256.New()
		for _, part := range [][]byte{[]byte(key), item.StatementFileSHA256[:], matches[0].StatementFileSHA256[:], item.RawJSON, matches[0].RawJSON} {
			_, _ = digest.Write(part)
		}
		evidence := exerciseEvidence{ID: hex.EncodeToString(digest.Sum(nil)), Key: key, At: trade.ExecutedAt}
		if !slices.ContainsFunc(out, func(v exerciseEvidence) bool { return v.Key == key }) {
			out = append(out, evidence)
		}
	}
	return out
}

func (s *Server) handleOpportunitiesExercises(ctx context.Context) (*rpc.ExercisePendingResult, error) {
	scope := s.currentBrokerStateScope()
	pending, err := s.pendingExercises(ctx, scope)
	if err != nil {
		return nil, err
	}
	resolutions, _, err := s.exerciseResolutions(ctx)
	if err != nil {
		return nil, err
	}
	snapshot, evidenceErr := s.exerciseEvidenceSnapshot(ctx)
	out := &rpc.ExercisePendingResult{Requests: []rpc.ExercisePendingRequest{}, AsOf: s.orderNow()}
	for _, record := range pending {
		event := record.Event
		row := rpc.ExercisePendingRequest{RequestRef: event.OrderRef, Revision: record.Revision, State: "pending", Symbol: event.Symbol, Quantity: int(event.Quantity), SubmittedAt: event.At, Evidence: []rpc.ExerciseEvidenceCandidate{}}
		if event.ExerciseReservation != nil {
			row.Option = event.ExerciseReservation.Option
			row.Underlying = event.ExerciseReservation.Underlying
		}
		if event.ExerciseReservation == nil {
			row.Blockers = []rpc.TradingBlocker{{Code: "exercise_legacy_identity", Message: "This historical request lacks exact option and underlying position authority. Its known symbol remains reserved; automatic evidence reconciliation is unsupported."}}
		} else if evidenceErr != nil {
			row.Blockers = []rpc.TradingBlocker{{Code: "exercise_evidence_unavailable", Message: evidenceErr.Error()}}
		} else {
			evidence := matchingExerciseEvidence(record, snapshot, resolutions)
			if len(evidence) != 1 {
				row.Blockers = []rpc.TradingBlocker{{Code: "exercise_evidence_unconfirmed", Message: "A unique complete IBKR exercise record and linked timed trade are required; unsupported cancellation and ambiguous evidence leave this request pending."}}
			} else {
				row.Evidence = append(row.Evidence, rpc.ExerciseEvidenceCandidate{ID: evidence[0].ID, At: evidence[0].At})
			}
		}
		out.Requests = append(out.Requests, row)
	}
	slices.SortFunc(out.Requests, func(a, b rpc.ExercisePendingRequest) int { return strings.Compare(a.RequestRef, b.RequestRef) })
	return out, nil
}

func (s *Server) exercisePositionsReflected(ctx context.Context, scope brokerStateScope, record pendingExerciseRecord) (daemonBrokerEvidenceBinding, error) {
	binding, ok := s.captureOrderIntegrityEvidenceBinding()
	if !ok && s.reduceFillSnapshotForTest != nil && s.stableBrokerEvidenceForTest != nil {
		binding.scope, ok = scope, true
	}
	if !ok || !sameBrokerScope(binding.scope, scope) {
		return binding, fmt.Errorf("current broker evidence unavailable")
	}
	r := record.Event.ExerciseReservation
	if r == nil {
		return binding, fmt.Errorf("legacy exercise lacks exact position authority")
	}
	frontier := s.reduceFillBinding()
	if binding.connector != nil && (frontier.connector != binding.connector || frontier.session != binding.broker.Session || frontier.generation != binding.broker.OrderLifecycleGeneration) {
		return binding, fmt.Errorf("exercise broker evidence changed")
	}
	if err := s.exercisePositionQuantities(ctx, frontier, r.Option, r.Underlying, r.OptionBefore-float64(r.Quantity), r.UnderlyingAfter); err != nil {
		return binding, err
	}
	// Keep the pre-request frontier: a callback or reconnect during/after
	// receipt invalidates the final commit even if the quantities happen to match.
	return binding, nil
}

func (s *Server) handleOpportunitiesReconcileExercise(ctx context.Context, req *rpc.Request) (*rpc.ExerciseReconcileResult, error) {
	var p rpc.ExerciseReconcileParams
	if err := decodeParams(req.Params, &p); err != nil {
		return nil, err
	}
	if p.Origin != rpc.OrderOriginHumanTTY {
		return nil, fmt.Errorf("exercise reconciliation requires a human terminal; agents and MCP cannot attest broker completion")
	}
	if strings.TrimSpace(p.RequestRef) == "" || strings.TrimSpace(p.Revision) == "" || strings.TrimSpace(p.EvidenceID) == "" || !p.ConfirmComplete {
		return nil, errBadRequest("request, revision, evidence and explicit confirmation of complete resolution with no remainder pending are required")
	}
	s.brokerWriteMu.Lock()
	defer s.brokerWriteMu.Unlock()
	scope := s.currentBrokerStateScope()
	if !brokerScopeConcrete(scope) {
		return nil, fmt.Errorf("current account and paper/live mode are required")
	}
	resolutions, revision, err := s.exerciseResolutions(ctx)
	if err != nil {
		return nil, err
	}
	for _, resolved := range resolutions.Resolutions {
		if resolved.RequestRef == p.RequestRef && sameBrokerScope(recordScope(resolved.Broker, resolved.Account, resolved.Mode), scope) {
			if resolved.Revision != p.Revision || resolved.EvidenceID != p.EvidenceID {
				return nil, errBadRequest("exercise was resolved with different evidence or revision")
			}
			return &rpc.ExerciseReconcileResult{Accepted: true, Replay: true, RequestRef: p.RequestRef, Revision: p.Revision, EvidenceID: p.EvidenceID, Provenance: resolved.Provenance, AsOf: resolved.At}, nil
		}
	}
	pending, err := s.pendingExercises(ctx, scope)
	if err != nil {
		return nil, err
	}
	var selected *pendingExerciseRecord
	for i := range pending {
		if pending[i].Event.OrderRef == p.RequestRef {
			if selected != nil {
				return nil, fmt.Errorf("exercise request reference is ambiguous")
			}
			selected = &pending[i]
		}
	}
	if selected == nil || selected.Revision != p.Revision {
		return nil, errBadRequest("pending exercise request or revision changed; list exercises again")
	}
	snapshot, err := s.exerciseEvidenceSnapshot(ctx)
	if err != nil {
		return nil, err
	}
	evidence := matchingExerciseEvidence(*selected, snapshot, resolutions)
	if len(evidence) != 1 || evidence[0].ID != p.EvidenceID {
		return nil, errBadRequest("evidence is missing, reused, partial, ambiguous, or does not exactly match this exercise; reservation remains pending")
	}
	// One broker event must not plausibly resolve multiple outstanding requests.
	for _, other := range pending {
		if other.Event.PreviewTokenID == selected.Event.PreviewTokenID {
			continue
		}
		for _, candidate := range matchingExerciseEvidence(other, snapshot, resolutions) {
			if candidate.Key == evidence[0].Key {
				return nil, errBadRequest("broker evidence matches more than one pending request; reservation remains pending")
			}
		}
	}
	binding, err := s.exercisePositionsReflected(ctx, scope, *selected)
	if err != nil {
		return nil, err
	}
	resolution := exerciseResolution{Broker: scope.brokerField(), Account: scope.Account, Mode: scope.Mode, RequestRef: p.RequestRef, Revision: p.Revision, EvidenceID: p.EvidenceID, EvidenceKey: evidence[0].Key, At: s.orderNow(), Provenance: exerciseHumanEvidenceProvenance}
	resolutions.Resolutions[selected.Event.PreviewTokenID] = resolution
	raw, err := json.Marshal(resolutions)
	if err != nil {
		return nil, err
	}
	receipt, _ := json.Marshal(resolution)
	committed, err := s.withStableBrokerEvidence(binding, func() error {
		_, _, persistErr := s.coreStore.CompareAndSwapStateDocumentWithEvents(ctx, corestore.StateDocumentCAS{ScopeKey: daemonStateScope, Kind: exerciseResolutionKind, ExpectedRevision: revision, ExpectedHead: &snapshot.Head, JSON: raw}, []corestore.EventInput{{ScopeKey: daemonStateScope, EventKey: "exercise-resolved:" + selected.Event.PreviewTokenID, Type: "exercise_resolved", Action: coreEventActionUpdate, Origin: rpc.OrderOriginHumanTTY, OccurredAt: resolution.At, PayloadJSON: receipt}})
		return persistErr
	})
	if !committed && err == nil {
		err = fmt.Errorf("broker evidence changed")
	}
	if err != nil {
		return nil, fmt.Errorf("exercise evidence or authority changed; reservation remains pending: %w", err)
	}
	return &rpc.ExerciseReconcileResult{Accepted: true, RequestRef: p.RequestRef, Revision: p.Revision, EvidenceID: p.EvidenceID, Provenance: resolution.Provenance, AsOf: resolution.At}, nil
}
