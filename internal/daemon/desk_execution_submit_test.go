package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/osauer/canary/v3/internal/rpc"
)

// deskExecutionTestCapability is the controller's private capability in
// these tests; its hash is the mandate's controller hash.
const deskExecutionTestCapability = "synthetic-private-controller-capability-phase-b"

// installDeskMandate writes a device-confirmed mandate the way confirm and
// control leave it: scope as given, running under this process, with the
// controller's hash and preferences. It returns the authorisation the
// controller would present.
func installDeskMandate(t *testing.T, s *Server, scope brokerStateScope, armed string, running bool) rpc.DeskAutomaticAuthorization {
	t.Helper()
	head, err := s.coreStore.AuthorityHead(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if s.startedAt.IsZero() {
		s.startedAt = s.orderNow()
	}
	hash := sha256.Sum256([]byte(deskExecutionTestCapability))
	terms := rpc.DeskAuthorityTerms{AuthorityEpoch: head.AuthorityEpoch, Version: 1, Kind: "desk_automatic_authority", ID: "synthetic-mandate-" + armed, AccountID: scope.Account, AccountMode: scope.Mode, MaximumScope: "full", ControllerHash: hex.EncodeToString(hash[:]),
		ConfirmBefore: s.orderNow().Add(time.Minute), Pricing: "fixed-midpoint", Algorithm: "Adaptive", TIF: "DAY", Protection: "existing-policy", Capacity: "existing-policy-reuse-confirmed-funds", Selection: "desk-deterministic-controller"}
	preferences := strings.Repeat("a", 64)
	row := deskAuthorityRecord{Terms: &terms, Scope: armed, Running: running, PreferencesHash: preferences, AdaptivePriority: "Normal", ControlProcess: s.startedAt, RegisterDigest: s.deskRegisterDigest()}
	current, err := s.readDeskAuthority(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	status, err := s.saveDeskAuthority(t.Context(), current.Generation, row, "test-confirmed")
	if err != nil {
		t.Fatal(err)
	}
	return rpc.DeskAutomaticAuthorization{ID: terms.ID, Capability: deskExecutionTestCapability, ExpectedGeneration: status.Generation, PreferencesHash: preferences}
}

func deskExecutionSubmit(t *testing.T, s *Server, in rpc.DeskExecutionSubmitParams) (rpc.DeskExecutionSubmitResult, error) {
	t.Helper()
	raw, _ := json.Marshal(in)
	return s.handleDeskExecutionSubmit(t.Context(), &rpc.Request{Params: raw})
}

func deskExecutionStrategyItem(request, episode, class string) rpc.DeskExecutionItem {
	return rpc.DeskExecutionItem{RequestID: request, EpisodeID: episode, Intent: rpc.DeskExecutionIntent{Class: class, Proposal: &rpc.DeskExecutionProposal{Key: "core_rebalance:syn", Revision: "r1"}}}
}

// notFoundEngine resolves every row as gone, the way a refresh that withdrew
// the row would: the submission runs the journal and the gate, sends nothing.
func notFoundEngine(s *Server) *proposalEngine {
	e := &proposalEngine{server: s, store: &proposalStore{}, automatic: &automaticSubmissionStore{}, queued: &queuedAuthStore{}, now: s.orderNow, ignored: map[string]struct{}{}}
	e.revalidateForTest = func(context.Context, string, string) (rpc.TradeProposal, []rpc.TradingBlocker, error) {
		return rpc.TradeProposal{}, []rpc.TradingBlocker{{Code: "proposal_not_found", Message: "the row was withdrawn by the last refresh"}}, nil
	}
	return e
}

// Without a confirmed mandate, or with one the controller does not hold,
// nothing is journaled and every item is refused authority_changed: the
// daemon invents no authority, and a lookup answers absent.
func TestDeskExecutionSubmitRefusesWithoutStandingAuthority(t *testing.T) {
	s, scope := deskExecutionTestServer(t)
	s.tradeProposals = notFoundEngine(s)
	item := deskExecutionTestItem("req-1", "ep-1", "r1")
	in := rpc.DeskExecutionSubmitParams{Automatic: &rpc.DeskAutomaticAuthorization{ID: "none", Capability: deskExecutionTestCapability, ExpectedGeneration: 1, PreferencesHash: strings.Repeat("a", 64)}, Items: []rpc.DeskExecutionItem{item}}
	got, err := deskExecutionSubmit(t, s, in)
	if err != nil || len(got.Receipts) != 1 || got.Receipts[0].Outcome != rpc.DeskOutcomeRefused || got.Receipts[0].Blockers[0].Code != rpc.DeskBlockerAuthorityChanged || got.Receipts[0].Validate(item, false) != nil {
		t.Fatalf("no mandate: %+v %v", got, err)
	}
	if r := deskExecutionLookup(t, s, "req-1")[0]; r.Outcome != rpc.DeskOutcomeAbsent {
		t.Fatalf("refused before journaling: %+v, want absent", r)
	}
	// A confirmed mandate the controller presents wrongly refuses the same
	// way: another capability, a stale generation, other preferences, not
	// running, or disarmed.
	auth := installDeskMandate(t, s, scope, "full", true)
	for name, change := range map[string]func(*rpc.DeskAutomaticAuthorization, *Server){
		"capability":  func(a *rpc.DeskAutomaticAuthorization, _ *Server) { a.Capability = strings.Repeat("x", 40) },
		"generation":  func(a *rpc.DeskAutomaticAuthorization, _ *Server) { a.ExpectedGeneration++ },
		"preferences": func(a *rpc.DeskAutomaticAuthorization, _ *Server) { a.PreferencesHash = strings.Repeat("b", 64) },
		"not running": func(_ *rpc.DeskAutomaticAuthorization, s *Server) { s.startedAt = s.startedAt.Add(time.Second) },
	} {
		t.Run(name, func(t *testing.T) {
			bad := auth
			started := s.startedAt
			change(&bad, s)
			defer func() { s.startedAt = started }()
			got, err := deskExecutionSubmit(t, s, rpc.DeskExecutionSubmitParams{Automatic: &bad, Items: []rpc.DeskExecutionItem{item}})
			if err != nil || got.Receipts[0].Outcome != rpc.DeskOutcomeRefused || got.Receipts[0].Blockers[0].Code != rpc.DeskBlockerAuthorityChanged {
				t.Fatalf("%s: %+v %v", name, got, err)
			}
		})
	}
	if r := deskExecutionLookup(t, s, "req-1")[0]; r.Outcome != rpc.DeskOutcomeAbsent {
		t.Fatalf("after authority refusals: %+v, want absent", r)
	}
	disarmed := installDeskMandate(t, s, scope, "off", false)
	got, err = deskExecutionSubmit(t, s, rpc.DeskExecutionSubmitParams{Automatic: &disarmed, Items: []rpc.DeskExecutionItem{item}})
	if err != nil || got.Receipts[0].Outcome != rpc.DeskOutcomeRefused || !strings.Contains(got.Receipts[0].Message, "disarmed") {
		t.Fatalf("disarmed: %+v %v", got, err)
	}
	// The manual batch is not served; an entry-rule addition is not served.
	if _, err := deskExecutionSubmit(t, s, rpc.DeskExecutionSubmitParams{Batch: &rpc.DeskBatchAuthorization{BatchRef: "x"}}); err == nil {
		t.Fatal("batch submission accepted")
	}
	armed := installDeskMandate(t, s, scope, "full", true)
	add := rpc.DeskExecutionItem{RequestID: "req-add", EpisodeID: "ep-add", Intent: rpc.DeskExecutionIntent{Class: rpc.DeskClassAdd, Add: &rpc.DeskExecutionAdd{ConID: 101, Symbol: "SYNA", Currency: "USD", RuleID: "rule-1", RuleRevision: 1, EvidenceID: "ev-1"}}}
	got, err = deskExecutionSubmit(t, s, rpc.DeskExecutionSubmitParams{Automatic: &armed, Items: []rpc.DeskExecutionItem{add}})
	if err != nil || got.Receipts[0].Outcome != rpc.DeskOutcomeRefused || got.Receipts[0].Blockers[0].Code != deskBlockerIntentNotServed {
		t.Fatalf("entry-rule addition: %+v %v", got, err)
	}
}

// Under Protection a strategy row's buy is refused and the refusal is
// recorded: the episode is consumed, the receipt says authority_changed, and
// a second request of the same episode is refused episode_consumed, so the
// row is not retried that session. Under Fully armed the same buy passes the
// scope check and meets the gates (here the row is gone), recorded the same
// way: once per episode, with its reason, and a retry answers from the
// journal without a second attempt.
func TestDeskExecutionSubmitRecordsARefusedStrategyRowOncePerEpisode(t *testing.T) {
	s, scope := deskExecutionTestServer(t)
	s.tradeProposals = notFoundEngine(s)
	protect := installDeskMandate(t, s, scope, "protect", true)
	buy := deskExecutionStrategyItem("req-buy-1", "core:DU1234567:paper:2026-09:2026-10-01", rpc.DeskClassAdd)
	got, err := deskExecutionSubmit(t, s, rpc.DeskExecutionSubmitParams{Automatic: &protect, Items: []rpc.DeskExecutionItem{buy}})
	if err != nil || got.Receipts[0].Outcome != rpc.DeskOutcomeRefused || got.Receipts[0].Blockers[0].Code != rpc.DeskBlockerAuthorityChanged || !strings.Contains(got.Receipts[0].Message, "Protection") {
		t.Fatalf("buy under Protection: %+v %v", got, err)
	}
	if r := deskExecutionLookup(t, s, "req-buy-1")[0]; r.Outcome != rpc.DeskOutcomeRefused || r.Validate(buy, true) != nil || r.Blockers[0].Code != rpc.DeskBlockerAuthorityChanged {
		t.Fatalf("the refusal is not journaled: %+v", r)
	}
	again := deskExecutionStrategyItem("req-buy-2", buy.EpisodeID, rpc.DeskClassAdd)
	got, err = deskExecutionSubmit(t, s, rpc.DeskExecutionSubmitParams{Automatic: &protect, Items: []rpc.DeskExecutionItem{again}})
	if err != nil || got.Receipts[0].Outcome != rpc.DeskOutcomeRefused || got.Receipts[0].Blockers[0].Code != rpc.DeskBlockerEpisodeConsumed {
		t.Fatalf("second request of the episode: %+v %v", got, err)
	}
	// A sale (a reduction) runs under Protection: it reaches the gate.
	sell := deskExecutionStrategyItem("req-sell-1", "core:DU1234567:paper:2026-09:2026-10-02", rpc.DeskClassReduce)
	got, err = deskExecutionSubmit(t, s, rpc.DeskExecutionSubmitParams{Automatic: &protect, Items: []rpc.DeskExecutionItem{sell}})
	if err != nil || got.Receipts[0].Outcome != rpc.DeskOutcomeRefused || got.Receipts[0].Blockers[0].Code != "proposal_not_found" {
		t.Fatalf("sale under Protection: %+v %v", got, err)
	}
	// Fully armed: the buy passes the scope check and meets the gate.
	full := installDeskMandate(t, s, scope, "full", true)
	next := deskExecutionStrategyItem("req-buy-3", "core:DU1234567:paper:2026-09:2026-10-05", rpc.DeskClassAdd)
	got, err = deskExecutionSubmit(t, s, rpc.DeskExecutionSubmitParams{Automatic: &full, Items: []rpc.DeskExecutionItem{next}})
	if err != nil || got.Receipts[0].Outcome != rpc.DeskOutcomeRefused || got.Receipts[0].Blockers[0].Code != "proposal_not_found" || got.Receipts[0].Validate(next, false) != nil {
		t.Fatalf("buy under Fully armed against a withdrawn row: %+v %v", got, err)
	}
	// The retry answers from the journal; the engine is not asked again.
	s.tradeProposals = nil
	got, err = deskExecutionSubmit(t, s, rpc.DeskExecutionSubmitParams{Automatic: &full, Items: []rpc.DeskExecutionItem{next}})
	if err == nil {
		t.Fatal("submission without an engine was accepted")
	}
	s.tradeProposals = &proposalEngine{server: s, store: &proposalStore{}, automatic: &automaticSubmissionStore{}, queued: &queuedAuthStore{}, now: s.orderNow, ignored: map[string]struct{}{},
		revalidateForTest: func(context.Context, string, string) (rpc.TradeProposal, []rpc.TradingBlocker, error) {
			t.Fatal("a retry reached the engine")
			return rpc.TradeProposal{}, nil, nil
		}}
	got, err = deskExecutionSubmit(t, s, rpc.DeskExecutionSubmitParams{Automatic: &full, Items: []rpc.DeskExecutionItem{next}})
	if err != nil || got.Receipts[0].Outcome != rpc.DeskOutcomeRefused || got.Receipts[0].Blockers[0].Code != "proposal_not_found" {
		t.Fatalf("retry: %+v %v", got, err)
	}
	changed := next
	changed.Intent.Proposal.Revision = "r2"
	got, err = deskExecutionSubmit(t, s, rpc.DeskExecutionSubmitParams{Automatic: &full, Items: []rpc.DeskExecutionItem{changed}})
	if err != nil || got.Receipts[0].Blockers[0].Code != rpc.DeskBlockerRequestConflict {
		t.Fatalf("reused ID with another revision: %+v %v", got, err)
	}
	// Capabilities say what is served: submit, for the automatic authority.
	caps := s.handleDeskExecutionCapabilities()
	if !slices.Contains(caps.Methods, rpc.MethodDeskExecutionSubmit) || !slices.Equal(caps.Authorities, []string{"automatic"}) {
		t.Fatalf("capabilities %+v", caps)
	}
}

// Canary classifies the row itself: the core's buy is the one addition,
// its sale a reduction, a stop or trail protection, and a buy outside a
// strategy row, a sweep or a conversion has no automatic class. A strategy
// row's episode is its month episode and session.
func TestDeskExecutionClassifiesRowsAndStrategyEpisodes(t *testing.T) {
	core := rpc.TradeProposal{Bucket: rpc.TradeProposalBucketCoreRebalance, Action: rpc.OrderActionBuy, OrderType: rpc.OrderTypeLMT,
		Core: &rpc.TradeProposalCore{Session: "2026-10-01", Sleeve: "core", Terms: rpc.CoreRowTerms{Episode: "core:DU1234567:paper:2026-09"}}}
	if got := deskExecutionClass(core); got != rpc.DeskClassAdd {
		t.Fatalf("core buy class %q", got)
	}
	if got := deskExecutionStrategyEpisode(core); got != "core:DU1234567:paper:2026-09:2026-10-01" {
		t.Fatalf("core episode %q", got)
	}
	if got := deskExecutionStrategyName(core); !strings.Contains(got, "core-ibcf-v1") || !strings.Contains(got, "sleeve core") {
		t.Fatalf("core name %q", got)
	}
	sale := core
	sale.Action = rpc.OrderActionSell
	if got := deskExecutionClass(sale); got != rpc.DeskClassReduce {
		t.Fatalf("core sale class %q", got)
	}
	for name, p := range map[string]rpc.TradeProposal{
		"trailing stop": {Bucket: rpc.TradeProposalBucketTrailingStop, Action: rpc.OrderActionSell, OrderType: "TRAIL"},
		"stop order":    {Bucket: rpc.TradeProposalBucketOptionLossExit, Action: rpc.OrderActionSell, OrderType: "STP"},
	} {
		if got := deskExecutionClass(p); got != rpc.DeskClassProtect {
			t.Fatalf("%s class %q", name, got)
		}
	}
	for name, p := range map[string]rpc.TradeProposal{
		"budget reduction": {Bucket: rpc.TradeProposalBucketBudgetReduction, Action: rpc.OrderActionSell, OrderType: rpc.OrderTypeLMT},
		"mandate cut":      {Bucket: rpc.TradeProposalBucketMandateReduction, Action: rpc.OrderActionSell, OrderType: rpc.OrderTypeLMT},
		"theta hygiene":    {Bucket: rpc.TradeProposalBucketThetaHygiene, Action: rpc.OrderActionSell, OrderType: rpc.OrderTypeLMT},
	} {
		if got := deskExecutionClass(p); got != rpc.DeskClassReduce {
			t.Fatalf("%s class %q", name, got)
		}
	}
	for name, p := range map[string]rpc.TradeProposal{
		"a buy outside a strategy": {Bucket: rpc.TradeProposalBucketRiskReduction, Action: rpc.OrderActionBuy, OrderType: rpc.OrderTypeLMT},
		"cash sweep":               {Bucket: rpc.TradeProposalBucketCashSweep, Action: rpc.OrderActionBuy, OrderType: rpc.OrderTypeLMT},
		"conversion":               {Bucket: rpc.TradeProposalBucketCurrencyLeveling, Action: rpc.OrderActionSell, OrderType: rpc.OrderTypeLMT},
	} {
		if got := deskExecutionClass(p); got != "" {
			t.Fatalf("%s class %q, want none", name, got)
		}
	}
	if got := deskExecutionStrategyEpisode(rpc.TradeProposal{Bucket: rpc.TradeProposalBucketTrailingStop}); got != "" {
		t.Fatalf("a stop has a strategy episode %q", got)
	}
	for class, action := range map[string]string{rpc.DeskClassProtect: "protection", rpc.DeskClassReduce: "reduction", rpc.DeskClassAdd: "entry", "hedge": ""} {
		if got := deskFenceAction(class); got != action {
			t.Fatalf("fence action for %s = %q", class, got)
		}
	}
}

// The origin gate accepts desk-automatic only while a submission holds a
// grant whose fence has not expired; the pre-authorised scheduler's grant
// does not open it, and human origins never see it.
func TestDeskAutomaticOriginNeedsAGrant(t *testing.T) {
	s, _ := deskExecutionTestServer(t)
	status := s.currentTradingStatus()
	if b := s.brokerWriteOriginBlockers(status, rpc.OrderOriginDeskAutomatic); len(b) != 1 || b[0].Code != "daemon_origin_unauthorised" {
		t.Fatalf("without a grant: %+v", b)
	}
	s.automaticGrant.Store(&automaticWriteGrant{Key: "k", Revision: "r", Bucket: preAuthorisedBucketTrailingStop})
	defer s.automaticGrant.Store(nil)
	if b := s.brokerWriteOriginBlockers(status, rpc.OrderOriginDeskAutomatic); len(b) != 1 {
		t.Fatalf("the scheduler's grant opened the desk origin: %+v", b)
	}
	grant := &deskExecutionGrant{Key: "k", Revision: "r", Class: rpc.DeskClassProtect, Fence: deskAuthorityFence{ID: "m", Generation: 1, Action: "protection", ValidUntil: s.orderNow()}}
	s.deskExecutionGrant.Store(grant)
	defer s.deskExecutionGrant.Store(nil)
	if b := s.brokerWriteOriginBlockers(status, rpc.OrderOriginDeskAutomatic); len(b) != 1 || b[0].Code != rpc.DeskBlockerAuthorityChanged {
		t.Fatalf("an expired fence passed: %+v", b)
	}
	grant.Fence.ValidUntil = s.orderNow().Add(time.Minute)
	if b := s.brokerWriteOriginBlockers(status, rpc.OrderOriginDeskAutomatic); len(b) != 0 {
		t.Fatalf("a live grant refused: %+v", b)
	}
	for _, origin := range []string{rpc.OrderOriginHumanTTY, rpc.OrderOriginPairedDevice, rpc.OrderOriginAgent} {
		if b := s.brokerWriteOriginBlockers(status, origin); len(b) != 0 {
			t.Fatalf("origin %q blockers = %+v", origin, b)
		}
	}
	if normalizedWriteOrigin(rpc.OrderOriginDeskAutomatic) != rpc.OrderOriginDeskAutomatic || orderOriginForRead(rpc.OrderOriginDeskAutomatic) != rpc.OrderOriginDeskAutomatic || !automaticSettlingMachineOrigin(rpc.OrderOriginDeskAutomatic) {
		t.Fatal("the desk-automatic origin is not journaled, read and settled as the machine's own")
	}
}

// A strategy row not ready now (here the broker link is down; a closed
// exchange or the opening window read the same way) is refused with the
// live readiness code before journaling, so the session's episode stays
// free and the controller sends the row once it reads ready; a ready row
// reaches the gate and is journaled.
func TestDeskExecutionSubmitSendsAStrategyRowOnlyWhenReady(t *testing.T) {
	s, scope := deskExecutionTestServer(t)
	e := notFoundEngine(s)
	s.tradeProposals = e
	row := rpc.TradeProposal{Key: "core_rebalance:syn", Revision: "r1", Bucket: rpc.TradeProposalBucketCoreRebalance, Action: rpc.OrderActionBuy, OrderType: rpc.OrderTypeLMT,
		Core: &rpc.TradeProposalCore{Session: "2026-10-01", Sleeve: "core", Terms: rpc.CoreRowTerms{Episode: "core:DU1234567:paper:2026-09"}}}
	e.snapshot = rpc.TradeProposalSnapshot{Kind: rpc.TradeProposalSnapshotKind, Revision: "r1", AccountID: scope.Account, AccountMode: scope.Mode, Proposals: []rpc.TradeProposal{row}}
	full := installDeskMandate(t, s, scope, "full", true)
	item := deskExecutionStrategyItem("req-closed", "core:DU1234567:paper:2026-09:2026-10-01", rpc.DeskClassAdd)
	if ready := e.snapshotForRead(false, false).Proposals[0].Readiness; ready == nil || ready.Code != rpc.ReadinessBrokerUnavailable {
		t.Fatalf("the served row reads %+v, want broker_unavailable", ready)
	}
	got, err := deskExecutionSubmit(t, s, rpc.DeskExecutionSubmitParams{Automatic: &full, Items: []rpc.DeskExecutionItem{item}})
	if err != nil || got.Receipts[0].Outcome != rpc.DeskOutcomeRefused || got.Receipts[0].Blockers[0].Code != rpc.ReadinessBrokerUnavailable || !strings.Contains(got.Receipts[0].Message, "in session") {
		t.Fatalf("not ready: %+v %v", got, err)
	}
	if r := deskExecutionLookup(t, s, item.RequestID)[0]; r.Outcome != rpc.DeskOutcomeAbsent {
		t.Fatalf("a not-ready refusal was journaled: %+v", r)
	}
	s.gatewayReadyForTrading = func() bool { return true }
	if ready := e.snapshotForRead(false, false).Proposals[0].Readiness; ready == nil || ready.Code != rpc.ReadinessReady {
		t.Fatalf("the served row reads %+v, want ready", ready)
	}
	got, err = deskExecutionSubmit(t, s, rpc.DeskExecutionSubmitParams{Automatic: &full, Items: []rpc.DeskExecutionItem{item}})
	if err != nil || got.Receipts[0].Outcome != rpc.DeskOutcomeRefused || got.Receipts[0].Blockers[0].Code != "proposal_not_found" {
		t.Fatalf("ready row at the gate: %+v %v", got, err)
	}
	if r := deskExecutionLookup(t, s, item.RequestID)[0]; r.Outcome != rpc.DeskOutcomeRefused {
		t.Fatalf("the gate's refusal is not journaled: %+v", r)
	}
}

// Under the standing mandate the core's row is refused before its due time
// (fifteen minutes after the US cash open of its session) with the readiness
// code opening_window and nothing journaled, and reaches the gate from the
// due time on: the controller sends it at its first ready read after it.
func TestDeskExecutionSubmitWaitsForTheCoreRowsDueTime(t *testing.T) {
	s, scope := deskExecutionTestServer(t)
	clock := utc(2026, 11, 2, 9, 0)
	s.now = func() time.Time { return clock }
	s.gatewayReadyForTrading = func() bool { return true }
	e := notFoundEngine(s)
	s.tradeProposals = e
	row := rpc.TradeProposal{Key: "core_rebalance:syn", Revision: "r1", Bucket: rpc.TradeProposalBucketCoreRebalance, Action: rpc.OrderActionBuy, OrderType: rpc.OrderTypeLMT,
		Core: &rpc.TradeProposalCore{Session: "2026-11-02", Sleeve: "core", Terms: rpc.CoreRowTerms{Episode: "core:DU1234567:paper:2026-10"}}}
	e.snapshot = rpc.TradeProposalSnapshot{Kind: rpc.TradeProposalSnapshotKind, Revision: "r1", AccountID: scope.Account, AccountMode: scope.Mode, Proposals: []rpc.TradeProposal{row}}
	full := installDeskMandate(t, s, scope, "full", true)
	item := deskExecutionStrategyItem("req-due", "core:DU1234567:paper:2026-10:2026-11-02", rpc.DeskClassAdd)
	if ready := e.snapshotForRead(false, false).Proposals[0].Readiness; ready == nil || ready.Code != rpc.ReadinessOpeningWindow || !strings.Contains(ready.Message, "US cash open") {
		t.Fatalf("the served row reads %+v, want opening_window until the due time", ready)
	}
	got, err := deskExecutionSubmit(t, s, rpc.DeskExecutionSubmitParams{Automatic: &full, Items: []rpc.DeskExecutionItem{item}})
	if err != nil || got.Receipts[0].Outcome != rpc.DeskOutcomeRefused || got.Receipts[0].Blockers[0].Code != rpc.ReadinessOpeningWindow || !strings.Contains(got.Receipts[0].Message, "in session") {
		t.Fatalf("before the due time: %+v %v", got, err)
	}
	if r := deskExecutionLookup(t, s, item.RequestID)[0]; r.Outcome != rpc.DeskOutcomeAbsent {
		t.Fatalf("a refusal before the due time was journaled: %+v", r)
	}
	clock = utc(2026, 11, 2, 14, 45)
	if ready := e.snapshotForRead(false, false).Proposals[0].Readiness; ready == nil || ready.Code != rpc.ReadinessReady {
		t.Fatalf("at the due time the served row reads %+v, want ready", ready)
	}
	got, err = deskExecutionSubmit(t, s, rpc.DeskExecutionSubmitParams{Automatic: &full, Items: []rpc.DeskExecutionItem{item}})
	if err != nil || got.Receipts[0].Outcome != rpc.DeskOutcomeRefused || got.Receipts[0].Blockers[0].Code != "proposal_not_found" {
		t.Fatalf("at the due time the row did not reach the gate: %+v %v", got, err)
	}
}

// A strategy row that reads ready on its served snapshot but whose preview
// refuses on a transient readiness (a stale quote here) is refused with that
// readiness code before journaling, so the episode stays free; a hard
// refusal (the row withdrawn) is journaled as before.
func TestDeskExecutionSubmitRefusesAStaleQuoteBeforeJournaling(t *testing.T) {
	s, scope := deskExecutionTestServer(t)
	s.now = func() time.Time { return utc(2026, 11, 2, 15, 0) }
	s.gatewayReadyForTrading = func() bool { return true }
	e := notFoundEngine(s)
	s.tradeProposals = e
	row := rpc.TradeProposal{Key: "core_rebalance:syn", Revision: "r1", Bucket: rpc.TradeProposalBucketCoreRebalance, Action: rpc.OrderActionBuy, OrderType: rpc.OrderTypeLMT,
		Core: &rpc.TradeProposalCore{Session: "2026-11-02", Sleeve: "core", Terms: rpc.CoreRowTerms{Episode: "core:DU1234567:paper:2026-10"}}}
	e.snapshot = rpc.TradeProposalSnapshot{Kind: rpc.TradeProposalSnapshotKind, Revision: "r1", AccountID: scope.Account, AccountMode: scope.Mode, Proposals: []rpc.TradeProposal{row}}
	stale := []rpc.TradingBlocker{{Code: previewQuoteStaleCode, Message: "the quote is older than the preview accepts"}}
	e.revalidateForTest = func(context.Context, string, string) (rpc.TradeProposal, []rpc.TradingBlocker, error) {
		return row, stale, nil
	}
	full := installDeskMandate(t, s, scope, "full", true)
	item := deskExecutionStrategyItem("req-stale", "core:DU1234567:paper:2026-10:2026-11-02", rpc.DeskClassAdd)
	if ready := e.snapshotForRead(false, false).Proposals[0].Readiness; ready == nil || ready.Code != rpc.ReadinessReady {
		t.Fatalf("the served row reads %+v, want ready", ready)
	}
	got, err := deskExecutionSubmit(t, s, rpc.DeskExecutionSubmitParams{Automatic: &full, Items: []rpc.DeskExecutionItem{item}})
	if err != nil || got.Receipts[0].Outcome != rpc.DeskOutcomeRefused || got.Receipts[0].Blockers[0].Code != rpc.ReadinessQuoteUnusable || !strings.Contains(got.Receipts[0].Message, "nothing was journaled") {
		t.Fatalf("stale quote: %+v %v", got, err)
	}
	if r := deskExecutionLookup(t, s, item.RequestID)[0]; r.Outcome != rpc.DeskOutcomeAbsent {
		t.Fatalf("a stale-quote refusal was journaled: %+v", r)
	}
	// The episode is free: the next pass, with the row withdrawn, reaches the
	// gate and that refusal is journaled.
	e.revalidateForTest = func(context.Context, string, string) (rpc.TradeProposal, []rpc.TradingBlocker, error) {
		return rpc.TradeProposal{}, []rpc.TradingBlocker{{Code: "proposal_not_found", Message: "the row was withdrawn by the last refresh"}}, nil
	}
	next := deskExecutionStrategyItem("req-stale-2", item.EpisodeID, rpc.DeskClassAdd)
	got, err = deskExecutionSubmit(t, s, rpc.DeskExecutionSubmitParams{Automatic: &full, Items: []rpc.DeskExecutionItem{next}})
	if err != nil || got.Receipts[0].Outcome != rpc.DeskOutcomeRefused || got.Receipts[0].Blockers[0].Code != "proposal_not_found" {
		t.Fatalf("the episode after a stale quote: %+v %v", got, err)
	}
	if r := deskExecutionLookup(t, s, next.RequestID)[0]; r.Outcome != rpc.DeskOutcomeRefused {
		t.Fatalf("the gate's refusal is not journaled: %+v", r)
	}
}
