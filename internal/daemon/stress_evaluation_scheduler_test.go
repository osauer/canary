package daemon

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/osauer/ibkr/v2/internal/rpc"
)

func TestStressEvaluationStartsImmediatelyWhenJournalDisabled(t *testing.T) {
	disabled := false
	server := &Server{
		platformSettings: &platformSettingsStore{data: platformSettingsData{
			Version: platformSettingsDocVersion,
			Stress: platformStressSettingsData{Journal: platformStressJournalSettingsData{
				Enabled: &disabled,
			}},
		}},
		stressDecisions: &stressDecisionJournal{path: filepath.Join(t.TempDir(), "canary-decisions.jsonl")},
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	evaluations := 0
	go func() {
		defer close(done)
		runStressEvaluationLoopWith(ctx, nil, time.Hour, time.Hour, func(context.Context) bool {
			evaluations++
			server.journalStressDecision(testStressResult("sha256:daemon-only"))
			cancel()
			return true
		})
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("daemon-owned Canary evaluation did not run immediately")
	}
	if evaluations != 1 {
		t.Fatalf("Canary evaluations = %d, want one immediate evaluation", evaluations)
	}
	if _, err := os.Stat(server.stressDecisions.path); !os.IsNotExist(err) {
		t.Fatalf("disabled journal retained the daemon-only evaluation: %v", err)
	}
}

func TestStressEvaluationCoalescesWakesDuringEvaluation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	wake := make(chan struct{}, 1)
	firstStarted := make(chan struct{})
	releaseFirst := make(chan struct{})
	done := make(chan struct{})
	evaluations := 0
	go func() {
		defer close(done)
		runStressEvaluationLoopWith(ctx, wake, time.Hour, time.Hour, func(context.Context) bool {
			evaluations++
			if evaluations == 1 {
				close(firstStarted)
				<-releaseFirst
				return true
			}
			cancel()
			return true
		})
	}()
	<-firstStarted
	select {
	case wake <- struct{}{}:
	default:
		t.Fatal("first Regime wake was not accepted")
	}
	select {
	case wake <- struct{}{}:
		t.Fatal("capacity-one Regime wake did not coalesce")
	default:
	}
	close(releaseFirst)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("coalesced Regime wake did not trigger a prompt evaluation")
	}
	if evaluations != 2 {
		t.Fatalf("Canary evaluations = %d, want initial plus one coalesced wake", evaluations)
	}
}

func TestRegimePublicationInvalidatesRulebookAndWakesConsumersOnce(t *testing.T) {
	server := &Server{}
	prior := &rpc.RulesResult{Status: "ok"}
	server.lastRules = prior
	server.lastRulesAt = time.Date(2026, 7, 22, 12, 0, 0, 0, time.UTC)
	stressWake := server.stressEvaluationWakeChannel()
	rulebookWake := server.rulebookRefreshWakeChannel()

	first := regimeDependencyTestPublication(1)
	server.publishRulesRegimeStageState(regimeDependencyTestStage(first), first)
	if server.lastRules != prior || !server.lastRulesAt.IsZero() {
		t.Fatalf("Regime publication did not invalidate only Rulebook cache freshness: last=%p at=%s", server.lastRules, server.lastRulesAt)
	}
	assertRegimeDependencyWake(t, "Canary", stressWake)
	assertRegimeDependencyWake(t, "Rulebook", rulebookWake)

	server.publishRulesRegimeStageState(regimeDependencyTestStage(first), first)
	assertNoRegimeDependencyWake(t, "duplicate Canary", stressWake)
	assertNoRegimeDependencyWake(t, "duplicate Rulebook", rulebookWake)

	second := regimeDependencyTestPublication(2)
	server.publishRulesRegimeStageState(regimeDependencyTestStage(second), second)
	assertRegimeDependencyWake(t, "next Canary", stressWake)
	assertRegimeDependencyWake(t, "next Rulebook", rulebookWake)
	stage := server.rulesRegimeStageSnapshot()
	if !exactRegimeSnapshotPublication(stage.publication, second) {
		t.Fatalf("published Rulebook stage = %+v, want revision %d", stage.publication, second.Revision)
	}
}

func TestRegimePublicationWaitsOutOlderRulebookEvaluation(t *testing.T) {
	server := &Server{lastRules: &rpc.RulesResult{Status: "ok"}, lastRulesAt: time.Now().UTC()}
	publication := regimeDependencyTestPublication(1)
	server.rulesEvaluationMu.Lock()
	started := make(chan struct{})
	done := make(chan struct{})
	go func() {
		close(started)
		server.publishRulesRegimeStageState(regimeDependencyTestStage(publication), publication)
		close(done)
	}()
	<-started
	select {
	case <-done:
		t.Fatal("Regime publication crossed an in-flight Rulebook evaluation")
	default:
	}
	server.rulesEvaluationMu.Unlock()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Regime publication did not resume after Rulebook evaluation")
	}
	if !server.lastRulesAt.IsZero() {
		t.Fatalf("older Rulebook evaluation remained cache-eligible at %s", server.lastRulesAt)
	}
}

func TestRulebookRefreshLoopStartsWithoutAlertObservation(t *testing.T) {
	server := &Server{} // alertShadow deliberately absent
	ctx, cancel := context.WithCancel(t.Context())
	started := make(chan struct{})
	server.startRulebookCanonicalRefreshLoopWith(ctx, func(runContext context.Context) {
		close(started)
		<-runContext.Done()
	})
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("daemon-owned Rulebook refresh did not start without alert observation")
	}
	cancel()
	done := make(chan struct{})
	go func() {
		server.rulebookRefreshLoopWG.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("daemon-owned Rulebook refresh did not drain on cancellation")
	}
}

func regimeDependencyTestPublication(revision int64) regimeSnapshotPublication {
	at := time.Date(2026, 7, 22, 12, int(revision), 0, 0, time.UTC)
	return regimeSnapshotPublication{
		Revision: revision, PublishedAt: at,
		Fingerprint: rpc.Fingerprint{Version: rpc.RegimeFingerprintVersion, Key: "sha256:regime"},
	}
}

func regimeDependencyTestStage(publication regimeSnapshotPublication) rulesRegimeStageState {
	return rulesRegimeStageState{
		Version: rulesRegimeStageStateVer, Bucket: "caution", Stage: "early_warning",
		AsOf: publication.PublishedAt, publication: publication,
	}
}

func assertRegimeDependencyWake(t *testing.T, name string, wake <-chan struct{}) {
	t.Helper()
	select {
	case <-wake:
	default:
		t.Fatalf("%s was not woken", name)
	}
}

func assertNoRegimeDependencyWake(t *testing.T, name string, wake <-chan struct{}) {
	t.Helper()
	select {
	case <-wake:
		t.Fatalf("%s woke twice for one Regime publication", name)
	default:
	}
}
