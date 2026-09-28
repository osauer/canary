package daemon

import (
	"slices"
	"strings"
	"time"

	"github.com/osauer/canary/v2/internal/marketcal"
	"github.com/osauer/canary/v2/internal/rpc"
)

// Opening offsets date default_send_at and the opening window: the queued
// authorisation design's recommended defaults, since listed options open by
// rotation with their widest quotes (15 minutes) and stocks settle sooner (5).
// The owner's queue decisions may change them; nothing sends from them yet.
const (
	readinessOptionsOpeningOffset = 15 * time.Minute
	readinessStockOpeningOffset   = 5 * time.Minute
)

// Blocker codes grouped by what they wait for. A code in none of these groups
// is a hard blocker: Canary cannot say the row becomes sendable by waiting.
var (
	readinessFrozenCodes = []string{tradingFrozenBlockerCode, tradingControlsChangedBlockerCode}
	readinessBrokerCodes = []string{"gateway_unavailable", "gateway_account_unconfirmed", optionQuoteBrokerUnavailable,
		"account_unavailable", "positions_unavailable", "positions_pending", "account_identity_unscoped", previewBrokerSessionChangedCode}
	readinessHaltCodes    = []string{"market_event_" + rpc.MarketEventHaltRegulatoryOrNews, "market_event_" + rpc.MarketEventLULDPause}
	readinessSessionCodes = []string{previewMarketClosedCode, "option_rth_closed"}
	readinessSpreadCodes  = []string{"option_spread_too_wide", "wide_spread", previewBoundedSpreadCode}
	readinessQuoteCodes   = []string{"live_option_quote_required", "fresh_option_quote_required", "two_sided_option_quote_required",
		optionQuoteRequestFailed, "missing_reference_price", previewQuoteStaleCode, previewQuoteNotLiveCode, previewQuoteNotTwoSidedCode, previewQuoteUnavailableCode}
)

// readinessQueueBuckets are the rows a queued authorisation may carry to the
// open: governor, theta and issuer-trim reductions. Loss exits need a live bid.
var readinessQueueBuckets = []string{rpc.TradeProposalBucketBudgetReduction, rpc.TradeProposalBucketThetaHygiene, rpc.TradeProposalBucketRiskReduction}

// readinessSessions memoizes one classification pass's calendar reads.
type readinessSessions map[marketcal.Market]readinessSession

type readinessSession struct {
	session marketcal.Session
	ok      bool
}

// classifyReadiness classifies prop with the blockers it is refused or held
// by. live adds the current trading freeze and broker link, which a row needs
// and a refusal does not: a refusal is classified by what refused it.
func (e *proposalEngine) classifyReadiness(prop rpc.TradeProposal, blockers []rpc.TradingBlocker, live bool, sessions readinessSessions) *rpc.TradeProposalReadiness {
	now := e.clock()
	out := &rpc.TradeProposalReadiness{SessionState: rpc.ReadinessSessionUnknown, AsOf: now}
	var codes []string
	message := map[string]string{}
	add := func(b rpc.TradingBlocker) {
		code := strings.TrimSpace(b.Code)
		if code == "" || slices.Contains(codes, code) {
			return
		}
		codes = append(codes, code)
		message[code] = strings.TrimSpace(b.Message)
	}
	for _, b := range blockers {
		add(b)
	}
	if live && e.server != nil {
		if e.server.tradingFrozen() {
			add(rpc.TradingBlocker{Code: tradingFrozenBlockerCode, Message: tradingFrozenBlockerMessage})
		}
		if !e.server.tradingGatewayReady() {
			add(rpc.TradingBlocker{Code: "gateway_unavailable", Message: "the broker link to IBKR Gateway/TWS is not ready"})
		}
	}
	out.CanaryCodes = codes

	needsSession := proposalHasContract(prop) && proposalNeedsOpenSession(prop)
	market, hasMarket := quoteSessionMarketForContract(prop.Contract)
	if !proposalHasContract(prop) {
		hasMarket = false
	}
	var session marketcal.Session
	var sessionKnown bool
	if hasMarket {
		out.Market = string(market)
		session, sessionKnown = e.readinessSession(market, now, sessions)
		if sessionKnown {
			out.MarketLabel = session.Label
			out.SessionState = readinessSessionState(session, now)
			out.OpensAt = readinessOpensAt(session, now)
		}
	}
	closed := needsSession && sessionKnown && session.State != marketcal.StateUnknown && !session.IsOpen

	first := func(group []string) (string, bool) {
		for _, code := range codes {
			if slices.Contains(group, code) {
				return code, true
			}
		}
		return "", false
	}
	transient := slices.Concat(readinessFrozenCodes, readinessBrokerCodes, readinessHaltCodes, readinessSessionCodes, readinessSpreadCodes, readinessQuoteCodes)
	hard := ""
	for _, code := range codes {
		if !slices.Contains(transient, code) {
			hard = code
			break
		}
	}
	decide := func(code, deciding string) {
		out.Code = code
		if deciding != "" {
			out.Message = message[deciding]
		}
	}
	if hard != "" {
		decide(rpc.ReadinessNotExecutable, hard)
	} else if code, ok := first(readinessFrozenCodes); ok {
		decide(rpc.ReadinessTradingFrozen, code)
	} else if code, ok := first(readinessBrokerCodes); ok {
		decide(rpc.ReadinessBrokerUnavailable, code)
	} else if code, ok := first(readinessHaltCodes); ok {
		decide(rpc.ReadinessHalted, code)
	} else if code, ok := first(readinessSessionCodes); ok || closed {
		decide(rpc.ReadinessMarketClosed, code)
		if out.Message == "" && sessionKnown {
			out.Message = sessionClosedPhrase(session)
		}
	} else if code, ok := first(readinessSpreadCodes); ok {
		decide(rpc.ReadinessSpreadTooWide, code)
	} else if code, ok := first(readinessQuoteCodes); ok {
		decide(rpc.ReadinessQuoteUnusable, code)
	} else if needsSession && sessionKnown && session.IsOpen && now.Before(session.Open.Add(readinessOpeningOffset(market))) {
		decide(rpc.ReadinessOpeningWindow, "")
	} else {
		decide(rpc.ReadinessReady, "")
	}

	if (out.Code == rpc.ReadinessMarketClosed || out.Code == rpc.ReadinessOpeningWindow) && out.OpensAt != nil {
		send := out.OpensAt.Add(readinessOpeningOffset(market))
		out.DefaultSendAt = &send
		out.Queueable = !prop.Shadow && slices.Contains(readinessQueueBuckets, prop.Bucket)
	}
	return out
}

// proposalHasContract reports whether prop names an instrument; a refusal
// that never resolved its proposal classifies by blockers alone.
func proposalHasContract(prop rpc.TradeProposal) bool {
	return strings.TrimSpace(prop.Contract.Symbol) != "" || prop.Contract.ConID > 0
}

func readinessOpeningOffset(market marketcal.Market) time.Duration {
	if market == marketcal.MarketUSOptions {
		return readinessOptionsOpeningOffset
	}
	return readinessStockOpeningOffset
}

func (e *proposalEngine) readinessSession(market marketcal.Market, now time.Time, sessions readinessSessions) (marketcal.Session, bool) {
	if cached, ok := sessions[market]; ok {
		return cached.session, cached.ok
	}
	var session marketcal.Session
	ok := false
	if e.server != nil {
		session, ok = e.server.previewSession(market, now)
	}
	if sessions != nil {
		sessions[market] = readinessSession{session: session, ok: ok}
	}
	return session, ok
}

// readinessSessionState names the regular session at now.
func readinessSessionState(s marketcal.Session, now time.Time) string {
	switch s.State {
	case marketcal.StateClosed:
		return rpc.ReadinessSessionClosed
	case marketcal.StateHoliday:
		return rpc.ReadinessSessionHoliday
	case marketcal.StateRegular, marketcal.StateEarlyClose:
	default:
		return rpc.ReadinessSessionUnknown
	}
	switch {
	case s.IsOpen:
		return rpc.ReadinessSessionOpen
	case s.Open.IsZero() || s.Close.IsZero():
		return rpc.ReadinessSessionUnknown
	case now.Before(s.Open):
		return rpc.ReadinessSessionPreOpen
	case !now.Before(s.Close):
		return rpc.ReadinessSessionAfterClose
	}
	return rpc.ReadinessSessionBreak
}

// readinessOpensAt is the regular open readiness refers to, in UTC: today's
// open while the session is open, else the next open the calendar dates.
func readinessOpensAt(s marketcal.Session, now time.Time) *time.Time {
	var at time.Time
	switch {
	case s.IsOpen && !s.Open.IsZero():
		at = s.Open
	case s.NextOpen != nil:
		at = *s.NextOpen
	default:
		return nil
	}
	at = at.UTC()
	return &at
}

// decorateReadiness classifies every row of a served snapshot. Rows read the
// snapshot's own blockers and trading status and the live freeze and broker
// link; the persisted snapshot never carries readiness.
func (e *proposalEngine) decorateReadiness(snap *rpc.TradeProposalSnapshot) {
	if e == nil || snap == nil || len(snap.Proposals) == 0 {
		return
	}
	sessions := readinessSessions{}
	shared := slices.Concat(snap.Blockers, snap.Trading.Blockers)
	for i := range snap.Proposals {
		prop := &snap.Proposals[i]
		prop.Readiness = e.classifyReadiness(*prop, slices.Concat(prop.Blockers, shared), true, sessions)
	}
}

// refusalReadiness classifies a refused preview, prepare or submit by its own
// blockers; an accepted result carries none.
func (e *proposalEngine) refusalReadiness(prop rpc.TradeProposal, blockers []rpc.TradingBlocker, err error) *rpc.TradeProposalReadiness {
	if len(blockers) == 0 && err == nil {
		return nil
	}
	if len(blockers) == 0 {
		blockers = previewFailureBlockers(err)
	}
	return e.classifyReadiness(prop, blockers, false, nil)
}
