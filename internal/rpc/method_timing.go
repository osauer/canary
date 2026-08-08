package rpc

import "time"

// MethodLifetime says whether a daemon method is a bounded request or owns a
// stream that lives until its caller cancels it.
type MethodLifetime uint8

// Method lifetime values distinguish bounded calls from caller-owned streams.
const (
	MethodLifetimeUnary MethodLifetime = iota + 1
	MethodLifetimeStreaming
)

// MethodTiming is the shared timing contract for one daemon RPC method.
// DaemonTimeout is the daemon's own handler deadline. Adapters must add an
// explicit positive headroom rather than copying this value into another
// timeout table; that leaves enough time for the daemon's classified response
// to cross the socket before the caller gives up.
type MethodTiming struct {
	Method        string
	Lifetime      MethodLifetime
	DaemonTimeout time.Duration
}

// ClientTimeout returns the deadline a caller should use after choosing its
// transport/rendering headroom. Streaming methods have no fixed deadline.
// A non-positive headroom is a programming error because equal client and
// daemon deadlines race and can hide the daemon's typed timeout response.
func (t MethodTiming) ClientTimeout(headroom time.Duration) time.Duration {
	if t.Lifetime == MethodLifetimeStreaming {
		return 0
	}
	if headroom <= 0 {
		panic("rpc: client timeout headroom must be positive")
	}
	return t.DaemonTimeout + headroom
}

// methodTimings is the timing authority for every method dispatched by the
// daemon. Keep policy and behavior in the daemon handlers; this catalog owns
// only the lifetime class and outer request budget shared by adapters.
var methodTimings = []MethodTiming{
	{Method: MethodAccountSummary, Lifetime: MethodLifetimeUnary, DaemonTimeout: 10 * time.Second},
	{Method: MethodPositionsList, Lifetime: MethodLifetimeUnary, DaemonTimeout: 30 * time.Second},
	{Method: MethodQuoteSnapshot, Lifetime: MethodLifetimeUnary, DaemonTimeout: 10 * time.Second},
	{Method: MethodQuoteSubscribe, Lifetime: MethodLifetimeStreaming},
	{Method: MethodChainFetch, Lifetime: MethodLifetimeUnary, DaemonTimeout: 50 * time.Second},
	{Method: MethodChainExpiries, Lifetime: MethodLifetimeUnary, DaemonTimeout: 50 * time.Second},
	{Method: MethodHistoryDaily, Lifetime: MethodLifetimeUnary, DaemonTimeout: 55 * time.Second},
	{Method: MethodTechnical, Lifetime: MethodLifetimeUnary, DaemonTimeout: 75 * time.Second},
	{Method: MethodMarketCalendar, Lifetime: MethodLifetimeUnary, DaemonTimeout: 2 * time.Second},
	{Method: MethodStatusHealth, Lifetime: MethodLifetimeUnary, DaemonTimeout: 5 * time.Second},
	{Method: MethodTradingStatus, Lifetime: MethodLifetimeUnary, DaemonTimeout: 5 * time.Second},
	{Method: MethodSettingsGet, Lifetime: MethodLifetimeUnary, DaemonTimeout: 5 * time.Second},
	{Method: MethodSettingsUpdate, Lifetime: MethodLifetimeUnary, DaemonTimeout: 5 * time.Second},
	{Method: MethodOrdersOpen, Lifetime: MethodLifetimeUnary, DaemonTimeout: 5 * time.Second},
	{Method: MethodOrdersHistory, Lifetime: MethodLifetimeUnary, DaemonTimeout: 5 * time.Second},
	{Method: MethodOrderStatus, Lifetime: MethodLifetimeUnary, DaemonTimeout: 5 * time.Second},
	{Method: MethodOrderPreview, Lifetime: MethodLifetimeUnary, DaemonTimeout: 55 * time.Second},
	{Method: MethodBreadthSPX, Lifetime: MethodLifetimeUnary, DaemonTimeout: 2 * time.Second},
	{Method: MethodGammaZeroSPX, Lifetime: MethodLifetimeUnary, DaemonTimeout: 55 * time.Second},
	{Method: MethodRegimeSnapshot, Lifetime: MethodLifetimeUnary, DaemonTimeout: 50 * time.Second},
	{Method: MethodCancel, Lifetime: MethodLifetimeUnary, DaemonTimeout: 15 * time.Second},
	{Method: MethodOrderPlace, Lifetime: MethodLifetimeUnary, DaemonTimeout: 15 * time.Second},
	{Method: MethodOrderModify, Lifetime: MethodLifetimeUnary, DaemonTimeout: 15 * time.Second},
	{Method: MethodOrderCancel, Lifetime: MethodLifetimeUnary, DaemonTimeout: 15 * time.Second},
	{Method: MethodAlertCandidates, Lifetime: MethodLifetimeUnary, DaemonTimeout: 2 * time.Second},
	{Method: MethodAlertStatus, Lifetime: MethodLifetimeUnary, DaemonTimeout: 2 * time.Second},
	{Method: MethodRegimeHistory, Lifetime: MethodLifetimeUnary, DaemonTimeout: 15 * time.Second},
	{Method: MethodRulesHistory, Lifetime: MethodLifetimeUnary, DaemonTimeout: 15 * time.Second},
	{Method: MethodStressHistory, Lifetime: MethodLifetimeUnary, DaemonTimeout: 15 * time.Second},
	{Method: MethodReconEquity, Lifetime: MethodLifetimeUnary, DaemonTimeout: 15 * time.Second},
	{Method: MethodMarketEventsSnapshot, Lifetime: MethodLifetimeUnary, DaemonTimeout: 20 * time.Second},
	{Method: MethodAutoTradeStatus, Lifetime: MethodLifetimeUnary, DaemonTimeout: 5 * time.Second},
	{Method: MethodRulesSnapshot, Lifetime: MethodLifetimeUnary, DaemonTimeout: 15 * time.Second},
	{Method: MethodBriefSnapshot, Lifetime: MethodLifetimeUnary, DaemonTimeout: 75 * time.Second},
	{Method: MethodBriefAck, Lifetime: MethodLifetimeUnary, DaemonTimeout: 75 * time.Second},
	{Method: MethodNudgesSnapshot, Lifetime: MethodLifetimeUnary, DaemonTimeout: 5 * time.Second},
	{Method: MethodNudgesCutoverReview, Lifetime: MethodLifetimeUnary, DaemonTimeout: 5 * time.Second},
	{Method: MethodRiskPolicySnapshot, Lifetime: MethodLifetimeUnary, DaemonTimeout: 15 * time.Second},
	{Method: MethodRiskPolicyCapitalEvent, Lifetime: MethodLifetimeUnary, DaemonTimeout: 15 * time.Second},
	{Method: MethodRiskPolicyOverride, Lifetime: MethodLifetimeUnary, DaemonTimeout: 15 * time.Second},
	{Method: MethodRiskPolicyResetDrawdown, Lifetime: MethodLifetimeUnary, DaemonTimeout: 15 * time.Second},
	{Method: MethodRiskPolicyCorrectPeak, Lifetime: MethodLifetimeUnary, DaemonTimeout: 15 * time.Second},
	{Method: MethodRiskPolicyArtefact, Lifetime: MethodLifetimeUnary, DaemonTimeout: 15 * time.Second},
	{Method: MethodReconSnapshot, Lifetime: MethodLifetimeUnary, DaemonTimeout: 15 * time.Second},
	{Method: MethodReconStatus, Lifetime: MethodLifetimeUnary, DaemonTimeout: 5 * time.Second},
	{Method: MethodReconCheck, Lifetime: MethodLifetimeUnary, DaemonTimeout: 5 * time.Second},
	{Method: MethodReconBacktest, Lifetime: MethodLifetimeUnary, DaemonTimeout: 15 * time.Second},
	{Method: MethodReconDismiss, Lifetime: MethodLifetimeUnary, DaemonTimeout: 15 * time.Second},
	{Method: MethodTradeProposalsSnapshot, Lifetime: MethodLifetimeUnary, DaemonTimeout: 5 * time.Second},
	{Method: MethodTradeProposalsRefresh, Lifetime: MethodLifetimeUnary, DaemonTimeout: 55 * time.Second},
	{Method: MethodTradeProposalsPreview, Lifetime: MethodLifetimeUnary, DaemonTimeout: 55 * time.Second},
	{Method: MethodTradeProposalsSubmit, Lifetime: MethodLifetimeUnary, DaemonTimeout: 55 * time.Second},
	{Method: MethodTradeProposalsIgnore, Lifetime: MethodLifetimeUnary, DaemonTimeout: 5 * time.Second},
	{Method: MethodTradeProposalsReducePreview, Lifetime: MethodLifetimeUnary, DaemonTimeout: 55 * time.Second},
	{Method: MethodTradeProposalsReduceSubmit, Lifetime: MethodLifetimeUnary, DaemonTimeout: 55 * time.Second},
	{Method: MethodTradeProposalsReducePortfolioPreview, Lifetime: MethodLifetimeUnary, DaemonTimeout: 120 * time.Second},
	{Method: MethodTradeProposalsReducePortfolioSubmit, Lifetime: MethodLifetimeUnary, DaemonTimeout: 120 * time.Second},
	{Method: MethodOpportunitiesStatus, Lifetime: MethodLifetimeUnary, DaemonTimeout: 5 * time.Second},
	{Method: MethodOpportunitiesSnapshot, Lifetime: MethodLifetimeUnary, DaemonTimeout: 5 * time.Second},
	{Method: MethodOpportunitiesRefresh, Lifetime: MethodLifetimeUnary, DaemonTimeout: 55 * time.Second},
	{Method: MethodOpportunitiesPreviewExercise, Lifetime: MethodLifetimeUnary, DaemonTimeout: 55 * time.Second},
	{Method: MethodOpportunitiesSubmitExercise, Lifetime: MethodLifetimeUnary, DaemonTimeout: 55 * time.Second},
	{Method: MethodOpportunitiesIgnore, Lifetime: MethodLifetimeUnary, DaemonTimeout: 5 * time.Second},
}

var methodTimingByName = func() map[string]MethodTiming {
	out := make(map[string]MethodTiming, len(methodTimings))
	for _, timing := range methodTimings {
		out[timing.Method] = timing
	}
	return out
}()

// LookupMethodTiming returns the shared lifetime and daemon deadline for a
// stable method name.
func LookupMethodTiming(method string) (MethodTiming, bool) {
	timing, ok := methodTimingByName[method]
	return timing, ok
}

// MethodTimings returns a copy of the complete method timing catalog.
func MethodTimings() []MethodTiming {
	out := make([]MethodTiming, len(methodTimings))
	copy(out, methodTimings)
	return out
}
