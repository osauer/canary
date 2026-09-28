package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/config"
	"github.com/osauer/canary/v2/internal/daemon/corestore"
	"github.com/osauer/canary/v2/internal/rpc"
)

// Witnesses of the #41 review of queued authorisations (2026-09-28).

// A live queue marks every served row for its contract and side, in the
// snapshot's account and mode only, and the submit gates refuse the row's own
// key too: only the executor's send of that record, under its grant, passes.
func TestQueuedRowIsMarkedAndGatedForEveryOtherCaller(t *testing.T) {
	t.Parallel()
	rig := newQueueTestRig(t)
	trim := rig.trimProposal(10)
	theta := rig.reductionRow(rpc.TradeProposalBucketThetaHygiene, automaticTestStockRow(), 10)
	revision := rig.install(trim, theta)
	prepared := rig.queuePrepare(trim, revision)
	for _, row := range rig.engine.Snapshot(false).Proposals {
		if row.Queued != nil {
			t.Fatalf("an unarmed preparation marked row %s: %+v", row.Key, row.Queued)
		}
	}
	id := rig.queueArm(prepared).Queue.Terms.QueueID
	for _, row := range rig.engine.Snapshot(false).Proposals {
		if row.Queued == nil || row.Queued.QueueID != id || row.Queued.Key != trim.Key || row.Queued.State != rpc.QueuedAuthArmed || !row.Queued.NotBefore.Equal(queueWindowOpenDefault) {
			t.Fatalf("row %s marker = %+v, want armed %s", row.Key, row.Queued, id)
		}
	}
	for _, row := range []rpc.TradeProposal{trim, theta} {
		if blockers := rig.engine.queuedIntentGateBlockers(row, nil); len(blockers) != 1 || blockers[0].Code != queuedIntentExistsCode {
			t.Fatalf("gate for %s without a grant = %+v", row.Key, blockers)
		}
	}
	rec := rig.queued(id)
	if blockers := rig.engine.queuedIntentGateBlockers(trim, &rec); len(blockers) != 1 {
		t.Fatalf("the record's own send passed without the executor's grant: %+v", blockers)
	}
	rig.server.queuedGrant.Store(&queuedWriteGrant{QueueID: id, Key: trim.Key, Bucket: trim.Bucket})
	if blockers := rig.engine.queuedIntentGateBlockers(trim, &rec); len(blockers) != 0 {
		t.Fatalf("the executor's own send was refused: %+v", blockers)
	}
	if blockers := rig.engine.queuedIntentGateBlockers(trim, nil); len(blockers) != 1 {
		t.Fatal("another caller passed while the executor held its grant")
	}
	rig.server.queuedGrant.Store(nil)

	rig.installWith(func(snap *rpc.TradeProposalSnapshot) { snap.AccountID = "DU7654321" }, trim, theta)
	for _, row := range rig.engine.Snapshot(false).Proposals {
		if row.Queued != nil {
			t.Fatalf("another account's rows carry this account's queue: %+v", row.Queued)
		}
	}
	rig.install(trim, theta)
	if _, err := rig.engine.QueueCancel(context.Background(), rpc.TradeProposalQueueCancelParams{QueueID: id}); err != nil {
		t.Fatal(err)
	}
	for _, row := range rig.engine.Snapshot(false).Proposals {
		if row.Queued != nil {
			t.Fatalf("a cancelled queue still marks %s", row.Key)
		}
	}
	if blockers := rig.engine.queuedIntentGateBlockers(trim, nil); len(blockers) != 0 {
		t.Fatalf("a cancelled queue still gates the row: %+v", blockers)
	}
}

// queueWindowOpenDefault is the default-build rig's send window start, the
// same instant as the trading rig's queueWindowOpen.
var queueWindowOpenDefault = time.Date(2026, 9, 28, 13, 35, 0, 0, time.UTC)

// A sent order that still works holds the contract and side: neither a new
// queue nor an arm goes beside it, and the rows stay marked sent.
func TestQueuedOrderWorkingRefusesAnotherQueue(t *testing.T) {
	t.Parallel()
	rig := newQueueTestRig(t)
	trim := rig.trimProposal(10)
	theta := rig.reductionRow(rpc.TradeProposalBucketThetaHygiene, automaticTestStockRow(), 10)
	revision := rig.install(trim, theta)
	other := rig.queuePrepare(theta, revision)
	id := rig.queueAndArm(trim, revision)
	err := rig.engine.queued.update(context.Background(), rig.now, func(records map[string]*queuedAuthRecord) []queuedAuthEvent {
		r := records[id]
		r.SentAt, r.OrderRef, r.QuantitySent = rig.now, "synthetic-ref", 10
		return []queuedAuthEvent{r.transition(queuedEventSent, rpc.QueuedAuthSent, rig.now)}
	})
	if err != nil {
		t.Fatal(err)
	}
	if res, err := rig.engine.QueuePrepare(context.Background(), rpc.TradeProposalQueuePrepareParams{Key: theta.Key, Revision: revision}); err != nil || res.Accepted || res.Blockers[0].Code != "queued_order_working" {
		t.Fatalf("prepare beside a working queued order = %+v err %v", res, err)
	}
	res, err := rig.engine.QueueArm(context.Background(), rpc.TradeProposalQueueArmParams{QueuedRef: other.QueuedRef, TermsDigest: other.Queue.TermsDigest})
	if err != nil || res.Accepted {
		t.Fatalf("arm beside a working queued order = %+v err %v", res, err)
	}
	for _, row := range rig.engine.Snapshot(false).Proposals {
		if row.Queued == nil || row.Queued.State != rpc.QueuedAuthSent {
			t.Fatalf("row %s while the queued order works: %+v", row.Key, row.Queued)
		}
	}
}

// Arm re-checks the pre-authorised scheduler: a record it created after
// prepare keeps the queue unarmed.
func TestQueuedArmRefusesBesideAnAutomaticRecordCreatedAfterPrepare(t *testing.T) {
	t.Parallel()
	rig := newAutomaticTestRig(t, `pre_authorised = ["budget_reduction"]`)
	rig.now = time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)
	useQueueCalendar(rig.server)
	rig.server.gatewayReadyForTrading = func() bool { return true }
	prop := rig.reductionRow(rpc.TradeProposalBucketBudgetReduction, automaticTestStockRow(), 10)
	revision := rig.install(prop)
	p := rig.queuePrepare(prop, revision)
	rig.cycle()
	if rec := rig.record(prop.Key, revision); !rec.waiting() {
		t.Fatalf("automatic record = %+v, want waiting", rec)
	}
	res, err := rig.engine.QueueArm(context.Background(), rpc.TradeProposalQueueArmParams{QueuedRef: p.QueuedRef, TermsDigest: p.Queue.TermsDigest})
	if err != nil || res.Accepted || len(res.Blockers) == 0 || res.Blockers[0].Code != "automatic_submission_pending" {
		t.Fatalf("arm beside a new automatic record = %+v err %v", res, err)
	}
	if rec := rig.queued(p.Queue.Terms.QueueID); rec.State != rpc.QueuedAuthPrepared {
		t.Fatalf("queued record = %+v, want still prepared", rec)
	}
}

// A cancel that arrives while the order is being placed is kept: cancel-all
// reports the record in flight instead of claiming it, and an attempt that
// proves unsent ends cancelled rather than waiting to send again.
func TestQueuedCancelWhileSendingIsKept(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		resolve func(rig *automaticTestRig)
	}{
		{"after a restart", func(rig *automaticTestRig) { rig.advance(time.Minute); rig.restart() }},
		{"in the same process once stale", func(rig *automaticTestRig) { rig.advance(queuedSendingStale + time.Second) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rig := newQueueTestRig(t)
			trim := rig.trimProposal(10)
			id := rig.queueAndArm(trim, rig.install(trim))
			rig.now = queueWindowOpenDefault
			err := rig.engine.queued.update(context.Background(), rig.now, func(records map[string]*queuedAuthRecord) []queuedAuthEvent {
				r := records[id]
				r.SendingAt, r.PreviewTokenID = rig.now, "token-never-staged"
				return []queuedAuthEvent{r.transition(queuedEventSending, rpc.QueuedAuthSending, rig.now)}
			})
			if err != nil {
				t.Fatal(err)
			}
			one, err := rig.engine.QueueCancel(context.Background(), rpc.TradeProposalQueueCancelParams{QueueID: id, Origin: rpc.OrderOriginHumanTTY})
			if err != nil || one.Accepted || one.Blockers[0].Code != "queued_sending" || one.Queue == nil || !one.Queue.CancelRequested {
				t.Fatalf("cancel while sending = %+v err %v", one, err)
			}
			all, err := rig.engine.QueueCancel(context.Background(), rpc.TradeProposalQueueCancelParams{All: true})
			if err != nil || !all.Accepted || len(all.Queues) != 0 || len(all.InFlight) != 1 || all.InFlight[0].Terms.QueueID != id || !strings.Contains(all.Message, "could not be cancelled here") {
				t.Fatalf("cancel all while sending = %+v err %v", all, err)
			}
			requests := 0
			for _, typ := range queuedEventTypes(rig.queuedEvents(id)) {
				if typ == queuedEventCancelRequested {
					requests++
				}
			}
			if requests != 1 {
				t.Fatalf("cancel requests journaled = %d, want 1", requests)
			}
			tc.resolve(rig)
			rig.queueCycle()
			rec := rig.queued(id)
			if rec.State != rpc.QueuedAuthCancelled || rec.ReasonCode != queuedReasonOwnerCancelled || rig.queuedRefreshes != 0 {
				t.Fatalf("an unsent attempt with a cancel request = %+v, %d revalidations", rec, rig.queuedRefreshes)
			}
		})
	}
}

// A stored record this build cannot fully read is loaded, never sent, and
// ended first: cancelled when it had not been sent, failed when its state is
// unknown.
func TestQueuedStoreEndsRecordsItCannotRead(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		mutate func(rec map[string]any)
		state  string
	}{
		{"a field in the terms", func(rec map[string]any) { rec["terms"].(map[string]any)["future_bound"] = 1.5 }, rpc.QueuedAuthCancelled},
		{"a field on the record", func(rec map[string]any) { rec["future_flag"] = true }, rpc.QueuedAuthCancelled},
		{"a newer version", func(rec map[string]any) { rec["version"] = 2 }, rpc.QueuedAuthCancelled},
		{"terms off their digest", func(rec map[string]any) { rec["terms"].(map[string]any)["max_quantity"] = 40 }, rpc.QueuedAuthCancelled},
		{"an unknown state", func(rec map[string]any) { rec["state"] = "paused" }, rpc.QueuedAuthFailed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rig := newQueueTestRig(t)
			trim := rig.trimProposal(10)
			id := rig.queueAndArm(trim, rig.install(trim))
			rewriteQueuedDocument(t, rig, func(records []map[string]any) { tc.mutate(records[0]) })
			rig.restart()
			rec := rig.queued(id)
			if queuedRecordProblem(rec) == "" {
				t.Fatalf("the altered record loaded without a problem: %+v", rec)
			}
			res, err := rig.engine.QueueArm(context.Background(), rpc.TradeProposalQueueArmParams{QueuedRef: "canaryqa1.x.y", TermsDigest: rec.TermsDigest})
			if err != nil || res.Accepted {
				t.Fatalf("arm = %+v err %v", res, err)
			}
			rig.now = queueWindowOpenDefault
			rig.queueCycle()
			rec = rig.queued(id)
			if rec.State != tc.state || rec.ReasonCode != queuedReasonRecordUnreadable || rig.queuedRefreshes != 0 {
				t.Fatalf("record = %+v, %d revalidations; want %s (%s)", rec, rig.queuedRefreshes, tc.state, queuedReasonRecordUnreadable)
			}
			if _, ok := rig.engine.queuedLiveIntentFor(sameContractSide(trim)); ok {
				t.Fatal("an ended unreadable record still blocks the contract")
			}
		})
	}
}

// rewriteQueuedDocument edits the stored queued document in daemon.db the way
// a newer or damaged build could have left it.
func rewriteQueuedDocument(t *testing.T, rig *automaticTestRig, edit func(records []map[string]any)) {
	t.Helper()
	ctx := context.Background()
	doc, ok, err := rig.core.GetStateDocument(ctx, daemonStateScope, queuedStateKind)
	if err != nil || !ok {
		t.Fatalf("load queued document: ok=%v err=%v", ok, err)
	}
	var parsed struct {
		Version int              `json:"version"`
		Records []map[string]any `json:"records"`
	}
	if err := json.Unmarshal(doc.JSON, &parsed); err != nil || len(parsed.Records) == 0 {
		t.Fatalf("decode queued document: %v", err)
	}
	edit(parsed.Records)
	raw, err := json.Marshal(parsed)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rig.core.CompareAndSwapStateDocument(ctx, corestore.StateDocumentCAS{ScopeKey: daemonStateScope, Kind: queuedStateKind, ExpectedRevision: doc.Revision, JSON: raw}); err != nil {
		t.Fatalf("rewrite queued document: %v", err)
	}
}

// While config.toml runs an automation section on Canary's defaults, a queued
// send holds inside its window (owner decision #42) and revalidates nothing.
func TestQueuedSendHoldsWhileConfigPausesAutomation(t *testing.T) {
	t.Parallel()
	rig := newQueueTestRig(t)
	trim := rig.trimProposal(10)
	id := rig.queueAndArm(trim, rig.install(trim))
	rig.server.addConfigIssue(config.Issue{Key: "auto_trade.fast_path_enabled", Problem: "synthetic: not a boolean"})
	rig.now = queueWindowOpenDefault
	rig.queueCycle()
	rec := rig.queued(id)
	if rec.State != rpc.QueuedAuthHeld || rec.HoldCode != queuedHoldConfigPaused || !strings.Contains(rec.HoldReason, "[auto_trade]") || rig.queuedRefreshes != 0 {
		t.Fatalf("record while config pauses automation = %+v, %d revalidations", rec, rig.queuedRefreshes)
	}
}

// A refresh that fails holds the send even when an older snapshot is still
// at hand: nothing revalidates against stale rows.
func TestQueuedResolveHoldsOnARefreshErrorWithAStaleSnapshot(t *testing.T) {
	t.Parallel()
	rig := newQueueTestRig(t)
	trim := rig.trimProposal(10)
	id := rig.queueAndArm(trim, rig.install(trim))
	rig.engine.queuedRefreshForTest = func(context.Context) (rpc.TradeProposalSnapshot, error) {
		return rig.engine.Snapshot(false), errors.New("synthetic: the refresh timed out")
	}
	rig.now = queueWindowOpenDefault
	rig.queueCycle()
	rec := rig.queued(id)
	if rec.State != rpc.QueuedAuthHeld || rec.HoldCode != rpc.ReadinessBrokerUnavailable || !strings.Contains(rec.HoldReason, "timed out") {
		t.Fatalf("record after a failed refresh = %+v", rec)
	}
}

func TestQueuedPrepareRefusesANegativeQuantity(t *testing.T) {
	t.Parallel()
	rig := newQueueTestRig(t)
	trim := rig.trimProposal(10)
	revision := rig.install(trim)
	if res, err := rig.engine.QueuePrepare(context.Background(), rpc.TradeProposalQueuePrepareParams{Key: trim.Key, Revision: revision, Quantity: -1}); err == nil || res.Accepted {
		t.Fatalf("negative quantity = %+v err %v", res, err)
	}
}

// The owner signs every byte of the terms, and Desk and the companion refuse
// a field they do not know (their queueTermsFields). Adding a field here is a
// contract change: teach Desk and the companion first, then this list.
func TestQueuedTermsFieldsAreTheSignedContract(t *testing.T) {
	t.Parallel()
	fields := func(typ reflect.Type) []string {
		var out []string
		for f := range typ.Fields() {
			name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
			out = append(out, name)
		}
		return out
	}
	for _, tc := range []struct {
		typ  reflect.Type
		want []string
	}{
		{reflect.TypeFor[rpc.QueuedAuthTerms](), []string{"version", "queue_id", "account_id", "account_mode", "key", "bucket", "revision_at_queue",
			"contract", "action", "position_effect", "position_quantity", "max_quantity", "style", "concession", "worst_price",
			"reference_mark", "reference_mark_at", "max_spread_pct_of_mid", "currency", "market", "session_date", "not_before",
			"not_after", "tif", "arm_deadline", "row_terms_digest", "policy_fingerprint", "rulebook_fingerprint"}},
		{reflect.TypeFor[rpc.ContractParams](), []string{"con_id", "symbol", "sec_type", "market", "exchange", "primary_exchange", "currency",
			"local_symbol", "trading_class", "expiry", "strike", "right", "multiplier", "min_tick"}},
		{reflect.TypeFor[rpc.Fingerprint](), []string{"version", "key"}},
	} {
		if got := fields(tc.typ); !slices.Equal(got, tc.want) {
			t.Errorf("%s JSON fields = %v, want %v: a change here needs Desk's and the companion's allowlists first", tc.typ.Name(), got, tc.want)
		}
	}
}

// A queued row is counted queued for the open, never ready to act: the brief
// raises no attention for it, and the row is not offered a second queue.
func TestQueuedRowIsCountedQueuedNotActionable(t *testing.T) {
	t.Parallel()
	rig := newQueueTestRig(t)
	trim := rig.trimProposal(10)
	revision := rig.install(trim)
	before := rig.engine.Snapshot(false)
	if before.Counts.Actionable != 1 || before.Counts.Queued != 0 || !before.Proposals[0].Readiness.Queueable {
		t.Fatalf("before the queue: counts %+v, readiness %+v", before.Counts, before.Proposals[0].Readiness)
	}
	rig.queueAndArm(trim, revision)
	snap := rig.engine.Snapshot(false)
	if snap.Counts.Actionable != 0 || snap.Counts.Queued != 1 || snap.Counts.Total != 1 {
		t.Fatalf("counts with the row queued = %+v", snap.Counts)
	}
	if r := snap.Proposals[0].Readiness; r == nil || r.Queueable {
		t.Fatalf("a queued row is still offered a queue: %+v", r)
	}
	rig.server.tradeProposals = rig.engine
	row := rig.server.briefReadyProposals()
	if row.Actionable != 0 || row.Queued != 1 || row.Blocked != 0 || row.Status == rpc.BriefStatusAttention || !strings.Contains(row.Detail, "1 queued for the open") {
		t.Fatalf("brief row with the row queued = %+v", row)
	}
}

// A sent order the broker reconciled as gone without a recorded end has no
// confirmed fill: it fails for the owner to check, never reads as unfilled.
func TestQueuedOrderReconciledAwayIsNotCalledUnfilled(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		view  rpc.OrderView
		state string
	}{
		{rpc.OrderView{LifecycleStatus: rpc.OrderLifecycleClosedReconciled}, rpc.QueuedAuthFailed},
		{rpc.OrderView{LifecycleStatus: rpc.OrderLifecycleClosedReconciled, Filled: 4}, rpc.QueuedAuthFailed},
		{rpc.OrderView{LifecycleStatus: rpc.OrderLifecycleClosedReconciled, Filled: 10}, rpc.QueuedAuthFilled},
		{rpc.OrderView{LifecycleStatus: rpc.OrderLifecycleCancelled}, rpc.QueuedAuthExpiredUnfilled},
		{rpc.OrderView{LifecycleStatus: rpc.OrderLifecycleCancelled, Filled: 4}, rpc.QueuedAuthPartiallyFilled},
		{rpc.OrderView{LifecycleStatus: rpc.OrderLifecycleFilled, Filled: 10}, rpc.QueuedAuthFilled},
	} {
		_, state, code, _, done := queuedOrderResolution(tc.view, 10)
		if !done || state != tc.state || state == rpc.QueuedAuthFailed && code != "fill_unconfirmed" {
			t.Errorf("%+v: state %s code %s done %v, want %s", tc.view, state, code, done, tc.state)
		}
	}
	if _, _, _, _, done := queuedOrderResolution(rpc.OrderView{LifecycleStatus: "working"}, 10); done {
		t.Error("a working order was resolved")
	}
}
