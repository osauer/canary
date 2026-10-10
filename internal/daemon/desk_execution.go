package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/osauer/canary/v2/internal/daemon/corestore"
	"github.com/osauer/canary/v2/internal/rpc"
)

// The Desk execution journal (internal-docs/design/desk-execution.md) records
// every request before anything can reach the broker, in the core store's
// append-only event log, whose (scope, key) pairs are unique:
//
//	desk-exec:<account>:<mode>:request:<request id>   intent digest, episode
//	desk-exec:<account>:<mode>:episode:<episode id>   the request that consumed it
//	desk-exec:<account>:<mode>:outcome:<request id>:<accepted|refused|unknown>
//
// The request and episode rows commit together, so an episode yields at most
// one request even across restarts. Because the request row precedes any
// dispatch, its absence proves that nothing was sent for that ID.
const (
	deskExecutionEventType = "desk_execution"
	deskExecutionKeyPrefix = "desk-exec"
)

type deskExecutionRequestRow struct {
	RequestID string `json:"request_id"`
	EpisodeID string `json:"episode_id"`
	Digest    string `json:"digest"`
	Class     string `json:"class"`
	Authority string `json:"authority"`
}

type deskExecutionOutcomeRow struct {
	Outcome  string               `json:"outcome"`
	OrderRef string               `json:"order_ref,omitempty"`
	Blockers []rpc.TradingBlocker `json:"blockers,omitempty"`
	Message  string               `json:"message,omitempty"`
}

func deskExecutionKey(scope brokerStateScope, kind, id string, suffix ...string) string {
	return strings.Join(append([]string{deskExecutionKeyPrefix, scope.Account, scope.Mode, kind, id}, suffix...), ":")
}

func (s *Server) deskExecutionScope() (brokerStateScope, error) {
	status := s.currentTradingStatus()
	if status.Account == "" || (status.Mode != rpc.AccountModeLive && status.Mode != rpc.AccountModePaper) {
		return brokerStateScope{}, errors.New("no current account; Desk execution is unavailable")
	}
	return brokerStateScope{Account: status.Account, Mode: status.Mode}, nil
}

func (s *Server) deskExecutionEvent(ctx context.Context, key string, out any) (bool, error) {
	if s.coreStore == nil {
		return false, errors.New("the execution journal is unavailable")
	}
	row, ok, err := s.coreStore.GetEvent(ctx, daemonStateScope, key)
	if err != nil || !ok {
		return false, err
	}
	if err := json.Unmarshal(row.PayloadJSON, out); err != nil {
		return false, fmt.Errorf("execution journal row %s cannot be read: %w", key, err)
	}
	return true, nil
}

// recordDeskExecutionRequest journals one item before dispatch. It returns
// prior when the request ID was journaled before with the same intent (a
// retry: the caller answers with that request's outcome), and a refusal
// blocker when the ID was used with another intent or the episode was
// already consumed by another request. The check and the append are one
// step under deskExecutionMu.
func (s *Server) recordDeskExecutionRequest(ctx context.Context, scope brokerStateScope, item rpc.DeskExecutionItem, authority string) (prior bool, refusal *rpc.TradingBlocker, err error) {
	if err := item.Validate(); err != nil {
		return false, nil, errBadRequest(err.Error())
	}
	s.deskExecutionMu.Lock()
	defer s.deskExecutionMu.Unlock()
	digest := rpc.DeskIntentDigest(item)
	var existing deskExecutionRequestRow
	found, err := s.deskExecutionEvent(ctx, deskExecutionKey(scope, "request", item.RequestID), &existing)
	if err != nil {
		return false, nil, err
	}
	if found {
		if existing.Digest != digest {
			return false, &rpc.TradingBlocker{Code: rpc.DeskBlockerRequestConflict, Message: "this request ID was used before for a different order; nothing was sent"}, nil
		}
		return true, nil, nil
	}
	var consumer deskExecutionRequestRow
	consumed, err := s.deskExecutionEvent(ctx, deskExecutionKey(scope, "episode", item.EpisodeID), &consumer)
	if err != nil {
		return false, nil, err
	}
	if consumed {
		return false, &rpc.TradingBlocker{Code: rpc.DeskBlockerEpisodeConsumed, Message: "this signal already produced an order request; it yields no second order"}, nil
	}
	row := deskExecutionRequestRow{RequestID: item.RequestID, EpisodeID: item.EpisodeID, Digest: digest, Class: item.Intent.Class, Authority: authority}
	raw, err := json.Marshal(row)
	if err != nil {
		return false, nil, err
	}
	now := s.orderNow()
	_, err = s.coreStore.AppendEvents(ctx, []corestore.EventInput{
		{ScopeKey: daemonStateScope, EventKey: deskExecutionKey(scope, "request", item.RequestID), Type: deskExecutionEventType, Action: "request", Origin: coreEventOriginDaemon, OccurredAt: now, PayloadJSON: raw},
		{ScopeKey: daemonStateScope, EventKey: deskExecutionKey(scope, "episode", item.EpisodeID), Type: deskExecutionEventType, Action: "episode", Origin: coreEventOriginDaemon, OccurredAt: now, PayloadJSON: raw},
	})
	return false, nil, err
}

// recordDeskExecutionOutcome journals what became of a request. Accepted and
// refused are final; unknown may later be resolved by either.
func (s *Server) recordDeskExecutionOutcome(ctx context.Context, scope brokerStateScope, requestID string, out deskExecutionOutcomeRow) error {
	switch out.Outcome {
	case rpc.DeskOutcomeAccepted, rpc.DeskOutcomeRefused, rpc.DeskOutcomeUnknown:
	default:
		return fmt.Errorf("outcome %q cannot be journaled", out.Outcome)
	}
	raw, err := json.Marshal(out)
	if err != nil {
		return err
	}
	_, err = s.coreStore.AppendEvents(ctx, []corestore.EventInput{{ScopeKey: daemonStateScope, EventKey: deskExecutionKey(scope, "outcome", requestID, out.Outcome),
		Type: deskExecutionEventType, Action: "outcome", Origin: coreEventOriginDaemon, OccurredAt: s.orderNow(), PayloadJSON: raw}})
	return err
}

// deskExecutionReceipt reads one request's receipt from the journal. A
// journaled request without a final outcome is unknown: it may have reached
// the broker.
func (s *Server) deskExecutionReceipt(ctx context.Context, scope brokerStateScope, requestID string, now time.Time) (rpc.DeskExecutionReceipt, error) {
	r := rpc.DeskExecutionReceipt{RequestID: requestID, AsOf: now}
	var req deskExecutionRequestRow
	found, err := s.deskExecutionEvent(ctx, deskExecutionKey(scope, "request", requestID), &req)
	if err != nil {
		return r, err
	}
	if !found {
		r.Outcome = rpc.DeskOutcomeAbsent
		r.Message = "no request with this ID was recorded for the current account; nothing was sent for it"
		return r, nil
	}
	r.EpisodeID, r.Digest, r.Class = req.EpisodeID, req.Digest, req.Class
	for _, outcome := range []string{rpc.DeskOutcomeAccepted, rpc.DeskOutcomeRefused, rpc.DeskOutcomeUnknown} {
		var row deskExecutionOutcomeRow
		ok, err := s.deskExecutionEvent(ctx, deskExecutionKey(scope, "outcome", requestID, outcome), &row)
		if err != nil {
			return r, err
		}
		if ok {
			r.Outcome, r.OrderRef, r.Blockers, r.Message = row.Outcome, row.OrderRef, row.Blockers, row.Message
			return r, nil
		}
	}
	r.Outcome = rpc.DeskOutcomeUnknown
	r.Message = "the request was recorded before sending and no outcome was recorded; it may have reached the broker"
	return r, nil
}

func (s *Server) handleDeskExecutionCapabilities() rpc.DeskExecutionCapabilitiesResult {
	// Submit and batch preparation are listed only once they are served;
	// Desk treats their absence as automatic and batch sending unavailable.
	return rpc.DeskExecutionCapabilitiesResult{Version: rpc.DeskExecutionContractVersion,
		Methods: []string{rpc.MethodDeskExecutionCapabilities, rpc.MethodDeskExecutionLookup}, Authorities: []string{}, AsOf: s.orderNow()}
}

func (s *Server) handleDeskExecutionLookup(ctx context.Context, req *rpc.Request) (rpc.DeskExecutionLookupResult, error) {
	var in rpc.DeskExecutionLookupParams
	if err := decodeDeskAuthority(req.Params, &in); err != nil {
		return rpc.DeskExecutionLookupResult{}, err
	}
	if len(in.RequestIDs) == 0 || len(in.RequestIDs) > 100 {
		return rpc.DeskExecutionLookupResult{}, errBadRequest("name 1 to 100 request IDs")
	}
	scope, err := s.deskExecutionScope()
	if err != nil {
		return rpc.DeskExecutionLookupResult{}, err
	}
	now := s.orderNow()
	out := rpc.DeskExecutionLookupResult{Receipts: make([]rpc.DeskExecutionReceipt, 0, len(in.RequestIDs)), AsOf: now}
	for _, id := range in.RequestIDs {
		if id == "" || len(id) > 128 {
			return rpc.DeskExecutionLookupResult{}, errBadRequest("request IDs are 1 to 128 bytes")
		}
		r, err := s.deskExecutionReceipt(ctx, scope, id, now)
		if err != nil {
			return rpc.DeskExecutionLookupResult{}, err
		}
		out.Receipts = append(out.Receipts, r)
	}
	return out, nil
}
