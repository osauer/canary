package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"time"

	"github.com/osauer/canary/v3/internal/daemon/corestore"
	edgecore "github.com/osauer/canary/v3/internal/edge"
	"github.com/osauer/canary/v3/internal/marketcal"
	"github.com/osauer/canary/v3/internal/rpc"
)

const (
	edgeForwardTestsStateKind = "edge_forward_tests"
	edgeForwardTestsVersion   = 1
)

// edgeForwardTestRegistry records when each forward-test rule was first
// registered, per account scope. It is the one Edge document that is not
// regenerable: the instants are the evidence that a rule was written before
// the results it is read on, so nothing rebuilds, resets or rewrites them.
// It lives outside the Edge publication, which every analytical version bump
// throws away, and outside the bar cache, which starts afresh on a scope
// switch. A record is only ever added: a rule whose text changed gets a new
// record with its own instant beside the old one, and another account or
// mode gets its own records without touching these.
type edgeForwardTestRegistry struct {
	Version int                                `json:"version"`
	Scopes  map[string][]edgeForwardTestRecord `json:"scopes"`
}

type edgeForwardTestRecord struct {
	ID           string    `json:"id"`
	RuleHash     string    `json:"rule_hash"`
	RegisteredAt time.Time `json:"registered_at"`
}

// edgeForwardTestRules is the shipped rule set. Tests replace it to change a
// rule's text.
var edgeForwardTestRules = edgecore.ForwardTests

func (s *Server) loadEdgeForwardTestRegistry(ctx context.Context) (edgeForwardTestRegistry, int64, error) {
	out := edgeForwardTestRegistry{Version: edgeForwardTestsVersion, Scopes: map[string][]edgeForwardTestRecord{}}
	doc, ok, err := s.coreStore.GetStateDocument(ctx, daemonStateScope, edgeForwardTestsStateKind)
	if err != nil || !ok {
		return out, 0, err
	}
	if err := json.Unmarshal(doc.JSON, &out); err != nil {
		return out, 0, fmt.Errorf("decode Edge forward-test registry: %w", err)
	}
	// A registry written by a later build is left alone: rewriting it in this
	// build's shape could drop what that build recorded.
	if out.Version > edgeForwardTestsVersion {
		return out, 0, fmt.Errorf("unsupported future Edge forward-test registry version %d", out.Version)
	}
	if out.Scopes == nil {
		out.Scopes = map[string][]edgeForwardTestRecord{}
	}
	return out, doc.Revision, nil
}

// registerEdgeForwardTests records, once, the instant each shipped rule was
// first seen for this account scope. It is safe to call on every rebuild: a
// rule already recorded with the same text keeps its instant.
func (s *Server) registerEdgeForwardTests(ctx context.Context, scopeFingerprint string) error {
	if s == nil || s.coreStore == nil || scopeFingerprint == "" {
		return nil
	}
	for range 4 {
		registry, revision, err := s.loadEdgeForwardTestRegistry(ctx)
		if err != nil {
			return err
		}
		records := registry.Scopes[scopeFingerprint]
		known := make(map[string]bool, len(records))
		for _, record := range records {
			known[record.ID+"\x00"+record.RuleHash] = true
		}
		now := s.edgeNow()
		added := false
		for _, rule := range edgeForwardTestRules() {
			hash := rule.RuleHash()
			if known[rule.ID+"\x00"+hash] {
				continue
			}
			records = append(records, edgeForwardTestRecord{ID: rule.ID, RuleHash: hash, RegisteredAt: now})
			added = true
		}
		if !added {
			return nil
		}
		registry.Version = edgeForwardTestsVersion
		registry.Scopes[scopeFingerprint] = records
		raw, err := json.Marshal(registry)
		if err != nil {
			return err
		}
		_, err = s.coreStore.CompareAndSwapStateDocument(ctx, corestore.StateDocumentCAS{ScopeKey: daemonStateScope, Kind: edgeForwardTestsStateKind, ExpectedRevision: revision, JSON: raw})
		if err == nil {
			return nil
		}
		// Another writer registered meanwhile: read again, so its instants
		// are kept and nothing is overwritten.
		if _, conflict := err.(*corestore.RevisionConflictError); !conflict {
			return err
		}
	}
	return fmt.Errorf("edge forward-test registration conflicted repeatedly")
}

// Reasons a reply carries no forward tests (EdgeResult.ForwardTestsUnavailable).
const (
	edgeForwardTestsNoScope    = "account_scope_unavailable"
	edgeForwardTestsUnreadable = "registry_unreadable"
)

// edgeForwardTestsForRequest reads the registry for an Edge reply. It runs
// before everything else in the request. Read last, a request that ran into
// its deadline while the daemon was busy after a restart got a failed read,
// and the failure was served as an empty list beside an otherwise full reply
// (seen 2026-11-20 08:21 CET, 53 seconds after a start): a reader could not
// tell "could not be read" from "nothing is registered". A registry that
// cannot be read is said so, by reason, and no rule is listed.
func (s *Server) edgeForwardTestsForRequest(ctx context.Context) ([]rpc.EdgeForwardTest, string) {
	none := []rpc.EdgeForwardTest{}
	if s == nil || s.coreStore == nil {
		return none, edgeForwardTestsUnreadable
	}
	scope := s.currentBrokerStateScope()
	if !brokerScopeConcrete(scope) {
		return none, edgeForwardTestsNoScope
	}
	tests, err := s.edgeForwardTestsView(ctx, edgeScopeFingerprint(scope))
	if err != nil {
		reason := err.Error()
		if previous := s.edgeForwardTestsWarned.Swap(&reason); previous == nil || *previous != reason {
			s.warnf("Canary Edge: the forward-test registry could not be read, so replies name no rule: %v", err)
		}
		return none, edgeForwardTestsUnreadable
	}
	return tests, ""
}

// edgeForwardTestsView serves the shipped rules with the instant each was
// registered for this scope. A rule not yet registered has no instant. A
// registry that cannot be read is an error, never an empty list.
func (s *Server) edgeForwardTestsView(ctx context.Context, scopeFingerprint string) ([]rpc.EdgeForwardTest, error) {
	out := []rpc.EdgeForwardTest{}
	if s == nil || s.coreStore == nil {
		return nil, fmt.Errorf("edge authority is unavailable")
	}
	registry, _, err := s.loadEdgeForwardTestRegistry(ctx)
	if err != nil {
		return nil, err
	}
	registered := map[string]time.Time{}
	for _, record := range registry.Scopes[scopeFingerprint] {
		registered[record.ID+"\x00"+record.RuleHash] = record.RegisteredAt
	}
	for _, rule := range edgeForwardTestRules() {
		hash := rule.RuleHash()
		test := rpc.EdgeForwardTest{ID: rule.ID, Title: rule.Title, Rule: rule.Rule, RuleHash: hash, RegisteredAt: registered[rule.ID+"\x00"+hash]}
		if facts, ok := edgecore.ForwardTestFacts(rule.ID); ok {
			test.Question, test.Informs, test.Condition, test.Unit, test.UnitText = facts.Question, facts.Informs, facts.Condition, facts.Unit, facts.UnitText
			test.Needs, test.NeedsGroup, test.NeedsDays, test.SampleLabel, test.Policy = facts.Needs, facts.NeedsGroup, facts.NeedsDays, facts.SampleLabel, facts.Policy
			test.AboutSales, test.NotSorted, test.CannotSort = facts.AboutSales, facts.NotSorted, facts.CannotSort
			test.PaceNoun, test.InShort = facts.PaceNoun, facts.InShort
			for _, group := range facts.Groups {
				test.Groups = append(test.Groups, rpc.EdgeForwardGroup{ID: group.ID, Label: group.Label})
			}
		}
		out = append(out, test)
	}
	return out, nil
}

// edgeForwardWindow is the review window the forward tests count from.
const edgeForwardWindow = "365d"

// edgeForwardEvidence is what one reply counts the forward tests from: the
// published full-window result, and whether its protection evidence is
// served: only then does a count say which sales Canary's protection sent.
type edgeForwardEvidence struct {
	result          edgecore.Result
	protectionKnown bool
}

// markEdgeForwardTestsNotCounted states, for every test, that this reply
// counts nothing and why: the test is not registered yet, or there is no
// published review to count from.
func markEdgeForwardTestsNotCounted(tests []rpc.EdgeForwardTest) {
	for i := range tests {
		reason := rpc.EdgeForwardNoReview
		if tests[i].RegisteredAt.IsZero() {
			reason = rpc.EdgeForwardNotRegistered
		}
		tests[i].State, tests[i].NotCountedReason, tests[i].Sample = rpc.EdgeForwardNotCounted, reason, nil
	}
}

// countEdgeForwardTests counts each registered test from the published
// review and attaches the count to the served test. A test not yet
// registered, or one whose facts this build does not know, gets no count
// and says why: absent, never zero. Nothing is stored; the count is the
// review's.
func (s *Server) countEdgeForwardTests(ctx context.Context, tests []rpc.EdgeForwardTest, evidence edgeForwardEvidence) {
	markEdgeForwardTestsNotCounted(tests)
	zone := edgeNewYork()
	asOf := evidence.result.AsOf
	stamps := map[string]edgeSessionStamp{}
	var latest *edgeSessionStamp
	if log, _, err := s.loadEdgeSessionStamps(ctx); err == nil {
		for _, stamp := range log.Sessions {
			stamps[stamp.Session] = stamp
		}
		if n := len(log.Sessions); n > 0 {
			latest = &log.Sessions[n-1]
		}
	}
	fifth := edgeFifthSessionCloses()
	in := edgecore.ForwardCountInput{
		Changes: evidence.result.Changes, AsOf: asOf, ProtectionKnown: evidence.protectionKnown, Zone: zone,
		FifthSessionClose: fifth,
		PriorSessionStage: func(at time.Time) (string, bool) {
			prior, _ := edgeDecisionSessions(at)
			stamp, ok := stamps[prior]
			return stamp.RegimeStage, ok && prior != ""
		},
	}
	for i := range tests {
		test := &tests[i]
		facts, ok := edgecore.ForwardTestFacts(test.ID)
		if !ok || test.RegisteredAt.IsZero() {
			continue
		}
		sampled := in
		sampled.After = test.RegisteredAt
		pace := in
		pace.After, pace.Through = asOf.AddDate(0, 0, -edgecore.ForwardReviewDays), test.RegisteredAt
		summary := edgecore.SummarizeForward(facts, edgecore.CountForwardUnits(test.ID, sampled), edgecore.CountForwardUnits(test.ID, pace), test.RegisteredAt, asOf, zone)
		test.Sample = rpcEdgeForwardSample(summary, asOf, evidence.protectionKnown)
		test.State, test.NotCountedReason = summary.State, ""
		test.Sample.Today = edgeForwardToday(test.ID, latest, s.edgeNow())
	}
}

// rpcEdgeForwardSample serves a count. Which units Canary's protection sent
// is served only while that evidence is current; otherwise it is not said.
func rpcEdgeForwardSample(in edgecore.ForwardSample, asOf time.Time, protectionKnown bool) *rpc.EdgeForwardSample {
	out := &rpc.EdgeForwardSample{
		State: in.State, AsOf: asOf, WindowDays: edgecore.ForwardReviewDays,
		Test:     rpc.EdgeForwardCount{Final: in.Test.Final, Waiting: in.Test.Waiting},
		Excluded: maps.Clone(in.Excluded), Months: []rpc.EdgeForwardMonth{}, ToFill: in.ToFill, NoDate: in.NoDate,
		CanFill: in.CanFill, CannotFill: in.CannotFill, WaitingUnits: rpcEdgeForwardUnits(in.WaitingUnits), NotCounted: rpcEdgeForwardUnits(in.NotCounted),
	}
	for _, g := range in.Groups {
		out.Groups = append(out.Groups, rpc.EdgeForwardGroupCount{ID: g.ID, Final: g.Final, Waiting: g.Waiting, Days: g.Days})
	}
	if in.Protection != nil && protectionKnown {
		out.SentByProtection = &rpc.EdgeForwardCount{Final: in.Protection.Final, Waiting: in.Protection.Waiting}
	}
	if !in.FirstUnit.IsZero() {
		out.FirstUnit, out.LastUnit = in.FirstUnit.Format(time.DateOnly), in.LastUnit.Format(time.DateOnly)
	}
	for _, m := range in.Months {
		out.Months = append(out.Months, rpc.EdgeForwardMonth{Month: m.Month, Final: m.Final})
	}
	if in.Expected != nil {
		out.Expected = &rpc.EdgeForwardExpected{Date: in.Expected.Date.In(edgeNewYork()).Format(time.DateOnly), Basis: in.Expected.Basis, BeyondWindow: in.Expected.BeyondWindow}
	}
	return out
}

func rpcEdgeForwardUnits(in []edgecore.ForwardUnitRef) []rpc.EdgeForwardUnit {
	out := make([]rpc.EdgeForwardUnit, 0, len(in))
	for _, u := range in {
		out = append(out, rpc.EdgeForwardUnit{Key: u.Key, Label: u.Label, Date: u.Date.Format(time.DateOnly), Group: u.Group, Reason: u.Reason})
	}
	return out
}

// edgeForwardToday says how an option buy made now would be counted, from
// the record of the US session such a buy would follow: the latest session
// closed by now. Only the two rules that label a buy by a recorded close
// have one, and only from that record; nothing else is read.
func edgeForwardToday(id string, latest *edgeSessionStamp, now time.Time) *rpc.EdgeForwardToday {
	if id != "option_buys_after_regime_warning" && id != "option_buys_below_premium_line" {
		return nil
	}
	prior, own := edgeDecisionSessions(now)
	if ended, ok := usSessionEndedBy(now); ok && ended.Date == own {
		prior = own
	}
	if prior == "" {
		return nil
	}
	out := &rpc.EdgeForwardToday{Session: prior, Group: edgecore.ForwardGroupCannotSort}
	if latest == nil || latest.Session != prior {
		out.Missing = rpc.EdgeSessionStampNotRecorded
		return out
	}
	if id == "option_buys_after_regime_warning" {
		out.RegimeStage, out.Group = latest.RegimeStage, edgecore.ForwardRegimeGroup(latest.RegimeStage)
		return out
	}
	// The line is recorded; premium at risk at the last statement is not
	// worked out, so a buy now is not sorted.
	out.Group = edgecore.ForwardGroupNotSorted
	out.PremiumBudgetActPct, out.PremiumBudgetSet = cloneAmount(latest.PremiumBudgetActPct), latest.PremiumBudgetSet
	return out
}

func edgeNewYork() *time.Location {
	if loc, err := time.LoadLocation("America/New_York"); err == nil {
		return loc
	}
	return time.UTC
}

// edgeFifthSessionCloses returns the close of the fifth US session after a
// decision's New York date, from the official calendar, memoised by date
// for one count. Outside the calendar's coverage there is none, and a unit
// waiting on it stays not final rather than being dated by a guessed day.
func edgeFifthSessionCloses() func(time.Time) (time.Time, bool) {
	cal := marketcal.New()
	type answer struct {
		at time.Time
		ok bool
	}
	seen := map[string]answer{}
	return func(executedAt time.Time) (time.Time, bool) {
		day := nyTime(executedAt)
		key := day.Format(time.DateOnly)
		if a, ok := seen[key]; ok {
			return a.at, a.ok
		}
		noon := time.Date(day.Year(), day.Month(), day.Day(), 12, 0, 0, 0, day.Location())
		a, sessions := answer{}, 0
		for ahead := 1; ahead <= 20; ahead++ {
			session, err := cal.SessionAt(marketcal.MarketUSEquity, noon.AddDate(0, 0, ahead))
			if err != nil || session.State == marketcal.StateUnknown {
				break
			}
			if session.State != marketcal.StateRegular && session.State != marketcal.StateEarlyClose {
				continue
			}
			if sessions++; sessions == 5 {
				a = answer{at: session.Close, ok: true}
				break
			}
		}
		seen[key] = a
		return a.at, a.ok
	}
}

// edgeForwardSampleIDs names the rules whose sample a decision executed at
// executedAt can belong to: those registered strictly before it. Whether the
// decision meets a rule's own conditions is the reader's question.
func edgeForwardSampleIDs(tests []rpc.EdgeForwardTest, executedAt time.Time) []string {
	var out []string
	for _, test := range tests {
		if edgecore.InForwardSample(test.RegisteredAt, executedAt) {
			out = append(out, test.ID)
		}
	}
	return out
}
