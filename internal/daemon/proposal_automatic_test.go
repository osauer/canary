package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/daemon/corestore"
	"github.com/osauer/canary/v2/internal/rpc"
	ibkrlib "github.com/osauer/canary/v2/pkg/ibkr"
)

// automaticTestRig is a hermetic proposal engine over a real daemon.db with
// a file-backed protection policy, a fixed clock, a seamed broker scope and
// a seamed drawdown latch. It never touches a gateway.
type automaticTestRig struct {
	t       *testing.T
	core    *corestore.Store
	now     time.Time
	engine  *proposalEngine
	server  *Server
	scope   brokerStateScope
	latched bool
	// revalidations counts submit-path revalidations; the default build has
	// no write capability, so every attempt that reaches the write gate is
	// refused there, and the count proves whether the scheduler tried.
	revalidations int
	// brokerOrders is the complete broker open-order inventory the settling
	// rule reads (empty: nothing working); freshOrders appear only in a fresh
	// read, as an order placed since the shared cached read would; and
	// inventoryErr makes the inventory unreadable.
	brokerOrders []ibkrlib.OrderLifecycleEvent
	freshOrders  []ibkrlib.OrderLifecycleEvent
	inventoryErr error
	// inventoryReads counts settling-rule reads of that inventory.
	inventoryReads int
}

// installInventory seams the broker open-order inventory to the rig's
// synthetic book, read at the rig clock.
func (r *automaticTestRig) installInventory(srv *Server) {
	srv.openOrderInventoryForTest = func(_ context.Context, fresh bool) (ibkrlib.OpenOrderSnapshot, brokerStateScope, error) {
		r.inventoryReads++
		if r.inventoryErr != nil {
			return ibkrlib.OpenOrderSnapshot{}, r.scope, r.inventoryErr
		}
		orders := append([]ibkrlib.OrderLifecycleEvent(nil), r.brokerOrders...)
		if fresh {
			orders = append(orders, r.freshOrders...)
		}
		return ibkrlib.OpenOrderSnapshot{Complete: true, AsOf: r.now, Orders: orders}, r.scope, nil
	}
}

func newAutomaticTestRig(t *testing.T, authority string) *automaticTestRig {
	t.Helper()
	ctx := context.Background()
	core, err := corestore.Open(ctx, corestore.Options{Path: filepath.Join(privateTestDir(t), "daemon.db")})
	if err != nil {
		t.Fatalf("open corestore: %v", err)
	}
	t.Cleanup(func() { _ = core.Close() })
	if err := initializeCleanProposalOpportunityAuthority(ctx, core); err != nil {
		t.Fatalf("initialize clean authority: %v", err)
	}
	rig := &automaticTestRig{t: t, core: core, now: time.Date(2026, 9, 21, 14, 0, 0, 0, time.UTC), scope: brokerStateScope{Account: "DU1234567", Mode: "paper"}}
	srv := newTestServer(t)
	srv.now = func() time.Time { return rig.now }
	rig.installInventory(srv)
	srv.protectionPolicies = writePreAuthPolicy(t, preAuthPolicyTOML(authority, 1))
	srv.protectionPolicies.reload()
	if st := srv.protectionPolicies.Status(); st.Status != rpc.ProtectionPolicyStatusActive {
		t.Fatalf("test policy status = %+v", st)
	}
	rig.server = srv
	rig.engine = rig.newEngine()
	srv.tradeProposals = rig.engine
	return rig
}

// newEngine binds a fresh engine to the same daemon.db: a restarted daemon.
func (r *automaticTestRig) newEngine() *proposalEngine {
	r.t.Helper()
	e := &proposalEngine{server: r.server, store: &proposalStore{}, automatic: &automaticSubmissionStore{}, now: r.server.now, ignored: map[string]struct{}{}}
	e.scope = func() brokerStateScope { return r.scope }
	e.latchedForTest = func(brokerStateScope) bool { return r.latched }
	e.startedAt = r.now
	e.revalidateForTest = func(_ context.Context, key, revision string) (rpc.TradeProposal, []rpc.TradingBlocker, error) {
		r.revalidations++
		snap := e.Snapshot(false)
		if snap.Revision != revision {
			return rpc.TradeProposal{}, []rpc.TradingBlocker{{Code: "stale_revision", Message: "stale"}}, nil
		}
		for _, prop := range snap.Proposals {
			if prop.Key == key {
				return prop, prop.Blockers, nil
			}
		}
		return rpc.TradeProposal{}, []rpc.TradingBlocker{{Code: "proposal_not_found", Message: "gone"}}, nil
	}
	if err := e.bindCore(context.Background(), r.core); err != nil {
		r.t.Fatalf("bind engine: %v", err)
	}
	return e
}

func (r *automaticTestRig) restart() {
	r.t.Helper()
	r.engine = r.newEngine()
	r.server.tradeProposals = r.engine
}

func (r *automaticTestRig) advance(d time.Duration) { r.now = r.now.Add(d) }

func automaticTestStockRow() rpc.PositionView {
	return rpc.PositionView{ConID: 101, Symbol: "SYN", SecType: "STK", Quantity: 40, Mark: 25, Bid: new(24.9), Ask: new(25.1), Currency: "USD", Exchange: "SMART"}
}

// stopProposal generates a real trailing-stop row through the engine's own
// generator so the key, bucket and order shape are the production ones.
func (r *automaticTestRig) stopProposal() rpc.TradeProposal {
	r.t.Helper()
	policy, status := r.server.protectionPolicies.Active()
	prop, ok := trailingStopStockProposal(policy, status, automaticTestStockRow(), rpc.TradeProposalSourceFingerprints{}, r.now, true, 0.01)
	if !ok {
		r.t.Fatal("no trailing-stop proposal generated")
	}
	return prop
}

func (r *automaticTestRig) thetaProposal() rpc.TradeProposal {
	prop := r.stopProposal()
	prop.Bucket = rpc.TradeProposalBucketThetaHygiene
	prop.Key = proposalKey(prop.Bucket, prop.Contract, prop.Action)
	return prop
}

// install serves props at one revision derived from their keys and
// quantities, the way a refresh would.
func (r *automaticTestRig) install(props ...rpc.TradeProposal) string {
	r.t.Helper()
	policy, status := r.server.protectionPolicies.Active()
	r.engine.mu.Lock()
	previous := r.engine.snapshot
	r.engine.mu.Unlock()
	revision, effectiveRevision := proposalPolicyRevision(previous, status, rpc.TradeProposalSourceFingerprints{}, r.scope, props)
	for i := range props {
		props[i].Revision = revision
		props[i].Rank = i + 1
	}
	snap := rpc.TradeProposalSnapshot{
		Kind: rpc.TradeProposalSnapshotKind, SchemaVersion: rpc.TradeProposalSnapshotSchemaVersion, AsOf: r.now, Revision: revision, EffectiveRevision: effectiveRevision,
		AccountID: r.scope.Account, AccountMode: r.scope.Mode, PolicyID: policy.PolicyID, PolicyVersion: policy.PolicyVersion,
		PolicyFingerprint: status.Fingerprint, EffectivePolicyFingerprint: status.EffectiveFingerprint, PolicyStatus: status, Proposals: props, Counts: proposalCounts(props, "USD"),
	}
	if len(props) == 0 {
		snap.Proposals = []rpc.TradeProposal{}
	}
	if err := r.engine.installSnapshot(snap, false); err != nil {
		r.t.Fatalf("install snapshot: %v", err)
	}
	return revision
}

func (r *automaticTestRig) cycle() { r.engine.runAutomaticCycle(context.Background()) }

func (r *automaticTestRig) record(key, revision string) automaticSubmissionRecord {
	r.t.Helper()
	rec, ok := r.engine.automatic.get(key, revision)
	if !ok {
		r.t.Fatalf("no automatic record for %s@%s", key, revision[:12])
	}
	return rec
}

func (r *automaticTestRig) noRecord(key, revision string) {
	r.t.Helper()
	if rec, ok := r.engine.automatic.get(key, revision); ok {
		r.t.Fatalf("unexpected automatic record %+v", rec)
	}
}

func (r *automaticTestRig) notice(rec automaticSubmissionRecord) {
	r.engine.markAutomaticNoticed(context.Background(), []automaticNoticeKey{{Key: rec.Key, Revision: rec.Revision}}, r.now)
}

// events returns the journaled automatic-submission events of one record,
// oldest first.
func (r *automaticTestRig) events(key, revision string) []automaticSubmissionEvent {
	r.t.Helper()
	records, err := r.core.LoadEvents(context.Background(), corestore.EventQuery{Type: automaticCoreEventType})
	if err != nil {
		r.t.Fatalf("load automatic events: %v", err)
	}
	var out []automaticSubmissionEvent
	for _, record := range records {
		var ev automaticSubmissionEvent
		if err := json.Unmarshal(record.PayloadJSON, &ev); err != nil {
			r.t.Fatalf("decode automatic event: %v", err)
		}
		if ev.Key == key && ev.Revision == revision {
			out = append(out, ev)
		}
	}
	return out
}

func (r *automaticTestRig) eventCount(key, revision, eventType string) int {
	n := 0
	for _, ev := range r.events(key, revision) {
		if ev.Type == eventType {
			n++
		}
	}
	return n
}

// handOrder is an order working at the broker that the journal does not
// track: placed by hand in TWS, so it has no origin.
func handOrder(permID int) ibkrlib.OrderLifecycleEvent {
	return ibkrlib.OrderLifecycleEvent{
		Type: ibkrlib.OrderLifecycleEventOpenOrder, PermID: permID, ClientID: 0, ClientIDPresent: true, Account: "DU1234567",
		Symbol: "HND", SecType: "STK", Action: rpc.OrderActionBuy, OrderType: rpc.OrderTypeLMT, TIF: rpc.OrderTIFDay,
		TotalQuantity: 5, LimitPrice: 12.5, Status: "Submitted",
	}
}

// A working hand order never matters while no bucket is pre-authorised:
// no record exists, so the settling rule never even reads the broker.
func TestAutomaticSettlingRuleIsDormantWithoutPreAuthorisedBuckets(t *testing.T) {
	t.Parallel()
	rig := newAutomaticTestRig(t, "")
	rig.brokerOrders = []ibkrlib.OrderLifecycleEvent{handOrder(7777)}
	prop := rig.stopProposal()
	revision := rig.install(prop)
	for range 3 {
		rig.cycle()
		rig.advance(time.Hour)
	}
	rig.noRecord(prop.Key, revision)
	if rig.inventoryReads != 0 || rig.revalidations != 0 {
		t.Fatalf("inventory reads = %d, attempts = %d with no pre-authorised bucket", rig.inventoryReads, rig.revalidations)
	}
}

// In the build without write capability the settling rule is visible as
// whether the scheduler attempts at all: a working hand order or an
// unreadable inventory holds the due record, its settling clears the hold,
// and the record is attempted on its original window.
func TestAutomaticSettlingHoldDecidesWhetherTheSchedulerAttempts(t *testing.T) {
	t.Parallel()
	rig := newAutomaticTestRig(t, automaticTrailingStopAuthority)
	// Classifying a working order needs the journal that knows its origin.
	rig.server.orderJournal = newTestOrderJournalStore(t, filepath.Join(t.TempDir(), "order-journal.jsonl"))
	prop := rig.stopProposal()
	revision := rig.install(prop)
	rig.cycle()
	rig.notice(rig.record(prop.Key, revision))
	window := rig.record(prop.Key, revision).SubmitAt
	rig.inventoryErr = errors.New("gateway down")
	rig.advance(31 * time.Minute)
	rig.cycle()
	held := rig.record(prop.Key, revision)
	if rig.revalidations != 0 || held.State != rpc.TradeProposalAutomaticPending || held.HeldAt.IsZero() || held.HoldReason != automaticHoldUnavailable {
		t.Fatalf("unreadable inventory: attempts = %d, record = %+v", rig.revalidations, held)
	}
	rig.inventoryErr = nil
	rig.brokerOrders = []ibkrlib.OrderLifecycleEvent{handOrder(7777)}
	rig.cycle()
	if held = rig.record(prop.Key, revision); rig.revalidations != 0 || held.HoldReason != automaticHoldHandOrder {
		t.Fatalf("working hand order: attempts = %d, record = %+v", rig.revalidations, held)
	}
	if a := rig.engine.Snapshot(false).Proposals[0].Automatic; a == nil || a.HeldAt.IsZero() || a.Reason != automaticHoldHandOrder {
		t.Fatalf("served automatic while held = %+v", a)
	}
	if got := rig.eventCount(prop.Key, revision, automaticEventHeld); got != 2 {
		t.Fatalf("held events = %d, want one per reason", got)
	}
	rig.brokerOrders = nil
	rig.cycle()
	if rig.revalidations != 1 {
		t.Fatalf("attempts after the hand order settled = %d, want 1", rig.revalidations)
	}
	if after := rig.record(prop.Key, revision); !after.SubmitAt.Equal(window) || !after.HeldAt.IsZero() {
		t.Fatalf("record after the hold = %+v, want the original window and no hold", after)
	}
}

const automaticTrailingStopAuthority = `pre_authorised = ["trailing_stop"]`

func TestAutomaticRecordCreatedWithWindowForPreAuthorisedRow(t *testing.T) {
	t.Parallel()
	rig := newAutomaticTestRig(t, automaticTrailingStopAuthority)
	prop := rig.stopProposal()
	revision := rig.install(prop)
	rig.cycle()
	rec := rig.record(prop.Key, revision)
	if rec.State != rpc.TradeProposalAutomaticPending || rec.Bucket != preAuthorisedBucketTrailingStop {
		t.Fatalf("record = %+v, want pending trailing_stop", rec)
	}
	if !rec.SubmitAt.Equal(rig.now.Add(30*time.Minute)) || rec.LatchSkippedWindow {
		t.Fatalf("submit_at = %s (latched=%v), want now+30m", rec.SubmitAt, rec.LatchSkippedWindow)
	}
	if rec.Symbol != "" && rec.Quantity == 0 {
		t.Fatalf("record lost its quantity: %+v", rec)
	}
	// The served row carries the record and the policy fact.
	snap := rig.engine.Snapshot(false)
	if a := snap.Proposals[0].Automatic; a == nil || !a.PreAuthorised || a.State != rpc.TradeProposalAutomaticPending || !a.SubmitAt.Equal(rec.SubmitAt) || a.VetoWindow != "30m0s" {
		t.Fatalf("served automatic = %+v", a)
	}
	// A second cycle at the same revision neither duplicates nor resets.
	rig.advance(time.Minute)
	rig.cycle()
	if again := rig.record(prop.Key, revision); !again.CreatedAt.Equal(rec.CreatedAt) || !again.SubmitAt.Equal(rec.SubmitAt) {
		t.Fatalf("second cycle rewrote the record: %+v", again)
	}
	if rig.server.autoTradeStatus().AutomaticPending != 1 {
		t.Fatalf("status pending count = %d, want 1", rig.server.autoTradeStatus().AutomaticPending)
	}
}

func TestAutomaticBlockedRowNeverGetsARecord(t *testing.T) {
	t.Parallel()
	rig := newAutomaticTestRig(t, automaticTrailingStopAuthority)
	prop := rig.stopProposal()
	proposalBlock(&prop, "active_halt", "halted")
	revision := rig.install(prop)
	rig.cycle()
	rig.noRecord(prop.Key, revision)
	if a := rig.engine.Snapshot(false).Proposals[0].Automatic; a == nil || !a.PreAuthorised || a.State != "" {
		t.Fatalf("blocked row automatic = %+v, want pre-authorised with no record", a)
	}
	// A snapshot-level blocker blocks every row the same way.
	clean := rig.stopProposal()
	revision = rig.install(clean)
	rig.engine.mu.Lock()
	rig.engine.snapshot.Blockers = []rpc.TradingBlocker{{Code: "positions_unavailable"}}
	rig.engine.mu.Unlock()
	rig.cycle()
	rig.noRecord(clean.Key, revision)
}

func TestAutomaticBucketOutsideListNeverGetsARecord(t *testing.T) {
	t.Parallel()
	rig := newAutomaticTestRig(t, `pre_authorised = ["option_loss_exit"]`)
	stop := rig.stopProposal()
	theta := rig.thetaProposal()
	revision := rig.install(stop, theta)
	rig.advance(time.Hour)
	rig.cycle()
	rig.noRecord(stop.Key, revision)
	rig.noRecord(theta.Key, revision)
	for _, p := range rig.engine.Snapshot(false).Proposals {
		if p.Automatic != nil && p.Automatic.PreAuthorised {
			t.Fatalf("%s reads as pre-authorised under a list that excludes it", p.Bucket)
		}
	}
	if rig.revalidations != 0 {
		t.Fatalf("scheduler attempted %d submissions for unlisted buckets", rig.revalidations)
	}
	// An empty list (the embedded default) pre-authorises nothing either.
	none := newAutomaticTestRig(t, "")
	stop = none.stopProposal()
	revision = none.install(stop)
	none.cycle()
	none.noRecord(stop.Key, revision)
}

func TestAutomaticVetoInsideWindowHoldsAcrossRestart(t *testing.T) {
	t.Parallel()
	rig := newAutomaticTestRig(t, automaticTrailingStopAuthority)
	prop := rig.stopProposal()
	revision := rig.install(prop)
	rig.cycle()
	rig.notice(rig.record(prop.Key, revision))
	rig.advance(10 * time.Minute)
	res, err := rig.engine.Veto(context.Background(), rpc.TradeProposalVetoParams{Key: prop.Key, Origin: rpc.OrderOriginPairedDevice, Reason: "not today"})
	if err != nil || !res.Accepted || res.State != rpc.TradeProposalAutomaticVetoed || res.Automatic == nil || res.Automatic.VetoedAt.IsZero() {
		t.Fatalf("veto = %+v err = %v", res, err)
	}
	// Past the window: nothing submits, the record stays vetoed.
	rig.advance(time.Hour)
	rig.cycle()
	if rec := rig.record(prop.Key, revision); rec.State != rpc.TradeProposalAutomaticVetoed || rec.Reason != "not today" || rec.Origin != rpc.OrderOriginPairedDevice {
		t.Fatalf("record after window = %+v", rec)
	}
	if rig.revalidations != 0 {
		t.Fatal("a vetoed record reached the submit path")
	}
	// Restart: the veto is durable and the same revision gets no new window.
	rig.restart()
	rig.install(prop)
	rig.advance(time.Hour)
	rig.cycle()
	if rec := rig.record(prop.Key, revision); rec.State != rpc.TradeProposalAutomaticVetoed {
		t.Fatalf("veto lost across restart: %+v", rec)
	}
	if rig.revalidations != 0 {
		t.Fatal("restarted scheduler submitted a vetoed record")
	}
	if again, err := rig.engine.Veto(context.Background(), rpc.TradeProposalVetoParams{Key: prop.Key, Origin: rpc.OrderOriginHumanTTY}); err != nil || !again.Accepted || !strings.Contains(again.Message, "already vetoed") {
		t.Fatalf("repeat veto = %+v err = %v", again, err)
	}
}

func TestAutomaticVetoRefusesAgentOrigin(t *testing.T) {
	t.Parallel()
	rig := newAutomaticTestRig(t, automaticTrailingStopAuthority)
	prop := rig.stopProposal()
	revision := rig.install(prop)
	rig.cycle()
	for _, origin := range []string{rpc.OrderOriginAgent, "", "robot"} {
		_, err := rig.engine.Veto(context.Background(), rpc.TradeProposalVetoParams{Key: prop.Key, Origin: origin})
		if err == nil || !strings.Contains(err.Error(), "human-only") {
			t.Fatalf("origin %q: err = %v, want human-only refusal", origin, err)
		}
	}
	if rec := rig.record(prop.Key, revision); rec.State != rpc.TradeProposalAutomaticPending {
		t.Fatalf("agent veto changed the record: %+v", rec)
	}
}

func TestAutomaticVetoWithoutRecordHoldsTheServedRevision(t *testing.T) {
	t.Parallel()
	rig := newAutomaticTestRig(t, automaticTrailingStopAuthority)
	prop := rig.stopProposal()
	proposalBlock(&prop, "active_halt", "halted")
	revision := rig.install(prop)
	rig.cycle()
	rig.noRecord(prop.Key, revision)
	res, err := rig.engine.Veto(context.Background(), rpc.TradeProposalVetoParams{Key: prop.Key, Origin: rpc.OrderOriginHumanTTY})
	if err != nil || !res.Accepted || res.Revision != revision {
		t.Fatalf("veto on blocked row = %+v err = %v", res, err)
	}
	// The row unblocks at the same revision: still no window.
	clean := rig.stopProposal()
	if rig.install(clean) != revision {
		t.Fatal("test expects the unblocked row to keep its revision")
	}
	rig.cycle()
	if rec := rig.record(prop.Key, revision); rec.State != rpc.TradeProposalAutomaticVetoed {
		t.Fatalf("record = %+v, want the earlier veto to hold", rec)
	}
	// A key nobody proposes cannot be vetoed into existence.
	if res, err := rig.engine.Veto(context.Background(), rpc.TradeProposalVetoParams{Key: "trailing_stop:ffffffffffffffff", Origin: rpc.OrderOriginHumanTTY}); err != nil || res.Accepted {
		t.Fatalf("veto of unknown key = %+v err = %v", res, err)
	}
}

func TestAutomaticRevisionChangeSupersedesAndRestartsWindow(t *testing.T) {
	t.Parallel()
	rig := newAutomaticTestRig(t, automaticTrailingStopAuthority)
	prop := rig.stopProposal()
	first := rig.install(prop)
	rig.cycle()
	rig.notice(rig.record(prop.Key, first))
	rig.advance(20 * time.Minute)
	// The position grows: same key, new quantity, new revision.
	grown := prop
	grown.Quantity = 60
	second := rig.install(grown)
	if second == first {
		t.Fatal("test expects a new revision")
	}
	rig.cycle()
	if rec := rig.record(prop.Key, first); rec.State != rpc.TradeProposalAutomaticSuperseded || !strings.Contains(rec.Reason, "revision changed") {
		t.Fatalf("old record = %+v", rec)
	}
	fresh := rig.record(prop.Key, second)
	if fresh.State != rpc.TradeProposalAutomaticPending || !fresh.SubmitAt.Equal(rig.now.Add(30*time.Minute)) || fresh.Quantity != 60 {
		t.Fatalf("new record = %+v, want a fresh 30m window", fresh)
	}
	// Twenty more minutes: the old window would have closed; the new one
	// has not, so nothing submits.
	rig.advance(20 * time.Minute)
	rig.cycle()
	if rig.revalidations != 0 {
		t.Fatal("superseded window leaked into a submission")
	}
}

func TestAutomaticDisappearedProposalIsSuperseded(t *testing.T) {
	t.Parallel()
	rig := newAutomaticTestRig(t, automaticTrailingStopAuthority)
	prop := rig.stopProposal()
	revision := rig.install(prop)
	rig.cycle()
	rig.install()
	rig.cycle()
	if rec := rig.record(prop.Key, revision); rec.State != rpc.TradeProposalAutomaticSuperseded || !strings.Contains(rec.Reason, "disappeared") {
		t.Fatalf("record = %+v", rec)
	}
	// Coming back at the same revision starts a new window only if no
	// record exists for it; the superseded one holds the key+revision.
	rig.install(prop)
	rig.cycle()
	if rec := rig.record(prop.Key, revision); rec.State != rpc.TradeProposalAutomaticSuperseded {
		t.Fatalf("record recreated over a superseded one: %+v", rec)
	}
}

func TestAutomaticLatchedBrakeSkipsTheWindow(t *testing.T) {
	t.Parallel()
	rig := newAutomaticTestRig(t, automaticTrailingStopAuthority)
	rig.latched = true
	prop := rig.stopProposal()
	revision := rig.install(prop)
	rig.cycle()
	rec := rig.record(prop.Key, revision)
	if !rec.LatchSkippedWindow || !rec.SubmitAt.Equal(rig.now) {
		t.Fatalf("latched record = %+v, want submit_at = now", rec)
	}
	// In this build the write gate refuses (no trading capability), and the
	// record says so instead of retrying.
	if got := rig.record(prop.Key, revision); got.State != rpc.TradeProposalAutomaticFailed || !(strings.Contains(got.Reason, "trading_disabled") || strings.Contains(got.Reason, "order_writes_unavailable")) {
		t.Fatalf("latched submission outcome = %+v, want failed on the build gate", got)
	}
	if rig.revalidations != 1 {
		t.Fatalf("revalidations = %d, want exactly one attempt", rig.revalidations)
	}
	rig.advance(time.Hour)
	rig.cycle()
	if rig.revalidations != 1 {
		t.Fatal("a failed record was retried")
	}
}

func TestAutomaticWindowWaitsForTheNotice(t *testing.T) {
	t.Parallel()
	rig := newAutomaticTestRig(t, automaticTrailingStopAuthority)
	prop := rig.stopProposal()
	revision := rig.install(prop)
	rig.cycle()
	rig.advance(2 * time.Hour)
	rig.cycle()
	rec := rig.record(prop.Key, revision)
	if rec.State != rpc.TradeProposalAutomaticPending || rig.revalidations != 0 {
		t.Fatalf("unnoticed record submitted: %+v attempts=%d", rec, rig.revalidations)
	}
	if a := rig.engine.Snapshot(false).Proposals[0].Automatic; a == nil || !strings.Contains(a.Reason, "notice") {
		t.Fatalf("served reason = %+v, want the missing notice named", a)
	}
	// The notice arrives late: the window is measured from it.
	rig.notice(rec)
	if rec = rig.record(prop.Key, revision); !rec.SubmitAt.Equal(rig.now.Add(30 * time.Minute)) {
		t.Fatalf("submit_at after late notice = %s, want notice+30m", rec.SubmitAt)
	}
	rig.cycle()
	if rig.revalidations != 0 {
		t.Fatal("submitted before the post-notice window closed")
	}
	rig.advance(31 * time.Minute)
	rig.cycle()
	if rig.revalidations != 1 {
		t.Fatalf("revalidations = %d, want one attempt after the window", rig.revalidations)
	}
}

func TestAutomaticRestartRecoveryReadsTheOrderJournal(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		events    []orderJournalEvent
		wantState string
		wantRef   string
		wantWords string
	}{
		"send completed before the crash": {
			events:    []orderJournalEvent{{Type: orderJournalEventSendAttempted, PreviewTokenID: "tok-a", OrderRef: "ref-a"}, {Type: orderJournalEventSendCompleted, PreviewTokenID: "tok-a", OrderRef: "ref-a"}},
			wantState: rpc.TradeProposalAutomaticSubmitted, wantRef: "ref-a", wantWords: "reached the broker",
		},
		"attempt staged, outcome unknown": {
			events:    []orderJournalEvent{{Type: orderJournalEventSendAttempted, PreviewTokenID: "tok-a", OrderRef: "ref-a"}},
			wantState: rpc.TradeProposalAutomaticFailed, wantRef: "ref-a", wantWords: "unknown across the restart",
		},
		"nothing staged": {
			events:    nil,
			wantState: rpc.TradeProposalAutomaticFailed, wantWords: "before the broker send",
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			state, ref, reason := automaticRestartOutcome(tc.events, "tok-a")
			if state != tc.wantState || ref != tc.wantRef || !strings.Contains(reason, tc.wantWords) {
				t.Fatalf("outcome = %s %q %q", state, ref, reason)
			}
		})
	}
	// End to end: a submitting record from a previous process resolves on
	// the first cycle and is never submitted again.
	rig := newAutomaticTestRig(t, automaticTrailingStopAuthority)
	prop := rig.stopProposal()
	revision := rig.install(prop)
	rig.cycle()
	rig.notice(rig.record(prop.Key, revision))
	stuckAt := rig.now
	if err := rig.engine.automatic.update(context.Background(), func(records map[string]*automaticSubmissionRecord) []automaticSubmissionEvent {
		r := records[automaticRecordKey(prop.Key, revision)]
		r.State, r.SubmittingAt, r.PreviewTokenID = rpc.TradeProposalAutomaticSubmitting, stuckAt, "tok-stuck"
		return []automaticSubmissionEvent{{At: stuckAt, Type: automaticEventSubmitting, Key: r.Key, Revision: r.Revision}}
	}); err != nil {
		t.Fatal(err)
	}
	rig.advance(time.Hour)
	rig.restart()
	rig.install(prop)
	rig.cycle()
	rec := rig.record(prop.Key, revision)
	if rec.State != rpc.TradeProposalAutomaticFailed || !strings.Contains(rec.Reason, "before the broker send") {
		t.Fatalf("recovered record = %+v", rec)
	}
	if rig.revalidations != 0 {
		t.Fatal("restart recovery re-submitted an in-flight record")
	}
}

func TestAutomaticPolicyDriftSupersedesPending(t *testing.T) {
	t.Parallel()
	rig := newAutomaticTestRig(t, automaticTrailingStopAuthority)
	prop := rig.stopProposal()
	revision := rig.install(prop)
	rig.cycle()
	// The owner edits the file without a version bump: drift, nothing is
	// pre-authorised, and the pending record is closed rather than acted on.
	m := rig.server.protectionPolicies
	if err := os.WriteFile(m.path, []byte(preAuthPolicyTOML(`pre_authorised = ["trailing_stop"]`+"\nveto_window = \"10m\"", 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	m.reload()
	if m.Status().Status != rpc.ProtectionPolicyStatusDrift {
		t.Fatalf("policy status = %q, want drift", m.Status().Status)
	}
	rig.cycle()
	if rec := rig.record(prop.Key, revision); rec.State != rpc.TradeProposalAutomaticSuperseded || !strings.Contains(rec.Reason, "not active") {
		t.Fatalf("record under drift = %+v", rec)
	}
	if st := rig.server.autoTradeStatus(); len(st.PreAuthorised) != 0 {
		t.Fatalf("status advertises pre-authorised buckets under drift: %v", st.PreAuthorised)
	}
}

// A governor row generated in shadow mode is observation, never an order:
// the scheduler must not record it even when the bucket is pre-authorised
// and the row carries no blocker of its own.
func TestAutomaticShadowRowNeverGetsARecord(t *testing.T) {
	t.Parallel()
	rig := newAutomaticTestRig(t, automaticTrailingStopAuthority)
	prop := rig.stopProposal()
	prop.Shadow = true
	revision := rig.install(prop)
	rig.cycle()
	rig.noRecord(prop.Key, revision)
	if got := proposalBlockedReason(rig.engine.Snapshot(false), prop); got != "proposal is a shadow row" {
		t.Fatalf("blocked reason = %q, want the shadow reason", got)
	}
}

// A reduction to budget is a discretionary-scale action, not a stop: the
// governor marks its rows NeverSkipVeto and they keep the full window even
// while the drawdown brake is latched.
func TestAutomaticNeverSkipVetoKeepsTheWindowWhenLatched(t *testing.T) {
	t.Parallel()
	rig := newAutomaticTestRig(t, automaticTrailingStopAuthority)
	rig.latched = true
	prop := rig.stopProposal()
	prop.NeverSkipVeto = true
	revision := rig.install(prop)
	rig.cycle()
	rec := rig.record(prop.Key, revision)
	if rec.LatchSkippedWindow || !rec.SubmitAt.Equal(rig.now.Add(30*time.Minute)) {
		t.Fatalf("never-skip record under the latch = %+v, want the full window", rec)
	}
}

// Only a refusal the freeze caused alone, with nothing sent, defers; every
// other refusal keeps failing the record as before.
func TestAutomaticFrozenRefusalDefersOnlyAnUnsentFreezeRefusal(t *testing.T) {
	t.Parallel()
	frozenBlocker := rpc.TradingBlocker{Code: tradingFrozenBlockerCode, Message: tradingFrozenBlockerMessage}
	typed := tradingBlockersError([]rpc.TradingBlocker{frozenBlocker})
	refused := rpc.TradeProposalSubmitResult{Blockers: []rpc.TradingBlocker{{Code: "submit_failed"}}}
	for name, tc := range map[string]struct {
		res      rpc.TradeProposalSubmitResult
		err      error
		placeErr error
		want     bool
	}{
		"freeze alone at the write gate":            {res: rpc.TradeProposalSubmitResult{Blockers: []rpc.TradingBlocker{frozenBlocker}}, want: true},
		"freeze committed during admission":         {res: rpc.TradeProposalSubmitResult{Blockers: []rpc.TradingBlocker{frozenBlocker, {Code: tradingControlsChangedBlockerCode}}}, want: true},
		"freeze with another gate":                  {res: rpc.TradeProposalSubmitResult{Blockers: []rpc.TradingBlocker{{Code: "gateway_unavailable"}, frozenBlocker}}},
		"build gate":                                {res: rpc.TradeProposalSubmitResult{Blockers: []rpc.TradingBlocker{{Code: "order_writes_unavailable"}}}},
		"freeze at place admission":                 {res: refused, placeErr: typed, want: true},
		"freeze before the frame, proven unsent":    {res: refused, placeErr: ibkrlib.WithSendDisposition(typed, ibkrlib.SendDispositionDefinitelyUnsent), want: true},
		"freeze with an unknown send outcome":       {res: refused, placeErr: ibkrlib.WithSendDisposition(typed, ibkrlib.SendDispositionUnknown)},
		"freeze with a may-have-written outcome":    {res: refused, placeErr: ibkrlib.WithSendDisposition(typed, ibkrlib.SendDispositionMayHaveWritten)},
		"another place refusal":                     {res: refused, placeErr: ibkrlib.WithSendDisposition(errors.New("socket closed"), ibkrlib.SendDispositionDefinitelyUnsent)},
		"the untyped freeze text is not the freeze": {res: refused, placeErr: errors.New("trading disabled: " + tradingFrozenBlockerMessage)},
		"revalidation error":                        {res: rpc.TradeProposalSubmitResult{Blockers: []rpc.TradingBlocker{frozenBlocker}}, err: errors.New("positions unavailable")},
		"accepted":                                  {res: rpc.TradeProposalSubmitResult{Accepted: true}},
	} {
		if got := automaticFrozenRefusal(tc.res, tc.err, tc.placeErr); got != tc.want {
			t.Errorf("%s: deferred = %v, want %v", name, got, tc.want)
		}
	}
}
