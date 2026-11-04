package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/osauer/canary/v2/internal/daemon/corestore"
	"github.com/osauer/canary/v2/internal/rpc"
	ibkrlib "github.com/osauer/canary/v2/pkg/ibkr"
)

// This metadata commits with the exercise attempt and token consumption. A
// flushed frame is not completion: capacity remains reserved after restart.
type exerciseReservation struct {
	Option              rpc.ContractParams `json:"option"`
	Underlying          rpc.ContractParams `json:"underlying"`
	Quantity            int                `json:"quantity"`
	UnderlyingAction    string             `json:"underlying_action"`
	UnderlyingBefore    float64            `json:"underlying_before"`
	UnderlyingAfter     float64            `json:"underlying_after"`
	OptionBefore        float64            `json:"option_before"`
	PortfolioGeneration uint64             `json:"portfolio_generation"`
}

type pendingExerciseRecord struct {
	Event    orderJournalEvent
	Revision string
}

type exerciseResolution struct {
	Broker      string    `json:"broker,omitempty"`
	Account     string    `json:"account"`
	Mode        string    `json:"mode"`
	RequestRef  string    `json:"request_ref"`
	Revision    string    `json:"revision"`
	EvidenceID  string    `json:"evidence_id"`
	EvidenceKey string    `json:"evidence_key"`
	At          time.Time `json:"at"`
	Provenance  string    `json:"provenance"`
}

type exerciseResolutionDocument struct {
	Version     int                           `json:"version"`
	Resolutions map[string]exerciseResolution `json:"resolutions"`
}

const exerciseResolutionKind = "exercise_resolutions"

func (s *Server) exerciseResolutions(ctx context.Context) (exerciseResolutionDocument, int64, error) {
	out := exerciseResolutionDocument{Version: 1, Resolutions: map[string]exerciseResolution{}}
	var revision int64
	if s == nil || s.coreStore == nil {
		return out, 0, fmt.Errorf("exercise reconciliation durability unavailable")
	}
	err := s.coreStore.WithAcceptedStateDocument(ctx, daemonStateScope, exerciseResolutionKind, func(doc corestore.StateDocument, found bool) error {
		if !found {
			return nil
		}
		if err := decodeStrictPlatformSettingsJSON(doc.JSON, &out); err != nil || out.Version != 1 || out.Resolutions == nil {
			return fmt.Errorf("invalid exercise reconciliation document")
		}
		for key, resolved := range out.Resolutions {
			if strings.TrimSpace(key) == "" || strings.TrimSpace(resolved.RequestRef) == "" || strings.TrimSpace(resolved.Revision) == "" || len(resolved.EvidenceID) != 64 || strings.TrimSpace(resolved.EvidenceKey) == "" || resolved.At.IsZero() || resolved.Provenance != exerciseHumanEvidenceProvenance || !brokerScopeConcrete(recordScope(resolved.Broker, resolved.Account, resolved.Mode)) {
				return fmt.Errorf("invalid exercise reconciliation entry")
			}
		}
		revision = doc.Revision
		return nil
	})
	return out, revision, err
}

func (s *Server) pendingExercises(ctx context.Context, scope brokerStateScope) ([]pendingExerciseRecord, error) {
	if s == nil || s.orderJournal == nil || !brokerScopeConcrete(scope) {
		return nil, fmt.Errorf("exercise reservation authority unavailable")
	}
	events, err := s.orderJournal.LoadEvents(0)
	if err != nil {
		return nil, err
	}
	resolutions, _, err := s.exerciseResolutions(ctx)
	if err != nil {
		return nil, err
	}
	pending := map[string]orderJournalEvent{}
	for _, event := range events {
		if (event.ActionKind != corestore.ActionExercise && event.Action != rpc.OpportunityActionExercise) || !strings.EqualFold(event.Account, scope.Account) || !strings.EqualFold(event.Mode, scope.Mode) {
			continue
		}
		key := event.AttemptID
		if key == "" {
			key = event.PreviewTokenID + "|" + event.OrderRef
		}
		if event.Type == orderJournalEventSendAttempted {
			pending[key] = event
		}
		if event.Type == orderJournalEventSendError && event.SendDisposition == ibkrlib.SendDispositionDefinitelyUnsent {
			delete(pending, key)
		}
	}
	var out []pendingExerciseRecord
	for _, event := range pending {
		raw, _ := json.Marshal(event)
		revision := shortStableHash(string(raw))
		if resolved, ok := resolutions.Resolutions[event.PreviewTokenID]; ok {
			if resolved.RequestRef != event.OrderRef || resolved.Revision != revision || !sameBrokerScope(recordScope(resolved.Broker, resolved.Account, resolved.Mode), scope) {
				return nil, fmt.Errorf("exercise resolution does not match reserved request")
			}
			continue
		}
		out = append(out, pendingExerciseRecord{Event: event, Revision: revision})
	}
	return out, nil
}

func exerciseReservationConflicts(event orderJournalEvent, contract rpc.ContractParams, action string) bool {
	reservation := event.ExerciseReservation
	// Owner instruction 2026-10-23 12:57 CEST: keep uncertain legacy capacity blocked,
	// while allowing provably unrelated instruments. Old journals lack exact
	// underlying identity, so their known symbol conservatively covers both sides.
	if reservation == nil || reservation.Underlying.ConID <= 0 || reservation.Option.ConID <= 0 {
		if contract.ConID <= 0 {
			return true
		}
		if event.ConID > 0 && contract.ConID == event.ConID && strings.EqualFold(action, rpc.OrderActionSell) {
			return true
		}
		symbol, candidate := strings.TrimSpace(event.Symbol), strings.TrimSpace(contract.Symbol)
		return symbol == "" || candidate == "" || strings.EqualFold(symbol, candidate)
	}
	if contract.ConID <= 0 {
		return true
	}
	return contract.ConID == reservation.Option.ConID && strings.EqualFold(action, rpc.OrderActionSell) || contract.ConID == reservation.Underlying.ConID && strings.EqualFold(action, reservation.UnderlyingAction)
}

func (s *Server) pendingExerciseOrderBlocker(ctx context.Context, status rpc.TradingStatus, draft rpc.OrderDraft) error {
	pending, err := s.pendingExercises(ctx, recordScope(status.Broker, status.Account, status.Mode))
	if err != nil {
		return fmt.Errorf("%w: read exercise reservations: %v", ErrTradingDisabled, err)
	}
	for _, record := range pending {
		conflict := exerciseReservationConflicts(record.Event, draft.Contract, draft.Action)
		if draft.StrategyGroup != nil {
			conflict = false
			for _, leg := range draft.StrategyGroup.Legs {
				conflict = conflict || exerciseReservationConflicts(record.Event, leg.Contract, leg.Action)
			}
		}
		if conflict {
			return fmt.Errorf("%w: exercise %s is unresolved; its option and underlying capacity remain reserved; use opportunities exercises", ErrTradingDisabled, record.Event.OrderRef)
		}
	}
	return nil
}

func (s *Server) exerciseCompetingOrders(ctx context.Context, status rpc.TradingStatus, opp rpc.Opportunity, underlying rpc.OrderDraft) error {
	if err := s.pendingExerciseOrderBlocker(ctx, status, underlying); err != nil {
		return err
	}
	if err := s.pendingExerciseOrderBlocker(ctx, status, rpc.OrderDraft{Contract: opp.Contract, Action: rpc.OrderActionSell}); err != nil {
		return err
	}
	snapshot, scope, err := s.brokerOpenOrderInventory(ctx, true)
	if err != nil || !snapshot.Complete || scope.Account != status.Account || scope.Mode != status.Mode {
		return fmt.Errorf("%w: complete current order inventory is required before exercise", ErrTradingDisabled)
	}
	journal, ok := s.journalOrderViewsForInventory()
	if !ok {
		return fmt.Errorf("%w: exercise order evidence unavailable", ErrTradingDisabled)
	}
	competes := func(identity workingOrderIdentity) bool {
		if strings.EqualFold(identity.SecType, "BAG") {
			if identity.Legs == nil {
				return true
			}
			for _, leg := range identity.Legs {
				if exerciseReservationConflicts(orderJournalEvent{ExerciseReservation: &exerciseReservation{Option: opp.Contract, Underlying: opp.UnderlyingContract, UnderlyingAction: underlying.Action}}, leg.Contract, leg.Action) {
					return true
				}
			}
			return false
		}
		return identity.sameContract(opp.Contract) && strings.EqualFold(identity.Action, rpc.OrderActionSell) || identity.sameContract(opp.UnderlyingContract) && strings.EqualFold(identity.Action, underlying.Action)
	}
	for _, row := range brokerWorkingOrders(snapshot, journal.views, scope) {
		identity := brokerOrderIdentity(row.Order)
		identity.Legs = journal.legsOf(row.Journal)
		if competes(identity) {
			return fmt.Errorf("%w: a working order competes for this exercise's option or underlying capacity", ErrTradingDisabled)
		}
	}
	// Local unacknowledged and uncertain sends remain capacity claims in
	// either direction. Mere age or broker inventory absence cannot settle them.
	for i := range journal.views {
		view := &journal.views[i]
		if !orderViewMatchesBrokerScope(*view, scope) || (!view.Open && view.LifecycleStatus != rpc.OrderLifecycleUnknownReconcileRequired) || view.Action == rpc.OpportunityActionExercise {
			continue
		}
		identity := journalOrderIdentity(*view)
		identity.Legs = journal.legsOf(view)
		if competes(identity) {
			return fmt.Errorf("%w: an unresolved journaled order competes for this exercise's option or underlying capacity", ErrTradingDisabled)
		}
	}
	return nil
}

// Exercise has no OCA linkage. A fresh request-correlated positions response
// must confirm both sides after all observed fills, including external fills
// with no Canary preview token. The captured lifecycle frontier survives to
// the first-byte guard through reduceFillEvidence.
func (s *Server) exercisePositionSnapshot(ctx context.Context, frontier protectionOrderSnapshotBinding, opp rpc.Opportunity) error {
	if opp.PositionQuantity < float64(opp.Quantity) || opp.PositionQuantity <= 0 {
		return fmt.Errorf("%w: exact long option quantity is unavailable", ErrTradingDisabled)
	}
	return s.exercisePositionQuantities(ctx, frontier, opp.Contract, opp.UnderlyingContract, opp.PositionQuantity, opp.UnderlyingQuantityBefore)
}

func (s *Server) exercisePositionQuantities(ctx context.Context, frontier protectionOrderSnapshotBinding, option, underlying rpc.ContractParams, optionQuantity, underlyingQuantity float64) error {
	var positions []*ibkrlib.RawPosition
	var err error
	if s.reduceFillSnapshotForTest != nil {
		positions, err = s.reduceFillSnapshotForTest(ctx, orderPreviewDefaultWait)
	} else if frontier.connector != nil {
		positions, err = frontier.connector.RefreshPositionsForSession(ctx, frontier.session, orderPreviewDefaultWait)
	} else {
		err = fmt.Errorf("broker positions snapshot unavailable")
	}
	if err != nil {
		return fmt.Errorf("%w: exercise positions cannot be confirmed: %v", ErrTradingDisabled, err)
	}
	if !s.reduceFillBindingCurrent(frontier) {
		return fmt.Errorf("%w: broker orders changed during exercise admission; preview again", ErrTradingDisabled)
	}
	quantities := map[int]float64{}
	for _, position := range positions {
		if position == nil || !strings.EqualFold(position.Account, frontier.scope.Account) {
			continue
		}
		id := position.Contract.ConID
		if id != option.ConID && id != underlying.ConID {
			continue
		}
		if _, exists := quantities[id]; exists || math.IsNaN(position.Position) || math.IsInf(position.Position, 0) {
			return fmt.Errorf("%w: exercise positions snapshot is ambiguous", ErrTradingDisabled)
		}
		quantities[id] = position.Position
	}
	if quantities[option.ConID] != optionQuantity || quantities[underlying.ConID] != underlyingQuantity {
		return fmt.Errorf("%w: broker option or underlying holdings changed after exercise preview", ErrTradingDisabled)
	}
	return nil
}
