package daemon

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/risk"
	"github.com/osauer/canary/v2/internal/rpc"
)

// narrativeText flattens runs to plain text for assertions, so the run split
// can change without rewriting every expectation.
func narrativeText(runs []rpc.BriefRun) string {
	var out strings.Builder
	for _, run := range runs {
		out.WriteString(run.Text)
	}
	return out.String()
}

func narrativeParagraphs(paragraphs []rpc.BriefParagraph) []string {
	out := make([]string, 0, len(paragraphs))
	for _, paragraph := range paragraphs {
		out = append(out, narrativeText(paragraph.Runs))
	}
	return out
}

// narrativeAll is every composed word in reading order.
func narrativeAll(narrative *rpc.BriefNarrative) string {
	parts := []string{narrativeText(narrative.Lead)}
	parts = append(parts, narrativeParagraphs(narrative.Review)...)
	parts = append(parts, narrativeParagraphs(narrative.Ready)...)
	parts = append(parts, narrativeText(narrative.Coda))
	return strings.Join(parts, "\n")
}

// narrativeRoleText collects the text of every run carrying one role.
func narrativeRoleText(narrative *rpc.BriefNarrative, role string) []string {
	var out []string
	collect := func(runs []rpc.BriefRun) {
		for _, run := range runs {
			if run.Role == role {
				out = append(out, run.Text)
			}
		}
	}
	collect(narrative.Lead)
	for _, paragraph := range narrative.Review {
		collect(paragraph.Runs)
	}
	for _, paragraph := range narrative.Ready {
		collect(paragraph.Runs)
	}
	collect(narrative.Coda)
	return out
}

// quietBriefResult is a completely clean, completely available payload: every
// row ok, nothing flagged, nothing missing. The other cases mutate it, so a
// case's diff is exactly the condition it is about.
func quietBriefResult() *rpc.BriefResult {
	equity, pnl := 1_250_000.0, 2_340.0
	other := -120.0
	consumed, drawdown, peak := 2.0, 25_000.0, 1_275_000.0
	spot, zero, gap := 620.0, 607.0, 2.1
	above50, above200, highs := 62.0, 58.0, 1.4
	premium, hedge := 1_564.0, -18.4
	days, orders := 5, 1
	return &rpc.BriefResult{
		AsOf: time.Date(2026, 7, 30, 14, 32, 0, 0, time.UTC),
		Review: rpc.BriefReviewSection{
			SessionPnL: rpc.BriefAccountRow{BriefRowState: briefOK("account summary in base currency"),
				EquityBase: &equity, DailyPnLBase: &pnl, BaseCurrency: "EUR"},
			Attribution: rpc.BriefMoversRow{BriefRowState: briefOK("daily P&L by underlying"),
				Rows: []rpc.BriefMover{{Symbol: "AAPL", DailyPnLBase: 900}, {Symbol: "NVDA", DailyPnLBase: 800.4},
					{Symbol: "SPY", DailyPnLBase: 639.6}}, OtherPnLBase: &other, OtherCount: 2},
			RulesDelta:    rpc.BriefRulesDeltaRow{BriefRowState: briefOK("no rule status changes"), BaselineAt: time.Date(2026, 7, 29, 7, 30, 0, 0, time.UTC)},
			Proposals:     rpc.BriefProposalsRow{BriefRowState: briefOK("2 offered, 1 acted"), Day: "2026-07-29", Offered: 2, Acted: 1},
			Overrides:     rpc.BriefOverridesRow{BriefRowState: briefOK("no active overrides")},
			CapitalEvents: rpc.BriefCapitalEventsRow{BriefRowState: briefOK("no capital events"), AdjustedPeakBase: &peak, BaseCurrency: "EUR"},
			Reconcile: rpc.BriefReconcileRow{BriefRowState: briefOK("reconcile evidence"),
				LastReconciledAt: time.Date(2026, 7, 25, 9, 0, 0, 0, time.UTC), DaysRemaining: &days},
			AutoExtend:    rpc.BriefAutoExtendRow{BriefRowState: briefOK("no automatic extension recorded")},
			OneTap:        rpc.BriefOneTapRow{BriefRowState: briefOK("current report is signable"), ReportID: "report-1", Signable: false},
			WorkingOrders: rpc.BriefCountRow{BriefRowState: briefOK("daemon open-orders journal view"), Count: &orders},
		},
		Ready: rpc.BriefReadySection{
			Regime:  rpc.BriefRegimeRow{BriefRowState: briefOK("daemon regime lifecycle"), Stage: "quiet", Verdict: "Normal regime"},
			Breadth: rpc.BriefBreadthRow{BriefRowState: briefOK("S&P 500 constituent breadth"), PctAbove50DMA: &above50, PctAbove200DMA: &above200, NetNewHighsPct: &highs},
			Gamma:   rpc.BriefGammaRow{BriefRowState: briefOK("SPX dealer zero-gamma"), Spot: &spot, ZeroGamma: &zero, GapPct: &gap, GammaSign: "positive"},
			Stress:  rpc.BriefStressRow{BriefRowState: briefOK("pure stress composition"), Action: "stand down", Severity: string(risk.SeverityObserve)},
			Session: rpc.BriefSessionRow{BriefRowState: briefOK("official session calendar"), Market: "US", State: "regular", IsOpen: true},
			MarketEvents: []rpc.BriefMarketEventRow{
				{BriefRowState: briefOK("0 held symbols with earnings context"), Kind: "earnings"},
				{BriefRowState: briefOK("0 held symbols flagged"), Kind: "halt"},
			},
			Capital: rpc.BriefCapitalRow{BriefRowState: briefOK("constitution capital state"), Tier: risk.CapitalTierOK,
				Enforcement: "shadow", ConsumedPct: &consumed, DrawdownBase: &drawdown, AdjustedPeakBase: &peak, BaseCurrency: "EUR"},
			Latch:         rpc.BriefLatchRow{BriefRowState: briefOK("drawdown latch is not engaged")},
			PremiumAtRisk: rpc.BriefMoneyCoverageRow{BriefRowState: briefOK("long-option market value"), AmountBase: &premium, BaseCurrency: "EUR", IncludedLegs: 1},
			HedgeCost:     rpc.BriefMoneyCoverageRow{BriefRowState: briefOK("daily theta of hedge legs"), AmountBase: &hedge, BaseCurrency: "EUR", IncludedLegs: 1},
			PolicyDrift:   rpc.BriefPolicyDriftRow{BriefRowState: briefOK("all approval pins match")},
			Artefacts: rpc.BriefArtefactsRow{BriefRowState: briefOK("declared cadence completion"), Rows: []rpc.BriefArtefact{
				{BriefRowState: briefOK("completed"), Kind: rpc.BriefKindMorning, Cadence: "daily", Declared: true, Completed: true},
				{BriefRowState: briefOK("declared"), Kind: rpc.BriefKindEOD, Cadence: "daily", Declared: true},
			}},
			MonthlyPulse: &rpc.BriefMonthlyPulseRow{Status: rpc.BriefMonthlyPulseNotDue, Month: "2026-07"},
		},
	}
}

// watchBriefResult flags exactly the attention-class rows a watch morning
// carries: an active override, a held-name earnings escalation, and a stress
// row the daemon severity vocabulary calls watch.
func watchBriefResult() *rpc.BriefResult {
	res := quietBriefResult()
	res.Review.Overrides = rpc.BriefOverridesRow{
		BriefRowState: briefAttention("1 active override temporarily widens policy controls"),
		Rows:          []rpc.BriefOverride{{Control: "hedge_coverage", ExpiresAt: time.Date(2026, 7, 31, 9, 0, 0, 0, time.UTC)}},
	}
	res.Ready.Stress = rpc.BriefStressRow{BriefRowState: briefOK("pure stress composition"),
		Action: "reduce risk", Severity: string(risk.SeverityWatch)}
	res.Ready.MarketEvents[0] = rpc.BriefMarketEventRow{
		BriefRowState: briefAttention("2 held earnings upcoming while the overwrite earnings rule reports unknown"),
		Kind:          "earnings", Count: 2, Symbols: []string{"AAPL", "NVDA"},
	}
	return res
}

// actBriefResult carries the strongest state each vocabulary has: a rule
// worsened to act, a breached block tier, an engaged latch, act severity.
func actBriefResult() *rpc.BriefResult {
	res := quietBriefResult()
	consumed, latchPct := 118.0, 30.4
	age := 2
	res.Review.RulesDelta = rpc.BriefRulesDeltaRow{
		BriefRowState: briefAttention("rulebook changed since the last stamped brief; 1 rule worsened to act"),
		BaselineAt:    time.Date(2026, 7, 29, 7, 30, 0, 0, time.UTC),
		Transitions:   []rpc.BriefRuleTransition{{RuleID: "hedge_coverage", From: "watch", To: risk.RuleStatusAct}},
	}
	res.Review.CapitalEvents = rpc.BriefCapitalEventsRow{
		BriefRowState: briefAttention("drawdown latch engaged this episode"),
		Latched:       true, LatchAgeDays: &age, ConsumedPctAtLatch: &latchPct,
		AdjustedPeakBase: res.Review.CapitalEvents.AdjustedPeakBase, BaseCurrency: "EUR",
	}
	res.Ready.Capital.BriefRowState = briefAttention("drawdown block tier is breached")
	res.Ready.Capital.Tier = risk.CapitalTierBlock
	res.Ready.Capital.ConsumedPct = &consumed
	res.Ready.Latch = rpc.BriefLatchRow{BriefRowState: briefAttention("drawdown latch is engaged"),
		Latched: true, AgeDays: &age, ConsumedPctAtLatch: &latchPct}
	res.Ready.Stress = rpc.BriefStressRow{BriefRowState: briefOK("pure stress composition"),
		Action: "de-risk", Severity: string(risk.SeverityAct)}
	return res
}

// degradedBriefResult loses whole sources: the account, positions-derived
// rows, the regime, breadth, and the proposal journal.
func degradedBriefResult() *rpc.BriefResult {
	res := quietBriefResult()
	res.Review.SessionPnL = rpc.BriefAccountRow{BriefRowState: briefUnavailable("account summary unavailable: broker down")}
	res.Review.Attribution = rpc.BriefMoversRow{BriefRowState: briefUnavailable("positions unavailable")}
	res.Review.Proposals = rpc.BriefProposalsRow{BriefRowState: briefUnavailable("proposal outcome journal is unavailable")}
	res.Review.WorkingOrders = rpc.BriefCountRow{BriefRowState: briefUnavailable("open-orders journal unavailable")}
	res.Ready.Regime = rpc.BriefRegimeRow{BriefRowState: briefUnavailable("regime snapshot unavailable")}
	res.Ready.Breadth = rpc.BriefBreadthRow{BriefRowState: briefUnavailable("breadth snapshot unavailable")}
	res.Ready.Gamma = rpc.BriefGammaRow{BriefRowState: briefDegraded("dealer gamma source is stale")}
	res.Ready.PremiumAtRisk = rpc.BriefMoneyCoverageRow{BriefRowState: briefUnavailable("positions unavailable")}
	res.Ready.HedgeCost = rpc.BriefMoneyCoverageRow{BriefRowState: briefUnavailable("positions unavailable")}
	res.Ready.Capital = rpc.BriefCapitalRow{BriefRowState: briefUnavailable("risk constitution absent")}
	res.Ready.MarketEvents = nil
	return res
}

func TestComposeBriefNarrativeCompositions(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name       string
		result     *rpc.BriefResult
		wantLead   []string
		wantText   []string
		wantAbsent []string
		wantWatch  []string
		wantAct    []string
		wantReview int
		wantReady  int
	}{
		{
			name:   "quiet folds every clean topic into summary clauses",
			result: quietBriefResult(),
			wantLead: []string{
				"Stress reads stand down at observe severity.",
				"Regime stage quiet, verdict Normal regime.",
				"Nothing across Review and Ready needs a decision.",
			},
			wantText: []string{
				"The session closed with Daily P/L EUR +2,340 on equity of EUR 1,250,000.",
				"By name in EUR: AAPL +900.00, NVDA +800.40, SPY +639.60, and 2 other names at -120.00.",
				"2 protection proposals were offered and 1 acted in the last recorded session (2026-07-29), with no overrides, no rule transitions, and no capital events.",
				"The adjusted peak holds at EUR 1,275,000.",
				"Admin is clean: reconcile clean and due in 5 days, auto-extend needs nothing, nothing waits for sign-off, and 1 order is working.",
				"Breadth has 62.0% above the 50-DMA and 58.0% above the 200-DMA, net new highs +1.4%.",
				"Dealer gamma is positive with spot 620.00 against zero gamma 607.00, a gap of +2.1%.",
				"The US session is regular.",
				"Capital sits in the ok tier under shadow enforcement with 2.0% of the drawdown budget consumed.",
				"Drawdown stands at EUR 25,000 from an adjusted peak of EUR 1,275,000.",
				"The drawdown latch is open.",
				"Premium at risk EUR 1,564 across 1 long option leg.",
				"Hedge cost EUR -18.40 per day across 1 classified hedge leg.",
				"Process folds clean: policy pins match, cadence artefacts are declared with 1 of 2 complete, the monthly pulse is not due, and no held-name events.",
				"Nothing owed before the bell.",
			},
			// Quiet has nothing to tint and nothing to disclose as unread.
			wantAbsent: []string{"could not be read", "unknown is not clean"},
			wantReview: 1,
			wantReady:  2,
		},
		{
			name:   "watch grows only where its rows are flagged",
			result: watchBriefResult(),
			wantLead: []string{
				"Stress reads reduce risk at watch severity.",
				"2 rows need a decision: overrides, held-name earnings.",
			},
			wantText: []string{
				"1 active override widens policy controls: hedge_coverage.",
				"No rule transitions.",
				"2 held names carry earnings context: AAPL, NVDA.",
				// The unflagged kinds are stated as checked and clean, never
				// left silent next to a flagged one.
				"The remaining held-name event source is clean.",
				"Owed before the bell: overrides, held-name earnings. Everything else holds.",
			},
			wantWatch: []string{
				"reduce risk at watch severity",
				"1 active override widens policy controls: hedge_coverage.",
				"2 held names carry earnings context",
			},
			wantAbsent: []string{"Process folds clean"},
			wantReview: 2,
			wantReady:  3,
		},
		{
			name:   "act tints only the rows whose own vocabulary says act",
			result: actBriefResult(),
			wantLead: []string{
				"Stress reads de-risk at act severity.",
				"4 rows need a decision: rules delta, capital events, capital, drawdown latch.",
				"Regime stage quiet, verdict Normal regime.",
			},
			wantText: []string{
				"1 rule worsened to act since the last stamped brief: hedge coverage.",
				"The drawdown latch engaged this episode and remains open until a human reset.",
				"Capital sits in the block tier under shadow enforcement with 118.0% of the drawdown budget consumed.",
				"The drawdown latch is engaged, 2 days old, and remains so until a human reset.",
				"It engaged at 30.4% consumed.",
			},
			wantAct: []string{
				"de-risk at act severity",
				"1 rule worsened to act since the last stamped brief: hedge coverage.",
				"The drawdown latch engaged this episode and remains open until a human reset.",
				"Capital sits in the block tier",
			},
			// Act's process rows are clean, so the Ready movement still folds
			// them into the book paragraph: the brief grows where the problems
			// are, not everywhere at once.
			wantReview: 2,
			wantReady:  2,
		},
		{
			name:   "degraded names every unread source and invents no value",
			result: degradedBriefResult(),
			wantLead: []string{
				"The regime read is unavailable.",
				"Nothing across Review and Ready needs a decision.",
				"11 inputs could not be read and are named below: session P/L, attribution, proposals, working orders, regime, breadth, dealer gamma, held-name events, capital, premium at risk, hedge cost.",
			},
			wantText: []string{
				"The last completed session's account P/L is unavailable, so the session cannot be summarized.",
				"Per-name attribution is unavailable.",
				"The proposal outcome journal is unavailable.",
				"The open-orders journal is unavailable.",
				"Breadth is unavailable, so participation cannot be stated.",
				"Dealer gamma is degraded, so the spot-to-zero-gamma relationship cannot be stated.",
				"The risk constitution is absent, so capital controls are unapproved and the drawdown budget cannot be stated.",
				"Premium at risk is unavailable.",
				"Hedge cost is unavailable.",
				"Held-name event coverage is unavailable.",
				"Nothing on the desk needs a decision, but 11 inputs could not be read: unknown is not clean.",
			},
			// Nothing may be asserted about values the payload did not serve.
			wantAbsent: []string{
				"Daily P/L EUR", "Breadth has", "Premium at risk EUR", "Hedge cost EUR",
				"drawdown budget consumed", "No held-name events", "Process folds clean",
			},
			wantReview: 3,
			wantReady:  3,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			narrative := composeBriefNarrative(tc.result)
			if narrative == nil {
				t.Fatal("composer returned no narrative")
			}
			all := narrativeAll(narrative)
			lead := narrativeText(narrative.Lead)
			for _, want := range tc.wantLead {
				if !strings.Contains(lead, want) {
					t.Errorf("lead missing %q\nlead: %s", want, lead)
				}
			}
			for _, want := range tc.wantText {
				if !strings.Contains(all, want) {
					t.Errorf("narrative missing %q\n%s", want, all)
				}
			}
			for _, absent := range tc.wantAbsent {
				if strings.Contains(all, absent) {
					t.Errorf("narrative must not contain %q\n%s", absent, all)
				}
			}
			assertRoleRuns(t, narrative, rpc.BriefRunRoleWatch, tc.wantWatch)
			assertRoleRuns(t, narrative, rpc.BriefRunRoleAct, tc.wantAct)
			if len(narrative.Review) != tc.wantReview {
				t.Errorf("review paragraphs = %d, want %d\n%s", len(narrative.Review), tc.wantReview, strings.Join(narrativeParagraphs(narrative.Review), "\n--\n"))
			}
			if len(narrative.Ready) != tc.wantReady {
				t.Errorf("ready paragraphs = %d, want %d\n%s", len(narrative.Ready), tc.wantReady, strings.Join(narrativeParagraphs(narrative.Ready), "\n--\n"))
			}
			assertNarrativeHygiene(t, narrative)
		})
	}
}

func assertRoleRuns(t *testing.T, narrative *rpc.BriefNarrative, role string, want []string) {
	t.Helper()
	tinted := strings.Join(narrativeRoleText(narrative, role), "\n")
	for _, fragment := range want {
		if !strings.Contains(tinted, fragment) {
			t.Errorf("%s-role runs missing %q\n%s", role, fragment, tinted)
		}
	}
	if len(want) == 0 && tinted != "" {
		t.Errorf("%s-role runs must be empty, got %q", role, tinted)
	}
}

// assertNarrativeHygiene holds the invariants every composition owes: runs
// carry text not markup, roles stay inside the declared vocabulary, and no
// paragraph is empty or padded.
func assertNarrativeHygiene(t *testing.T, narrative *rpc.BriefNarrative) {
	t.Helper()
	roles := map[string]bool{"": true, rpc.BriefRunRoleFigure: true, rpc.BriefRunRoleWatch: true, rpc.BriefRunRoleAct: true}
	check := func(where string, runs []rpc.BriefRun) {
		if len(runs) == 0 {
			t.Errorf("%s has no runs", where)
			return
		}
		for i, run := range runs {
			if !roles[run.Role] {
				t.Errorf("%s run %d carries unknown role %q", where, i, run.Role)
			}
			if run.Text == "" {
				t.Errorf("%s run %d is empty", where, i)
			}
			if strings.ContainsAny(run.Text, "<>") || strings.Contains(run.Text, "[f]") || strings.Contains(run.Text, "[w]") || strings.Contains(run.Text, "[a]") {
				t.Errorf("%s run %d carries markup, not text: %q", where, i, run.Text)
			}
			if i > 0 && runs[i-1].Role == run.Role {
				t.Errorf("%s run %d repeats role %q instead of merging: %q", where, i, run.Role, run.Text)
			}
		}
		text := narrativeText(runs)
		if strings.Contains(text, "  ") || strings.TrimSpace(text) != text {
			t.Errorf("%s has stray whitespace: %q", where, text)
		}
	}
	check("lead", narrative.Lead)
	check("coda", narrative.Coda)
	for i, paragraph := range narrative.Review {
		check("review paragraph "+string(rune('1'+i)), paragraph.Runs)
	}
	for i, paragraph := range narrative.Ready {
		check("ready paragraph "+string(rune('1'+i)), paragraph.Runs)
	}
}

// TestBriefNarrativeQuietTapeStaysShort pins the growth rule: the Ready tape
// restates the posture only when the posture carries tone or cannot be read.
func TestBriefNarrativeQuietTapeStaysShort(t *testing.T) {
	t.Parallel()
	quiet := composeBriefNarrative(quietBriefResult())
	tape := narrativeText(quiet.Ready[0].Runs)
	for _, restatement := range []string{"Stress reads", "Regime "} {
		if strings.Contains(tape, restatement) {
			t.Errorf("a quiet tape must not restate the lead (%q):\n%s", restatement, tape)
		}
	}
	if !strings.HasPrefix(tape, "Breadth has ") {
		t.Errorf("a quiet tape opens on the market read:\n%s", tape)
	}
	watch := composeBriefNarrative(watchBriefResult())
	if !strings.HasPrefix(narrativeText(watch.Ready[0].Runs), "Stress reads reduce risk at watch severity.") {
		t.Errorf("a flagged tape opens on the flagged reading:\n%s", narrativeText(watch.Ready[0].Runs))
	}
}

// TestComposeBriefServesNarrativeWithTheMovements exercises the composer
// through the real brief composition: the daemon serves prose next to the
// rows, and the prose stays outside the content identity.
func TestComposeBriefServesNarrativeWithTheMovements(t *testing.T) {
	s := newRiskPolicyTestServer(t, dailyBriefPolicyTOML())
	res, _ := s.composeBrief(context.Background())
	if res == nil || res.Narrative == nil {
		t.Fatal("composeBrief served no narrative")
	}
	withNarrative := briefContentFingerprint(res)
	res.Narrative = nil
	if withoutNarrative := briefContentFingerprint(res); withoutNarrative != withNarrative {
		t.Fatalf("the narrative must stay outside the brief identity: %s vs %s", withNarrative, withoutNarrative)
	}
	res.Narrative = composeBriefNarrative(res)
	if len(res.Narrative.Review) == 0 || len(res.Narrative.Ready) == 0 {
		t.Fatalf("both movements must compose: review=%d ready=%d", len(res.Narrative.Review), len(res.Narrative.Ready))
	}
	assertNarrativeHygiene(t, res.Narrative)
}

// TestComposeBriefNarrativeIsDeterministic pins the contract the SPA depends
// on: same payload, same prose, every time.
func TestComposeBriefNarrativeIsDeterministic(t *testing.T) {
	t.Parallel()
	for _, build := range []func() *rpc.BriefResult{quietBriefResult, watchBriefResult, actBriefResult, degradedBriefResult} {
		first, second := composeBriefNarrative(build()), composeBriefNarrative(build())
		if narrativeAll(first) != narrativeAll(second) {
			t.Fatalf("composition is not deterministic:\n%s\n----\n%s", narrativeAll(first), narrativeAll(second))
		}
	}
	if composeBriefNarrative(nil) != nil {
		t.Fatal("nil result must compose no narrative")
	}
}

// TestBriefNarrativeStaysOutsideBriefIdentity is the stamp-safety pin: the
// narrative may never move the fingerprint, or revised prose would silently
// invalidate an operator's stamped brief.
func TestBriefNarrativeStaysOutsideBriefIdentity(t *testing.T) {
	t.Parallel()
	res := quietBriefResult()
	before := briefContentFingerprint(res)
	res.Narrative = composeBriefNarrative(res)
	if after := briefContentFingerprint(res); after != before {
		t.Fatalf("narrative changed the brief identity: %s -> %s", before, after)
	}
}

// TestBriefNarrativeNeverInventsMissingFigures walks the payload's optional
// money and percentage fields: with the value absent, the prose must state the
// absence and must not print a number in its place.
func TestBriefNarrativeNeverInventsMissingFigures(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		mutate func(*rpc.BriefResult)
		want   string
		absent []string
	}{
		{
			name: "consumed share absent",
			mutate: func(res *rpc.BriefResult) {
				res.Ready.Capital.ConsumedPct = nil
				res.Ready.Capital.BriefRowState = briefDegraded("capital inputs are unapproved")
			},
			want:   "the consumed share is unavailable",
			absent: []string{"of the drawdown budget consumed"},
		},
		{
			name: "adjusted peak absent",
			mutate: func(res *rpc.BriefResult) {
				res.Review.CapitalEvents.AdjustedPeakBase = nil
			},
			want:   "The adjusted peak is unavailable.",
			absent: []string{"The adjusted peak holds at"},
		},
		{
			name: "reconcile deadline unapproved",
			mutate: func(res *rpc.BriefResult) {
				res.Review.Reconcile.DaysRemaining = nil
				res.Review.Reconcile.BriefRowState = briefDegraded("capital.max_unreconciled_days is unapproved")
			},
			want:   "Reconcile evidence exists, but its horizon is unapproved, so no deadline can be stated.",
			absent: []string{"due in"},
		},
		{
			name: "no reconcile evidence at all",
			mutate: func(res *rpc.BriefResult) {
				res.Review.Reconcile = rpc.BriefReconcileRow{BriefRowState: briefDegraded("no reconcile evidence has been recorded")}
			},
			want:   "No reconcile evidence has been recorded.",
			absent: []string{"reconcile clean"},
		},
		{
			name: "reconcile past its horizon",
			mutate: func(res *rpc.BriefResult) {
				overdue := -3
				res.Review.Reconcile.DaysRemaining = &overdue
				res.Review.Reconcile.BriefRowState = briefDegraded("reconcile evidence is past its declared horizon")
			},
			want:   "Reconcile evidence is 3 days past its declared horizon.",
			absent: []string{"reconcile clean"},
		},
		{
			name: "zero-gamma level absent",
			mutate: func(res *rpc.BriefResult) {
				res.Ready.Gamma.ZeroGamma = nil
				res.Ready.Gamma.BriefRowState = briefDegraded("gamma result lacks a complete classification")
			},
			want:   "Dealer gamma is degraded, so the spot-to-zero-gamma relationship cannot be stated.",
			absent: []string{"against zero gamma"},
		},
		{
			name: "rulebook delta has no baseline",
			mutate: func(res *rpc.BriefResult) {
				res.Review.RulesDelta = rpc.BriefRulesDeltaRow{BriefRowState: briefDegraded("no delta baseline yet")}
			},
			want:   "There is no rulebook delta baseline yet, so rule changes since the last stamped brief cannot be verified.",
			absent: []string{"No rule transitions."},
		},
		{
			name: "premium at risk has no convertible leg",
			mutate: func(res *rpc.BriefResult) {
				res.Ready.PremiumAtRisk.AmountBase = nil
				res.Ready.PremiumAtRisk.ExcludedLegs = 2
				res.Ready.PremiumAtRisk.IncludedLegs = 0
				res.Ready.PremiumAtRisk.BriefRowState = briefDegraded("2 long option legs excluded")
			},
			want:   "Premium at risk cannot be totalled: no long option leg carries a base market value.",
			absent: []string{"Premium at risk EUR"},
		},
		{
			name: "monthly pulse blocked",
			mutate: func(res *rpc.BriefResult) {
				res.Ready.MonthlyPulse = &rpc.BriefMonthlyPulseRow{Status: rpc.BriefMonthlyPulseBlocked, Month: "2026-07"}
			},
			want:   "The monthly pulse is blocked by policy evidence.",
			absent: []string{"the monthly pulse is not due"},
		},
		{
			name: "stress serves neither action nor severity",
			mutate: func(res *rpc.BriefResult) {
				res.Ready.Stress = rpc.BriefStressRow{BriefRowState: briefDegraded("stress inputs are partial")}
			},
			want:   "Stress carries no action or severity.",
			absent: []string{"Stress reads"},
		},
		{
			name: "stress row unavailable",
			mutate: func(res *rpc.BriefResult) {
				res.Ready.Stress = rpc.BriefStressRow{BriefRowState: briefUnavailable("stress snapshot unavailable")}
			},
			want:   "Stress is unavailable, so the desk posture cannot be stated.",
			absent: []string{"Stress reads"},
		},
		{
			name: "policy version serves no monthly pulse",
			mutate: func(res *rpc.BriefResult) {
				res.Ready.MonthlyPulse = nil
			},
			want:   "Process folds clean: policy pins match",
			absent: []string{"monthly pulse"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			res := quietBriefResult()
			tc.mutate(res)
			narrative := composeBriefNarrative(res)
			all := narrativeAll(narrative)
			if !strings.Contains(all, tc.want) {
				t.Errorf("narrative missing %q\n%s", tc.want, all)
			}
			for _, absent := range tc.absent {
				if strings.Contains(all, absent) {
					t.Errorf("narrative must not contain %q\n%s", absent, all)
				}
			}
			assertNarrativeHygiene(t, narrative)
		})
	}
}

// TestBriefNarrativeTintsFollowServedStatus is the tone contract: data
// conditions never borrow risk tone, and risk conditions never lose it.
func TestBriefNarrativeTintsFollowServedStatus(t *testing.T) {
	t.Parallel()
	degraded := composeBriefNarrative(degradedBriefResult())
	if tinted := narrativeRoleText(degraded, rpc.BriefRunRoleWatch); len(tinted) > 0 {
		t.Errorf("degraded inputs must not tint watch: %q", tinted)
	}
	if tinted := narrativeRoleText(degraded, rpc.BriefRunRoleAct); len(tinted) > 0 {
		t.Errorf("degraded inputs must not tint act: %q", tinted)
	}

	// An attention row without an act-class fact stays watch even when the
	// desk severity around it is act.
	res := actBriefResult()
	res.Review.Overrides = rpc.BriefOverridesRow{BriefRowState: briefAttention("1 active override"),
		Rows: []rpc.BriefOverride{{Control: "per_name_exposure"}}}
	narrative := composeBriefNarrative(res)
	watch := strings.Join(narrativeRoleText(narrative, rpc.BriefRunRoleWatch), "\n")
	if !strings.Contains(watch, "per_name_exposure") {
		t.Errorf("attention override must carry the watch role, got %q", watch)
	}
	act := strings.Join(narrativeRoleText(narrative, rpc.BriefRunRoleAct), "\n")
	if strings.Contains(act, "per_name_exposure") {
		t.Errorf("attention override must not be promoted to act: %q", act)
	}

	// Stress reads its tone from the served severity, not the row status.
	quiet := quietBriefResult()
	quiet.Ready.Stress.BriefRowState = briefDegraded("stress inputs are partial")
	if tinted := narrativeRoleText(composeBriefNarrative(quiet), rpc.BriefRunRoleWatch); len(tinted) > 0 {
		t.Errorf("degraded stress inputs at observe severity must not tint: %q", tinted)
	}
}
