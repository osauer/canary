package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/risk"
	"github.com/osauer/canary/v2/internal/rpc"
)

func dailyBriefPolicyTOML() string {
	return validRiskPolicyTOML
}

func TestComposeBriefCapturesBoundaryAfterInputs(t *testing.T) {
	s := newRiskPolicyTestServer(t, dailyBriefPolicyTOML())
	brief, rules := s.composeBrief(context.Background())
	if brief == nil || rules == nil {
		t.Fatalf("composeBrief returned brief=%v rules=%v", brief, rules)
	}
	if brief.AsOf.Before(rules.AsOf) {
		t.Fatalf("brief boundary %s precedes rulebook snapshot %s", brief.AsOf, rules.AsOf)
	}
	for _, source := range rules.InputHealth {
		if !source.AsOf.IsZero() && brief.AsOf.Before(source.AsOf) {
			t.Fatalf("brief boundary %s precedes %s input %s", brief.AsOf, source.Source, source.AsOf)
		}
	}
}

func TestBriefSnapshotPurityAndDegradedRows(t *testing.T) {
	s := newRiskPolicyTestServer(t, dailyBriefPolicyTOML())
	root := os.Getenv("XDG_STATE_HOME")
	before := stateTree(t, root)
	for range 3 {
		res, _ := s.composeBrief(context.Background())
		if res.Ready.Regime.Status != rpc.BriefStatusUnavailable || res.Review.SessionPnL.Status != rpc.BriefStatusUnavailable {
			t.Fatalf("gateway rows not unavailable: regime=%+v session_pnl=%+v", res.Ready.Regime, res.Review.SessionPnL)
		}
		if res.Ready.Capital.Status == "" || res.Review.Reconcile.Status == "" || res.BriefFingerprint == "" {
			t.Fatalf("policy/process rows did not render: %+v", res)
		}
	}
	after := stateTree(t, root)
	if !slices.Equal(before, after) {
		t.Fatalf("brief.snapshot mutated state tree: before=%v after=%v", before, after)
	}
}

func TestBriefRulesStatusUsesCurrentPolicyEvidence(t *testing.T) {
	rows := &rpc.RulesResult{Status: "ok", Rules: []risk.RuleRow{
		{Status: risk.RuleStatusPass}, {Status: risk.RuleStatusPass},
		{Status: risk.RuleStatusWatch}, {Status: risk.RuleStatusAct}, {Status: risk.RuleStatusUnknown},
	}}
	got := briefRulesStatus(rows)
	if got.Pass != 2 || got.Watch != 1 || got.Act != 1 || got.Unknown != 1 || got.Status != rpc.BriefStatusAttention {
		t.Fatalf("current policy summary = %+v", got)
	}
	if empty := briefRulesStatus(nil); empty.Status != rpc.BriefStatusUnavailable {
		t.Fatalf("missing policy summary = %+v", empty)
	}
}

func stateTree(t *testing.T, root string) []string {
	t.Helper()
	var out []string
	_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err == nil && path != root {
			rel, _ := filepath.Rel(root, path)
			out = append(out, rel)
		}
		return nil
	})
	slices.Sort(out)
	return out
}

// The kept rule worsened to act: a risk deterioration lifts the row to
// attention instead of hiding under data-quality vocabulary.

func TestBriefNilMoneyAndGreeksDegradeWithoutZeroFill(t *testing.T) {
	pos := &rpc.PositionsResult{Options: []rpc.PositionView{
		{Symbol: "AAPL", SecType: "OPT", Right: "C", Quantity: 1},
		{Symbol: "SPY", SecType: "OPT", Right: "P", Quantity: 1, Multiplier: 100},
	}}
	premium := briefPremiumAtRisk(pos, "EUR")
	if premium.Status != rpc.BriefStatusDegraded || premium.AmountBase != nil || premium.ExcludedLegs != 2 {
		t.Fatalf("premium=%+v", premium)
	}
	hedge := briefHedgeCost(pos, "EUR")
	if hedge.Status != rpc.BriefStatusDegraded || hedge.AmountBase != nil || hedge.ExcludedLegs != 1 {
		t.Fatalf("hedge=%+v", hedge)
	}
}

func TestBriefProposalsSessionSummaryFromJournal(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "trade-proposal-outcomes.jsonl")
	lines := []proposalOutcomeMark{
		// Older session: one proposal offered (marked), not acted.
		{Version: 1, MarkDate: "2026-07-17", State: proposalOutcomeStateMarked, ProposalKey: "P-OLD-1"},
		// Latest session (2026-07-18): three distinct proposals offered, one
		// submitted+filled (acted once, deduped), one only marked.
		{Version: 1, MarkDate: "2026-07-18", State: proposalOutcomeStateMarked, ProposalKey: "P-A"},
		{Version: 1, MarkDate: "2026-07-18", State: proposalOutcomeStateSubmitted, ProposalKey: "P-B"},
		{Version: 1, MarkDate: "2026-07-18", State: proposalOutcomeStateFilled, ProposalKey: "P-B"},
		{Version: 1, MarkDate: "2026-07-18", State: proposalOutcomeStateMarked, ProposalKey: "P-C"},
	}
	var buf strings.Builder
	for _, m := range lines {
		raw, _ := json.Marshal(m)
		buf.Write(raw)
		buf.WriteByte('\n')
	}
	if err := os.WriteFile(path, []byte(buf.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	store := newProposalOutcomeStore(path)
	offered, acted, day, ok, err := store.SessionSummary()
	if err != nil || !ok {
		t.Fatalf("summary ok=%v err=%v", ok, err)
	}
	if day != "2026-07-18" || offered != 3 || acted != 1 {
		t.Fatalf("latest session summary day=%q offered=%d acted=%d", day, offered, acted)
	}

	// The wire row leaks no proposal identity — counts and the day only.
	s := &Server{proposalOutcomes: store}
	row := s.briefProposals(time.Now())
	if row.Status != rpc.BriefStatusOK || row.Offered != 3 || row.Acted != 1 || row.Day != "2026-07-18" {
		t.Fatalf("proposals row=%+v", row)
	}
	raw, _ := json.Marshal(row)
	for _, forbidden := range []string{"P-A", "P-B", "P-C", "proposal_key"} {
		if strings.Contains(string(raw), forbidden) {
			t.Fatalf("proposals row leaked %q: %s", forbidden, raw)
		}
	}

	// Missing journal reads as a clean "no proposals" row, never an error.
	empty := &Server{proposalOutcomes: newProposalOutcomeStore(filepath.Join(dir, "missing.jsonl"))}
	if got := empty.briefProposals(time.Now()); got.Status != rpc.BriefStatusOK || got.Offered != 0 {
		t.Fatalf("missing-journal proposals row=%+v", got)
	}
}

func TestBriefCapitalEventsRegroupsLatchAndPeak(t *testing.T) {
	age := 4
	consumed := 30.4
	peak := 260000.0
	peakAsOf := time.Date(2026, 7, 15, 20, 0, 0, 0, time.UTC)
	latch := rpc.BriefLatchRow{BriefRowState: briefAttention("engaged"), Latched: true, AgeDays: &age, ConsumedPctAtLatch: &consumed}
	capital := rpc.BriefCapitalRow{BriefRowState: briefOK("ok"), AdjustedPeakBase: &peak, PeakAsOf: peakAsOf, BaseCurrency: "EUR"}
	got := briefCapitalEvents(capital, latch)
	if got.Status != rpc.BriefStatusAttention || !got.Latched || got.LatchAgeDays == nil || *got.LatchAgeDays != 4 {
		t.Fatalf("latched capital events=%+v", got)
	}
	if got.AdjustedPeakBase == nil || *got.AdjustedPeakBase != peak || !got.PeakAsOf.Equal(peakAsOf) {
		t.Fatalf("peak provenance did not flow: %+v", got)
	}

	// An absent constitution renders capital events unavailable, not a clean line.
	if unavailable := briefCapitalEvents(rpc.BriefCapitalRow{BriefRowState: briefUnavailable("absent")}, rpc.BriefLatchRow{}); unavailable.Status != rpc.BriefStatusUnavailable {
		t.Fatalf("absent constitution capital events=%+v", unavailable)
	}
	// A quiet book reads ok.
	if quiet := briefCapitalEvents(rpc.BriefCapitalRow{BriefRowState: briefOK("ok")}, rpc.BriefLatchRow{BriefRowState: briefOK("open")}); quiet.Status != rpc.BriefStatusOK || quiet.Latched {
		t.Fatalf("quiet capital events=%+v", quiet)
	}
}

func TestBriefResultContainsNoPrivateIdentityOrTokenFields(t *testing.T) {
	s := newRiskPolicyTestServer(t, dailyBriefPolicyTOML())
	res, _ := s.composeBrief(context.Background())
	raw, err := json.Marshal(res)
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	for _, forbidden := range []string{"account_id", "order_id", "order_ref", "preview_token", "submit_eligible"} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("brief result contains forbidden field %q: %s", forbidden, text)
		}
	}
}

func TestUnreconciledClockSharedProjection(t *testing.T) {
	now := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
	last := now.Add(-5 * 24 * time.Hour)
	maxDays := 7
	clock := risk.EvaluateUnreconciledClock(&maxDays, last, time.Time{}, now)
	if !clock.Approved || clock.Stale || !clock.Deadline.Equal(last.Add(7*24*time.Hour)) || clock.DaysRemaining == nil || *clock.DaysRemaining != 2 {
		t.Fatalf("clock=%+v", clock)
	}
	override := now.Add(4 * 24 * time.Hour)
	clock = risk.EvaluateUnreconciledClock(&maxDays, last, override, now)
	if !clock.Deadline.Equal(override) || clock.DaysRemaining == nil || *clock.DaysRemaining != 4 {
		t.Fatalf("override clock=%+v", clock)
	}
	never := risk.EvaluateUnreconciledClock(&maxDays, time.Time{}, time.Time{}, now)
	if !never.Stale || !never.Deadline.IsZero() || never.DaysRemaining != nil {
		t.Fatalf("never clock=%+v", never)
	}
}

func TestBriefRiskStatusDerivesFromValues(t *testing.T) {
	now := time.Date(2026, 7, 19, 20, 0, 0, 0, time.UTC)
	base := func() *rpc.RiskPolicyResult {
		consumed := 10.0
		return &rpc.RiskPolicyResult{Status: rpc.RiskPolicyStatusActive,
			Capital: rpc.CapitalStateReport{Tier: risk.CapitalTierOK, Enforcement: "shadow", ConsumedPct: &consumed}}
	}

	blocked := base()
	consumed := 1589.7
	blocked.Capital.Tier = risk.CapitalTierBlock
	blocked.Capital.ConsumedPct = &consumed
	blocked.Capital.BlockLatched = true
	blocked.Capital.LatchedAt = now.Add(-4 * 24 * time.Hour)
	blocked.Capital.PeakAsOf = now.Add(-3 * time.Hour)
	latchPct := 30.41
	blocked.Capital.LatchConsumedPct = &latchPct
	blocked.Capital.Enforcement = "shadow"
	out := composeBriefRisk(blocked, now)
	if out.Capital.Status != rpc.BriefStatusAttention || out.Latch.Status != rpc.BriefStatusAttention {
		t.Fatalf("blocked tier must render attention: capital=%+v latch=%+v", out.Capital.BriefRowState, out.Latch.BriefRowState)
	}
	if out.Capital.PeakAsOf.IsZero() || out.Latch.ConsumedPctAtLatch == nil || *out.Latch.ConsumedPctAtLatch != latchPct {
		t.Fatalf("provenance must flow into the brief: capital=%+v latch=%+v", out.Capital, out.Latch)
	}
	if !strings.Contains(out.Capital.Detail, "shadow enforcement journals what would block") {
		t.Fatalf("shadow enforcement must not imply an active block: %q", out.Capital.Detail)
	}
	if out.Latch.AgeDays == nil || *out.Latch.AgeDays != 4 || !out.Latch.Latched {
		t.Fatalf("latch row=%+v", out.Latch)
	}
	if out.Status != rpc.BriefStatusAttention || !strings.Contains(out.Detail, "need attention") {
		t.Fatalf("section must roll up worst child: %+v", out.BriefRowState)
	}

	warn := base()
	warn.Capital.Tier = risk.CapitalTierWarn
	if got := composeBriefRisk(warn, now); got.Capital.Status != rpc.BriefStatusAttention {
		t.Fatalf("warn tier=%+v", got.Capital.BriefRowState)
	}

	overConsumed := base()
	full := 120.0
	overConsumed.Capital.ConsumedPct = &full
	if got := composeBriefRisk(overConsumed, now); got.Capital.Status != rpc.BriefStatusAttention {
		t.Fatalf("consumed>=100%% with ok tier must still render attention: %+v", got.Capital.BriefRowState)
	}

	unapproved := base()
	unapproved.Capital.Tier = risk.CapitalTierUnapproved
	unapproved.Capital.ConsumedPct = nil
	if got := composeBriefRisk(unapproved, now); got.Capital.Status != rpc.BriefStatusDegraded {
		t.Fatalf("unapproved=%+v", got.Capital.BriefRowState)
	}

	override := base()
	override.Overrides = []rpc.OverrideRecord{{Control: "drawdown.block", Active: true, ExpiresAt: now.Add(time.Hour)}}
	got := composeBriefRisk(override, now)
	if got.Overrides.Status != rpc.BriefStatusAttention || len(got.Overrides.Rows) != 1 {
		t.Fatalf("active override=%+v", got.Overrides)
	}

	if got := composeBriefRisk(base(), now); got.Status != rpc.BriefStatusOK || got.Detail != "risk and limits section complete" {
		t.Fatalf("healthy section=%+v", got.BriefRowState)
	}
}

func TestBriefSectionStateWorstChildAndCompleteness(t *testing.T) {
	att := briefSectionState("risk", briefOK(""), briefAttention(""), briefDegraded(""))
	if att.Status != rpc.BriefStatusAttention || !strings.Contains(att.Detail, "1 of 3 rows needs attention") || !strings.Contains(att.Detail, "1 degraded or unavailable") {
		t.Fatalf("attention rollup=%+v", att)
	}
	deg := briefSectionState("market", briefOK(""), briefDegraded(""), briefUnavailable(""))
	if deg.Status != rpc.BriefStatusDegraded || !strings.Contains(deg.Detail, "2 of 3 rows degraded or unavailable") {
		t.Fatalf("degraded rollup=%+v", deg)
	}
	if got := briefSectionState("x", briefUnavailable(""), briefUnavailable("")); got.Status != rpc.BriefStatusUnavailable {
		t.Fatalf("all-unavailable rollup=%+v", got)
	}
	if got := briefSectionState("x", briefOK("")); got.Status != rpc.BriefStatusOK || got.Detail != "x section complete" {
		t.Fatalf("ok rollup=%+v", got)
	}
}

func TestBriefClosedSessionDowngradesExpectedColdness(t *testing.T) {
	asOf := time.Date(2026, 7, 17, 21, 58, 0, 0, time.UTC)
	events := &rpc.MarketEventsResult{SourceHealth: []rpc.SourceHealth{
		{Source: "trading_halts", Status: rpc.SourceStatusStale, AsOf: asOf},
		{Source: "reg_sho_threshold", Status: rpc.SourceStatusOK, AsOf: asOf},
		{Source: "borrow_fee", Status: rpc.SourceStatusDegraded, AsOf: asOf},
	}}
	rules := &rpc.RulesResult{}

	byKind := func(rows []rpc.BriefMarketEventRow) map[string]rpc.BriefMarketEventRow {
		out := map[string]rpc.BriefMarketEventRow{}
		for _, row := range rows {
			out[row.Kind] = row
		}
		return out
	}

	closed := byKind(briefMarketEventRows(events, rules, nil, false))
	if row := closed["halt"]; row.Status != rpc.BriefStatusOK || !strings.Contains(row.Detail, "no fresh update expected while the market is closed") || !strings.Contains(row.Detail, "last checked") {
		t.Fatalf("closed stale halt row=%+v", row.BriefRowState)
	}
	if row := closed["ssr"]; row.Status != rpc.BriefStatusOK || strings.Contains(row.Detail, "market is closed") {
		t.Fatalf("healthy ssr source must render plain ok: %+v", row.BriefRowState)
	}
	// Degraded is abnormal-for-session and keeps its weight even while closed.
	if row := closed["borrow"]; row.Status != rpc.BriefStatusDegraded || !strings.Contains(row.Detail, "source health is degraded") {
		t.Fatalf("closed degraded borrow row=%+v", row.BriefRowState)
	}
	borrowMissed := &rpc.MarketEventsResult{SourceHealth: []rpc.SourceHealth{{
		Source: "borrow_fee", Status: rpc.SourceStatusStale, RefreshState: rpc.SourceRefreshNotDue, AsOf: asOf,
	}}}
	if row := byKind(briefMarketEventRows(borrowMissed, rules, nil, false))["borrow"]; row.Status != rpc.BriefStatusDegraded {
		t.Fatalf("stale last-good from before the latest completed session was quieted: %+v", row.BriefRowState)
	}
	borrowCold := &rpc.MarketEventsResult{SourceHealth: []rpc.SourceHealth{{
		Source: "borrow_fee", Status: rpc.SourceStatusUnknown, RefreshState: rpc.SourceRefreshNotDue, AsOf: asOf,
	}}}
	if row := byKind(briefMarketEventRows(borrowCold, rules, nil, false))["borrow"]; row.Status != rpc.BriefStatusOK || !strings.Contains(row.Detail, "no fresh update expected") {
		t.Fatalf("not-yet-due cold borrow source=%+v", row.BriefRowState)
	}
	// A status outside the known vocabulary is never quiet-eligible: only
	// stale/unknown may read as expected idleness while the market is closed.
	weird := &rpc.MarketEventsResult{SourceHealth: []rpc.SourceHealth{{Source: "trading_halts", Status: "auth_failed", AsOf: asOf}}}
	if row := byKind(briefMarketEventRows(weird, rules, nil, false))["halt"]; row.Status != rpc.BriefStatusDegraded {
		t.Fatalf("unrecognized status must degrade even closed: %+v", row.BriefRowState)
	}

	open := byKind(briefMarketEventRows(events, rules, nil, true))
	if row := open["halt"]; row.Status != rpc.BriefStatusDegraded {
		t.Fatalf("open-session stale source must stay degraded: %+v", row.BriefRowState)
	}
	if row := open["borrow"]; row.Status != rpc.BriefStatusDegraded {
		t.Fatalf("open-session degraded source must stay degraded: %+v", row.BriefRowState)
	}

	for _, row := range briefMarketEventRows(nil, rules, errors.New("positions unavailable"), false) {
		if row.Status != rpc.BriefStatusDegraded {
			t.Fatalf("hard source error must degrade even closed: %s=%+v", row.Kind, row.BriefRowState)
		}
	}

	cold := &rpc.GammaZeroSPXResult{Status: rpc.GammaZeroStatusCold}
	if got := composeBriefGamma(cold, false, asOf); got.Status != rpc.BriefStatusDegraded || !strings.Contains(got.Detail, rpc.DataCadenceNoLastGood) {
		t.Fatalf("closed cold gamma=%+v", got.BriefRowState)
	}
	if got := composeBriefGamma(cold, true, asOf); got.Status != rpc.BriefStatusDegraded {
		t.Fatalf("open cold gamma=%+v", got.BriefRowState)
	}
	if got := composeBriefGamma(&rpc.GammaZeroSPXResult{Status: rpc.GammaZeroStatusError}, false, asOf); got.Status != rpc.BriefStatusDegraded {
		t.Fatalf("gamma error must degrade even closed: %+v", got.BriefRowState)
	}
	mondayPreopen := time.Date(2026, 7, 20, 5, 5, 0, 0, time.UTC)
	lastSession := &rpc.GammaZeroSPXResult{Status: rpc.GammaZeroStatusReady, Result: &rpc.GammaZeroComputed{
		AsOf: asOf, SpotUnderlying: 6300, GammaSign: "negative",
		Quality: &rpc.GammaSignalQuality{Rankability: rpc.GammaRankabilityContextOnly, RankabilityReason: "freshness: market is closed; cached gamma is context only"},
	}}
	if got := composeBriefGamma(lastSession, false, mondayPreopen); got.Status != rpc.BriefStatusOK || !strings.Contains(got.Detail, "no newer regular-session compute is due") {
		t.Fatalf("last-completed-session gamma=%+v", got.BriefRowState)
	}
	blocked := *lastSession
	blockedResult := *lastSession.Result
	blocked.Result = &blockedResult
	blocked.Result.Quality = &rpc.GammaSignalQuality{Rankability: rpc.GammaRankabilityBlocked, RankabilityReason: "oi_observed_coverage: SPX OI is incomplete"}
	if got := composeBriefGamma(&blocked, false, mondayPreopen); got.Status != rpc.BriefStatusDegraded || !strings.Contains(got.Detail, "OI is incomplete") {
		t.Fatalf("blocked last-session gamma=%+v", got.BriefRowState)
	}
}

func TestBriefBorrowHealthRequiresShortStockExposure(t *testing.T) {
	events := &rpc.MarketEventsResult{SourceHealth: []rpc.SourceHealth{{
		Source: "borrow_fee", Status: rpc.SourceStatusUnknown,
		LastFailure: &rpc.SourceFailure{Code: "timeout", Stage: "ftp_control_connect", Retryable: true},
	}}}
	rules := &rpc.RulesResult{}
	row := func(rows []rpc.BriefMarketEventRow) rpc.BriefMarketEventRow {
		for _, candidate := range rows {
			if candidate.Kind == "borrow" {
				return candidate
			}
		}
		return rpc.BriefMarketEventRow{}
	}
	allLong := false
	if got := row(briefMarketEventRows(events, rules, nil, true, &allLong)); got.Status != rpc.BriefStatusOK || !strings.Contains(got.Detail, "no short-stock exposure") {
		t.Fatalf("all-long borrow row=%+v, want explicit non-required OK", got.BriefRowState)
	}
	short := true
	if got := row(briefMarketEventRows(events, rules, nil, true, &short)); got.Status != rpc.BriefStatusDegraded {
		t.Fatalf("short-book borrow row=%+v, want degraded", got.BriefRowState)
	}
	if got := row(briefMarketEventRows(events, rules, nil, true, nil)); got.Status != rpc.BriefStatusDegraded {
		t.Fatalf("unknown exposure borrow row=%+v, want fail-closed degraded", got.BriefRowState)
	}

	if relevant := briefBorrowFeeRelevant(&rpc.PositionsResult{Stocks: []rpc.PositionView{{Quantity: 10}}}, nil); relevant == nil || *relevant {
		t.Fatalf("all-long relevance=%v, want false", relevant)
	}
	if relevant := briefBorrowFeeRelevant(&rpc.PositionsResult{ByUnderlying: []rpc.PositionGroup{{Stock: &rpc.PositionView{Quantity: -1}}}}, nil); relevant == nil || !*relevant {
		t.Fatalf("grouped short relevance=%v, want true", relevant)
	}
	if relevant := briefBorrowFeeRelevant(&rpc.PositionsResult{Stocks: []rpc.PositionView{{SecType: "FUT", Quantity: -1}}}, nil); relevant == nil || *relevant {
		t.Fatalf("short future relevance=%v, want false", relevant)
	}
	if relevant := briefBorrowFeeRelevant(&rpc.PositionsResult{ByUnderlying: []rpc.PositionGroup{{Stock: &rpc.PositionView{SecType: "IND", Quantity: -1}}}}, nil); relevant == nil || *relevant {
		t.Fatalf("short index relevance=%v, want false", relevant)
	}
	if relevant := briefBorrowFeeRelevant(nil, errors.New("positions unavailable")); relevant != nil {
		t.Fatalf("unavailable positions relevance=%v, want nil", relevant)
	}
}

func TestBriefEarningsRowEscalatesWhenGoverningRuleUnknown(t *testing.T) {
	rules := &rpc.RulesResult{
		Rules:    []risk.RuleRow{{ID: "catalyst_coverage", Status: risk.RuleStatusUnknown}},
		Earnings: []rpc.EarningsInfo{{Symbol: "MSFT", Date: "2026-07-29", Source: "fetched"}},
	}
	rows := briefMarketEventRows(&rpc.MarketEventsResult{}, rules, nil, false)
	var earnings, halt rpc.BriefMarketEventRow
	for _, row := range rows {
		switch row.Kind {
		case "earnings":
			earnings = row
		case "halt":
			halt = row
		}
	}
	if earnings.Status != rpc.BriefStatusAttention || !strings.Contains(earnings.Detail, "catalyst coverage") || earnings.Count != 1 {
		t.Fatalf("earnings row must escalate on unknown governing rule: %+v", earnings)
	}
	if halt.Status != rpc.BriefStatusOK {
		t.Fatalf("halt row must not inherit the earnings escalation: %+v", halt.BriefRowState)
	}

	rules.Earnings = []rpc.EarningsInfo{{
		Symbol: "NOW", Source: "unknown", Status: rpc.EarningsStatusNoDatePublished,
		Reason: rpc.EarningsStatusNoDatePublished,
	}}
	for _, row := range briefMarketEventRows(&rpc.MarketEventsResult{}, rules, nil, false) {
		if row.Kind != "earnings" {
			continue
		}
		if row.Status != rpc.BriefStatusAttention || row.Count != 1 || len(row.Symbols) != 1 || row.Symbols[0] != "NOW" ||
			!strings.Contains(row.Detail, "NOW (no date published)") {
			t.Fatalf("sole unresolved catalyst must remain visible and non-green: %+v", row)
		}
	}

	rules.Rules[0].Status = risk.RuleStatusPass
	rules.Earnings = []rpc.EarningsInfo{{Symbol: "MSFT", Date: "2026-07-29", Source: "fetched", Status: rpc.EarningsStatusDate}}
	for _, row := range briefMarketEventRows(&rpc.MarketEventsResult{}, rules, nil, false) {
		if row.Kind == "earnings" && row.Status != rpc.BriefStatusOK {
			t.Fatalf("passing governing rule must not escalate: %+v", row.BriefRowState)
		}
	}
}

func TestBriefWSHEntitlementNoticeRequiresExactTypedTuple(t *testing.T) {
	makeRules := func(provider, code, stage string, retryable bool) *rpc.RulesResult {
		return &rpc.RulesResult{Earnings: []rpc.EarningsInfo{{Symbol: "SYNTH1", Providers: []rpc.EarningsProviderInfo{{
			Provider: provider, LastFailure: &rpc.SourceFailure{Code: code, Stage: stage, Retryable: retryable},
		}}}}}
	}
	for _, tc := range []struct {
		name  string
		rules *rpc.RulesResult
		want  bool
	}{
		{"metadata", makeRules("ibkr_wsh", rpc.SourceFailureNotEntitled, rpc.SourceFailureStageWSHMetadata, false), true},
		{"event", makeRules("ibkr_wsh", rpc.SourceFailureNotEntitled, rpc.SourceFailureStageWSHEvent, false), true},
		{"provider", makeRules("nasdaq", rpc.SourceFailureNotEntitled, rpc.SourceFailureStageWSHMetadata, false), false},
		{"code", makeRules("ibkr_wsh", rpc.SourceFailureInvalidPayload, rpc.SourceFailureStageWSHMetadata, false), false},
		{"stage", makeRules("ibkr_wsh", rpc.SourceFailureNotEntitled, rpc.SourceFailureStageWSHContractResolve, false), false},
		{"retryable", makeRules("ibkr_wsh", rpc.SourceFailureNotEntitled, rpc.SourceFailureStageWSHMetadata, true), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := briefWSHEntitlementNotice(tc.rules)
			if (got != "") != tc.want {
				t.Fatalf("notice=%q want=%t", got, tc.want)
			}
			if strings.Contains(got, "ibkr_wsh") || strings.Contains(got, "not_entitled") {
				t.Fatalf("notice exposed provider internals: %q", got)
			}
		})
	}
}

func TestBriefMoversAggregateByUnderlyingWithResidual(t *testing.T) {
	pos := &rpc.PositionsResult{ByUnderlying: []rpc.PositionGroup{
		{Underlying: "spy", GroupDailyPnLBase: new(10500.50)},
		{Underlying: "MSFT", GroupDailyPnLBase: new(-1600.25)},
		{Underlying: "GOOG", GroupDailyPnLBase: new(800.10)},
		{Underlying: "AMZN", GroupDailyPnLBase: new(-750.40)},
		{Underlying: "TSLA", GroupDailyPnLBase: new(-1400.30)},
		{Underlying: "ZVZZT"},
	}}
	row := briefMovers(pos, false)
	if len(row.Rows) != 3 || row.Rows[0].Symbol != "SPY" || row.Rows[1].Symbol != "MSFT" || row.Rows[2].Symbol != "TSLA" {
		t.Fatalf("rows=%+v", row.Rows)
	}
	if row.OtherPnLBase == nil || row.OtherCount != 2 {
		t.Fatalf("residual=%+v count=%d", row.OtherPnLBase, row.OtherCount)
	}
	if diff := *row.OtherPnLBase - (800.10 - 750.40); diff < -0.001 || diff > 0.001 {
		t.Fatalf("residual sum=%v", *row.OtherPnLBase)
	}
	if !strings.Contains(row.Detail, "by underlying") || !strings.Contains(row.Detail, "off-session marks") {
		t.Fatalf("detail=%q", row.Detail)
	}
	if open := briefMovers(pos, true); strings.Contains(open.Detail, "off-session") {
		t.Fatalf("open-session detail=%q", open.Detail)
	}
	if got := briefMovers(&rpc.PositionsResult{}, true); got.Status != rpc.BriefStatusDegraded {
		t.Fatalf("empty movers=%+v", got.BriefRowState)
	}
}

func briefTestCurrentAccountDataAuthority(source rpc.AccountDataSource) *rpc.AccountDataAuthority {
	return &rpc.AccountDataAuthority{
		Scope:        rpc.AccountDataScope{AccountID: "DU123", AccountMode: rpc.AccountModePaper},
		Source:       source,
		Availability: rpc.AccountDataAvailable,
		Freshness:    rpc.AccountDataFreshnessCurrent,
		AsOf:         time.Date(2026, 8, 6, 9, 30, 0, 0, time.UTC),
	}
}

func TestBriefPortfolioRequiresCurrentAccountDataAuthority(t *testing.T) {
	s := newRiskPolicyTestServer(t, dailyBriefPolicyTOML())
	dailyPnL := 125.0
	account := &rpc.AccountResult{
		NetLiquidation: 230175, DailyPnL: &dailyPnL, BaseCurrency: "EUR",
		Authority: briefTestCurrentAccountDataAuthority(rpc.AccountDataSourceAccountSummaryRequest),
	}
	currentPositions := briefTestCurrentAccountDataAuthority(rpc.AccountDataSourcePortfolioStream)

	tests := []struct {
		name      string
		authority *rpc.AccountDataAuthority
	}{
		{name: "missing"},
		{name: "unavailable", authority: &rpc.AccountDataAuthority{Availability: rpc.AccountDataUnavailable, Freshness: rpc.AccountDataFreshnessUnknown}},
		{name: "stale", authority: &rpc.AccountDataAuthority{Availability: rpc.AccountDataUnavailable, Freshness: rpc.AccountDataFreshnessStale}},
		{name: "unknown age", authority: &rpc.AccountDataAuthority{Availability: rpc.AccountDataAvailable, Freshness: rpc.AccountDataFreshnessUnknown}},
	}
	for _, tc := range tests {
		t.Run("positions "+tc.name, func(t *testing.T) {
			out := s.composeBriefPortfolio(account, &rpc.PositionsResult{Authority: tc.authority}, nil, nil, true)
			for name, row := range map[string]rpc.BriefRowState{
				"movers": out.Movers.BriefRowState, "premium": out.PremiumAtRisk.BriefRowState, "hedge": out.HedgeCost.BriefRowState,
			} {
				if row.Status != rpc.BriefStatusUnavailable {
					t.Fatalf("%s row=%+v, want unavailable", name, row)
				}
			}
			if out.PremiumAtRisk.AmountBase != nil || out.HedgeCost.AmountBase != nil {
				t.Fatalf("unavailable positions produced clean zero amounts: premium=%v hedge=%v", out.PremiumAtRisk.AmountBase, out.HedgeCost.AmountBase)
			}
		})
	}

	t.Run("account unavailable", func(t *testing.T) {
		unavailable := *account
		unavailable.Authority = &rpc.AccountDataAuthority{Availability: rpc.AccountDataUnavailable, Freshness: rpc.AccountDataFreshnessUnknown}
		out := s.composeBriefPortfolio(&unavailable, &rpc.PositionsResult{Authority: currentPositions}, nil, nil, true)
		if out.Account.Status != rpc.BriefStatusUnavailable || out.Account.EquityBase != nil || out.Account.DailyPnLBase != nil {
			t.Fatalf("unavailable account row=%+v, want unavailable without money", out.Account)
		}
	})

	t.Run("current empty book", func(t *testing.T) {
		out := s.composeBriefPortfolio(account, &rpc.PositionsResult{Authority: currentPositions}, nil, nil, true)
		if out.PremiumAtRisk.Status != rpc.BriefStatusOK || out.HedgeCost.Status != rpc.BriefStatusOK ||
			out.PremiumAtRisk.AmountBase == nil || *out.PremiumAtRisk.AmountBase != 0 ||
			out.HedgeCost.AmountBase == nil || *out.HedgeCost.AmountBase != 0 {
			t.Fatalf("current empty book lost its genuine zero: premium=%+v hedge=%+v", out.PremiumAtRisk, out.HedgeCost)
		}
	})
}

func TestBriefPremiumDisclosesUnknownHedgeClassification(t *testing.T) {
	s := newRiskPolicyTestServer(t, dailyBriefPolicyTOML())
	pos := &rpc.PositionsResult{Options: []rpc.PositionView{
		{Symbol: "NOW", SecType: "OPT", Right: "C", Quantity: 10, MarketValueBase: new(4265.0)},
		{Symbol: "SPY", SecType: "OPT", Right: "P", Quantity: 50, Multiplier: 100, MarketValueBase: new(54544.0)},
	}, Authority: briefTestCurrentAccountDataAuthority(rpc.AccountDataSourcePortfolioStream)}
	acct := &rpc.AccountResult{NetLiquidation: 230175, DailyPnL: new(7389.46), BaseCurrency: "EUR", Authority: briefTestCurrentAccountDataAuthority(rpc.AccountDataSourceAccountSummaryRequest)}
	out := s.composeBriefPortfolio(acct, pos, nil, nil, false)
	if out.PremiumAtRisk.Status != rpc.BriefStatusDegraded || !strings.Contains(out.PremiumAtRisk.Detail, "protective share") {
		t.Fatalf("premium must disclose unknown hedge classification: %+v", out.PremiumAtRisk)
	}
	if out.PremiumAtRisk.IncludedLegs != 2 || out.PremiumAtRisk.AmountBase == nil {
		t.Fatalf("premium amount must stay complete: %+v", out.PremiumAtRisk)
	}
	if out.HedgeCost.ExcludedLegs != 1 {
		t.Fatalf("hedge=%+v", out.HedgeCost)
	}
	if !strings.Contains(out.Account.Detail, "market closed") {
		t.Fatalf("closed-session account detail=%q", out.Account.Detail)
	}
}
