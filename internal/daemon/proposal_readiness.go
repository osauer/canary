package daemon

import (
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/osauer/canary/v2/internal/marketcal"
	"github.com/osauer/canary/v2/internal/risk"
	"github.com/osauer/canary/v2/internal/rpc"
)

// Opening offsets date default_send_at and the opening window, and are the
// earliest time the queue and the pre-authorisation scheduler send after the
// regular open: listed options open by rotation with their widest quotes (15
// minutes), stocks settle sooner (5). At a stress open, while the latched
// regime stage in force reads confirmed stress, discretionary-scale options
// rows wait 30 minutes (owner decision 2026-09-30 12:35 CEST, the senior
// review's ask for no governor sends in the first 30 minutes of a stress
// open; narrowed to those rows by the reviewer decision of 13:18 CEST).
const (
	readinessOptionsOpeningOffset       = 15 * time.Minute
	readinessStressOptionsOpeningOffset = 30 * time.Minute
	readinessStockOpeningOffset         = 5 * time.Minute
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
		optionQuoteRequestFailed, "missing_reference_price", previewQuoteStaleCode, previewQuoteNotLiveCode, previewQuoteNotTwoSidedCode, previewQuoteUnavailableCode,
		rpc.CashSweepBlockerFreshQuote}
)

// readinessQueueBuckets are the rows a queued authorisation may carry to the
// open: governor, theta and issuer-trim reductions. Loss exits need a live bid.
var readinessQueueBuckets = []string{rpc.TradeProposalBucketBudgetReduction, rpc.TradeProposalBucketThetaHygiene, rpc.TradeProposalBucketRiskReduction}

// readinessStressOpenBuckets are the discretionary-scale rows a stress open
// holds for 30 minutes, with their queued or pre-authorised sends: governor,
// theta and issuer-trim reductions and the cash sweep (reviewer decision
// 2026-09-30 13:18 CEST). Every other row, loss exits, expiry closes and
// trailing stops among them, keeps the 15-minute options offset: a stop is a
// stop.
var readinessStressOpenBuckets = []string{rpc.TradeProposalBucketBudgetReduction, rpc.TradeProposalBucketThetaHygiene,
	rpc.TradeProposalBucketRiskReduction, rpc.TradeProposalBucketCashSweep}

// stressOpenRule says how a confirmed-stress regime bears on one row's
// opening offset.
type stressOpenRule int

const (
	// stressOpenNone: no stress open, because the market is not options or
	// the regime does not read confirmed stress.
	stressOpenNone stressOpenRule = iota
	// stressOpenApplies: a discretionary-scale options row waits 30 minutes.
	stressOpenApplies
	// stressOpenExempt: the regime reads confirmed stress, but the row is not
	// discretionary-scale and keeps the 15-minute options offset.
	stressOpenExempt
)

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
	// A sweep bill's session comes from its line's hours (else the assumed
	// hours); every other row reads its exchange calendar.
	market, session, hasMarket, sessionKnown := e.proposalSessionAt(prop, now, sessions)
	if hasMarket {
		out.Market = string(market)
		if sessionKnown {
			out.MarketLabel = session.Label
			out.SessionState = readinessSessionState(session, now)
			out.OpensAt = readinessOpensAt(session, now)
		}
	}
	closed := needsSession && sessionKnown && session.State != marketcal.StateUnknown && !session.IsOpen
	offset, stress := e.server.readinessOpeningOffset(market, prop.Bucket, now)

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
	} else if needsSession && sessionKnown && session.IsOpen && now.Before(session.Open.Add(offset)) {
		decide(rpc.ReadinessOpeningWindow, "")
	} else {
		decide(rpc.ReadinessReady, "")
	}

	if (out.Code == rpc.ReadinessMarketClosed || out.Code == rpc.ReadinessOpeningWindow) && out.OpensAt != nil {
		send := out.OpensAt.Add(offset)
		out.DefaultSendAt = &send
		// Readiness says which offset applies at a stress open.
		switch stress {
		case stressOpenApplies:
			out.StressOpen = true
			out.Message = strings.TrimPrefix(out.Message+"; "+stressOpenPhrase(send), "; ")
		case stressOpenExempt:
			out.StressOpenExempt = true
			out.Message = strings.TrimPrefix(out.Message+"; "+stressOpenExemptPhrase(prop.Bucket, send), "; ")
		}
		// A row a queue already covers is not offered a second one.
		out.Queueable = !prop.Shadow && prop.Queued == nil && slices.Contains(readinessQueueBuckets, prop.Bucket)
	}
	return out
}

// proposalHasContract reports whether prop names an instrument; a refusal
// that never resolved its proposal classifies by blockers alone.
func proposalHasContract(prop rpc.TradeProposal) bool {
	return strings.TrimSpace(prop.Contract.Symbol) != "" || prop.Contract.ConID > 0
}

// readinessOpeningOffset is how long after the regular open Canary waits
// before an order of bucket that prices off the session may go out: 5
// minutes for stocks, 15 for options, and 30 for a discretionary-scale
// options row at a stress open. stress says whether the stress open applied,
// or the regime reads confirmed stress and the row is exempt.
func (s *Server) readinessOpeningOffset(market marketcal.Market, bucket string, now time.Time) (offset time.Duration, stress stressOpenRule) {
	switch {
	case market != marketcal.MarketUSOptions:
		return readinessStockOpeningOffset, stressOpenNone
	case !s.regimeStressOpen(now):
		return readinessOptionsOpeningOffset, stressOpenNone
	case slices.Contains(readinessStressOpenBuckets, bucket):
		return readinessStressOptionsOpeningOffset, stressOpenApplies
	default:
		return readinessOptionsOpeningOffset, stressOpenExempt
	}
}

// regimeStressOpen reports whether the latched regime stage in force reads
// confirmed stress, read as rules 3, 4, 12 and 15 read it: a carried (stale)
// stage counts as its own stage, never as calm, and a stage never observed
// reads calm. A latch that fails closed reads confirmed.
func (s *Server) regimeStressOpen(now time.Time) bool {
	if s == nil {
		return false
	}
	pol, _ := s.activeRulebookPolicy()
	stage, _ := s.rulebookRegimeStage(pol, now)
	return stage.Bucket == risk.RegimeBucketConfirmed
}

// stressOpenPhrase says why an options send waits 30 minutes, with the time.
func stressOpenPhrase(send time.Time) string {
	return fmt.Sprintf("stress open: the regime reads confirmed stress, so options send from %s, %d minutes after the open",
		send.UTC().Format(time.RFC3339), int(readinessStressOptionsOpeningOffset/time.Minute))
}

// stressOpenExemptPhrase says why a row that is not discretionary-scale keeps
// the 15-minute options offset at a stress open, with the time.
func stressOpenExemptPhrase(bucket string, send time.Time) string {
	kind := "protective exit"
	switch bucket {
	case rpc.TradeProposalBucketOptionLossExit:
		kind = "loss exit"
	case rpc.TradeProposalBucketOptionExpiryClose:
		kind = "expiry close"
	case rpc.TradeProposalBucketTrailingStop:
		kind = "trailing stop"
	}
	return fmt.Sprintf("stress open: the regime reads confirmed stress, but a %s keeps the %d-minute options offset, so it sends from %s",
		kind, int(readinessOptionsOpeningOffset/time.Minute), send.UTC().Format(time.RFC3339))
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
