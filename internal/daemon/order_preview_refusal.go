package daemon

import (
	"errors"
	"fmt"
	"strings"
	"time"

	ibkrlib "github.com/osauer/canary/v2/pkg/ibkr"

	"github.com/osauer/canary/v2/internal/marketcal"
	"github.com/osauer/canary/v2/internal/rpc"
)

// Typed order-preview refusal codes. A preview that fails before a token is
// minted reports one of these instead of the generic preview_failed, so the
// proposal engine, its readiness and its decision log keep the reason's kind.
const (
	previewMarketClosedCode         = "market_closed"
	previewQuoteStaleCode           = "quote_stale"
	previewQuoteNotLiveCode         = "quote_not_live"
	previewQuoteNotTwoSidedCode     = "quote_not_two_sided"
	previewQuoteUnavailableCode     = "quote_unavailable"
	previewBrokerSessionChangedCode = "broker_session_changed"
	previewContractUnresolvedCode   = "contract_unresolved"
	previewPositionUnavailableCode  = "position_unavailable"
	previewNotionalUnavailableCode  = "notional_unavailable"
	previewRiskLimitCode            = "order_risk_limit"
	previewWhatIfFailedCode         = "what_if_failed"
	previewTokenUnavailableCode     = "preview_token_unavailable"
	previewGenericFailureCode       = "preview_failed"
)

// previewRefusal types an order-preview refusal with the blockers that caused
// it and keeps the error it wraps, so callers that classify the error (bad
// request, ErrTradingDisabled, gateway unavailable) see what they saw before.
type previewRefusal struct {
	err      error
	blockers []rpc.TradingBlocker
}

func (r *previewRefusal) Error() string { return r.err.Error() }
func (r *previewRefusal) Unwrap() error { return r.err }

// refusePreview attaches blockers to err; a nil err stays nil.
func refusePreview(err error, blockers ...rpc.TradingBlocker) error {
	if err == nil {
		return nil
	}
	if _, typed := errors.AsType[*previewRefusal](err); typed {
		return err
	}
	return &previewRefusal{err: err, blockers: blockers}
}

// refusePreviewCode is refusePreview with one blocker carrying the error's text.
func refusePreviewCode(code string, err error) error {
	if err == nil {
		return nil
	}
	return refusePreview(err, rpc.TradingBlocker{Code: code, Message: err.Error()})
}

// previewStageRefusal types the failure of one broker-backed preview stage:
// an unavailable gateway or a changed broker session is the broker's, any
// other failure the stage's own code.
func previewStageRefusal(code string, err error) error {
	if err == nil {
		return nil
	}
	if _, typed := errors.AsType[*previewRefusal](err); typed {
		return err
	}
	switch {
	case errors.Is(err, ibkrlib.ErrIBKRUnavailable):
		return refusePreviewCode("gateway_unavailable", err)
	case strings.Contains(err.Error(), "broker session changed"):
		return refusePreviewCode(previewBrokerSessionChangedCode, err)
	}
	return refusePreviewCode(code, err)
}

// previewFailureBlockers lists the typed blockers of a failed preview, or the
// generic preview_failed blocker when the failure carries no type.
func previewFailureBlockers(err error) []rpc.TradingBlocker {
	if err == nil {
		return nil
	}
	if r, ok := errors.AsType[*previewRefusal](err); ok && len(r.blockers) > 0 {
		return append([]rpc.TradingBlocker(nil), r.blockers...)
	}
	return []rpc.TradingBlocker{{Code: previewGenericFailureCode, Message: err.Error()}}
}

// previewNeedsOpenSession reports whether an order's pricing needs the
// regular session: a patient limit prices off the live mid, and a broker trail
// without an initial stop seeds it from the live bid or ask. Both fail
// requireFreshPreviewQuote whenever the session is closed. An explicit limit
// or a seeded trail can be previewed outside the session.
func previewNeedsOpenSession(orderType, strategy string, limit *float64, trail *rpc.OrderTrailSpec) bool {
	switch strings.ToUpper(strings.TrimSpace(orderType)) {
	case "", rpc.OrderTypeLMT:
		return normalizePreviewStrategy(strategy, limit) == rpc.OrderStrategyPatientLimit
	case rpc.OrderTypeTRAIL, rpc.OrderTypeTRAILLIMIT:
		return trail == nil || trail.InitialStopPrice <= 0
	default:
		return false
	}
}

// proposalNeedsOpenSession applies previewNeedsOpenSession to the order a
// proposal previews (proposalOrderPreviewParams).
func proposalNeedsOpenSession(prop rpc.TradeProposal) bool {
	p := proposalOrderPreviewParams(prop, prop.Quantity, 0)
	return previewNeedsOpenSession(p.OrderType, p.Strategy, p.LimitPrice, p.Trail)
}

// previewSession reads the regular session of market at the instant. A test
// that replaced the broker quote source without a session hook keeps its
// quotes' own session semantics, so the calendar stays out of its way.
func (s *Server) previewSession(market marketcal.Market, at time.Time) (marketcal.Session, bool) {
	if s == nil {
		return marketcal.Session{}, false
	}
	if s.previewSessionAt != nil {
		session, err := s.previewSessionAt(market, at)
		return session, err == nil
	}
	if s.hasOrderPreviewBrokerTestSeam() {
		return marketcal.Session{}, false
	}
	session, err := marketcal.New().SessionAt(market, at)
	return session, err == nil
}

// previewMarketClosedRefusal refuses, before any broker request, a preview
// whose pricing needs the regular session while the contract's official
// calendar says the session is closed. The quote such a preview would wait
// for could only fail requireFreshPreviewQuote, so no request is sent.
// Contracts without an embedded calendar, and dates outside its coverage,
// keep the quote path's own judgement.
func (s *Server) previewMarketClosedRefusal(contract rpc.ContractParams, orderType, strategy string, limit *float64, trail *rpc.OrderTrailSpec, now time.Time) error {
	if !previewNeedsOpenSession(orderType, strategy, limit, trail) {
		return nil
	}
	market, ok := quoteSessionMarketForContract(contract)
	if !ok {
		return nil
	}
	session, ok := s.previewSession(market, now)
	if !ok || session.State == marketcal.StateUnknown || session.IsOpen {
		return nil
	}
	useCase := "patient-limit"
	if t := strings.ToUpper(strings.TrimSpace(orderType)); t == rpc.OrderTypeTRAIL || t == rpc.OrderTypeTRAILLIMIT {
		useCase = "broker-trail"
	}
	message := fmt.Sprintf("%s requires an open market session: %s", useCase, sessionClosedPhrase(session))
	return refusePreview(errBadRequest(message), rpc.TradingBlocker{
		Code:    previewMarketClosedCode,
		Message: message,
		Action:  "Preview again once the regular session is open; no quote was requested.",
	})
}

// sessionClosedPhrase names a closed session and, when the calendar dates
// it, the next regular open in UTC.
func sessionClosedPhrase(session marketcal.Session) string {
	label := nonEmptyString(session.Label, string(session.Market))
	phrase := label + " regular session is closed"
	if reason := strings.TrimSpace(session.Reason); reason != "" && session.State != marketcal.StateRegular && session.State != marketcal.StateEarlyClose {
		phrase += " (" + reason + ")"
	}
	if session.NextOpen != nil {
		phrase += "; it opens " + session.NextOpen.UTC().Format(time.RFC3339)
	}
	return phrase
}
