package daemon

import (
	"encoding/json"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/osauer/canary/v3/internal/risk"
	"github.com/osauer/canary/v3/internal/rpc"
)

// stampTestServer is a daemon on the store at path whose Rulebook latch says
// early warning since the start of the year, so every stamp reads a carried
// stage: fixed, whatever today's date is.
func stampTestServer(t *testing.T, path string, now *time.Time) *Server {
	t.Helper()
	s := forwardTestServer(t, path, now)
	s.version = "v0.0.0-test"
	s.rulesRegimeStageLoaded = true
	s.rulesRegimeStage = rulesRegimeStageState{Version: rulesRegimeStageStateVer, Bucket: risk.RegimeBucketEarlyWarning, Stage: rpc.LifecycleEarlyWarning, AsOf: time.Date(2026, 1, 2, 15, 0, 0, 0, time.UTC)}
	return s
}

// servedSnapshot is a complete Regime snapshot served at the given UTC time.
func servedSnapshot(at string, stage string, revision int64) regimeSnapshotCacheView {
	asOf, err := time.Parse("2006-01-02 15:04:05", at)
	if err != nil {
		panic(err)
	}
	return regimeSnapshotCacheView{
		Snapshot: &rpc.RegimeSnapshotResult{AsOf: asOf, Lifecycle: rpc.LifecycleState{Stage: stage}},
		Revision: revision, Fingerprint: rpc.Fingerprint{Version: rpc.RegimeFingerprintVersion, Key: "sha256:test"},
	}
}

func stampLog(t *testing.T, s *Server) []edgeSessionStamp {
	t.Helper()
	log, _, err := s.loadEdgeSessionStamps(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	return log.Sessions
}

// A US session's Regime stage is written once, from the first snapshot
// served at or after the official close, and nothing rewrites it: not a
// later snapshot of the same evening with another stage, not the next day's
// reads, not a clock set back, not a restart, and not an update that throws
// the Edge publication away. The week is Thanksgiving 2026: Wednesday the
// 25th closes at 16:00 New York (21:00 UTC), Thursday has no session, and
// Friday the 27th closes early at 13:00 (18:00 UTC).
func TestEdgeSessionStampIsWrittenOnceAtTheClose(t *testing.T) {
	path := filepath.Join(privateTestDir(t), "daemon.db")
	now := time.Date(2026, 11, 25, 21, 0, 40, 0, time.UTC)
	s := stampTestServer(t, path, &now)
	ctx := t.Context()

	// During the session nothing is recorded, whatever the stage.
	s.recordEdgeSessionStamp(ctx, servedSnapshot("2026-11-25 20:59:30", rpc.LifecycleEarlyWarning, 10))
	if got := stampLog(t, s); len(got) != 0 {
		t.Fatalf("a snapshot before the close was recorded: %+v", got)
	}
	// The first snapshot after the close is the session's record.
	s.recordEdgeSessionStamp(ctx, servedSnapshot("2026-11-25 21:00:30", rpc.LifecycleQuiet, 11))
	first := stampLog(t, s)
	if len(first) != 1 || first[0].Session != "2026-11-25" || first[0].RegimeStage != rpc.LifecycleQuiet || first[0].SnapshotRevision != 11 {
		t.Fatalf("after the close=%+v, want one record: 2026-11-25 quiet from revision 11", first)
	}
	wednesday := first[0]
	if !wednesday.ClosedAt.Equal(time.Date(2026, 11, 25, 21, 0, 0, 0, time.UTC)) || !wednesday.RecordedAt.Equal(now) || wednesday.DaemonVersion != "v0.0.0-test" || wednesday.DaemonRevision == "" || wednesday.RegimeFingerprintVersion != rpc.RegimeFingerprintVersion {
		t.Fatalf("record=%+v, want the official close, the time written and the build that computed the stage", wednesday)
	}
	// The premium line is the Rulebook's own answer for its latched stage.
	policy := risk.DefaultRulebookPolicy()
	_, wantAct, wantSet := policy.PremiumBudgetInForce(risk.RegimeBucketEarlyWarning, true)
	if wednesday.PremiumBudgetActPct == nil || *wednesday.PremiumBudgetActPct != wantAct || wednesday.PremiumBudgetSet != wantSet || wednesday.RulebookPolicyFingerprint == "" {
		t.Fatalf("premium line=%v %q %q, want the Rulebook's %v %q with its policy fingerprint", wednesday.PremiumBudgetActPct, wednesday.PremiumBudgetSet, wednesday.RulebookPolicyFingerprint, wantAct, wantSet)
	}

	unchanged := func(step string) {
		t.Helper()
		got := stampLog(t, s)
		if len(got) == 0 || !reflect.DeepEqual(got[0], wednesday) {
			t.Fatalf("%s: the record of 2026-11-25 changed to %+v, was %+v", step, got, wednesday)
		}
	}
	// Later the same evening the vote says panic; on Thanksgiving, which has
	// no session, it still does.
	now = now.Add(5 * time.Minute)
	s.recordEdgeSessionStamp(ctx, servedSnapshot("2026-11-25 21:05:00", rpc.LifecyclePanic, 12))
	s.recordEdgeSessionStamp(ctx, servedSnapshot("2026-11-26 21:30:00", rpc.LifecyclePanic, 13))
	if got := stampLog(t, s); len(got) != 1 {
		t.Fatalf("records=%+v, want one: a later snapshot adds nothing and a holiday has no session", got)
	}
	unchanged("a later snapshot and a holiday")

	// Friday closes at 13:00 New York. A minute before, nothing; a minute
	// after, the session's record, three hours before a 16:00 close.
	s.recordEdgeSessionStamp(ctx, servedSnapshot("2026-11-27 17:59:00", rpc.LifecycleQuiet, 14))
	if got := stampLog(t, s); len(got) != 1 {
		t.Fatalf("a snapshot before Friday's early close was recorded: %+v", got)
	}
	s.recordEdgeSessionStamp(ctx, servedSnapshot("2026-11-27 18:01:00", rpc.LifecycleConfirmedStress, 15))
	if got := stampLog(t, s); len(got) != 2 || got[1].Session != "2026-11-27" || got[1].RegimeStage != rpc.LifecycleConfirmedStress || !got[1].ClosedAt.Equal(time.Date(2026, 11, 27, 18, 0, 0, 0, time.UTC)) {
		t.Fatalf("after Friday's early close=%+v, want a second record: 2026-11-27 confirmed_stress, closed 18:00 UTC", got)
	}
	unchanged("the next session's record")

	// A clock set back to Wednesday evening writes nothing into the past.
	s.recordEdgeSessionStamp(ctx, servedSnapshot("2026-11-25 22:00:00", rpc.LifecyclePanic, 16))
	// An update replaces the Edge publication and empties the bar cache.
	for _, kind := range []string{edgePublicationStateKind, edgeBarCacheStateKind} {
		raw, err := json.Marshal(map[string]any{"version": 999})
		if err != nil {
			t.Fatal(err)
		}
		if err := s.replaceEdgeStateDocument(ctx, kind, raw); err != nil {
			t.Fatal(err)
		}
	}
	// A restart: a new process on the same store, with nothing remembered,
	// is served another stage for a session already recorded.
	if err := s.coreStore.Close(); err != nil {
		t.Fatal(err)
	}
	s = stampTestServer(t, path, &now)
	s.recordEdgeSessionStamp(ctx, servedSnapshot("2026-11-27 19:00:00", rpc.LifecycleQuiet, 17))
	got := stampLog(t, s)
	if len(got) != 2 || got[1].RegimeStage != rpc.LifecycleConfirmedStress {
		t.Fatalf("after a clock set back, an update and a restart=%+v, want the two records as written", got)
	}
	unchanged("a clock set back, an update and a restart")

	// The daemon was not running from Monday's close to midnight: Monday
	// has no record, and Tuesday's close does not go back to write one.
	s.recordEdgeSessionStamp(ctx, servedSnapshot("2026-11-30 15:00:00", rpc.LifecycleEarlyWarning, 18))
	s.recordEdgeSessionStamp(ctx, servedSnapshot("2026-12-01 21:02:00", rpc.LifecycleDataQuality, 19))
	got = stampLog(t, s)
	if len(got) != 3 || got[2].Session != "2026-12-01" || got[2].RegimeStage != rpc.LifecycleDataQuality {
		t.Fatalf("records=%+v, want Wednesday, Friday and Tuesday: Monday was never served after its close, and a data_quality stage is recorded as served", got)
	}

	// A decision is given the record of the session before its date and of
	// its own session, each by exact date.
	at := func(v string) time.Time {
		parsed, err := time.Parse("2006-01-02 15:04", v)
		if err != nil {
			t.Fatal(err)
		}
		return parsed
	}
	// Friday 10:00 New York: the session before is Wednesday, across the
	// holiday.
	prior, own := s.edgeChangeSessionStamps(ctx, at("2026-11-27 15:00"))
	if prior == nil || prior.Session != "2026-11-25" || prior.RegimeStage != rpc.LifecycleQuiet || prior.Missing != "" || prior.ServedAt == nil || !prior.ServedAt.Equal(wednesday.ServedAt) {
		t.Fatalf("prior session of a Friday decision=%+v, want Wednesday's record", prior)
	}
	if own == nil || own.Session != "2026-11-27" || own.RegimeStage != rpc.LifecycleConfirmedStress || own.PremiumBudgetActPct == nil || *own.PremiumBudgetActPct != wantAct {
		t.Fatalf("own session of a Friday decision=%+v, want Friday's record with its premium line", own)
	}
	// Thanksgiving: no session of its own.
	prior, own = s.edgeChangeSessionStamps(ctx, at("2026-11-26 15:00"))
	if own != nil || prior == nil || prior.Session != "2026-11-25" {
		t.Fatalf("a decision on the holiday: prior=%+v own=%+v, want Wednesday and no session of its own", prior, own)
	}
	// Tuesday: the session before it, Monday, was not recorded, and is
	// served as missing, not with Friday's stage.
	prior, own = s.edgeChangeSessionStamps(ctx, at("2026-12-01 15:00"))
	if prior == nil || prior.Session != "2026-11-30" || prior.Missing != rpc.EdgeSessionStampNotRecorded || prior.RegimeStage != "" || prior.ServedAt != nil {
		t.Fatalf("prior session of a Tuesday decision=%+v, want Monday as not recorded", prior)
	}
	if own == nil || own.Session != "2026-12-01" || own.RegimeStage != rpc.LifecycleDataQuality {
		t.Fatalf("own session of a Tuesday decision=%+v, want Tuesday's record", own)
	}
	for _, stamp := range []*rpc.EdgeSessionStamp{prior, own} {
		if !rpc.ValidEdgeSessionStamp(stamp) {
			t.Fatalf("served stamp %+v does not pass the contract", stamp)
		}
	}
	// Before the calendar's coverage no session is named at all.
	if prior, own = s.edgeChangeSessionStamps(ctx, at("2025-06-02 15:00")); prior != nil || own != nil {
		t.Fatalf("outside calendar coverage: prior=%+v own=%+v, want neither", prior, own)
	}
}

// The log is not something the daemon can fetch or derive again, so the
// store's repair path must never drop it: a damaged log stops the repair
// instead of being replaced by an empty one that later reads would take for
// "nothing was recorded".
func TestEdgeSessionStampLogIsNotRebuildable(t *testing.T) {
	if rebuildableStateKinds[edgeSessionStampsStateKind] {
		t.Fatal("the Edge session stamp log is listed as rebuildable: a store repair would drop every recorded stage")
	}
}

// A log written by a later build is never rewritten by this one, and a
// failed write neither stops nor reaches the Regime read it runs beside.
func TestEdgeSessionStampLogFromALaterBuildIsLeftAlone(t *testing.T) {
	now := time.Date(2026, 11, 25, 21, 1, 0, 0, time.UTC)
	s := stampTestServer(t, filepath.Join(privateTestDir(t), "daemon.db"), &now)
	raw := []byte(`{"version":99,"sessions":[{"session":"2026-11-24","regime_stage":"quiet"}],"later":"field"}`)
	if err := s.replaceEdgeStateDocument(t.Context(), edgeSessionStampsStateKind, raw); err != nil {
		t.Fatal(err)
	}
	s.recordEdgeSessionStamp(t.Context(), servedSnapshot("2026-11-25 21:00:30", rpc.LifecyclePanic, 1))
	doc, ok, err := s.coreStore.GetStateDocument(t.Context(), daemonStateScope, edgeSessionStampsStateKind)
	if err != nil || !ok || string(doc.JSON) != string(raw) {
		t.Fatalf("the later build's log was changed: ok=%v err=%v json=%s", ok, err, doc.JSON)
	}
	if settled := s.edgeSessionStamped.Load(); settled != nil {
		t.Fatalf("session %s counts as recorded although nothing was written", *settled)
	}
	if prior, own := s.edgeChangeSessionStamps(t.Context(), time.Date(2026, 11, 25, 15, 0, 0, 0, time.UTC)); prior != nil || own != nil {
		t.Fatalf("a log this build cannot read was served: prior=%+v own=%+v", prior, own)
	}
}
