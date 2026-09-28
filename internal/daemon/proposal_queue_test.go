package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"math"
	"math/rand/v2"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/daemon/corestore"
	"github.com/osauer/canary/v2/internal/marketcal"
	"github.com/osauer/canary/v2/internal/rpc"
)

// queuedFixtureNow is the moment of the refused approval that started this
// work: 15:19 CEST on Monday 28 September 2026, eleven minutes before the US
// open at 13:30Z.
var queuedFixtureNow = time.Date(2026, 9, 28, 13, 19, 0, 0, time.UTC)

// useQueueCalendar gives the server the embedded trading calendar even where
// broker test seams would otherwise keep it out of the way.
func useQueueCalendar(srv *Server) {
	srv.previewSessionAt = func(m marketcal.Market, at time.Time) (marketcal.Session, error) {
		return marketcal.New().SessionAt(m, at)
	}
}

// newQueueTestRig is the automatic rig at queuedFixtureNow with the calendar,
// a ready broker link and no pre-authorised bucket. The default build has no
// write capability, so a send that passes every earlier gate is refused at
// the write gate.
func newQueueTestRig(t *testing.T) *automaticTestRig {
	t.Helper()
	rig := newAutomaticTestRig(t, "")
	rig.now = queuedFixtureNow
	useQueueCalendar(rig.server)
	rig.server.gatewayReadyForTrading = func() bool { return true }
	// Classifying a working order needs the journal that knows its origin.
	rig.server.orderJournal = newTestOrderJournalStore(t, filepath.Join(t.TempDir(), "order-journal.jsonl"))
	return rig
}

// trimProposal is an issuer-trim row on the rig's 40-share stock position,
// built by the engine's own row constructor: a DAY limit selling qty.
func (r *automaticTestRig) trimProposal(qty int) rpc.TradeProposal {
	r.t.Helper()
	return r.reductionRow(rpc.TradeProposalBucketRiskReduction, automaticTestStockRow(), qty)
}

func (r *automaticTestRig) reductionRow(bucket string, row rpc.PositionView, qty int) rpc.TradeProposal {
	r.t.Helper()
	policy, status := r.server.protectionPolicies.Active()
	effect := rpc.OrderPositionEffectReduce
	if float64(qty) >= math.Abs(row.Quantity) {
		effect = rpc.OrderPositionEffectClose
	}
	return baseProposal(policy, status, rpc.TradeProposalSourceFingerprints{}, r.now, bucket, row, rpc.OrderActionSell, qty, effect, "synthetic reduction")
}

func queueTestOptionRow() rpc.PositionView {
	return rpc.PositionView{ConID: 202, Symbol: "SYN", SecType: "OPT", LocalSymbol: "SYN   261120C00025000", Expiry: "20261120", Strike: 25, Right: "C",
		Multiplier: 100, Quantity: 5, Mark: 2.5, Bid: new(2.4), Ask: new(2.6), Currency: "USD", Exchange: "SMART"}
}

func (r *automaticTestRig) queuePrepare(prop rpc.TradeProposal, revision string) rpc.TradeProposalQueuePrepareResult {
	r.t.Helper()
	res, err := r.engine.QueuePrepare(context.Background(), rpc.TradeProposalQueuePrepareParams{Key: prop.Key, Revision: revision})
	if err != nil || !res.Accepted || res.QueuedRef == "" || res.Queue == nil {
		r.t.Fatalf("queue prepare: accepted=%v blockers=%+v err=%v", res.Accepted, res.Blockers, err)
	}
	return res
}

func (r *automaticTestRig) queueArm(p rpc.TradeProposalQueuePrepareResult) rpc.TradeProposalQueueResult {
	r.t.Helper()
	res, err := r.engine.QueueArm(context.Background(), rpc.TradeProposalQueueArmParams{QueuedRef: p.QueuedRef, TermsDigest: p.Queue.TermsDigest,
		DeskActionID: "desk-action-1", Credential: "companion", Envelope: `{"signature":"synthetic"}`, Origin: rpc.OrderOriginAgent})
	if err != nil || !res.Accepted || res.Queue == nil || res.Queue.State != rpc.QueuedAuthArmed {
		r.t.Fatalf("queue arm: accepted=%v blockers=%+v err=%v", res.Accepted, res.Blockers, err)
	}
	return res
}

// queueAndArm prepares and arms prop at the rig clock and returns its ID.
func (r *automaticTestRig) queueAndArm(prop rpc.TradeProposal, revision string) string {
	r.t.Helper()
	return r.queueArm(r.queuePrepare(prop, revision)).Queue.Terms.QueueID
}

func (r *automaticTestRig) queued(id string) queuedAuthRecord {
	r.t.Helper()
	rec, ok := r.engine.queued.get(id)
	if !ok {
		r.t.Fatalf("no queued record %s", id)
	}
	return rec
}

func (r *automaticTestRig) queueCycle() { r.engine.runQueuedCycle(context.Background()) }

// queuedEvents returns one record's journaled events, oldest first.
func (r *automaticTestRig) queuedEvents(id string) []queuedAuthEvent {
	r.t.Helper()
	records, err := r.core.LoadEvents(context.Background(), corestore.EventQuery{Type: queuedCoreEventType})
	if err != nil {
		r.t.Fatalf("load queued events: %v", err)
	}
	var out []queuedAuthEvent
	for _, record := range records {
		var ev queuedAuthEvent
		if err := json.Unmarshal(record.PayloadJSON, &ev); err != nil {
			r.t.Fatalf("decode queued event: %v", err)
		}
		if ev.QueueID == id {
			out = append(out, ev)
		}
	}
	return out
}

func queuedEventTypes(events []queuedAuthEvent) []string {
	out := make([]string, 0, len(events))
	for _, ev := range events {
		out = append(out, ev.Type)
	}
	return out
}

// installWith serves props like install, then lets edit change the served
// snapshot before it is installed.
func (r *automaticTestRig) installWith(edit func(*rpc.TradeProposalSnapshot), props ...rpc.TradeProposal) string {
	r.t.Helper()
	revision := r.install(props...)
	snap := r.engine.Snapshot(false)
	edit(&snap)
	if err := r.engine.installSnapshot(snap, false); err != nil {
		r.t.Fatalf("install edited snapshot: %v", err)
	}
	return revision
}

// The 15:19 CEST fixture: the US session is closed and opens at 13:30Z, so a
// stock trim queues for 13:35Z to 14:35Z with the default bounds, and the
// private reference appears nowhere but the prepare result.
func TestQueuedPrepareAt1519CESTDatesTheSendWindow(t *testing.T) {
	t.Parallel()
	rig := newQueueTestRig(t)
	prop := rig.trimProposal(10)
	revision := rig.install(prop)
	res := rig.queuePrepare(prop, revision)
	if res.Readiness == nil || res.Readiness.Code != rpc.ReadinessMarketClosed || !res.Readiness.Queueable {
		t.Fatalf("readiness = %+v, want queueable market_closed", res.Readiness)
	}
	terms := res.Queue.Terms
	want := rpc.QueuedAuthTerms{
		Version: queuedTermsVersion, QueueID: terms.QueueID, AccountID: "DU1234567", AccountMode: "paper", Key: prop.Key, Bucket: rpc.TradeProposalBucketRiskReduction,
		RevisionAtQueue: revision, Contract: prop.Contract, Action: rpc.OrderActionSell, PositionEffect: rpc.OrderPositionEffectReduce, PositionQuantity: 40,
		MaxQuantity: 10, Style: rpc.QueuedAuthStyleBoundedLimit, Concession: 0.5, WorstPrice: 18.75, ReferenceMark: 25, ReferenceMarkAt: queuedFixtureNow,
		MaxSpreadPctOfMid: 2, Currency: "USD", Market: string(marketcal.MarketUSEquity), SessionDate: "2026-09-28",
		NotBefore: time.Date(2026, 9, 28, 13, 35, 0, 0, time.UTC), NotAfter: time.Date(2026, 9, 28, 14, 35, 0, 0, time.UTC), TIF: rpc.OrderTIFDay,
		ArmDeadline: queuedFixtureNow.Add(10 * time.Minute), RowTermsDigest: queuedRowTermsDigest(prop), PolicyFingerprint: terms.PolicyFingerprint,
	}
	gotJSON, _ := json.Marshal(terms)
	wantJSON, _ := json.Marshal(want)
	if string(gotJSON) != string(wantJSON) {
		t.Fatalf("terms =\n%s\nwant\n%s", gotJSON, wantJSON)
	}
	if terms.PolicyFingerprint.Key == "" {
		t.Fatal("terms carry no policy fingerprint")
	}
	if digest, err := queuedTermsDigest(terms); err != nil || digest != res.Queue.TermsDigest {
		t.Fatalf("terms digest %q does not recompute (%q, %v)", res.Queue.TermsDigest, digest, err)
	}
	if !strings.HasPrefix(res.QueuedRef, queuedReferencePrefix+"."+terms.QueueID+".") {
		t.Fatalf("queued reference %q does not name its record", res.QueuedRef)
	}
	rec := rig.queued(terms.QueueID)
	if rec.State != rpc.QueuedAuthPrepared || rec.ReferenceHash != sha256.Sum256([]byte(res.QueuedRef)) || rec.ContractSide != sameContractSide(prop) {
		t.Fatalf("stored record = %+v", rec)
	}
	secret := res.QueuedRef[strings.LastIndex(res.QueuedRef, ".")+1:]
	for name, v := range map[string]any{"list": rig.engine.QueueList(rpc.TradeProposalQueueListParams{}), "status": rig.engine.QueueStatus(rpc.TradeProposalQueueStatusParams{QueueID: terms.QueueID}),
		"events": rig.queuedEvents(terms.QueueID)} {
		raw, _ := json.Marshal(v)
		if strings.Contains(string(raw), secret) || strings.Contains(string(raw), "reference_hash") {
			t.Fatalf("%s leaks the private reference: %s", name, raw)
		}
	}
	if got := queuedEventTypes(rig.queuedEvents(terms.QueueID)); len(got) != 1 || got[0] != queuedEventPrepared {
		t.Fatalf("events = %v, want one prepared", got)
	}
}

// Only a queueable row in a closed or opening session gets a queue.
func TestQueuedPrepareRefusesWhatItCannotCarry(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		setup func(rig *automaticTestRig) rpc.TradeProposal
		code  string
	}{
		{"an exit needs a live bid", func(rig *automaticTestRig) rpc.TradeProposal { return rig.stopProposal() }, "queue_bucket_not_queueable"},
		{"a shadow row", func(rig *automaticTestRig) rpc.TradeProposal {
			p := rig.trimProposal(10)
			p.Shadow = true
			return p
		}, "shadow_mode"},
		{"an open market past its opening window", func(rig *automaticTestRig) rpc.TradeProposal {
			rig.now = time.Date(2026, 9, 28, 14, 30, 0, 0, time.UTC)
			return rig.trimProposal(10)
		}, "queue_not_offered"},
		{"a frozen desk", func(rig *automaticTestRig) rpc.TradeProposal {
			rig.server.platformSettings = frozenPlatformSettings()
			return rig.trimProposal(10)
		}, "queue_not_offered"},
		{"proposal submit disabled", func(rig *automaticTestRig) rpc.TradeProposal {
			rig.server.cfg.AutoTrade.FastPathEnabled = new(false)
			return rig.trimProposal(10)
		}, "fast_path_disabled"},
		{"a row with its own blocker", func(rig *automaticTestRig) rpc.TradeProposal {
			p := rig.trimProposal(10)
			p.State, p.Blockers = rpc.TradeProposalStateBlocked, []rpc.TradingBlocker{{Code: reductionOrderExistingCode, Message: "an order is working"}}
			return p
		}, reductionOrderExistingCode},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rig := newQueueTestRig(t)
			prop := tc.setup(rig)
			revision := rig.install(prop)
			res, err := rig.engine.QueuePrepare(context.Background(), rpc.TradeProposalQueuePrepareParams{Key: prop.Key, Revision: revision})
			if err != nil || res.Accepted || res.QueuedRef != "" || len(res.Blockers) == 0 || res.Blockers[0].Code != tc.code {
				t.Fatalf("prepare = accepted %v ref issued %v blockers %+v err %v, want %s", res.Accepted, res.QueuedRef != "", res.Blockers, err, tc.code)
			}
			if records := rig.engine.queued.list(); len(records) != 0 {
				t.Fatalf("a refused prepare stored %d records", len(records))
			}
		})
	}
}

// A queue is never prepared beside a pre-authorised submission Canary may
// place for the same row.
func TestQueuedPrepareRefusesBesideAPendingAutomaticSubmission(t *testing.T) {
	t.Parallel()
	rig := newAutomaticTestRig(t, `pre_authorised = ["budget_reduction"]`)
	rig.now = time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)
	useQueueCalendar(rig.server)
	rig.server.gatewayReadyForTrading = func() bool { return true }
	prop := rig.reductionRow(rpc.TradeProposalBucketBudgetReduction, automaticTestStockRow(), 10)
	revision := rig.install(prop)
	rig.cycle()
	if rec := rig.record(prop.Key, revision); !rec.waiting() {
		t.Fatalf("automatic record = %+v, want waiting", rec)
	}
	res, err := rig.engine.QueuePrepare(context.Background(), rpc.TradeProposalQueuePrepareParams{Key: prop.Key, Revision: revision})
	if err != nil || res.Accepted || len(res.Blockers) == 0 || res.Blockers[0].Code != "automatic_submission_pending" {
		t.Fatalf("prepare beside an automatic record = %+v err %v", res, err)
	}
}

// Arm needs the exact reference, the prepared digest, the current scope and
// a signature inside ten minutes; nothing else arms a record.
func TestQueuedArmNeedsTheSignedDigestInTime(t *testing.T) {
	t.Parallel()
	rig := newQueueTestRig(t)
	prop := rig.trimProposal(10)
	revision := rig.install(prop)
	p := rig.queuePrepare(prop, revision)
	id := p.Queue.Terms.QueueID
	arm := func(ref, digest string) rpc.TradeProposalQueueResult {
		t.Helper()
		res, err := rig.engine.QueueArm(context.Background(), rpc.TradeProposalQueueArmParams{QueuedRef: ref, TermsDigest: digest, Origin: rpc.OrderOriginAgent})
		if err != nil {
			t.Fatalf("arm: %v", err)
		}
		return res
	}
	otherSecret, _ := randomTokenID()
	forged := queuedReferencePrefix + "." + id + "." + otherSecret
	for name, tc := range map[string]struct{ ref, digest, code string }{
		"altered terms":    {p.QueuedRef, strings.Repeat("0", 64), "queued_terms_mismatch"},
		"forged reference": {forged, p.Queue.TermsDigest, "queued_reference_unavailable"},
		"no reference":     {"not-a-reference", p.Queue.TermsDigest, "queued_reference_unavailable"},
	} {
		if res := arm(tc.ref, tc.digest); res.Accepted || len(res.Blockers) == 0 || res.Blockers[0].Code != tc.code {
			t.Fatalf("%s: arm = %+v, want %s", name, res, tc.code)
		}
		if rec := rig.queued(id); rec.State != rpc.QueuedAuthPrepared {
			t.Fatalf("%s left the record %s", name, rec.State)
		}
	}
	rig.scope = brokerStateScope{Account: "DU7654321", Mode: "paper"}
	if res := arm(p.QueuedRef, p.Queue.TermsDigest); res.Accepted || res.Blockers[0].Code != "queued_scope_mismatch" {
		t.Fatalf("arm in another account = %+v", res)
	}
	rig.scope = brokerStateScope{Account: "DU1234567", Mode: "paper"}
	armed := rig.queueArm(p)
	rec := rig.queued(id)
	if rec.ArmedAt.IsZero() || rec.DeskActionID != "desk-action-1" || rec.Credential != "companion" || rec.Envelope == "" || rec.ArmOrigin != rpc.OrderOriginAgent || !rec.SettlingBaselineKnown {
		t.Fatalf("armed record = %+v", rec)
	}
	if armed.Queue.DeskActionID != "desk-action-1" || strings.Contains(armed.Message, p.QueuedRef) {
		t.Fatalf("arm result = %+v", armed)
	}
	if res := arm(p.QueuedRef, p.Queue.TermsDigest); res.Accepted || res.Blockers[0].Code != "queued_not_prepared" {
		t.Fatalf("second arm = %+v", res)
	}
	if got := queuedEventTypes(rig.queuedEvents(id)); strings.Join(got, ",") != queuedEventPrepared+","+queuedEventArmed {
		t.Fatalf("events = %v", got)
	}

	late := newQueueTestRig(t)
	lateProp := late.trimProposal(10)
	lp := late.queuePrepare(lateProp, late.install(lateProp))
	late.advance(10 * time.Minute)
	res, err := late.engine.QueueArm(context.Background(), rpc.TradeProposalQueueArmParams{QueuedRef: lp.QueuedRef, TermsDigest: lp.Queue.TermsDigest})
	if err != nil || res.Accepted || res.Blockers[0].Code != "queued_arm_deadline_passed" {
		t.Fatalf("late arm = %+v err %v", res, err)
	}
	if rec := late.queued(lp.Queue.Terms.QueueID); rec.State != rpc.QueuedAuthExpired || rec.ReasonCode != queuedReasonNotArmed {
		t.Fatalf("record after a late arm = %+v", rec)
	}
}

// One live intent per contract and side: an armed record blocks every other
// row for its contract and side, refuses a second queue for them, and a
// newer unarmed preparation replaces an older one.
func TestQueuedAuthorisationIsOneLiveIntentPerContractSide(t *testing.T) {
	t.Parallel()
	rig := newQueueTestRig(t)
	trim := rig.trimProposal(10)
	theta := rig.reductionRow(rpc.TradeProposalBucketThetaHygiene, automaticTestStockRow(), 10)
	revision := rig.install(trim, theta)
	id := rig.queueAndArm(trim, revision)
	side := sameContractSide(trim)
	if key, ok := rig.engine.liveIntentFor(side); !ok || key != trim.Key {
		t.Fatalf("live intent = %q %v, want %s", key, ok, trim.Key)
	}
	if blockers := rig.engine.liveIntentBlockers(theta); len(blockers) != 1 || blockers[0].Code != reductionIntentExistingCode {
		t.Fatalf("other row's blockers = %+v", blockers)
	}
	if blockers := rig.engine.liveIntentBlockers(trim); len(blockers) != 0 {
		t.Fatalf("the queued row blocks itself: %+v", blockers)
	}
	res, err := rig.engine.QueuePrepare(context.Background(), rpc.TradeProposalQueuePrepareParams{Key: theta.Key, Revision: revision})
	if err != nil || res.Accepted || res.Blockers[0].Code != "queued_intent_exists" {
		t.Fatalf("second queue for the contract = %+v err %v", res, err)
	}
	rig.scope = brokerStateScope{Account: "DU7654321", Mode: "paper"}
	if _, ok := rig.engine.liveIntentFor(side); ok {
		t.Fatal("another account's queue blocks this account's rows")
	}
	rig.scope = brokerStateScope{Account: "DU1234567", Mode: "paper"}
	if _, err := rig.engine.QueueCancel(context.Background(), rpc.TradeProposalQueueCancelParams{QueueID: id, Origin: rpc.OrderOriginHumanTTY}); err != nil {
		t.Fatal(err)
	}
	if _, ok := rig.engine.liveIntentFor(side); ok {
		t.Fatal("a cancelled queue still blocks the contract")
	}

	first := rig.queuePrepare(trim, revision)
	second := rig.queuePrepare(theta, revision)
	if rec := rig.queued(first.Queue.Terms.QueueID); rec.State != rpc.QueuedAuthCancelled || rec.ReasonCode != queuedReasonSuperseded {
		t.Fatalf("older unarmed preparation = %+v, want superseded", rec)
	}
	if rec := rig.queued(second.Queue.Terms.QueueID); rec.State != rpc.QueuedAuthPrepared {
		t.Fatalf("newer preparation = %+v", rec)
	}
}

// Cancelling withdraws authority for good, from any origin, and the executor
// then never revalidates or sends the record.
func TestQueuedCancelWithdrawsAuthority(t *testing.T) {
	t.Parallel()
	rig := newQueueTestRig(t)
	trim := rig.trimProposal(10)
	other := rig.reductionRow(rpc.TradeProposalBucketRiskReduction, rpc.PositionView{ConID: 102, Symbol: "SYO", SecType: "STK", Quantity: 20, Mark: 50, Currency: "USD", Exchange: "SMART"}, 5)
	revision := rig.install(trim, other)
	a := rig.queueAndArm(trim, revision)
	res, err := rig.engine.QueueCancel(context.Background(), rpc.TradeProposalQueueCancelParams{QueueID: a, Reason: "changed my mind", Origin: rpc.OrderOriginHumanTTY})
	if err != nil || !res.Accepted || res.Queue == nil || res.Queue.State != rpc.QueuedAuthCancelled {
		t.Fatalf("cancel = %+v err %v", res, err)
	}
	rec := rig.queued(a)
	if rec.ReasonCode != queuedReasonOwnerCancelled || rec.Reason != "changed my mind" || rec.CancelOrigin != rpc.OrderOriginHumanTTY {
		t.Fatalf("cancelled record = %+v", rec)
	}
	if events := rig.queuedEvents(a); events[len(events)-1].Type != queuedEventCancelled || events[len(events)-1].Origin != rpc.OrderOriginHumanTTY {
		t.Fatalf("cancel event = %+v", events[len(events)-1])
	}
	b := rig.queueAndArm(other, revision)
	c := rig.queuePrepare(trim, revision).Queue.Terms.QueueID
	all, err := rig.engine.QueueCancel(context.Background(), rpc.TradeProposalQueueCancelParams{All: true, Origin: rpc.OrderOriginAgent})
	if err != nil || !all.Accepted || len(all.Queues) != 2 {
		t.Fatalf("cancel all = %+v err %v", all, err)
	}
	for _, id := range []string{b, c} {
		if rec := rig.queued(id); rec.State != rpc.QueuedAuthCancelled || rec.CancelOrigin != rpc.OrderOriginAgent {
			t.Fatalf("record %s after cancel all = %+v", id, rec)
		}
	}
	rig.advance(20 * time.Minute)
	rig.queueCycle()
	if rig.queuedRefreshes != 0 {
		t.Fatalf("the executor revalidated %d cancelled records", rig.queuedRefreshes)
	}
	if res, err := rig.engine.QueueCancel(context.Background(), rpc.TradeProposalQueueCancelParams{QueueID: "unknown"}); err != nil || res.Accepted || res.Blockers[0].Code != "queued_not_found" {
		t.Fatalf("cancel unknown = %+v err %v", res, err)
	}
	if _, err := rig.engine.QueueCancel(context.Background(), rpc.TradeProposalQueueCancelParams{QueueID: a, All: true}); err == nil {
		t.Fatal("cancel with both an ID and all was accepted")
	}
	if list := rig.engine.QueueList(rpc.TradeProposalQueueListParams{LiveOnly: true}); len(list.Queues) != 0 {
		t.Fatalf("live list after cancelling everything = %+v", list.Queues)
	}
}

// A record that is never armed, or whose window ends before every gate
// passes, expires with its reason.
func TestQueuedRecordsExpire(t *testing.T) {
	t.Parallel()
	rig := newQueueTestRig(t)
	prop := rig.trimProposal(10)
	revision := rig.install(prop)
	unarmed := rig.queuePrepare(prop, revision).Queue.Terms.QueueID
	rig.advance(10 * time.Minute)
	rig.queueCycle()
	if rec := rig.queued(unarmed); rec.State != rpc.QueuedAuthExpired || rec.ReasonCode != queuedReasonNotArmed {
		t.Fatalf("unarmed record = %+v", rec)
	}
	armed := rig.queueAndArm(prop, revision)
	rig.server.gatewayReadyForTrading = func() bool { return false }
	rig.now = time.Date(2026, 9, 28, 13, 40, 0, 0, time.UTC)
	rig.queueCycle()
	if rec := rig.queued(armed); rec.State != rpc.QueuedAuthHeld || rec.HoldCode != rpc.ReadinessBrokerUnavailable {
		t.Fatalf("record without a broker link = %+v", rec)
	}
	rig.now = time.Date(2026, 9, 28, 14, 35, 0, 0, time.UTC)
	rig.queueCycle()
	rec := rig.queued(armed)
	if rec.State != rpc.QueuedAuthExpired || rec.ReasonCode != queuedReasonWindowEnded || !strings.Contains(rec.Reason, queuedHoldBrokerLinkReason) {
		t.Fatalf("record at the window's end = %+v", rec)
	}
	if got := queuedEventTypes(rig.queuedEvents(armed)); strings.Join(got, ",") != strings.Join([]string{queuedEventPrepared, queuedEventArmed, queuedEventHeld, queuedEventExpired}, ",") {
		t.Fatalf("events = %v", got)
	}
}

// Inside the window the executor revalidates by key: it waits out what can
// clear and cancels on a changed position, row, policy or account, while the
// same condition twice journals one hold.
func TestQueuedExecutorHoldsAndCancelsByCause(t *testing.T) {
	t.Parallel()
	inWindow := time.Date(2026, 9, 28, 13, 36, 0, 0, time.UTC)
	cases := []struct {
		name   string
		change func(rig *automaticTestRig, prop rpc.TradeProposal)
		state  string
		code   string
	}{
		{"trading frozen", func(rig *automaticTestRig, _ rpc.TradeProposal) {
			rig.server.platformSettings = frozenPlatformSettings()
		}, rpc.QueuedAuthHeld, rpc.ReadinessTradingFrozen},
		{"broker link down", func(rig *automaticTestRig, _ rpc.TradeProposal) {
			rig.server.gatewayReadyForTrading = func() bool { return false }
		}, rpc.QueuedAuthHeld, rpc.ReadinessBrokerUnavailable},
		{"hand order since the arm", func(rig *automaticTestRig, _ rpc.TradeProposal) {
			rig.freshOrders = append(rig.freshOrders, handOrder(777))
		}, rpc.QueuedAuthHeld, queuedHoldHandOrder},
		{"order inventory unreadable", func(rig *automaticTestRig, _ rpc.TradeProposal) { rig.inventoryErr = errTestInventory }, rpc.QueuedAuthHeld, rpc.ReadinessBrokerUnavailable},
		{"row blocked behind a working order", func(rig *automaticTestRig, prop rpc.TradeProposal) {
			prop.State, prop.Blockers = rpc.TradeProposalStateBlocked, []rpc.TradingBlocker{{Code: reductionOrderExistingCode, Message: "an order to sell 5 is working"}}
			rig.install(prop)
		}, rpc.QueuedAuthHeld, queuedHoldHandOrder},
		{"position changed", func(rig *automaticTestRig, prop rpc.TradeProposal) {
			prop.PositionQuantity = 30
			rig.install(prop)
		}, rpc.QueuedAuthCancelled, queuedReasonPositionChanged},
		{"row gone", func(rig *automaticTestRig, _ rpc.TradeProposal) { rig.install() }, rpc.QueuedAuthCancelled, queuedReasonRowGone},
		{"row effect changed", func(rig *automaticTestRig, prop rpc.TradeProposal) {
			prop.PositionEffect = rpc.OrderPositionEffectClose
			rig.install(prop)
		}, rpc.QueuedAuthCancelled, queuedReasonRowTermsChanged},
		{"policy changed", func(rig *automaticTestRig, prop rpc.TradeProposal) {
			rig.installWith(func(s *rpc.TradeProposalSnapshot) { s.EffectivePolicyFingerprint.Key = "sha256:changed" }, prop)
		}, rpc.QueuedAuthCancelled, queuedReasonPolicyChanged},
		{"account changed", func(rig *automaticTestRig, _ rpc.TradeProposal) {
			rig.scope = brokerStateScope{Account: "DU7654321", Mode: "paper"}
		}, rpc.QueuedAuthCancelled, queuedReasonAccountChanged},
		{"proposal submit disabled", func(rig *automaticTestRig, _ rpc.TradeProposal) {
			rig.server.cfg.AutoTrade.FastPathEnabled = new(false)
		}, rpc.QueuedAuthCancelled, "fast_path_disabled"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rig := newQueueTestRig(t)
			prop := rig.trimProposal(10)
			id := rig.queueAndArm(prop, rig.install(prop))
			rig.now = time.Date(2026, 9, 28, 13, 34, 0, 0, time.UTC)
			rig.queueCycle()
			if rec := rig.queued(id); rec.State != rpc.QueuedAuthArmed || rig.queuedRefreshes != 0 {
				t.Fatalf("before the window: record %s, %d revalidations", rec.State, rig.queuedRefreshes)
			}
			tc.change(rig, prop)
			rig.now = inWindow
			rig.queueCycle()
			rig.queueCycle()
			rec := rig.queued(id)
			if rec.State != tc.state || rec.HoldCode != tc.code && rec.ReasonCode != tc.code {
				t.Fatalf("record = state %s hold %q reason %q (%s), want %s %s", rec.State, rec.HoldCode, rec.ReasonCode, nonEmptyString(rec.HoldReason, rec.Reason), tc.state, tc.code)
			}
			n := 0
			for _, ev := range rig.queuedEvents(id) {
				if ev.Type == queuedEventHeld || ev.Type == queuedEventCancelled {
					n++
				}
			}
			if n != 1 {
				t.Fatalf("hold or cancel journaled %d times, want once", n)
			}
		})
	}
}

var errTestInventory = errors.New("open-order inventory unavailable")

func TestQueuedDispositionClassifiesRefusals(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		codes []string
		hold  bool
		code  string
	}{
		{[]string{tradingFrozenBlockerCode}, true, rpc.ReadinessTradingFrozen},
		{[]string{"gateway_unavailable"}, true, rpc.ReadinessBrokerUnavailable},
		{[]string{previewWhatIfFailedCode}, true, rpc.ReadinessBrokerUnavailable},
		{[]string{previewMarketClosedCode}, true, rpc.ReadinessMarketClosed},
		{[]string{previewBoundedSpreadCode}, true, rpc.ReadinessSpreadTooWide},
		{[]string{previewQuoteNotLiveCode}, true, rpc.ReadinessQuoteUnusable},
		{[]string{previewBoundedWorstCode}, true, queuedHoldWorstPrice},
		{[]string{reductionOrderExistingCode}, true, queuedHoldHandOrder},
		{[]string{"preview_not_submit_eligible"}, false, queuedReasonWhatIfRefused},
		{[]string{previewRiskLimitCode}, false, previewRiskLimitCode},
		{[]string{"shadow_mode"}, false, "shadow_mode"},
		// A refusal the send cannot wait out wins over one it can.
		{[]string{tradingFrozenBlockerCode, "proposal_effect_not_close_reduce"}, false, "proposal_effect_not_close_reduce"},
		{nil, false, ""},
	} {
		var blockers []rpc.TradingBlocker
		for _, c := range tc.codes {
			blockers = append(blockers, rpc.TradingBlocker{Code: c, Message: "m"})
		}
		hold, code, _ := queuedDisposition(blockers)
		if hold != tc.hold || code != tc.code {
			t.Fatalf("%v: hold %v code %q, want %v %q", tc.codes, hold, code, tc.hold, tc.code)
		}
	}
}

// Property: a bounded limit lands inside the spread, on the tick grid, and
// never beyond the worst price; a spread over the bound refuses.
func TestBoundedLimitPriceNeverPassesTheWorstPrice(t *testing.T) {
	t.Parallel()
	rng := rand.New(rand.NewPCG(28, 9))
	stock := rpc.ContractParams{Symbol: "SYN", SecType: "STK", Currency: "USD"}
	option := rpc.ContractParams{Symbol: "SYN", SecType: "OPT", Currency: "USD", Right: "C", Strike: 25, Expiry: "20261120"}
	for i := range 20000 {
		contract := stock
		if i%2 == 1 {
			contract = option
		}
		bid := math.Round((0.05+rng.Float64()*60)*100) / 100
		ask := bid + math.Round((0.01+rng.Float64()*bid*0.4)*100)/100
		mark := (bid + ask) / 2 * (0.6 + rng.Float64()*0.8)
		action := rpc.OrderActionSell
		if rng.IntN(2) == 0 {
			action = rpc.OrderActionBuy
		}
		bound := &rpc.OrderBoundedLimit{Concession: 0.5, WorstPrice: queuedDefaultWorstPrice(action, mark, contract), MaxSpreadPctOfMid: 25}
		quote := rpc.OrderQuoteSnapshot{Bid: new(bid), Ask: new(ask), DataType: rpc.MarketDataLive}
		limit, err := boundedLimitPrice(action, bound, nil, contract, quote)
		mid := (bid + ask) / 2
		if (ask-bid)/mid*100 > bound.MaxSpreadPctOfMid {
			if !previewRefusedWith(err, previewBoundedSpreadCode) {
				t.Fatalf("spread %.2f%% over the bound priced %v (%v)", (ask-bid)/mid*100, limit, err)
			}
			continue
		}
		if err != nil {
			if !previewRefusedWith(err, previewBoundedWorstCode) {
				t.Fatalf("bid %v ask %v: unexpected refusal %v", bid, ask, err)
			}
			continue
		}
		if !queuedLimitInside(action, limit, bound.WorstPrice) {
			t.Fatalf("%s limit %v beyond the worst price %v", action, limit, bound.WorstPrice)
		}
		if limit < bid-1e-9 || limit > ask+1e-9 {
			t.Fatalf("limit %v outside the spread %v–%v", limit, bid, ask)
		}
		tick := patientLimitTick(contract, quote, mid)
		if steps := limit / tick; math.Abs(steps-math.Round(steps)) > 1e-6 && limit != ask && limit != bid {
			t.Fatalf("limit %v is off the %v grid", limit, tick)
		}
		if action == rpc.OrderActionSell && limit > mid+tick+1e-9 || action == rpc.OrderActionBuy && limit < mid-tick-1e-9 {
			t.Fatalf("%s limit %v concedes nothing from the mid %v", action, limit, mid)
		}
	}
}

func previewRefusedWith(err error, code string) bool {
	for _, b := range previewFailureBlockers(err) {
		if b.Code == code {
			return true
		}
	}
	return false
}

// The halfway concession on a concrete quote: sell 24.95 inside 24.90–25.10.
func TestBoundedLimitPriceIsHalfwayToTheBid(t *testing.T) {
	t.Parallel()
	quote := rpc.OrderQuoteSnapshot{Bid: new(24.9), Ask: new(25.1), DataType: rpc.MarketDataLive}
	bound := &rpc.OrderBoundedLimit{Concession: 0.5, WorstPrice: 18.75, MaxSpreadPctOfMid: 2}
	stock := rpc.ContractParams{Symbol: "SYN", SecType: "STK"}
	if limit, err := boundedLimitPrice(rpc.OrderActionSell, bound, nil, stock, quote); err != nil || limit != 24.95 {
		t.Fatalf("sell limit = %v, %v; want 24.95", limit, err)
	}
	bound.WorstPrice = 25.2
	if limit, err := boundedLimitPrice(rpc.OrderActionBuy, bound, nil, stock, quote); err != nil || limit != 25.05 {
		t.Fatalf("buy limit = %v, %v; want 25.05", limit, err)
	}
	if _, err := boundedLimitPrice(rpc.OrderActionSell, nil, nil, stock, quote); err == nil {
		t.Fatal("a bounded limit without its bound was priced")
	}
	stale := quote
	stale.Stale = true
	if _, err := boundedLimitPrice(rpc.OrderActionSell, bound, nil, stock, stale); !previewRefusedWith(err, previewQuoteStaleCode) {
		t.Fatalf("stale quote: %v", err)
	}
}

// Dates across the calendar: the 26–30 October 2026 week opens at 14:30 CET
// (Europe has left summer time, the US has not), a holiday and the weekend
// move the window to the next regular open, and an option waits 15 minutes.
func TestQueuedTermsFollowTheCalendar(t *testing.T) {
	t.Parallel()
	cet := time.FixedZone("CET", 3600)
	for _, tc := range []struct {
		name          string
		now           time.Time
		option        bool
		notBefore     time.Time
		sessionDate   string
		refusedByCode string
	}{
		{"options in the 26–30 October week", time.Date(2026, 10, 27, 13, 0, 0, 0, cet), true, time.Date(2026, 10, 27, 14, 45, 0, 0, cet), "2026-10-27", ""},
		{"a stock on Thanksgiving waits for Friday", time.Date(2026, 11, 26, 15, 0, 0, 0, time.UTC), false, time.Date(2026, 11, 27, 14, 35, 0, 0, time.UTC), "2026-11-27", ""},
		{"options after the Christmas Eve early close wait for Monday", time.Date(2026, 12, 24, 19, 0, 0, 0, time.UTC), true, time.Date(2026, 12, 28, 14, 45, 0, 0, time.UTC), "2026-12-28", ""},
		{"a Saturday queues for Monday", time.Date(2026, 10, 3, 10, 0, 0, 0, time.UTC), false, time.Date(2026, 10, 5, 13, 35, 0, 0, time.UTC), "2026-10-05", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rig := newQueueTestRig(t)
			rig.now = tc.now
			prop := rig.trimProposal(10)
			if tc.option {
				prop = rig.reductionRow(rpc.TradeProposalBucketThetaHygiene, queueTestOptionRow(), 5)
			}
			res := rig.queuePrepare(prop, rig.install(prop))
			terms := res.Queue.Terms
			if !terms.NotBefore.Equal(tc.notBefore) || !terms.NotAfter.Equal(tc.notBefore.Add(time.Hour)) || terms.SessionDate != tc.sessionDate {
				t.Fatalf("window %s–%s on %s, want from %s on %s", terms.NotBefore, terms.NotAfter, terms.SessionDate, tc.notBefore.UTC(), tc.sessionDate)
			}
			if tc.option && (terms.Market != string(marketcal.MarketUSOptions) || terms.WorstPrice != 1.88 || terms.MaxSpreadPctOfMid != 25) {
				t.Fatalf("option terms = market %s worst %v spread %v", terms.Market, terms.WorstPrice, terms.MaxSpreadPctOfMid)
			}
		})
	}
}

// Stored terms re-encode to their digest after a restart, and the restarted
// executor still knows the armed record.
func TestQueuedTermsDigestSurvivesARestart(t *testing.T) {
	t.Parallel()
	rig := newQueueTestRig(t)
	prop := rig.trimProposal(10)
	p := rig.queuePrepare(prop, rig.install(prop))
	rig.queueArm(p)
	rig.restart()
	rec := rig.queued(p.Queue.Terms.QueueID)
	if digest, err := queuedTermsDigest(rec.Terms); err != nil || digest != p.Queue.TermsDigest || rec.State != rpc.QueuedAuthArmed {
		t.Fatalf("after restart: digest %q (%v), state %s; want %q armed", digest, err, rec.State, p.Queue.TermsDigest)
	}
	if next, ok := rig.engine.queuedWake(rig.now); !ok || next != 16*time.Minute {
		t.Fatalf("wake after restart = %v %v, want 16m to the window", next, ok)
	}
}

// A pre-authorised submission that falls due before its row's session has
// been open for the opening offset waits for the open plus the offset. The
// budget governor's patient limit prices off the live mid; a stock trail
// seeds its own stop and keeps its window.
func TestPreAuthorisedDueWaitsForTheOpenPlusOffset(t *testing.T) {
	t.Parallel()
	rig := newAutomaticTestRig(t, `pre_authorised = ["budget_reduction", "trailing_stop"]`)
	rig.now = time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)
	useQueueCalendar(rig.server)
	prop := rig.reductionRow(rpc.TradeProposalBucketBudgetReduction, automaticTestStockRow(), 10)
	stop := rig.stopProposal()
	stop.Contract.ConID, stop.Symbol, stop.Contract.Symbol = 303, "STP", "STP"
	stop.Key = proposalKey(stop.Bucket, stop.Contract, stop.Action)
	revision := rig.install(prop, stop)
	rig.cycle()
	if rec := rig.record(stop.Key, revision); !rec.SubmitAt.Equal(rig.now.Add(defaultVetoWindow)) {
		t.Fatalf("seeded stop submit at %s, want its plain window %s", rec.SubmitAt, rig.now.Add(defaultVetoWindow))
	}
	rec := rig.record(prop.Key, revision)
	if want := time.Date(2026, 9, 28, 13, 35, 0, 0, time.UTC); !rec.SubmitAt.Equal(want) {
		t.Fatalf("submit at %s, want the open plus five minutes %s", rec.SubmitAt, want)
	}
	rig.notice(rec)
	if rec = rig.record(prop.Key, revision); !rec.SubmitAt.Equal(time.Date(2026, 9, 28, 13, 35, 0, 0, time.UTC)) {
		t.Fatalf("the notice moved the session bound: %s", rec.SubmitAt)
	}
	// A record already due while the session is closed is rescheduled
	// rather than attempted.
	err := rig.engine.automatic.update(context.Background(), func(records map[string]*automaticSubmissionRecord) []automaticSubmissionEvent {
		r := records[automaticRecordKey(prop.Key, revision)]
		r.SubmitAt = rig.now
		return []automaticSubmissionEvent{{At: rig.now, Type: automaticEventNoticed, Key: r.Key, Revision: r.Revision}}
	})
	if err != nil {
		t.Fatal(err)
	}
	rig.cycle()
	if rec = rig.record(prop.Key, revision); !rec.SubmitAt.Equal(time.Date(2026, 9, 28, 13, 35, 0, 0, time.UTC)) || rig.revalidations != 0 {
		t.Fatalf("due before the open: submit at %s after %d attempts", rec.SubmitAt, rig.revalidations)
	}
	if rig.eventCount(prop.Key, revision, automaticEventRescheduled) != 1 {
		t.Fatalf("events = %+v", rig.events(prop.Key, revision))
	}
	rig.now = time.Date(2026, 9, 28, 13, 35, 0, 0, time.UTC)
	rig.cycle()
	if rig.revalidations != 1 {
		t.Fatalf("attempts at the open plus the offset = %d, want 1", rig.revalidations)
	}
}
