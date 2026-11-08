package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/osauer/canary/v2/internal/daemon/corestore"
	"github.com/osauer/canary/v2/internal/rpc"
	"github.com/osauer/canary/v2/internal/rpc/api"
	ibkrlib "github.com/osauer/canary/v2/pkg/ibkr"
)

// TWS order control (owner decision "TWS B", 2026-10-11 19:36 CEST): an
// owner-confirmed cancel or change of an order placed by hand in TWS runs
// over a separate client-0 connection that pkg/ibkr opens for that one
// request (ibkrlib.RunTWSOrderControl). This file is the daemon's side: the
// runtime switch and its paper proof, the eligibility it lends to untracked
// rows, the ordinary broker-write gates, the identity check against the
// broker's open-order inventory, the request journal that makes a repeat
// safe, and the receipt.
//
// The journal lives in the core store's append-only event log, whose
// (scope, key) pairs are unique:
//
//	tws-control:<account>:<mode>:request:<request id>   action, digest, origin
//	tws-control:<account>:<mode>:outcome:<request id>   what became of it
//
// The request row precedes any socket, so its absence proves nothing was
// sent for that id; a request row without an outcome row is uncertain.
const (
	twsOrderControlEventType = "tws_order_control"
	twsOrderControlKeyPrefix = "tws-control"
	// twsOrderControlJournalOrigin marks every row of this path: the order
	// was bound from TWS, never placed by Canary.
	twsOrderControlJournalOrigin = "bound_from_tws"
)

type twsOrderControlRequestRow struct {
	RequestID string                          `json:"request_id"`
	Action    string                          `json:"action"`
	Digest    string                          `json:"digest"`
	PermID    int                             `json:"perm_id"`
	Origin    string                          `json:"origin"`
	Bound     string                          `json:"bound"` // twsOrderControlJournalOrigin
	Terms     rpc.OrderTWSControlTerms        `json:"terms"`
	Change    *rpc.OrderTWSControlReplacement `json:"replacement,omitempty"`
}

type twsOrderControlOutcomeRow struct {
	Outcome      string               `json:"outcome"`
	BoundOrderID int                  `json:"bound_order_id,omitempty"`
	BrokerStatus string               `json:"broker_status,omitempty"`
	BrokerCode   int                  `json:"broker_code,omitempty"`
	Blockers     []rpc.TradingBlocker `json:"blockers,omitempty"`
	Message      string               `json:"message,omitempty"`
}

// twsOrderControlAvailability is the switch, its proof and the verdict for
// one trading mode.
type twsOrderControlAvailability struct {
	Enabled       bool
	PaperProvenAt time.Time
	Available     bool
	Status        string
}

// twsOrderControlAvailability reads the runtime switch and says whether the
// path may run in mode: paper once enabled; live once enabled and proven on
// paper, in a build with broker writes.
func (s *Server) twsOrderControlAvailability(mode string) twsOrderControlAvailability {
	out := twsOrderControlAvailability{Status: rpc.TWSOrderControlStatusOff}
	if s == nil || s.platformSettings == nil {
		return out
	}
	ctl := s.platformSettings.snapshot().Trading.TWSOrderControl
	if ctl.Enabled != nil {
		out.Enabled = *ctl.Enabled
	}
	if ctl.PaperProvenAt != nil {
		out.PaperProvenAt = ctl.PaperProvenAt.UTC()
	}
	if !out.Enabled {
		return out
	}
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case rpc.AccountModePaper:
		out.Available, out.Status = true, rpc.TWSOrderControlStatusAvailable
	case rpc.AccountModeLive:
		switch {
		case !orderWritesAvailable:
			out.Status = "unavailable: this build has no broker writes"
		case out.PaperProvenAt.IsZero():
			out.Status = rpc.TWSOrderControlStatusProofRequired
		default:
			out.Available, out.Status = true, rpc.TWSOrderControlStatusAvailable
		}
	default:
		out.Status = "unavailable: trading mode is " + nonEmptyOrderText(mode, "disabled")
	}
	return out
}

func (s *Server) twsOrderControlSettings(mode string) rpc.TWSOrderControlSettings {
	avail := s.twsOrderControlAvailability(mode)
	provenAt := ""
	if !avail.PaperProvenAt.IsZero() {
		provenAt = avail.PaperProvenAt.Format(time.RFC3339)
	}
	return rpc.TWSOrderControlSettings{
		Enabled:       settingsBool(avail.Enabled, rpc.SettingsAccessWrite, rpc.SettingsSourceRuntime, "lets an owner-confirmed cancel or change reach an order placed by hand in TWS over a separate client-0 connection; the order becomes an API order; human-only by policy"),
		PaperProvenAt: settingsString(provenAt, rpc.SettingsAccessRead, rpc.SettingsSourceRuntime, "recorded by the daemon when a control action on the paper account was confirmed; live stays off until then"),
		Status:        settingsString(avail.Status, rpc.SettingsAccessRead, rpc.SettingsSourceObserved, "whether the path may run in the current trading mode"),
	}
}

// markTWSOrderControlEligibility lends cancel and modify eligibility to the
// untracked rows the control path can reach: every working row with a
// permanent id may be cancelled; a DAY limit order on a stock or ETF may be
// changed. Without the path, nothing is eligible, as before.
func (s *Server) markTWSOrderControlEligibility(rows []rpc.OrderView, mode string) {
	avail := s.twsOrderControlAvailability(mode)
	if !avail.Available {
		return
	}
	for i := range rows {
		if rows[i].PermID <= 0 || !rows[i].Open || orderLifecycleStatusIsTerminal(rows[i].LifecycleStatus) {
			continue
		}
		rows[i].CancelEligible = true
		rows[i].ModifyEligible = twsOrderModifyEligible(rows[i])
	}
}

func twsOrderModifyEligible(row rpc.OrderView) bool {
	secType := strings.ToUpper(strings.TrimSpace(row.SecType))
	tif := strings.ToUpper(strings.TrimSpace(row.TIF))
	return (secType == "STK" || secType == "ETF") && strings.EqualFold(strings.TrimSpace(row.OrderType), rpc.OrderTypeLMT) &&
		(tif == "" || tif == rpc.OrderTIFDay) && row.LimitPrice > 0 && row.Quantity-row.Filled > 1e-9
}

func twsOrderControlKey(scope brokerStateScope, kind, id string) string {
	return strings.Join([]string{twsOrderControlKeyPrefix, scope.keyAccount(), scope.Mode, kind, id}, ":")
}

func (s *Server) handleOrderTWSCancel(ctx context.Context, req *rpc.Request) (*rpc.OrderTWSControlResult, error) {
	return s.handleOrderTWSControl(ctx, req, ibkrlib.TWSOrderControlCancel)
}

func (s *Server) handleOrderTWSModify(ctx context.Context, req *rpc.Request) (*rpc.OrderTWSControlResult, error) {
	return s.handleOrderTWSControl(ctx, req, ibkrlib.TWSOrderControlModify)
}

func (s *Server) handleOrderTWSControl(ctx context.Context, req *rpc.Request, action string) (*rpc.OrderTWSControlResult, error) {
	var p rpc.OrderTWSControlParams
	if err := decodeFor(api.OrderTWSCancel, req.Params, &p); err != nil {
		return nil, err
	}
	if err := p.Validate(action == ibkrlib.TWSOrderControlModify); err != nil {
		return nil, errBadRequest(err.Error())
	}
	// One broker write at a time, and the client-0 slot is one.
	s.brokerWriteMu.Lock()
	defer s.brokerWriteMu.Unlock()
	return s.twsOrderControl(ctx, action, p)
}

// twsOrderControl runs the gates, the identity check and the journal around
// one control request. A refusal is a result, not an error: the caller
// reads the outcome and the blockers. Errors are reserved for a request the
// daemon cannot read or journal.
func (s *Server) twsOrderControl(ctx context.Context, action string, p rpc.OrderTWSControlParams) (*rpc.OrderTWSControlResult, error) {
	origin := normalizedWriteOrigin(p.Origin)
	result := &rpc.OrderTWSControlResult{
		Action: action, RequestID: p.RequestID, Digest: rpc.OrderTWSControlDigest(action, p), PermID: p.PermID,
		Origin: origin, BoundFromTWS: true, Outcome: rpc.TWSOrderControlOutcomeRefused, AsOf: s.orderNow(),
	}
	refuse := func(code, message string) (*rpc.OrderTWSControlResult, error) {
		result.Blockers = appendTradingBlockerOnce(result.Blockers, rpc.TradingBlocker{Code: code, Message: message})
		result.Message = message
		result.AsOf = s.orderNow()
		return result, nil
	}

	auth, binding, bindingErr := s.authorizeBrokerWriteTransaction(p.Origin, action == ibkrlib.TWSOrderControlCancel)
	status := auth.Status
	result.Mode, result.Account, result.Endpoint = status.Mode, status.Account, status.Endpoint
	if !auth.Allowed {
		result.Blockers = append(result.Blockers, auth.Blockers...)
		result.Message = firstTradingBlockerMessage(auth.Blockers)
		return result, nil
	}
	if bindingErr != nil {
		return refuse(rpc.TWSOrderControlBlockerGate, bindingErr.Error())
	}
	avail := s.twsOrderControlAvailability(status.Mode)
	if !avail.Available {
		return refuse(rpc.TWSOrderControlBlockerOff, "orders placed in TWS cannot be changed from here: "+avail.Status)
	}
	scope := recordScope(status.Broker, status.Account, status.Mode)
	if !brokerScopeConcrete(scope) {
		return refuse(rpc.TWSOrderControlBlockerGate, "no concrete account and mode; nothing was sent")
	}

	// The journal first: a repeat answers from it and never reaches a socket.
	if p.RequestID != "" {
		prior, refusal, err := s.recordTWSOrderControlRequest(ctx, scope, action, p, origin)
		if err != nil {
			return refuse(rpc.TWSOrderControlBlockerJournalFailure, "the request journal is unavailable; nothing was sent: "+err.Error())
		}
		if refusal != nil {
			return refuse(refusal.Code, refusal.Message)
		}
		if prior != nil {
			return prior, nil
		}
	}

	// The order must be working at the broker outside the journal, as the
	// owner reviewed it. The inventory is the daemon's own all-client read;
	// the client-0 bind checks the same terms again on the wire.
	target, blocker := s.twsOrderControlTarget(ctx, scope, p)
	if blocker != nil {
		return s.finishTWSOrderControl(ctx, scope, p, refuseTWS(result, *blocker))
	}
	if p.RequestID == "" {
		// Journal every attempt, under a daemon-made id when the caller
		// brought none.
		id, err := randomTokenID()
		if err != nil {
			return refuse(rpc.TWSOrderControlBlockerJournalFailure, "prepare the request journal row: "+err.Error())
		}
		p.RequestID, result.RequestID = id, id
		if _, _, err := s.recordTWSOrderControlRequest(ctx, scope, action, p, origin); err != nil {
			return refuse(rpc.TWSOrderControlBlockerJournalFailure, "the request journal is unavailable; nothing was sent: "+err.Error())
		}
	}

	s.mu.Lock()
	ep := s.endpoint
	s.mu.Unlock()
	cfg := ibkrlib.TWSOrderControlConfig{
		Host: status.GatewayHost, Port: status.GatewayPort, Mode: status.Mode, Account: status.Account,
		UseTLS: ep.TLS, EnableTLSFallback: ep.EnableTLSFallback,
		Authority: binding.connector, AuthoritySession: binding.session,
		NewWireGuard: func() (func() error, func()) {
			return s.twsOrderControlWireGuard(binding, status, action == ibkrlib.TWSOrderControlCancel)
		},
	}
	if cfg.Host == "" {
		cfg.Host = ep.Host
	}
	if cfg.Port == 0 {
		cfg.Port = ep.Port
	}
	var replacement ibkrlib.TWSOrderControlReplacement
	if p.Replacement != nil {
		replacement = ibkrlib.TWSOrderControlReplacement{Quantity: p.Replacement.Quantity, LimitPrice: p.Replacement.LimitPrice}
	}
	run := s.twsOrderControlRun
	if run == nil {
		run = ibkrlib.RunTWSOrderControl
	}
	// Reject drift during inventory/journaling before opening client 0. Each
	// later physical send gets a fresh guard and a short settings lease.
	guard, release := cfg.NewWireGuard()
	gateErr := guard()
	release()
	if gateErr != nil {
		return s.finishTWSOrderControl(ctx, scope, p, refuseTWS(result, rpc.TradingBlocker{Code: rpc.TWSOrderControlBlockerGate, Message: gateErr.Error()}))
	}
	receipt, runErr := run(ctx, cfg, action, target, replacement)
	result.BoundOrderID = receipt.BoundOrderID
	result.BrokerStatus, result.BrokerCode = receipt.BrokerStatus, receipt.BrokerCode
	if receipt.Bound.PermID != 0 {
		view := untrackedOrderView(receipt.Bound, scope, s.orderNow())
		result.Order = &view
	}
	switch {
	case runErr == nil:
		result.Accepted = true
		result.Outcome = receipt.Outcome
		result.Message = twsOrderControlAcceptedMessage(action, receipt)
	case errors.Is(runErr, ibkrlib.ErrTWSOrderControlUncertain):
		result.Outcome = rpc.TWSOrderControlOutcomeUncertain
		result.Blockers = appendTradingBlockerOnce(result.Blockers, rpc.TradingBlocker{Code: rpc.TWSOrderControlBlockerUncertain, Message: runErr.Error(),
			Action: "Read the broker's open orders again before deciding anything; do not repeat the request."})
		result.Message = runErr.Error()
	default:
		result.Outcome = rpc.TWSOrderControlOutcomeRefused
		result.Blockers = appendTradingBlockerOnce(result.Blockers, rpc.TradingBlocker{Code: twsOrderControlBlockerCode(runErr), Message: runErr.Error()})
		result.Message = runErr.Error()
	}
	return s.finishTWSOrderControl(ctx, scope, p, result)
}

// twsOrderControlWireGuard retains the ordinary first-byte authority and
// additionally holds the manual-control switch stable through the send.
func (s *Server) twsOrderControlWireGuard(binding brokerWriteTransactionBinding, status rpc.TradingStatus, cancel bool) (func() error, func()) {
	guard, release := s.brokerWireGuard(binding, status, cancel)
	var unlockControl func()
	return func() error {
			if err := guard(); err != nil {
				return err
			}
			if s.platformSettings == nil {
				return fmt.Errorf("%w: manual order control unavailable", ErrTradingDisabled)
			}
			// The ordinary modify guard already holds this lease. Cancel bypasses
			// freeze, so acquire its switch lease separately without a generation check.
			if cancel {
				s.platformSettings.mu.RLock()
				unlockControl = s.platformSettings.mu.RUnlock
			}
			ctl := s.platformSettings.data.Trading.TWSOrderControl
			if ctl.Enabled == nil || !*ctl.Enabled || (status.Mode == rpc.AccountModeLive && (ctl.PaperProvenAt == nil || ctl.PaperProvenAt.IsZero())) {
				return fmt.Errorf("%w: manual order control disabled or unproved", ErrTradingDisabled)
			}
			return nil
		}, func() {
			if unlockControl != nil {
				unlockControl()
				unlockControl = nil
			}
			release()
		}
}

func refuseTWS(result *rpc.OrderTWSControlResult, blocker rpc.TradingBlocker) *rpc.OrderTWSControlResult {
	result.Outcome = rpc.TWSOrderControlOutcomeRefused
	result.Blockers = appendTradingBlockerOnce(result.Blockers, blocker)
	result.Message = blocker.Message
	return result
}

// finishTWSOrderControl journals the outcome and, after an accepted paper
// outcome, records the paper proof the live switch waits for.
func (s *Server) finishTWSOrderControl(ctx context.Context, scope brokerStateScope, p rpc.OrderTWSControlParams, result *rpc.OrderTWSControlResult) (*rpc.OrderTWSControlResult, error) {
	result.AsOf = s.orderNow()
	if p.RequestID != "" {
		if err := s.recordTWSOrderControlOutcome(ctx, scope, p.RequestID, twsOrderControlOutcomeRow{
			Outcome: result.Outcome, BoundOrderID: result.BoundOrderID, BrokerStatus: result.BrokerStatus, BrokerCode: result.BrokerCode,
			Blockers: result.Blockers, Message: result.Message,
		}); err != nil {
			s.warnf("tws order control: journal outcome for %s: %v", p.RequestID, err)
			result.Message = strings.TrimSpace(result.Message + " (the outcome could not be journaled: " + err.Error() + ")")
		}
	}
	if result.Accepted && strings.EqualFold(scope.Mode, rpc.AccountModePaper) {
		provenAt, err := s.recordTWSOrderControlPaperProof(ctx, p.Origin)
		if err != nil {
			s.warnf("tws order control: record paper proof: %v", err)
		} else {
			result.PaperProvenAt = provenAt
		}
	}
	return result, nil
}

// twsOrderControlTarget finds the reviewed order in the broker's all-client
// open-order inventory and refuses when the inventory is unavailable, the
// order is tracked by the journal (then the ordinary path owns it), absent,
// or different from the reviewed terms.
func (s *Server) twsOrderControlTarget(ctx context.Context, scope brokerStateScope, p rpc.OrderTWSControlParams) (ibkrlib.TWSOrderControlTarget, *rpc.TradingBlocker) {
	views, _, err := s.loadOrderViews()
	if err != nil {
		return ibkrlib.TWSOrderControlTarget{}, &rpc.TradingBlocker{Code: rpc.TWSOrderControlBlockerInventory, Message: "the order journal is unavailable; nothing was sent: " + err.Error()}
	}
	waitCtx, cancel := context.WithTimeout(ctx, ordersOpenInventoryWait)
	defer cancel()
	snapshot, inventoryScope, err := s.brokerOpenOrderInventory(waitCtx, false)
	if err != nil || !sameBrokerScope(inventoryScope, scope) {
		return ibkrlib.TWSOrderControlTarget{}, &rpc.TradingBlocker{Code: rpc.TWSOrderControlBlockerInventory, Message: "the broker's open-order list is unavailable; nothing was sent"}
	}
	for _, row := range brokerWorkingOrders(snapshot, views, scope) {
		if row.Order.PermID != p.PermID {
			continue
		}
		if row.Journal != nil {
			return ibkrlib.TWSOrderControlTarget{}, &rpc.TradingBlocker{Code: rpc.TWSOrderControlBlockerNotEligible, Message: "this order was placed through Canary; cancel or change it on the ordinary path"}
		}
		view := untrackedOrderView(row.Order, scope, snapshot.AsOf)
		if reason := twsOrderControlTermsDiffer(view, p.Terms); reason != "" {
			return ibkrlib.TWSOrderControlTarget{}, &rpc.TradingBlocker{Code: rpc.TWSOrderControlBlockerChanged, Message: "the order changed since it was reviewed: " + reason + "; nothing was sent"}
		}
		return ibkrlib.TWSOrderControlTarget{
			PermID: row.Order.PermID, ConID: row.Order.ConID, Symbol: row.Order.Symbol, SecType: row.Order.SecType, Action: row.Order.Action,
			Quantity: row.Order.TotalQuantity, Filled: row.Order.Filled, OrderType: row.Order.OrderType, LimitPrice: row.Order.LimitPrice,
			AuxPrice: row.Order.AuxPrice, TIF: row.Order.TIF,
		}, nil
	}
	return ibkrlib.TWSOrderControlTarget{}, &rpc.TradingBlocker{Code: rpc.TWSOrderControlBlockerNotFound, Message: "the order is not working at the broker outside Canary's journal; it may have filled or been cancelled; nothing was sent"}
}

func twsOrderControlTermsDiffer(view rpc.OrderView, terms rpc.OrderTWSControlTerms) string {
	text := func(name, got, want string) string {
		if want == "" || strings.EqualFold(strings.TrimSpace(got), strings.TrimSpace(want)) {
			return ""
		}
		return fmt.Sprintf("%s is %s, reviewed %s", name, nonEmptyOrderText(got, "empty"), want)
	}
	number := func(name string, got, want float64) string {
		if math.Abs(got-want) <= 1e-9 {
			return ""
		}
		return fmt.Sprintf("%s is %.4g, reviewed %.4g", name, got, want)
	}
	if terms.ConID != 0 && view.ConID != 0 && terms.ConID != view.ConID {
		return fmt.Sprintf("contract id is %d, reviewed %d", view.ConID, terms.ConID)
	}
	for _, reason := range []string{
		text("symbol", view.Symbol, terms.Symbol), text("security type", view.SecType, terms.SecType), text("side", view.Action, terms.Action),
		text("order type", view.OrderType, terms.OrderType), text("time in force", view.TIF, terms.TIF),
		number("quantity", view.Quantity, terms.Quantity), number("filled", view.Filled, terms.Filled), number("limit price", view.LimitPrice, terms.LimitPrice),
	} {
		if reason != "" {
			return reason
		}
	}
	return ""
}

func twsOrderControlBlockerCode(err error) string {
	switch {
	case errors.Is(err, ibkrlib.ErrTWSOrderControlSlotBusy):
		return rpc.TWSOrderControlBlockerSlotBusy
	case errors.Is(err, ibkrlib.ErrTWSOrderControlConnect):
		return rpc.TWSOrderControlBlockerConnect
	case errors.Is(err, ibkrlib.ErrTWSOrderControlBind):
		return rpc.TWSOrderControlBlockerBind
	case errors.Is(err, ibkrlib.ErrTWSOrderControlNotFound):
		return rpc.TWSOrderControlBlockerNotFound
	case errors.Is(err, ibkrlib.ErrTWSOrderControlNotWorking):
		return rpc.TWSOrderControlBlockerNotWorking
	case errors.Is(err, ibkrlib.ErrTWSOrderControlChanged):
		return rpc.TWSOrderControlBlockerChanged
	case errors.Is(err, ibkrlib.ErrTWSOrderControlRefused):
		return rpc.TWSOrderControlBlockerBrokerRefused
	default:
		return rpc.TWSOrderControlBlockerGate
	}
}

func twsOrderControlAcceptedMessage(action string, receipt ibkrlib.TWSOrderControlReceipt) string {
	what := "cancel"
	if action == ibkrlib.TWSOrderControlModify {
		what = "change"
	}
	switch receipt.Outcome {
	case ibkrlib.TWSOrderControlConfirmed:
		return fmt.Sprintf("TWS confirmed the %s of order %d (bound as API order %d); the client-0 connection is closed", what, receipt.PermID, receipt.BoundOrderID)
	default:
		return fmt.Sprintf("TWS acknowledged the %s of order %d (bound as API order %d, status %s) without a final status; the client-0 connection is closed", what, receipt.PermID, receipt.BoundOrderID, nonEmptyOrderText(receipt.BrokerStatus, "unknown"))
	}
}

func (s *Server) twsOrderControlEvent(ctx context.Context, key string, out any) (bool, error) {
	if s.coreStore == nil {
		return false, errors.New("the request journal is unavailable")
	}
	row, ok, err := s.coreStore.GetEvent(ctx, daemonStateScope, key)
	if err != nil || !ok {
		return false, err
	}
	if err := json.Unmarshal(row.PayloadJSON, out); err != nil {
		return false, fmt.Errorf("request journal row %s cannot be read: %w", key, err)
	}
	return true, nil
}

// recordTWSOrderControlRequest journals the request before any socket. A
// repeat with the same digest answers with the journaled outcome (prior),
// or an uncertain result when the outcome row is missing; a repeat with
// another digest is refused.
func (s *Server) recordTWSOrderControlRequest(ctx context.Context, scope brokerStateScope, action string, p rpc.OrderTWSControlParams, origin string) (prior *rpc.OrderTWSControlResult, refusal *rpc.TradingBlocker, err error) {
	if s.coreStore == nil {
		return nil, nil, errors.New("the request journal is unavailable")
	}
	digest := rpc.OrderTWSControlDigest(action, p)
	var existing twsOrderControlRequestRow
	found, err := s.twsOrderControlEvent(ctx, twsOrderControlKey(scope, "request", p.RequestID), &existing)
	if err != nil {
		return nil, nil, err
	}
	if found {
		if existing.Digest != digest {
			return nil, &rpc.TradingBlocker{Code: rpc.TWSOrderControlBlockerRequestReplay, Message: "this request ID was used before for a different order or change; nothing was sent"}, nil
		}
		result := &rpc.OrderTWSControlResult{Action: action, RequestID: p.RequestID, Digest: digest, PermID: p.PermID, Origin: existing.Origin,
			BoundFromTWS: true, Mode: scope.Mode, Account: scope.Account, AsOf: s.orderNow()}
		var outcome twsOrderControlOutcomeRow
		ok, err := s.twsOrderControlEvent(ctx, twsOrderControlKey(scope, "outcome", p.RequestID), &outcome)
		if err != nil {
			return nil, nil, err
		}
		if !ok {
			result.Outcome = rpc.TWSOrderControlOutcomeUncertain
			result.Message = "the request was recorded before sending and no outcome was recorded; it may have reached TWS"
			result.Blockers = []rpc.TradingBlocker{{Code: rpc.TWSOrderControlBlockerUncertain, Message: result.Message}}
			return result, nil, nil
		}
		result.Outcome, result.BoundOrderID, result.BrokerStatus, result.BrokerCode = outcome.Outcome, outcome.BoundOrderID, outcome.BrokerStatus, outcome.BrokerCode
		result.Blockers, result.Message = outcome.Blockers, outcome.Message
		result.Accepted = outcome.Outcome == rpc.TWSOrderControlOutcomeConfirmed || outcome.Outcome == rpc.TWSOrderControlOutcomePending
		return result, nil, nil
	}
	row := twsOrderControlRequestRow{RequestID: p.RequestID, Action: action, Digest: digest, PermID: p.PermID, Origin: origin, Bound: twsOrderControlJournalOrigin, Terms: p.Terms, Change: p.Replacement}
	raw, err := json.Marshal(row)
	if err != nil {
		return nil, nil, err
	}
	_, err = s.coreStore.AppendEvents(ctx, []corestore.EventInput{{
		ScopeKey: daemonStateScope, EventKey: twsOrderControlKey(scope, "request", p.RequestID), Type: twsOrderControlEventType,
		Action: "request", Origin: origin, OccurredAt: s.orderNow(), PayloadJSON: raw,
	}})
	return nil, nil, err
}

func (s *Server) recordTWSOrderControlOutcome(ctx context.Context, scope brokerStateScope, requestID string, out twsOrderControlOutcomeRow) error {
	if s.coreStore == nil {
		return errors.New("the request journal is unavailable")
	}
	raw, err := json.Marshal(out)
	if err != nil {
		return err
	}
	_, err = s.coreStore.AppendEvents(ctx, []corestore.EventInput{{
		ScopeKey: daemonStateScope, EventKey: twsOrderControlKey(scope, "outcome", requestID), Type: twsOrderControlEventType,
		Action: "outcome", Origin: coreEventOriginDaemon, OccurredAt: s.orderNow(), PayloadJSON: raw,
	}})
	return err
}

// recordTWSOrderControlPaperProof records the first confirmed paper
// outcome as trading.tws_order_control.paper_proven_at; later proofs leave
// the first one standing.
func (s *Server) recordTWSOrderControlPaperProof(ctx context.Context, origin string) (time.Time, error) {
	if s.platformSettings == nil {
		return time.Time{}, errors.New("runtime settings are unavailable")
	}
	now := s.orderNow().UTC()
	var provenAt time.Time
	err := s.platformSettings.updateWithAudit(ctx, now, origin, []string{"trading.tws_order_control.paper_proven_at"}, func(next *platformSettingsData) error {
		if next.Trading.TWSOrderControl.PaperProvenAt != nil {
			provenAt = next.Trading.TWSOrderControl.PaperProvenAt.UTC()
			return nil
		}
		at := now
		next.Trading.TWSOrderControl.PaperProvenAt = &at
		provenAt = at
		return nil
	})
	return provenAt, err
}

func nonEmptyOrderText(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}
