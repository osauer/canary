package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/daemon/corestore"
	"github.com/osauer/canary/v2/internal/risk"
	"github.com/osauer/canary/v2/internal/rpc"
)

func TestRegimeProjectionReceiptIdentityAndGapRejection(t *testing.T) {
	tests := []struct {
		name            string
		snapshotRevs    int
		receiptRevision int64
		mutateReceipt   func(regimeSnapshotPublication) regimeSnapshotPublication
		wantErr         string
		wantRevision    int64
	}{
		{name: "first snapshot bootstraps missing receipt", snapshotRevs: 1, wantRevision: 1},
		{name: "exact receipt", snapshotRevs: 3, receiptRevision: 3, wantRevision: 3},
		{name: "one revision crash gap", snapshotRevs: 3, receiptRevision: 2, wantRevision: 3},
		{name: "missing receipt only valid for first snapshot", snapshotRevs: 3, wantErr: "receipt is missing"},
		{name: "gap larger than one", snapshotRevs: 3, receiptRevision: 1, wantErr: "cannot safely recover"},
		{name: "receipt ahead", snapshotRevs: 3, receiptRevision: 4, wantErr: "ahead of snapshot"},
		{
			name:            "same revision different fingerprint",
			snapshotRevs:    3,
			receiptRevision: 3,
			mutateReceipt: func(publication regimeSnapshotPublication) regimeSnapshotPublication {
				publication.Fingerprint.Key = "sha256:wrong-publication"
				return publication
			},
			wantErr: "cannot safely recover",
		},
		{
			name:            "same revision different publication time",
			snapshotRevs:    3,
			receiptRevision: 3,
			mutateReceipt: func(publication regimeSnapshotPublication) regimeSnapshotPublication {
				publication.PublishedAt = publication.PublishedAt.Add(-time.Second)
				return publication
			},
			wantErr: "cannot safely recover",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store := openRegimeSnapshotTestStore(t)
			snapshot := regimeSnapshotCacheFixture(regimeSnapshotTestNow(), test.name)
			snapshot.Lifecycle.Stage = rpc.LifecycleQuiet
			snapshot.Fingerprint = rpc.BuildRegimeFingerprint(snapshot)
			cache := projectionRecoveryPersistSnapshot(t, store, snapshot, test.snapshotRevs)
			publication, _, err := cache.publication()
			if err != nil {
				t.Fatalf("read snapshot publication: %v", err)
			}

			server := &Server{
				coreStore:              store,
				rulesRegimeStageLoaded: true,
				logger:                 NewLogger(&bytes.Buffer{}, "error"),
			}
			if test.receiptRevision > 0 {
				receiptPublication := publication
				receiptPublication.Revision = test.receiptRevision
				receiptPublication.PublishedAt = publication.PublishedAt.Add(-time.Duration(publication.Revision-test.receiptRevision) * time.Second)
				projectionRecoverySeedExactProjections(t, server, snapshot, receiptPublication, regimeDecisionEventRecorded)
				if test.mutateReceipt != nil {
					receiptPublication = test.mutateReceipt(receiptPublication)
				}
				if err := server.recordRegimeProjectionReceipt(t.Context(), receiptPublication); err != nil {
					t.Fatalf("seed projection receipt: %v", err)
				}
			}

			err = server.reconcileRegimeSnapshotProjections(t.Context(), cache)
			if test.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantErr) {
					t.Fatalf("reconcile error=%v, want containing %q", err, test.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("reconcile projections: %v", err)
			}
			receipt, ok, err := server.loadRegimeProjectionReceipt(t.Context())
			if err != nil || !ok {
				t.Fatalf("load reconciled receipt: ok=%v err=%v", ok, err)
			}
			if receipt.SnapshotRevision != test.wantRevision ||
				!receipt.SnapshotPublishedAt.Equal(publication.PublishedAt) ||
				receipt.SnapshotFingerprint != publication.Fingerprint {
				t.Fatalf("receipt=%+v, publication=%+v", receipt, publication)
			}
		})
	}
}

func TestRegimeProjectionReceiptWithoutSnapshotFailsStartupReconciliation(t *testing.T) {
	store := openRegimeSnapshotTestStore(t)
	publishedAt := regimeSnapshotTestNow()
	snapshot := regimeSnapshotCacheFixture(publishedAt, "orphan receipt")
	server := &Server{coreStore: store, logger: NewLogger(&bytes.Buffer{}, "error")}
	if err := server.recordRegimeProjectionReceipt(t.Context(), regimeSnapshotPublication{
		Revision: 1, PublishedAt: publishedAt, Fingerprint: snapshot.Fingerprint,
	}); err != nil {
		t.Fatalf("seed orphan receipt: %v", err)
	}
	daemonContext, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	cache, err := loadRegimeSnapshotCache(t.Context(), daemonContext, store, regimeSnapshotCacheOptions{
		FreshFor: time.Minute, RefreshTimeout: time.Second, FailureRetryAfter: time.Minute,
	})
	if err != nil {
		t.Fatalf("load cold cache: %v", err)
	}
	if err := server.reconcileRegimeSnapshotProjections(t.Context(), cache); err == nil || !strings.Contains(err.Error(), "without an authoritative snapshot") {
		t.Fatalf("orphan receipt reconciliation error=%v", err)
	}
}

func TestRegimeStreakProjectionRecoveryFrozenAndHiddenLatch(t *testing.T) {
	const (
		priorSince = "2026-07-20"
		resetSince = "2026-07-21"
	)
	publishedAt := time.Date(2026, 7, 21, 15, 0, 0, 0, time.UTC)
	priorAt := publishedAt.Add(-24 * time.Hour)
	publication := regimeSnapshotPublication{Revision: 2, PublishedAt: publishedAt}
	redRatio := 1.08

	tests := []struct {
		name       string
		prior      StreakEntry
		configure  func(*rpc.RegimeSnapshotResult)
		want       StreakEntry
		wantFrozen bool
	}{
		{
			name: "frozen row leaves prior entry untouched",
			prior: StreakEntry{
				LastBand: "red", SinceDate: priorSince, LastSession: priorSince,
				Sessions: 2, LastValue: 1.04, EligibleLatched: true,
			},
			configure:  func(*rpc.RegimeSnapshotResult) {},
			wantFrozen: true,
		},
		{
			name: "newly eligible red earns hidden latch",
			prior: StreakEntry{
				LastBand: "red", SinceDate: priorSince, LastSession: priorSince,
				Sessions: 1, LastValue: 1.02,
			},
			configure: func(snapshot *rpc.RegimeSnapshotResult) {
				snapshot.VIXTermStructure.Ratio = &redRatio
				snapshot.VIXTermStructure.Streak = &rpc.StreakInfo{Band: "red", Sessions: 2, Since: priorSince}
				snapshot.VIXTermStructure.RegimeIndicatorMeta = rpc.RegimeIndicatorMeta{
					Band: "red", Eligibility: &rpc.RegimeEligibility{Eligible: true},
				}
			},
			want: StreakEntry{
				LastBand: "red", SinceDate: priorSince, LastSession: resetSince,
				Sessions: 2, LastValue: redRatio, EligibleLatched: true,
				// The session advanced, so the since-green run advances with it,
				// exactly as the live tick would have written it.
			},
		},
		{
			// Overdue is a freeze: the live tick returned the stored entry
			// untouched, so the projection must reproduce it whole — the value
			// included — rather than bank the snapshot's newer ratio.
			name: "overdue same streak reproduces the frozen entry",
			prior: StreakEntry{
				LastBand: "red", SinceDate: priorSince, LastSession: priorSince,
				Sessions: 2, LastValue: 1.04, EligibleLatched: true,
			},
			configure: func(snapshot *rpc.RegimeSnapshotResult) {
				snapshot.VIXTermStructure.Ratio = &redRatio
				snapshot.VIXTermStructure.Streak = &rpc.StreakInfo{Band: "red", Sessions: 2, Since: priorSince}
				snapshot.VIXTermStructure.RegimeIndicatorMeta = rpc.RegimeIndicatorMeta{
					Band:        "red",
					Freshness:   &rpc.RegimeFreshness{Class: rpc.RegimeFreshnessOverdue},
					Eligibility: &rpc.RegimeEligibility{Eligible: false, Reasons: []string{"data_overdue"}},
				}
			},
			want: StreakEntry{
				LastBand: "red", SinceDate: priorSince, LastSession: priorSince,
				Sessions: 2, LastValue: 1.04, EligibleLatched: true,
			},
		},
		{
			name: "red reset cannot inherit ended latch",
			prior: StreakEntry{
				LastBand: "red", SinceDate: priorSince, LastSession: priorSince,
				Sessions: 4, LastValue: 1.05, EligibleLatched: true,
			},
			configure: func(snapshot *rpc.RegimeSnapshotResult) {
				snapshot.VIXTermStructure.Ratio = &redRatio
				snapshot.VIXTermStructure.Streak = &rpc.StreakInfo{Band: "red", Sessions: 1, Since: resetSince}
				snapshot.VIXTermStructure.RegimeIndicatorMeta = rpc.RegimeIndicatorMeta{
					Band: "red", Eligibility: &rpc.RegimeEligibility{Eligible: false, Reasons: []string{"streak_1_of_2"}},
				}
			},
			want: StreakEntry{
				LastBand: "red", SinceDate: resetSince, LastSession: resetSince,
				Sessions: 1, LastValue: redRatio,
				// Sessions resets with the new red streak; the since-green run
				// deliberately does not, which is why it is a separate clock.
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store := openRegimeSnapshotTestStore(t)
			snapshot := regimeSnapshotCacheFixture(publishedAt, test.name)
			test.configure(snapshot)
			publication.Fingerprint = rpc.BuildRegimeFingerprint(snapshot)
			priorPublication := regimeSnapshotPublication{Revision: 1, PublishedAt: priorAt, Fingerprint: publication.Fingerprint}
			streaks := projectionRecoverySeedStreakStore(t, store, priorPublication, map[string]StreakEntry{
				StreakKeyVIXTerm: test.prior,
			})
			if err := streaks.reconcileRegimeProjection(t.Context(), snapshot, regimeProjectionPlan{
				publication: publication, previous: &priorPublication,
			}); err != nil {
				t.Fatalf("reconcile streak projection: %v", err)
			}
			streaks.mu.Lock()
			got := streaks.entries[StreakKeyVIXTerm]
			gotAsOf := streaks.asOf
			streaks.mu.Unlock()
			if test.wantFrozen {
				if !reflect.DeepEqual(got, test.prior) {
					t.Fatalf("frozen entry=%+v, want unchanged %+v", got, test.prior)
				}
			} else if !reflect.DeepEqual(got, test.want) {
				t.Fatalf("reconciled entry=%+v, want %+v", got, test.want)
			}
			if !gotAsOf.Equal(publishedAt) {
				t.Fatalf("streak projection as_of=%s, want %s", gotAsOf, publishedAt)
			}
		})
	}
}

// TestRegimeStreakProjectionSurvivesNotDueFreezeAcrossRestart pins the live
// writer and the recovery re-derivation to one freeze rule. Tick returns the
// stored entry untouched for a row measured off its own cadence, so the
// committed value stays at the last fresh publication's while the snapshot's
// moves on. Re-deriving that value from the snapshot instead made every
// restart after such a publication refuse to start, with no repair path at the
// current position — vix_term is not_due every evening and overnight while the
// VIX leg keeps moving the ratio, so any graceful restart was a candidate.
func TestRegimeStreakProjectionSurvivesNotDueFreezeAcrossRestart(t *testing.T) {
	ny := newYorkLocation()
	// 01:05 ET is outside VIX3M's dissemination window.
	asOf := time.Date(2026, 7, 20, 1, 5, 0, 0, ny)
	frozenRatio, movedRatio := 0.8632345293811753, 0.8633766233766235
	prior := StreakEntry{
		LastBand: "green", SinceDate: "2026-07-15", LastSession: "2026-07-17",
		Sessions: 3, LastValue: frozenRatio,
	}
	previous := regimeSnapshotPublication{
		Revision: 1, PublishedAt: asOf.Add(-time.Hour).UTC(),
		Fingerprint: rpc.Fingerprint{Version: "test", Key: "prior-publication"},
	}

	store := openRegimeSnapshotTestStore(t)
	snapshot := regimeSnapshotCacheFixture(asOf.UTC(), "not-due-freeze")
	snapshot.VIXTermStructure = rpc.RegimeVIXTerm{
		Status: rpc.RegimeStatusStale, Ratio: &movedRatio,
		VIX3MCrossCheck: rpc.VIX3MCrossCheckAgree,
		VIXQuality:      &rpc.Quality{AsOf: asOf, FreshnessClass: rpc.FreshnessFrozen, Confidence: rpc.ConfidenceFirm},
		VIX3MQuality:    &rpc.Quality{AsOf: asOf, FreshnessClass: rpc.FreshnessFrozen, Confidence: rpc.ConfidenceFirm},
	}
	streaks := projectionRecoverySeedStreakStore(t, store, previous, map[string]StreakEntry{StreakKeyVIXTerm: prior})

	// The live publication path: classify onto a volatile clone, annotate what
	// the snapshot serves, then commit the clone behind the publication barrier.
	evaluated := streaks.cloneForRegimeEvaluation()
	annotateRegimeMetadata(snapshot, (&Server{}).populateStreaksWithStore(snapshot, evaluated))
	snapshot.Fingerprint = rpc.BuildRegimeFingerprint(snapshot)
	publication := regimeSnapshotPublication{Revision: 2, PublishedAt: asOf.UTC(), Fingerprint: snapshot.Fingerprint}
	plan := regimeProjectionPlan{publication: publication, previous: &previous}

	// Guard the fixture: the row must still be not_due and still serve a
	// streak, or the frozen re-derivation is never reached.
	if class := snapshot.VIXTermStructure.Freshness; class == nil || class.Class != rpc.RegimeFreshnessNotDue {
		t.Fatalf("vix_term freshness=%+v, want not_due", class)
	}
	if snapshot.VIXTermStructure.Streak == nil {
		t.Fatal("vix_term served no streak; the frozen row cannot be reconciled")
	}
	if err := streaks.commitRegimeEvaluation(t.Context(), evaluated, plan); err != nil {
		t.Fatalf("commit regime evaluation: %v", err)
	}

	// Restart: a fresh store over the same authority reloads the committed
	// document, and startup reconciles it against the persisted snapshot.
	restarted := NewStreakStore("")
	if err := restarted.UseCoreStore(store); err != nil {
		t.Fatal(err)
	}
	if err := restarted.reconcileRegimeProjection(t.Context(), snapshot, plan); err != nil {
		t.Fatalf("startup reconcile after a not_due freeze: %v", err)
	}
	restarted.mu.Lock()
	got := restarted.entries[StreakKeyVIXTerm]
	restarted.mu.Unlock()
	if !reflect.DeepEqual(got, prior) {
		t.Fatalf("frozen entry=%+v, want unchanged %+v", got, prior)
	}

	// The comparison is intact, not skipped. A frozen row's value has no
	// witness in the snapshot — the writer did not write it — but a fresh row
	// is still re-derived, so a stored value that disagrees with what the
	// snapshot serves fails closed exactly as before.
	fresh := regimeSnapshotCacheFixture(asOf.UTC(), "fresh-row")
	fresh.VIXTermStructure = rpc.RegimeVIXTerm{
		Status: rpc.RegimeStatusOK, Ratio: &movedRatio,
		Streak: &rpc.StreakInfo{Band: prior.LastBand, Sessions: prior.Sessions, Since: prior.SinceDate},
		RegimeIndicatorMeta: rpc.RegimeIndicatorMeta{
			Band: prior.LastBand, Freshness: &rpc.RegimeFreshness{Class: rpc.RegimeFreshnessFresh},
		},
	}
	fresh.Fingerprint = rpc.BuildRegimeFingerprint(fresh)
	freshPublication := regimeSnapshotPublication{Revision: 2, PublishedAt: asOf.UTC(), Fingerprint: fresh.Fingerprint}
	stale := projectionRecoverySeedStreakStore(t, openRegimeSnapshotTestStore(t), freshPublication,
		map[string]StreakEntry{StreakKeyVIXTerm: prior})
	err := stale.reconcileRegimeProjection(t.Context(), fresh, regimeProjectionPlan{publication: freshPublication})
	if err == nil || !strings.Contains(err.Error(), "content mismatch at snapshot revision 2") {
		t.Fatalf("fresh row with a stale stored value err=%v, want a content mismatch", err)
	}
}

// TestRegimeStreakProjectionCarriesFieldsTheSnapshotCannotWitness is the same
// rule as the not-due freeze above, applied to the entry field that never
// reaches the wire at all. LastBandAt is the wall clock of a live measurement,
// so a reloaded snapshot never witnesses it and the recovery re-derivation must
// carry it. Zeroing it made the boot-time compare fail at the current position,
// where there is no repair path, and no binary could start against state a
// running daemon had written.
func TestRegimeStreakProjectionCarriesFieldsTheSnapshotCannotWitness(t *testing.T) {
	ny := newYorkLocation()
	asOf := time.Date(2026, 7, 20, 16, 5, 0, 0, ny)
	ratio := 0.8632345293811753
	// The live writer stamps LastBandAt from its own clock, so it never lands on
	// the snapshot's AsOf to the nanosecond. Re-deriving it cannot reproduce the
	// stored value even in principle.
	measuredAt := asOf.Add(-3 * time.Millisecond)
	// A red row carrying an accumulated stress run is the case that bit: the run
	// survives a band change, so the served streak's own session count cannot
	// recover it.
	stressed := StreakEntry{
		LastBand: "red", SinceDate: "2026-07-15", LastSession: "2026-07-20",
		Sessions: 4, LastValue: ratio, EligibleLatched: true,
		LastBandAt: measuredAt,
	}

	snapshot := regimeSnapshotCacheFixture(asOf.UTC(), "carries-unwitnessed-fields")
	snapshot.VIXTermStructure = rpc.RegimeVIXTerm{
		Status: rpc.RegimeStatusOK, Ratio: &ratio,
		Streak: &rpc.StreakInfo{Band: stressed.LastBand, Sessions: stressed.Sessions, Since: stressed.SinceDate},
		RegimeIndicatorMeta: rpc.RegimeIndicatorMeta{
			Band:        stressed.LastBand,
			Freshness:   &rpc.RegimeFreshness{Class: rpc.RegimeFreshnessFresh},
			Eligibility: &rpc.RegimeEligibility{Eligible: true},
		},
	}
	snapshot.Fingerprint = rpc.BuildRegimeFingerprint(snapshot)
	publication := regimeSnapshotPublication{Revision: 2, PublishedAt: asOf.UTC(), Fingerprint: snapshot.Fingerprint}

	seeded := map[string]StreakEntry{StreakKeyVIXTerm: stressed}
	streaks := projectionRecoverySeedStreakStore(t, openRegimeSnapshotTestStore(t), publication, seeded)
	if err := streaks.reconcileRegimeProjection(t.Context(), snapshot, regimeProjectionPlan{publication: publication}); err != nil {
		t.Fatalf("startup reconcile at the current position: %v", err)
	}
	streaks.mu.Lock()
	got := cloneStreakEntries(streaks.entries)
	streaks.mu.Unlock()
	for key, want := range seeded {
		if !reflect.DeepEqual(got[key], want) {
			t.Fatalf("%s after reconcile=%+v, want unchanged %+v", key, got[key], want)
		}
	}

}

func TestRegimeRuleProjectionDeterministicAndHolds(t *testing.T) {
	publishedAt := time.Date(2026, 7, 21, 15, 5, 0, 0, time.UTC)
	prior := rulesRegimeStageState{
		Version: rulesRegimeStageStateVer,
		Bucket:  risk.RegimeBucketCalm,
		Stage:   rpc.LifecycleQuiet,
		AsOf:    publishedAt.Add(-time.Hour),
	}

	tests := []struct {
		name      string
		configure func(*rpc.RegimeSnapshotResult)
		want      rulesRegimeStageState
	}{
		{
			name: "closed date holds prior latch and age",
			configure: func(snapshot *rpc.RegimeSnapshotResult) {
				snapshot.TapeSessionState = rpc.TapeSessionClosedDate
				snapshot.Lifecycle.Stage = rpc.LifecyclePanic
			},
			want: prior,
		},
		{
			name: "data quality holds prior latch and age",
			configure: func(snapshot *rpc.RegimeSnapshotResult) {
				snapshot.Lifecycle.Stage = rpc.LifecycleDataQuality
			},
			want: prior,
		},
		{
			name: "trading date projects exact publication time",
			configure: func(snapshot *rpc.RegimeSnapshotResult) {
				snapshot.TapeSessionState = rpc.TapeSessionTradingDate
				snapshot.Lifecycle.Stage = rpc.LifecycleConfirmedStress
			},
			want: rulesRegimeStageState{
				Version: rulesRegimeStageStateVer,
				Bucket:  risk.RegimeBucketConfirmed,
				Stage:   rpc.LifecycleConfirmedStress,
				AsOf:    publishedAt,
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store := openRegimeSnapshotTestStore(t)
			server, beforeRevision := projectionRecoveryServerWithRuleState(t, store, prior)
			snapshot := regimeSnapshotCacheFixture(publishedAt, test.name)
			test.configure(snapshot)
			publication := regimeSnapshotPublication{
				Revision: 1, PublishedAt: publishedAt, Fingerprint: rpc.BuildRegimeFingerprint(snapshot),
			}
			if err := server.reconcileRulesRegimeStageProjection(t.Context(), snapshot, regimeProjectionPlan{publication: publication, initial: true}); err != nil {
				t.Fatalf("reconcile rule projection: %v", err)
			}
			server.rulesRegimeStageMu.Lock()
			visibleBeforeReceipt := server.rulesRegimeStage
			server.rulesRegimeStageMu.Unlock()
			if visibleBeforeReceipt != prior {
				t.Fatalf("unreceipted rule projection became visible: %+v", visibleBeforeReceipt)
			}
			doc, ok, err := store.GetStateDocument(t.Context(), daemonStateScope, stateKindRulesRegimeStage)
			if err != nil || !ok {
				t.Fatalf("load rule projection: ok=%v err=%v", ok, err)
			}
			wantRevision := beforeRevision + 1
			if doc.Revision != wantRevision {
				t.Fatalf("rule state revision=%d, want %d", doc.Revision, wantRevision)
			}
			durable, err := decodeRulesRegimeStageState(doc.JSON)
			if err != nil {
				t.Fatal(err)
			}
			if durable.Bucket != test.want.Bucket || durable.Stage != test.want.Stage || !durable.AsOf.Equal(test.want.AsOf) ||
				!exactRegimeSnapshotPublication(durable.publication, publication) {
				t.Fatalf("durable rule projection=%+v, want semantics=%+v publication=%+v", durable, test.want, publication)
			}

			if err := server.reconcileRulesRegimeStageProjection(t.Context(), snapshot, regimeProjectionPlan{publication: publication, initial: true}); err != nil {
				t.Fatalf("repeat rule reconciliation: %v", err)
			}
			repeated, _, err := store.GetStateDocument(t.Context(), daemonStateScope, stateKindRulesRegimeStage)
			if err != nil {
				t.Fatal(err)
			}
			if repeated.Revision != wantRevision {
				t.Fatalf("repeat reconciliation advanced revision to %d, want %d", repeated.Revision, wantRevision)
			}
			if err := server.recordRegimeProjectionReceipt(t.Context(), publication); err != nil {
				t.Fatal(err)
			}
			if err := server.publishRulesRegimeStageProjection(t.Context(), publication); err != nil {
				t.Fatal(err)
			}
			server.rulesRegimeStageMu.Lock()
			got := server.rulesRegimeStage
			server.rulesRegimeStageMu.Unlock()
			if got.Bucket != test.want.Bucket || got.Stage != test.want.Stage || !got.AsOf.Equal(test.want.AsOf) ||
				!exactRegimeSnapshotPublication(got.publication, publication) {
				t.Fatalf("published rule projection=%+v, want %+v", got, test.want)
			}
		})
	}
}

func TestRegimeDecisionProjectionStableReplayDoesNotDuplicate(t *testing.T) {
	store := openRegimeSnapshotTestStore(t)
	server := &Server{
		coreStore:       store,
		regimeDecisions: &regimeDecisionJournal{core: store},
		logger:          NewLogger(&bytes.Buffer{}, "error"),
	}
	publishedAt := time.Date(2026, 7, 21, 15, 10, 0, 0, time.UTC)
	snapshot := regimeSnapshotCacheFixture(publishedAt, "stable journal replay")
	snapshot.Lifecycle.Stage = rpc.LifecycleEarlyWarning
	snapshot.Fingerprint = rpc.BuildRegimeFingerprint(snapshot)
	publication := regimeSnapshotPublication{
		Revision: 7, PublishedAt: publishedAt, Fingerprint: snapshot.Fingerprint,
	}

	if _, err := server.reconcileRegimeDecisionProjection(t.Context(), snapshot, regimeProjectionPlan{publication: publication, initial: true}); err != nil {
		t.Fatalf("first journal reconciliation: %v", err)
	}
	events, err := loadAllCoreEvents(t.Context(), store, coreEventRegimeDecision)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 {
		t.Fatalf("events=%d, want one", len(events))
	}
	wantKey := "regime_decision:snapshot:00000000000000000007"
	if events[0].EventKey != wantKey || !events[0].OccurredAt.Equal(publishedAt) {
		t.Fatalf("event key/time=%q %s, want %q %s", events[0].EventKey, events[0].OccurredAt, wantKey, publishedAt)
	}
	var line regimeDecisionLine
	if err := json.Unmarshal(events[0].PayloadJSON, &line); err != nil {
		t.Fatal(err)
	}
	if line.SnapshotRevision != publication.Revision || line.Fingerprint != snapshot.Fingerprint.Key || !line.TS.Equal(publishedAt) {
		t.Fatalf("decision line=%+v", line)
	}

	// Simulate a crash after SQLite committed but before process-local dedupe
	// fields could be trusted. Recovery must find the durable revision key.
	server.regimeDecisions.mu.Lock()
	server.regimeDecisions.lastFingerprint = ""
	server.regimeDecisions.lastWrite = time.Time{}
	server.regimeDecisions.mu.Unlock()
	if _, err := server.reconcileRegimeDecisionProjection(t.Context(), snapshot, regimeProjectionPlan{publication: publication, initial: true}); err != nil {
		t.Fatalf("replayed journal reconciliation: %v", err)
	}
	events, err = loadAllCoreEvents(t.Context(), store, coreEventRegimeDecision)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 {
		t.Fatalf("replay produced %d events, want one", len(events))
	}
}

func TestRegimeDecisionProjectionSkipsEqualFingerprintsUntilHourlyHeartbeat(t *testing.T) {
	store := openRegimeSnapshotTestStore(t)
	server := &Server{
		coreStore:       store,
		regimeDecisions: &regimeDecisionJournal{core: store},
		logger:          NewLogger(&bytes.Buffer{}, "error"),
	}
	publishedAt := time.Date(2026, 7, 21, 15, 10, 0, 0, time.UTC)
	snapshot := regimeSnapshotCacheFixture(publishedAt, "unchanged decision")
	snapshot.Lifecycle.Stage = rpc.LifecycleEarlyWarning
	snapshot.Fingerprint = rpc.BuildRegimeFingerprint(snapshot)

	var (
		previous            *regimeSnapshotPublication
		previousDisposition string
		dispositions        []string
	)
	publications := []regimeSnapshotPublication{
		{Revision: 7, PublishedAt: publishedAt, Fingerprint: snapshot.Fingerprint},
		{Revision: 8, PublishedAt: publishedAt.Add(time.Second), Fingerprint: snapshot.Fingerprint},
		{Revision: 9, PublishedAt: publishedAt.Add(regimeDecisionHeartbeat), Fingerprint: snapshot.Fingerprint},
	}
	for _, publication := range publications {
		plan := regimeProjectionPlan{publication: publication, initial: publication.Revision == 7, previous: previous}
		if previous != nil {
			plan.previousDecision = previousDisposition
		}
		disposition, err := server.reconcileRegimeDecisionProjection(t.Context(), snapshot, plan)
		if err != nil {
			t.Fatalf("reconcile revision %d: %v", publication.Revision, err)
		}
		dispositions = append(dispositions, disposition)
		publicationCopy := publication
		previous = &publicationCopy
		previousDisposition = disposition
	}
	if got, want := fmt.Sprint(dispositions), fmt.Sprint([]string{
		regimeDecisionEventRecorded, regimeDecisionEventSkippedUnchanged, regimeDecisionEventRecorded,
	}); got != want {
		t.Fatalf("dispositions=%s, want %s", got, want)
	}

	events, err := loadAllCoreEvents(t.Context(), store, coreEventRegimeDecision)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 {
		t.Fatalf("events=%d, want initial transition plus hourly heartbeat", len(events))
	}
	for index, wantRevision := range []int64{7, 9} {
		event := events[index]
		wantKey := fmt.Sprintf("regime_decision:snapshot:%020d", wantRevision)
		if event.EventKey != wantKey {
			t.Fatalf("event %d key=%q, want %q", index, event.EventKey, wantKey)
		}
		var line regimeDecisionLine
		if err := json.Unmarshal(event.PayloadJSON, &line); err != nil {
			t.Fatal(err)
		}
		if line.SnapshotRevision != wantRevision || line.Fingerprint != snapshot.Fingerprint.Key {
			t.Fatalf("event %d line=%+v", index, line)
		}
	}
}

func TestRegimeDecisionSkippedDispositionRequiresDurableHeartbeatAnchor(t *testing.T) {
	publishedAt := time.Date(2026, 7, 21, 15, 10, 0, 0, time.UTC)
	snapshot := regimeSnapshotCacheFixture(publishedAt, "skipped recovery")
	snapshot.Lifecycle.Stage = rpc.LifecycleEarlyWarning
	snapshot.Fingerprint = rpc.BuildRegimeFingerprint(snapshot)
	publication := regimeSnapshotPublication{Revision: 2, PublishedAt: publishedAt, Fingerprint: snapshot.Fingerprint}
	store := openRegimeSnapshotTestStore(t)
	server := &Server{coreStore: store, regimeDecisions: &regimeDecisionJournal{core: store}, logger: NewLogger(&bytes.Buffer{}, "error")}
	if err := server.persistRegimeDecisionProjectionState(
		t.Context(), corestore.StateDocument{}, false, publication, regimeDecisionEventSkippedUnchanged,
	); err != nil {
		t.Fatal(err)
	}
	_, err := server.reconcileRegimeDecisionProjection(t.Context(), snapshot, regimeProjectionPlan{
		publication: publication,
		receipt: regimeProjectionReceipt{
			Version: regimeProjectionReceiptVersion, SnapshotRevision: publication.Revision,
			SnapshotPublishedAt: publication.PublishedAt, SnapshotFingerprint: publication.Fingerprint,
			DecisionEvent: regimeDecisionEventSkippedUnchanged,
		},
		receiptOK: true, validateOnly: true,
	})
	if err == nil || !strings.Contains(err.Error(), "no current heartbeat anchor") {
		t.Fatalf("skipped projection without anchor error=%v", err)
	}
}

func TestRegimeStreakProjectionRejectsDivergentEqualAndAheadPublication(t *testing.T) {
	publishedAt := time.Date(2026, 7, 20, 15, 0, 0, 0, time.UTC)
	snapshot := regimeSnapshotCacheFixture(publishedAt, "streak tuple mismatch")
	snapshot.Fingerprint = rpc.BuildRegimeFingerprint(snapshot)
	want := regimeSnapshotPublication{Revision: 2, PublishedAt: publishedAt, Fingerprint: snapshot.Fingerprint}

	tests := []struct {
		name    string
		stored  regimeSnapshotPublication
		wantErr string
	}{
		{name: "equal revision wrong timestamp", stored: regimeSnapshotPublication{Revision: 2, PublishedAt: publishedAt.Add(-time.Second), Fingerprint: snapshot.Fingerprint}, wantErr: "diverges"},
		{name: "ahead revision even with older timestamp", stored: regimeSnapshotPublication{Revision: 3, PublishedAt: publishedAt.Add(-time.Hour), Fingerprint: snapshot.Fingerprint}, wantErr: "ahead"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store := openRegimeSnapshotTestStore(t)
			streaks := projectionRecoverySeedStreakStore(t, store, test.stored, nil)
			err := streaks.reconcileRegimeProjection(t.Context(), snapshot, regimeProjectionPlan{publication: want})
			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("reconcile error=%v, want containing %q", err, test.wantErr)
			}
		})
	}
}

func TestRegimeStreakProjectionPersistsUnchangedEntriesAcrossRevisionAndRestart(t *testing.T) {
	priorAt := time.Date(2026, 7, 20, 15, 0, 0, 0, time.UTC)
	currentAt := priorAt.Add(time.Minute)
	ratio := 0.95
	snapshot := regimeSnapshotCacheFixture(currentAt, "unchanged streak")
	snapshot.VIXTermStructure.Ratio = &ratio
	snapshot.VIXTermStructure.Streak = &rpc.StreakInfo{Band: "yellow", Sessions: 1, Since: "2026-07-20"}
	snapshot.VIXTermStructure.RegimeIndicatorMeta.Band = "yellow"
	snapshot.Fingerprint = rpc.BuildRegimeFingerprint(snapshot)
	prior := regimeSnapshotPublication{Revision: 1, PublishedAt: priorAt, Fingerprint: snapshot.Fingerprint}
	current := regimeSnapshotPublication{Revision: 2, PublishedAt: currentAt, Fingerprint: snapshot.Fingerprint}
	entry := StreakEntry{LastBand: "yellow", SinceDate: "2026-07-20", LastSession: "2026-07-20", Sessions: 1, LastValue: ratio}
	store := openRegimeSnapshotTestStore(t)
	streaks := projectionRecoverySeedStreakStore(t, store, prior, map[string]StreakEntry{StreakKeyVIXTerm: entry})
	if err := streaks.reconcileRegimeProjection(t.Context(), snapshot, regimeProjectionPlan{publication: current, previous: &prior}); err != nil {
		t.Fatal(err)
	}

	restarted := NewStreakStore("")
	if err := restarted.UseCoreStore(store); err != nil {
		t.Fatal(err)
	}
	restarted.mu.Lock()
	restarted.loadLocked()
	gotPublication := restarted.publication
	gotEntry := restarted.entries[StreakKeyVIXTerm]
	restarted.mu.Unlock()
	if !exactRegimeSnapshotPublication(gotPublication, current) || gotEntry != entry {
		t.Fatalf("restart projection publication=%+v entry=%+v, want %+v %+v", gotPublication, gotEntry, current, entry)
	}
}

func TestRegimeDecisionProjectionRejectsWrongPublicationTimeAndFingerprintVersion(t *testing.T) {
	publishedAt := time.Date(2026, 7, 20, 15, 10, 0, 0, time.UTC)
	snapshot := regimeSnapshotCacheFixture(publishedAt, "decision tuple mismatch")
	snapshot.Lifecycle.Stage = rpc.LifecycleQuiet
	snapshot.Fingerprint = rpc.BuildRegimeFingerprint(snapshot)
	publication := regimeSnapshotPublication{Revision: 1, PublishedAt: publishedAt, Fingerprint: snapshot.Fingerprint}

	tests := []struct {
		name   string
		mutate func(*regimeDecisionLine)
	}{
		{name: "wrong event time", mutate: func(line *regimeDecisionLine) { line.TS = line.TS.Add(-time.Second) }},
		{name: "wrong fingerprint version", mutate: func(line *regimeDecisionLine) { line.SnapshotFingerprint.Version = "wrong-version" }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store := openRegimeSnapshotTestStore(t)
			line := buildRegimeDecisionLine(publishedAt, snapshot, publication)
			test.mutate(&line)
			raw, err := json.Marshal(line)
			if err != nil {
				t.Fatal(err)
			}
			key := fmt.Sprintf("%s:snapshot:%020d", coreEventRegimeDecision, publication.Revision)
			if _, err := store.AppendEvents(t.Context(), []corestore.EventInput{{
				ScopeKey: daemonStateScope, EventKey: key, Type: coreEventRegimeDecision,
				Action: coreEventActionRecord, Origin: coreEventOriginDaemon,
				OccurredAt: publishedAt, PayloadJSON: raw,
			}}); err != nil {
				t.Fatal(err)
			}
			server := &Server{coreStore: store, regimeDecisions: &regimeDecisionJournal{core: store}, logger: NewLogger(&bytes.Buffer{}, "error")}
			if _, err := server.reconcileRegimeDecisionProjection(t.Context(), snapshot, regimeProjectionPlan{publication: publication, initial: true}); err == nil {
				t.Fatal("wrong exact decision tuple was accepted")
			}
		})
	}
}

// TestRegimeDecisionProjectionToleratesPriorCurrencyPolicyLine pins the
// upgrade boot: a retained decision line written before the input-currency
// cutover carries no currency_policy marker and legitimately differs from a
// recompute under the current policy. Its key, publication time and
// fingerprint still bind it to the publication, so reconciliation must accept
// it as prior-policy history instead of refusing to boot — while the same
// content divergence under the current policy stays a hard failure.
func TestRegimeDecisionProjectionToleratesPriorCurrencyPolicyLine(t *testing.T) {
	publishedAt := time.Date(2026, 7, 20, 15, 10, 0, 0, time.UTC)
	snapshot := regimeSnapshotCacheFixture(publishedAt, "prior-policy decision line")
	snapshot.Lifecycle.Stage = rpc.LifecycleQuiet
	depth := 42.0
	snapshot.FundingStress.SpreadBps = &depth
	snapshot.Fingerprint = rpc.BuildRegimeFingerprint(snapshot)
	publication := regimeSnapshotPublication{Revision: 1, PublishedAt: publishedAt, Fingerprint: snapshot.Fingerprint}

	tests := []struct {
		name    string
		mutate  func(*regimeDecisionLine)
		wantErr bool
	}{
		{name: "prior policy with divergent content is history", mutate: func(line *regimeDecisionLine) {
			line.CurrencyPolicy = ""
			line.Stage = rpc.LifecycleEarlyWarning
		}},
		{name: "current policy with divergent content still fails", mutate: func(line *regimeDecisionLine) {
			line.Stage = rpc.LifecycleEarlyWarning
		}, wantErr: true},
		{name: "v2 line may predate a newly added depth", mutate: func(line *regimeDecisionLine) {
			line.V = 2
			indicator := line.Indicators[StreakKeyFunding]
			indicator.Depth = nil
			line.Indicators[StreakKeyFunding] = indicator
		}},
		{name: "v2 line keeps its rendered depth as immutable history", mutate: func(line *regimeDecisionLine) {
			line.V = 2
			wrong := 41.0
			indicator := line.Indicators[StreakKeyFunding]
			indicator.Depth = &wrong
			line.Indicators[StreakKeyFunding] = indicator
		}},
		{name: "v3 line cannot omit a current depth", mutate: func(line *regimeDecisionLine) {
			indicator := line.Indicators[StreakKeyFunding]
			indicator.Depth = nil
			line.Indicators[StreakKeyFunding] = indicator
		}, wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store := openRegimeSnapshotTestStore(t)
			line := buildRegimeDecisionLine(publishedAt, snapshot, publication)
			test.mutate(&line)
			raw, err := json.Marshal(line)
			if err != nil {
				t.Fatal(err)
			}
			key := fmt.Sprintf("%s:snapshot:%020d", coreEventRegimeDecision, publication.Revision)
			if _, err := store.AppendEvents(t.Context(), []corestore.EventInput{{
				ScopeKey: daemonStateScope, EventKey: key, Type: coreEventRegimeDecision,
				Action: coreEventActionRecord, Origin: coreEventOriginDaemon,
				OccurredAt: publishedAt, PayloadJSON: raw,
			}}); err != nil {
				t.Fatal(err)
			}
			server := &Server{coreStore: store, regimeDecisions: &regimeDecisionJournal{core: store}, logger: NewLogger(&bytes.Buffer{}, "error")}
			_, err = server.reconcileRegimeDecisionProjection(t.Context(), snapshot, regimeProjectionPlan{publication: publication, initial: true})
			if test.wantErr && err == nil {
				t.Fatal("divergent content under the current policy was accepted")
			}
			if !test.wantErr && err != nil {
				t.Fatalf("prior-policy line refused: %v", err)
			}
		})
	}
}

func TestRegimeReceiptMatchValidatesEveryProjection(t *testing.T) {
	publishedAt := time.Date(2026, 7, 20, 15, 20, 0, 0, time.UTC)
	snapshot := regimeSnapshotCacheFixture(publishedAt, "false completion receipt")
	snapshot.Lifecycle.Stage = rpc.LifecycleQuiet
	snapshot.Fingerprint = rpc.BuildRegimeFingerprint(snapshot)

	tests := []struct {
		name string
		seed func(*testing.T, *Server, *rpc.RegimeSnapshotResult, regimeSnapshotPublication)
		want string
	}{
		{name: "missing streak projection", seed: func(*testing.T, *Server, *rpc.RegimeSnapshotResult, regimeSnapshotPublication) {}, want: "streak projection metadata is missing"},
		{name: "missing rule projection", seed: func(t *testing.T, server *Server, _ *rpc.RegimeSnapshotResult, publication regimeSnapshotPublication) {
			server.streaks = projectionRecoverySeedStreakStore(t, server.coreStore, publication, nil)
		}, want: "rules regime stage projection metadata is missing"},
		{name: "missing decision projection", seed: func(t *testing.T, server *Server, snapshot *rpc.RegimeSnapshotResult, publication regimeSnapshotPublication) {
			server.streaks = projectionRecoverySeedStreakStore(t, server.coreStore, publication, nil)
			projectionRecoverySeedRuleProjection(t, server.coreStore, snapshot, publication)
		}, want: "decision projection marker is missing"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store := openRegimeSnapshotTestStore(t)
			cache := projectionRecoveryPersistSnapshot(t, store, snapshot, 1)
			publication, _, err := cache.publication()
			if err != nil {
				t.Fatal(err)
			}
			server := &Server{coreStore: store, logger: NewLogger(&bytes.Buffer{}, "error")}
			test.seed(t, server, snapshot, publication)
			if err := server.recordRegimeProjectionReceipt(t.Context(), publication); err != nil {
				t.Fatal(err)
			}
			err = server.reconcileRegimeSnapshotProjections(t.Context(), cache)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("reconcile error=%v, want containing %q", err, test.want)
			}
		})
	}
}

func TestRegimeDecisionDisabledDispositionSurvivesSettingChange(t *testing.T) {
	store := openRegimeSnapshotTestStore(t)
	disabled := false
	enabled := true
	server := &Server{
		coreStore: store, logger: NewLogger(&bytes.Buffer{}, "error"),
		platformSettings: &platformSettingsStore{data: platformSettingsData{Version: platformSettingsDocVersion, Regime: platformRegimeSettingsData{
			Journal: platformRegimeJournalSettingsData{Enabled: &disabled},
		}}},
	}
	publishedAt := time.Now().UTC().Add(-2 * time.Minute)
	snapshot := regimeSnapshotCacheFixture(publishedAt, "disabled decision")
	snapshot.Lifecycle.Stage = rpc.LifecycleQuiet
	snapshot.Fingerprint = rpc.BuildRegimeFingerprint(snapshot)
	publication := regimeSnapshotPublication{Revision: 1, PublishedAt: publishedAt, Fingerprint: snapshot.Fingerprint}
	if err := server.commitRegimeSnapshotProjections(t.Context(), snapshot, nil, publication); err != nil {
		t.Fatal(err)
	}
	receipt, ok, err := server.loadRegimeProjectionReceipt(t.Context())
	if err != nil || !ok || receipt.DecisionEvent != regimeDecisionEventDisabled {
		t.Fatalf("disabled receipt=%+v ok=%v err=%v", receipt, ok, err)
	}
	if events, err := loadAllCoreEvents(t.Context(), store, coreEventRegimeDecision); err != nil || len(events) != 0 {
		t.Fatalf("disabled decision events=%d err=%v", len(events), err)
	}
	server.platformSettings.mu.Lock()
	server.platformSettings.data.Regime.Journal.Enabled = &enabled
	server.platformSettings.mu.Unlock()
	plan, err := server.prepareRegimeProjectionPlan(t.Context(), publication)
	if err != nil {
		t.Fatal(err)
	}
	if disposition, err := server.reconcileRegimeDecisionProjection(t.Context(), snapshot, plan); err != nil || disposition != regimeDecisionEventDisabled {
		t.Fatalf("setting change invalidated disabled projection: disposition=%q err=%v", disposition, err)
	}
	second := regimeSnapshotCacheFixture(publishedAt.Add(time.Minute), "partial disabled decision")
	second.Lifecycle.Stage = rpc.LifecycleQuiet
	second.Fingerprint = rpc.BuildRegimeFingerprint(second)
	secondPublication := regimeSnapshotPublication{Revision: 2, PublishedAt: publishedAt.Add(time.Minute), Fingerprint: second.Fingerprint}
	_, markerOK, markerDoc, err := server.loadRegimeDecisionProjectionState(t.Context())
	if err != nil || !markerOK {
		t.Fatalf("load first decision marker: ok=%v err=%v", markerOK, err)
	}
	if err := server.persistRegimeDecisionProjectionState(t.Context(), markerDoc, true, secondPublication, regimeDecisionEventDisabled); err != nil {
		t.Fatal(err)
	}
	partialPlan := regimeProjectionPlan{
		publication: secondPublication, previous: &publication, previousDecision: regimeDecisionEventDisabled,
	}
	if disposition, err := server.reconcileRegimeDecisionProjection(t.Context(), second, partialPlan); err != nil || disposition != regimeDecisionEventDisabled {
		t.Fatalf("partial disabled marker did not survive setting change: disposition=%q err=%v", disposition, err)
	}
}

func TestRulesRegimeStageVisibilityWaitsForReceiptAndSurvivesDecisionFailure(t *testing.T) {
	store := openRegimeSnapshotTestStore(t)
	server := &Server{coreStore: store, logger: NewLogger(&bytes.Buffer{}, "error")}
	firstAt := time.Now().UTC().Add(-3 * time.Minute)
	first := regimeSnapshotCacheFixture(firstAt, "receipted stress")
	first.Lifecycle.Stage = rpc.LifecycleConfirmedStress
	first.Fingerprint = rpc.BuildRegimeFingerprint(first)
	firstPublication := regimeSnapshotPublication{Revision: 1, PublishedAt: firstAt, Fingerprint: first.Fingerprint}
	if err := server.commitRegimeSnapshotProjections(t.Context(), first, nil, firstPublication); err != nil {
		t.Fatal(err)
	}
	if got := server.rulesRegimeStageSnapshot(); got.Bucket != risk.RegimeBucketConfirmed {
		t.Fatalf("first receipted latch=%+v", got)
	}

	secondAt := firstAt.Add(time.Minute)
	second := regimeSnapshotCacheFixture(secondAt, "unreceipted calm")
	second.Lifecycle.Stage = rpc.LifecycleQuiet
	second.Fingerprint = rpc.BuildRegimeFingerprint(second)
	secondPublication := regimeSnapshotPublication{Revision: 2, PublishedAt: secondAt, Fingerprint: second.Fingerprint}
	server.regimeDecisions.mu.Lock()
	done := make(chan error, 1)
	go func() {
		done <- server.commitRegimeSnapshotProjections(context.Background(), second, nil, secondPublication)
	}()
	deadline := time.Now().Add(3 * time.Second)
	for {
		doc, ok, err := store.GetStateDocument(t.Context(), daemonStateScope, stateKindRulesRegimeStage)
		if err == nil && ok {
			state, decodeErr := decodeRulesRegimeStageState(doc.JSON)
			if decodeErr == nil && exactRegimeSnapshotPublication(state.publication, secondPublication) {
				if state.Bucket != risk.RegimeBucketCalm {
					t.Fatalf("staged rule bucket=%q, want calm", state.Bucket)
				}
				break
			}
		}
		if time.Now().After(deadline) {
			server.regimeDecisions.mu.Unlock()
			t.Fatal("timed out waiting for staged rule projection")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got := server.rulesRegimeStageSnapshot(); got.Bucket != risk.RegimeBucketConfirmed {
		server.regimeDecisions.mu.Unlock()
		t.Fatalf("unreceipted callback window relaxed rulebook: %+v", got)
	}
	if err := store.Close(); err != nil {
		server.regimeDecisions.mu.Unlock()
		t.Fatal(err)
	}
	server.regimeDecisions.mu.Unlock()
	if err := <-done; err == nil {
		t.Fatal("closed decision authority did not fail the projection callback")
	}
	if got := server.rulesRegimeStageSnapshot(); got.Bucket != risk.RegimeBucketConfirmed {
		t.Fatalf("failed projection relaxed in-memory rulebook: %+v", got)
	}
}

func TestRulesRegimeStageFutureIdentityFailsClosedWithoutLosingEvidence(t *testing.T) {
	future := time.Now().UTC().Add(time.Hour)
	state := rulesRegimeStageState{
		Version: rulesRegimeStageStateVer, Bucket: risk.RegimeBucketCalm, Stage: rpc.LifecycleQuiet,
		AsOf: future, publication: regimeSnapshotPublication{
			Revision: 2, PublishedAt: future, Fingerprint: rpc.Fingerprint{Version: "v1", Key: "sha256:future"},
		},
	}
	server := &Server{rulesRegimeStage: state, rulesRegimeStageLoaded: true}
	got := server.rulesRegimeStageSnapshot()
	if got.Bucket != risk.RegimeBucketConfirmed || got.Stage != "projection_pending_or_clock_invalid" || !got.AsOf.IsZero() {
		t.Fatalf("future latch did not fail closed: %+v", got)
	}
	server.rulesRegimeStageMu.Lock()
	preserved := server.rulesRegimeStage
	server.rulesRegimeStageMu.Unlock()
	if !equalRulesRegimeStageState(preserved, state) {
		t.Fatalf("future evidence was mutated: got %+v want %+v", preserved, state)
	}
}

func TestRegimeProjectionHigherRevisionAcceptsLowerPublicationTime(t *testing.T) {
	store := openRegimeSnapshotTestStore(t)
	snapshot := regimeSnapshotCacheFixture(time.Now().UTC(), "clock rollback revision")
	snapshot.Fingerprint = rpc.BuildRegimeFingerprint(snapshot)
	prior := regimeSnapshotPublication{Revision: 1, PublishedAt: time.Now().UTC(), Fingerprint: snapshot.Fingerprint}
	current := regimeSnapshotPublication{Revision: 2, PublishedAt: prior.PublishedAt.Add(-time.Minute), Fingerprint: snapshot.Fingerprint}
	streaks := projectionRecoverySeedStreakStore(t, store, prior, nil)
	if err := streaks.reconcileRegimeProjection(t.Context(), snapshot, regimeProjectionPlan{publication: current, previous: &prior}); err != nil {
		t.Fatalf("higher revision with lower publication time was rejected: %v", err)
	}
}

func TestRegimeProjectionCallbackFailureBlocksNextSnapshotCAS(t *testing.T) {
	store := openRegimeSnapshotTestStore(t)
	daemonContext, cancelDaemon := context.WithCancel(context.Background())
	t.Cleanup(cancelDaemon)
	firstAt := regimeSnapshotTestNow()
	clock := &regimeSnapshotTestClock{now: firstAt}
	cache := newRegimeSnapshotTestCache(t, store, daemonContext, clock)
	projectionErr := errors.New("injected projection failure")

	view, err := cache.serve(t.Context(), func(context.Context) (*rpc.RegimeSnapshotResult, bool, regimeSnapshotAfterPublishFunc, error) {
		return regimeSnapshotCacheFixture(firstAt, "revision one"), true,
			func(context.Context, regimeSnapshotPublication) error { return projectionErr }, nil
	})
	var unavailable *regimeSnapshotCacheUnavailableError
	if !errors.As(err, &unavailable) || !errors.Is(unavailable, projectionErr) {
		t.Fatalf("first serve error=%v, want wrapped projection failure", err)
	}
	if view.Revision != 1 || view.Snapshot == nil {
		t.Fatalf("committed first view=%+v", view)
	}
	pending, revision := cache.projectionFailure()
	if !pending || revision != 1 {
		t.Fatalf("projection pending=%v revision=%d, want true/1", pending, revision)
	}

	clock.Set(firstAt.Add(10 * time.Minute))
	var nextRefreshCalls atomic.Int32
	served, err := cache.serve(t.Context(), func(context.Context) (*rpc.RegimeSnapshotResult, bool, regimeSnapshotAfterPublishFunc, error) {
		nextRefreshCalls.Add(1)
		return regimeSnapshotCacheFixture(clock.Now(), "must not publish revision two"), true, nil, nil
	})
	if err == nil {
		t.Fatal("pending projection should withhold the last-good snapshot")
	}
	if got := nextRefreshCalls.Load(); got != 0 {
		t.Fatalf("N+1 refresh calls=%d, want zero while projection receipt is pending", got)
	}
	if cache.refreshing() || served.Revision != 1 || served.Snapshot == nil || served.Snapshot.Summary.Label != "revision one" {
		t.Fatalf("forensic view while pending=%+v refreshing=%v", served, cache.refreshing())
	}
	doc, ok, err := store.GetStateDocument(t.Context(), daemonStateScope, regimeSnapshotStateKind)
	if err != nil || !ok || doc.Revision != 1 {
		t.Fatalf("snapshot document after blocked N+1: ok=%v revision=%d err=%v", ok, doc.Revision, err)
	}
}

func projectionRecoveryPersistSnapshot(t *testing.T, store *corestore.Store, snapshot *rpc.RegimeSnapshotResult, revisions int) *regimeSnapshotCache {
	t.Helper()
	raw, _, err := encodeRegimeSnapshotDocument(snapshot)
	if err != nil {
		t.Fatalf("encode snapshot: %v", err)
	}
	var saved corestore.StateDocument
	for revision := range revisions {
		saved, err = store.CompareAndSwapStateDocument(t.Context(), corestore.StateDocumentCAS{
			ScopeKey: daemonStateScope, Kind: regimeSnapshotStateKind,
			ExpectedRevision: int64(revision), JSON: raw,
		})
		if err != nil {
			t.Fatalf("persist snapshot revision %d: %v", revision+1, err)
		}
	}
	daemonContext, cancelDaemon := context.WithCancel(context.Background())
	t.Cleanup(cancelDaemon)
	cache, err := loadRegimeSnapshotCache(t.Context(), daemonContext, store, regimeSnapshotCacheOptions{
		FreshFor: time.Minute, RefreshTimeout: time.Second, FailureRetryAfter: time.Minute,
		Now: func() time.Time { return saved.UpdatedAt.Add(time.Second) },
	})
	if err != nil {
		t.Fatalf("load snapshot cache: %v", err)
	}
	return cache
}

func projectionRecoverySeedStreakStore(t *testing.T, store *corestore.Store, publication regimeSnapshotPublication, entries map[string]StreakEntry) *StreakStore {
	t.Helper()
	streaks := NewStreakStore("")
	if err := streaks.UseCoreStore(store); err != nil {
		t.Fatal(err)
	}
	streaks.mu.Lock()
	streaks.entries = cloneStreakEntries(entries)
	streaks.loaded = true
	err := streaks.saveLockedContextPublication(t.Context(), publication)
	streaks.mu.Unlock()
	if err != nil {
		t.Fatalf("seed streak state: %v", err)
	}
	return streaks
}

func projectionRecoveryServerWithRuleState(t *testing.T, store *corestore.Store, state rulesRegimeStageState) (*Server, int64) {
	t.Helper()
	raw, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	doc, err := store.CompareAndSwapStateDocument(t.Context(), corestore.StateDocumentCAS{
		ScopeKey: daemonStateScope, Kind: stateKindRulesRegimeStage, JSON: raw,
	})
	if err != nil {
		t.Fatalf("seed rule state: %v", err)
	}
	return &Server{
		coreStore:              store,
		rulesRegimeStage:       state,
		rulesRegimeStageLoaded: true,
		logger:                 NewLogger(&bytes.Buffer{}, "error"),
	}, doc.Revision
}

func projectionRecoverySeedExactProjections(t *testing.T, server *Server, snapshot *rpc.RegimeSnapshotResult, publication regimeSnapshotPublication, decisionEvent string) {
	t.Helper()
	streaks := NewStreakStore("")
	if err := streaks.UseCoreStore(server.coreStore); err != nil {
		t.Fatal(err)
	}
	streaks.mu.Lock()
	streaks.loaded = true
	if err := streaks.saveLockedContextPublication(t.Context(), publication); err != nil {
		streaks.mu.Unlock()
		t.Fatalf("seed exact streak projection: %v", err)
	}
	streaks.mu.Unlock()
	server.streaks = streaks

	projectionRecoverySeedRuleProjection(t, server.coreStore, snapshot, publication)
	server.regimeDecisions = &regimeDecisionJournal{core: server.coreStore}
	if decisionEvent == regimeDecisionEventRecorded {
		if err := server.regimeDecisions.appendPublicationContext(t.Context(), publication.PublishedAt, snapshot, publication); err != nil {
			t.Fatalf("seed exact decision event: %v", err)
		}
	}
	if err := server.persistRegimeDecisionProjectionState(t.Context(), corestore.StateDocument{}, false, publication, decisionEvent); err != nil {
		t.Fatalf("seed exact decision projection marker: %v", err)
	}
}

func projectionRecoverySeedRuleProjection(t *testing.T, store *corestore.Store, snapshot *rpc.RegimeSnapshotResult, publication regimeSnapshotPublication) {
	t.Helper()
	ruleBase := rulesRegimeStageState{Version: rulesRegimeStageStateVer}
	if publication.Revision > 1 {
		ruleBase = rulesRegimeStageState{
			Version: rulesRegimeStageStateVer, Bucket: risk.RegimeBucketCalm,
			Stage: rpc.LifecycleQuiet, AsOf: publication.PublishedAt.Add(-time.Second),
			publication: regimeSnapshotPublication{
				Revision: publication.Revision - 1, PublishedAt: publication.PublishedAt.Add(-time.Second),
				Fingerprint: publication.Fingerprint,
			},
		}
	}
	ruleState := projectedRulesRegimeStageState(ruleBase, snapshot, publication)
	raw, err := json.Marshal(ruleState)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.CompareAndSwapStateDocument(t.Context(), corestore.StateDocumentCAS{
		ScopeKey: daemonStateScope, Kind: stateKindRulesRegimeStage, JSON: raw,
	}); err != nil {
		t.Fatalf("seed exact rule-stage projection: %v", err)
	}
}
