package rpc

import (
	"testing"
	"time"
)

func TestMethodTimingsAreCompleteAndSafe(t *testing.T) {
	t.Parallel()

	seen := make(map[string]bool, len(methodTimings))
	for _, timing := range MethodTimings() {
		if timing.Method == "" {
			t.Error("method timing has an empty method")
		}
		if seen[timing.Method] {
			t.Errorf("duplicate method timing for %q", timing.Method)
		}
		seen[timing.Method] = true
		switch timing.Lifetime {
		case MethodLifetimeUnary:
			if timing.DaemonTimeout <= 0 {
				t.Errorf("unary method %q has timeout %s", timing.Method, timing.DaemonTimeout)
			}
			if got := timing.ClientTimeout(time.Second); got <= timing.DaemonTimeout {
				t.Errorf("client timeout for %q = %s, want above daemon timeout %s", timing.Method, got, timing.DaemonTimeout)
			}
		case MethodLifetimeStreaming:
			if timing.DaemonTimeout != 0 {
				t.Errorf("streaming method %q has fixed timeout %s", timing.Method, timing.DaemonTimeout)
			}
			if got := timing.ClientTimeout(time.Second); got != 0 {
				t.Errorf("streaming client timeout for %q = %s, want 0", timing.Method, got)
			}
		default:
			t.Errorf("method %q has unknown lifetime %d", timing.Method, timing.Lifetime)
		}
	}

	for _, method := range []string{
		MethodAccountSummary, MethodPositionsList, MethodQuoteSnapshot, MethodQuoteSubscribe,
		MethodChainFetch, MethodChainExpiries,
		MethodHistoryDaily, MethodTechnical, MethodMarketCalendar, MethodStatusHealth,
		MethodTradingStatus, MethodTradingPaperSmoke, MethodSettingsGet, MethodSettingsUpdate,
		MethodOrdersOpen, MethodOrdersHistory, MethodOrderStatus, MethodOrderPreview,
		MethodBreadthSPX, MethodGammaZeroSPX, MethodRegimeSnapshot, MethodCancel,
		MethodOrderPlace, MethodOrderModify, MethodOrderCancel, MethodPurgeStatus,
		MethodPurgeExecute, MethodPurgeRestorePreview, MethodPurgeRestoreExecute,
		MethodAlertCandidates, MethodAlertStatus, MethodRegimeHistory, MethodRulesHistory,
		MethodStressHistory, MethodReconEquity, MethodMarketEventsSnapshot, MethodAutoTradeStatus,
		MethodRulesSnapshot, MethodBriefSnapshot, MethodBriefAck, MethodNudgesSnapshot,
		MethodNudgesCutoverReview, MethodRiskPolicySnapshot, MethodRiskPolicyCapitalEvent,
		MethodRiskPolicyOverride, MethodRiskPolicyResetDrawdown, MethodRiskPolicyCorrectPeak,
		MethodRiskPolicyArtefact, MethodReconSnapshot, MethodReconStatus, MethodReconCheck,
		MethodReconBacktest, MethodReconDismiss, MethodTradeProposalsSnapshot,
		MethodTradeProposalsRefresh, MethodTradeProposalsPreview, MethodTradeProposalsSubmit,
		MethodTradeProposalsIgnore, MethodTradeProposalsReducePreview,
		MethodTradeProposalsReduceSubmit, MethodTradeProposalsReducePortfolioPreview,
		MethodTradeProposalsReducePortfolioSubmit, MethodOpportunitiesStatus,
		MethodOpportunitiesSnapshot, MethodOpportunitiesRefresh,
		MethodOpportunitiesPreviewExercise, MethodOpportunitiesSubmitExercise,
		MethodOpportunitiesIgnore,
	} {
		if !seen[method] {
			t.Errorf("stable method %q has no timing entry", method)
		}
	}
	if len(seen) != len(methodTimings) {
		t.Fatalf("timing catalog has %d unique methods for %d entries", len(seen), len(methodTimings))
	}
}

func TestMethodTimingClientTimeoutRequiresHeadroom(t *testing.T) {
	t.Parallel()
	defer func() {
		if recover() == nil {
			t.Fatal("ClientTimeout accepted zero headroom")
		}
	}()
	methodTimings[0].ClientTimeout(0)
}
