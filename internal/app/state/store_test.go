package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/rpc"
)

func attentionMatches(got, want Attention) bool {
	if got.UnreadCount != want.UnreadCount || got.HighWaterSeq != want.HighWaterSeq || got.ReadThroughSeq != want.ReadThroughSeq {
		return false
	}
	return want.UnreadRefs == nil || reflect.DeepEqual(got.UnreadRefs, want.UnreadRefs)
}

func TestClearAlertHistoryPreservesUnreadAndReportsActualCount(t *testing.T) {
	t.Parallel()
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := store.RecordAlert(AlertRecord{
		ID:          "alert-1",
		Fingerprint: "fp-1",
		Title:       "canary",
		Body:        "watch",
		CreatedAt:   time.Now().UTC(),
	}); err != nil {
		t.Fatalf("RecordAlert: %v", err)
	}
	if got := store.AlertHistory(10); len(got) != 1 {
		t.Fatalf("AlertHistory len=%d, want 1", len(got))
	}
	cleared, err := store.ClearAlertHistory()
	if err != nil {
		t.Fatalf("ClearAlertHistory: %v", err)
	}
	if cleared != 0 {
		t.Fatalf("cleared=%d, want 0 unread rows removed", cleared)
	}
	if got := store.AlertHistory(10); len(got) != 1 || got[0].ID != "alert-1" {
		t.Fatalf("clear erased unread history: %+v", got)
	}
}

func TestCompactAlertHistoryExpiresOnlyReadPreviousContext(t *testing.T) {
	t.Parallel()
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	base := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	// canary-unread-previous records last so it alone holds the unread
	// (highest) attention sequence after MarkAttentionRead below.
	records := []AlertRecord{
		{ID: "canary-current", Fingerprint: "fp-live", CreatedAt: base, Title: "current", Body: "b"},
		{ID: "canary-previous", Fingerprint: "fp-dead", CreatedAt: base, Title: "previous", Body: "b"},
		{ID: "stop-mismatch", Fingerprint: "stop-fp", Account: "old-account", CreatedAt: base, Title: "other source", Body: "b"},
		{ID: "canary-unread-previous", Fingerprint: "fp-dead", CreatedAt: base, Title: "unread previous", Body: "b"},
	}
	for _, rec := range records {
		if err := store.RecordAlert(rec); err != nil {
			t.Fatalf("RecordAlert(%s): %v", rec.ID, err)
		}
	}
	unreadSeq := store.Attention().HighWaterSeq
	if _, err := store.MarkAttentionRead(unreadSeq - 1); err != nil {
		t.Fatalf("MarkAttentionRead: %v", err)
	}
	for _, rec := range store.AlertHistory(0) {
		if rec.AttentionSeq == unreadSeq && rec.ID != "canary-unread-previous" {
			t.Fatalf("fixture drift: unread seq belongs to %s", rec.ID)
		}
	}

	// Within the window nothing expires; matching records get stamped.
	if err := store.CompactAlertHistory("fp-live", "live-account", "live", base.Add(24*time.Hour)); err != nil {
		t.Fatalf("CompactAlertHistory: %v", err)
	}
	byID := map[string]AlertRecord{}
	for _, rec := range store.AlertHistory(0) {
		byID[rec.ID] = rec
	}
	if len(byID) != 4 {
		t.Fatalf("nothing may expire inside the window, got %d records", len(byID))
	}
	if byID["canary-current"].LastMatchedAt.IsZero() {
		t.Fatal("matching record must carry a last-matched stamp")
	}
	if !byID["canary-previous"].LastMatchedAt.IsZero() {
		t.Fatal("mismatched record must not be stamped")
	}
	if !byID["stop-mismatch"].LastMatchedAt.IsZero() {
		t.Fatal("different-account record must not be stamped")
	}

	// Past the window: read previous-context records expire, the matching
	// record survives on its refreshed stamp, unread survives regardless.
	late := base.Add(alertPreviousContextRetention + 48*time.Hour)
	if err := store.CompactAlertHistory("fp-live", "live-account", "live", late); err != nil {
		t.Fatalf("CompactAlertHistory late: %v", err)
	}
	byID = map[string]AlertRecord{}
	for _, rec := range store.AlertHistory(0) {
		byID[rec.ID] = rec
	}
	if _, ok := byID["canary-previous"]; ok {
		t.Fatal("read previous-context record must expire after the window")
	}
	if _, ok := byID["stop-mismatch"]; ok {
		t.Fatal("read different-account record must expire after the window")
	}
	if _, ok := byID["canary-current"]; !ok {
		t.Fatal("still-matching record must never expire")
	}
	if _, ok := byID["canary-unread-previous"]; !ok {
		t.Fatal("unread record must never expire, previous-context or not")
	}

	// An unknown live context (no fingerprint) expires nothing and keeps
	// stamping conservative: everything still present stays present.
	if err := store.CompactAlertHistory("", "", "", late.Add(alertPreviousContextRetention+time.Hour)); err != nil {
		t.Fatalf("CompactAlertHistory unknown context: %v", err)
	}
	if got := len(store.AlertHistory(0)); got != 2 {
		t.Fatalf("unknown context must not expire records, got %d", got)
	}
}

func TestAttentionSnapshotReturnsOrderedAllowlistedRefs(t *testing.T) {
	t.Parallel()
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := store.RecordAlert(AlertRecord{ID: "canary-1", Fingerprint: "private-canary", Account: "private-account"}); err != nil {
		t.Fatal(err)
	}
	governance, _, err := store.UpsertGovernanceOccurrence(GovernanceOccurrence{Fingerprint: "private-governance", Kind: "policy_drift"}, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	got := store.Attention()
	wantRefs := []AttentionRef{{Kind: AttentionKindStress, ID: "canary-1"}, {Kind: AttentionKindGovernance, ID: governance.DisplayID}}
	if got.UnreadCount != len(wantRefs) || !reflect.DeepEqual(got.UnreadRefs, wantRefs) {
		t.Fatalf("attention=%+v want refs=%+v", got, wantRefs)
	}
	raw, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(raw, &envelope); err != nil {
		t.Fatal(err)
	}
	if len(envelope) != 4 {
		t.Fatalf("attention public fields=%v", envelope)
	}
	var publicRefs []map[string]json.RawMessage
	if err := json.Unmarshal(envelope["unread_refs"], &publicRefs); err != nil {
		t.Fatal(err)
	}
	if len(publicRefs) != 2 {
		t.Fatalf("public refs=%v", publicRefs)
	}
	for _, ref := range publicRefs {
		if len(ref) != 2 || ref["kind"] == nil || ref["id"] == nil {
			t.Fatalf("attention ref is not an exact kind/id allowlist: %v", ref)
		}
	}
	for _, private := range []string{"private-canary", "private-governance", "private-account", "attention_seq", "fingerprint", "account"} {
		if strings.Contains(string(raw), private) {
			t.Fatalf("attention leaked %q: %s", private, raw)
		}
	}
}

func TestGovernanceEpisodeSamplesModeAtCreationAndPreservesSuppression(t *testing.T) {
	t.Parallel()
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetAlertMode(AlertModeNone); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	created, _, err := store.UpsertGovernanceOccurrence(GovernanceOccurrence{Fingerprint: "episode", Kind: "policy_drift"}, now)
	if err != nil {
		t.Fatal(err)
	}
	if created.DeliveryDisposition != GovernanceDispositionSuppressedAtCreation {
		t.Fatal("episode created under none was not durably suppressed")
	}
	if err := store.SetAlertMode(AlertModeActOnly); err != nil {
		t.Fatal(err)
	}
	updated, err := store.ObserveGovernanceOccurrences([]GovernanceOccurrence{{Fingerprint: "episode", Kind: "updated"}}, true, now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if len(updated) != 1 || updated[0].DeliveryDisposition != GovernanceDispositionSuppressedAtCreation {
		t.Fatalf("update weakened suppression boundary: %+v", updated)
	}
	public, err := json.Marshal(store.Governance(now))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(public), "delivery_disposition") {
		t.Fatalf("private suppression field leaked in governance DTO: %s", public)
	}
}

func TestGovernanceEpisodeModeSampleFollowsStoreLockOrder(t *testing.T) {
	for _, tc := range []struct {
		name           string
		modeOwnsFirst  bool
		wantSuppressed bool
	}{
		{name: "creation owns lock while off", wantSuppressed: true},
		{name: "enable owns lock before creation", modeOwnsFirst: true, wantSuppressed: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store, err := Open(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			if err := store.SetAlertMode(AlertModeNone); err != nil {
				t.Fatal(err)
			}
			locked := make(chan struct{})
			release := make(chan struct{})
			var once sync.Once
			store.saveObserver = func() {
				once.Do(func() {
					close(locked)
					<-release
				})
			}

			type occurrenceResult struct {
				row GovernanceOccurrence
				err error
			}
			occurrenceDone := make(chan occurrenceResult, 1)
			modeDone := make(chan error, 1)
			create := func() {
				rows, err := store.ObserveGovernanceOccurrences([]GovernanceOccurrence{{Fingerprint: tc.name, Kind: "policy_drift"}}, true, time.Now().UTC())
				var row GovernanceOccurrence
				if len(rows) == 1 {
					row = rows[0]
				}
				occurrenceDone <- occurrenceResult{row: row, err: err}
			}
			enable := func() { modeDone <- store.SetAlertMode(AlertModeActOnly) }

			if tc.modeOwnsFirst {
				go enable()
				<-locked
				go create()
			} else {
				go create()
				<-locked
				go enable()
			}
			close(release)
			if err := <-modeDone; err != nil {
				t.Fatal(err)
			}
			result := <-occurrenceDone
			if result.err != nil {
				t.Fatal(result.err)
			}
			gotSuppressed := result.row.DeliveryDisposition == GovernanceDispositionSuppressedAtCreation
			if gotSuppressed != tc.wantSuppressed {
				t.Fatalf("suppressed=%v want %v row=%+v", gotSuppressed, tc.wantSuppressed, result.row)
			}
		})
	}
}

func TestSetAlertModeFailureRollsBackSampledMode(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	store.saveHook = func(string) error { return errors.New("injected mode save failure") }
	if err := store.SetAlertMode(AlertModeNone); err == nil {
		t.Fatal("SetAlertMode succeeded")
	}
	if got := store.AlertSettings().Mode; got != AlertModeWatchAndAct {
		t.Fatalf("failed mode save changed in-memory authority to %q", got)
	}
}

func TestGovernanceForensicSaveFailureKeepsTerminalEpisodeBoundary(t *testing.T) {
	dir := t.TempDir()
	store, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetAlertMode(AlertModeNone); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	occurrence, _, err := store.UpsertGovernanceOccurrence(GovernanceOccurrence{Fingerprint: "suppressed", Kind: "policy_drift"}, now)
	if err != nil || occurrence.DeliveryDisposition != GovernanceDispositionSuppressedAtCreation {
		t.Fatalf("occurrence=%+v err=%v", occurrence, err)
	}
	store.saveHook = func(string) error { return errors.New("injected forensic save failure") }
	attempt := GovernanceAttempt{
		OccurrenceID: occurrence.DisplayID,
		TargetRef:    GovernanceTargetRef("governance", "episode-suppressed"),
		ReceiptKey:   GovernanceReceiptKey(occurrence.DisplayID, GovernanceTargetRef("governance", "episode-suppressed")),
		At:           now,
		Class:        GovernanceTransportSuppressed,
	}
	if err := store.RecordGovernanceAttempt(attempt, false); err == nil {
		t.Fatal("forensic attempt unexpectedly persisted")
	}
	store.saveHook = nil
	if len(store.data.GovernanceAttempts) != 0 || store.data.GovernanceTotals.Suppressed != 0 || store.data.GovernanceOccurrences[0].DeliveryDisposition != GovernanceDispositionSuppressedAtCreation {
		t.Fatalf("failed forensic save weakened boundary or left partial accounting: %+v", store.data)
	}
	reopened, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(reopened.data.GovernanceOccurrences) != 1 || reopened.data.GovernanceOccurrences[0].DeliveryDisposition != GovernanceDispositionSuppressedAtCreation || len(reopened.data.GovernanceAttempts) != 0 || reopened.data.GovernanceTotals.Accepted != 0 {
		t.Fatalf("reopened boundary/accounting=%+v", reopened.data)
	}
}

func TestAttentionCanaryCreationIncrementsUnread(t *testing.T) {
	t.Parallel()
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := store.RecordAlert(AlertRecord{ID: "canary-1", Fingerprint: "fp-1", CreatedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	history := store.AlertHistory(10)
	if len(history) != 1 || history[0].AttentionSeq != 1 {
		t.Fatalf("history=%+v, want first attention sequence", history)
	}
	if got := store.Attention(); !attentionMatches(got, Attention{UnreadCount: 1, HighWaterSeq: 1}) {
		t.Fatalf("attention=%+v", got)
	}
}

func TestAttentionGovernanceEpisodeLifecycle(t *testing.T) {
	t.Parallel()
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 7, 19, 9, 0, 0, 0, time.UTC)
	record := GovernanceOccurrence{Fingerprint: "episode", Kind: "policy_drift", State: "open", OccurredAt: now}
	first, created, err := store.UpsertGovernanceOccurrence(record, now)
	if err != nil || !created || first.AttentionSeq != 1 {
		t.Fatalf("first=%+v created=%v err=%v", first, created, err)
	}
	if _, err := store.ObserveGovernanceOccurrences([]GovernanceOccurrence{record}, true, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	record.State = "updated"
	updated, err := store.ObserveGovernanceOccurrences([]GovernanceOccurrence{record}, true, now.Add(2*time.Minute))
	if err != nil || len(updated) != 1 || updated[0].AttentionSeq != first.AttentionSeq {
		t.Fatalf("updated=%+v err=%v", updated, err)
	}
	if err := store.ResolveGovernanceOccurrences(nil, now.Add(3*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if got := store.Attention(); !attentionMatches(got, Attention{UnreadCount: 1, HighWaterSeq: 1}) {
		t.Fatalf("attention after update/resolution=%+v", got)
	}
	second, created, err := store.UpsertGovernanceOccurrence(record, now.Add(4*time.Minute))
	if err != nil || !created || second.AttentionSeq != 2 || second.DisplayID == first.DisplayID {
		t.Fatalf("second=%+v created=%v err=%v", second, created, err)
	}
	if got := store.Attention(); !attentionMatches(got, Attention{UnreadCount: 2, HighWaterSeq: 2}) {
		t.Fatalf("attention after reopened episode=%+v", got)
	}
}

func TestAttentionLegacyRowsDoNotCreateUnread(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	raw := `{"alert_settings":{"mode":"watch_and_act"},"alert_history":[{"id":"legacy-alert"}],"governance_occurrences":[{"fingerprint":"legacy-governance","display_id":"legacy-display"}]}`
	if err := os.WriteFile(filepath.Join(dir, "state.json"), []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := store.Attention(); !attentionMatches(got, Attention{}) {
		t.Fatalf("legacy attention=%+v, want no rollout unread flood", got)
	}
	if history := store.AlertHistory(1); len(history) != 1 || history[0].AttentionSeq != 0 {
		t.Fatalf("legacy alert=%+v", history)
	}
}

func TestOpenMigratesLegacyGovernanceDispositionAndDefaultsBlankMode(t *testing.T) {
	dir := t.TempDir()
	raw := `{"alert_settings":{"mode":""},"governance_occurrences":[{"fingerprint":"legacy","display_id":"gov-legacy","kind":"policy_drift"}]}`
	if err := os.WriteFile(filepath.Join(dir, "state.json"), []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if store.data.AlertSettings.Mode != AlertModeWatchAndAct {
		t.Fatalf("legacy blank mode=%q", store.data.AlertSettings.Mode)
	}
	if got := store.data.GovernanceOccurrences[0].DeliveryDisposition; got != GovernanceDispositionLegacyUnknown {
		t.Fatalf("legacy disposition=%q", got)
	}
}

func TestOpenRejectsCorruptPersistedAuthority(t *testing.T) {
	t.Parallel()
	fixtures := map[string]string{
		"invalid mode":        `{"alert_settings":{"mode":"surprise"}}`,
		"invalid disposition": `{"alert_settings":{"mode":"act_only"},"governance_occurrences":[{"fingerprint":"g","display_id":"gov","delivery_disposition":"surprise"}]}`,
		"cursor inversion":    `{"alert_settings":{"mode":"act_only"},"attention_high_water_seq":1,"attention_read_through_seq":2}`,
		"sequence gap":        `{"alert_settings":{"mode":"act_only"},"attention_high_water_seq":2,"alert_history":[{"id":"a","attention_seq":1}]}`,
		"duplicate sequence":  `{"alert_settings":{"mode":"act_only"},"attention_high_water_seq":1,"alert_history":[{"id":"a","attention_seq":1},{"id":"b","attention_seq":1}]}`,
		"duplicate refs":      `{"alert_settings":{"mode":"act_only"},"attention_high_water_seq":2,"alert_history":[{"id":"same","attention_seq":1},{"id":"same","attention_seq":2}]}`,
		"out of range":        `{"alert_settings":{"mode":"act_only"},"attention_high_water_seq":1,"alert_history":[{"id":"a","attention_seq":2}]}`,
		"empty unread id":     `{"alert_settings":{"mode":"act_only"},"attention_high_water_seq":1,"alert_history":[{"id":"","attention_seq":1}]}`,
	}
	for name, raw := range fixtures {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "state.json"), []byte(raw), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := Open(dir); !errors.Is(err, ErrInvalidPersistedState) {
				t.Fatalf("Open err=%v, want ErrInvalidPersistedState", err)
			}
		})
	}
}

func TestRecordAlertIfNewIsAtomicAcrossConcurrentFingerprintObservations(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	const observers = 32
	start := make(chan struct{})
	results := make(chan bool, observers)
	errs := make(chan error, observers)
	var wg sync.WaitGroup
	for range observers {
		wg.Go(func() {
			<-start
			created, err := store.RecordAlertIfNew(AlertRecord{ID: "canary-atomic", Fingerprint: "fp-atomic"})
			results <- created
			errs <- err
		})
	}
	close(start)
	wg.Wait()
	close(results)
	close(errs)
	createdCount := 0
	for created := range results {
		if created {
			createdCount++
		}
	}
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if createdCount != 1 || len(store.AlertHistory(0)) != 1 {
		t.Fatalf("created=%d history=%+v", createdCount, store.AlertHistory(0))
	}
	attention := store.Attention()
	if attention.HighWaterSeq != 1 || attention.UnreadCount != 1 || len(attention.UnreadRefs) != 1 {
		t.Fatalf("attention=%+v", attention)
	}
}

func TestRecordAlertRequiresUniqueNonEmptyDurableID(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := store.RecordAlert(AlertRecord{}); err == nil {
		t.Fatal("RecordAlert accepted an empty public id")
	}
	if err := store.RecordAlert(AlertRecord{ID: "stable"}); err != nil {
		t.Fatal(err)
	}
	before := store.Attention()
	if err := store.RecordAlert(AlertRecord{ID: "stable", Fingerprint: "different"}); err == nil {
		t.Fatal("RecordAlert accepted a duplicate public id")
	}
	if got := store.Attention(); !reflect.DeepEqual(got, before) || len(store.AlertHistory(0)) != 1 {
		t.Fatalf("rejected id changed state: attention=%+v history=%+v", got, store.AlertHistory(0))
	}
}

func TestUpsertGovernanceOccurrenceCreatedIsLinearizable(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	const observers = 32
	start := make(chan struct{})
	created := make(chan bool, observers)
	errs := make(chan error, observers)
	var wg sync.WaitGroup
	for range observers {
		wg.Go(func() {
			<-start
			_, wasCreated, err := store.UpsertGovernanceOccurrence(GovernanceOccurrence{Fingerprint: "governance-atomic", Kind: "policy_drift"}, time.Now().UTC())
			created <- wasCreated
			errs <- err
		})
	}
	close(start)
	wg.Wait()
	close(created)
	close(errs)
	createdCount := 0
	for value := range created {
		if value {
			createdCount++
		}
	}
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if createdCount != 1 || len(store.data.GovernanceOccurrences) != 1 || store.data.AttentionHighWaterSeq != 1 {
		t.Fatalf("created=%d occurrences=%+v high_water=%d", createdCount, store.data.GovernanceOccurrences, store.data.AttentionHighWaterSeq)
	}
}

func TestMarkAttentionReadMonotonicIdempotentAndRenderedHighWaterRace(t *testing.T) {
	t.Parallel()
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := store.RecordAlert(AlertRecord{ID: "a"}); err != nil {
		t.Fatal(err)
	}
	rendered := store.Attention()
	if err := store.RecordAlert(AlertRecord{ID: "b"}); err != nil {
		t.Fatal(err)
	}
	marked, err := store.MarkAttentionRead(rendered.HighWaterSeq)
	if err != nil || !attentionMatches(marked, Attention{UnreadCount: 1, HighWaterSeq: 2, ReadThroughSeq: 1}) {
		t.Fatalf("marked=%+v err=%v", marked, err)
	}
	saves := 0
	store.saveObserver = func() { saves++ }
	if got, err := store.MarkAttentionRead(1); err != nil || !reflect.DeepEqual(got, marked) || saves != 0 {
		t.Fatalf("idempotent mark got=%+v err=%v saves=%d", got, err, saves)
	}
	if _, err := store.MarkAttentionRead(0); !errors.Is(err, ErrAttentionReadRegression) {
		t.Fatalf("regression err=%v", err)
	}
	if _, err := store.MarkAttentionRead(3); !errors.Is(err, ErrAttentionReadBeyondHighWater) {
		t.Fatalf("beyond-high-water err=%v", err)
	}
	if got := store.Attention(); !reflect.DeepEqual(got, marked) {
		t.Fatalf("invalid mark changed attention: %+v", got)
	}
}

func TestMarkAttentionReadFailureRollsBackCursor(t *testing.T) {
	for _, stage := range []string{"write", "rename"} {
		t.Run(stage, func(t *testing.T) {
			dir := t.TempDir()
			store, err := Open(dir)
			if err != nil {
				t.Fatal(err)
			}
			if err := store.RecordAlert(AlertRecord{ID: "unread"}); err != nil {
				t.Fatal(err)
			}
			store.saveHook = func(got string) error {
				if got == stage {
					return errors.New("injected mark-read failure")
				}
				return nil
			}
			if _, err := store.MarkAttentionRead(1); err == nil {
				t.Fatal("MarkAttentionRead succeeded")
			}
			want := Attention{UnreadCount: 1, HighWaterSeq: 1}
			if got := store.Attention(); !attentionMatches(got, want) {
				t.Fatalf("failed mark changed in-memory cursor: %+v", got)
			}
			reopened, err := Open(dir)
			if err != nil {
				t.Fatal(err)
			}
			if got := reopened.Attention(); !attentionMatches(got, want) {
				t.Fatalf("failed mark changed persisted cursor: %+v", got)
			}
		})
	}
}

func TestAttentionPersistsAcrossReopen(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	store, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.RecordAlert(AlertRecord{ID: "a"}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.UpsertGovernanceOccurrence(GovernanceOccurrence{Fingerprint: "g", Kind: "policy_drift"}, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if _, err := store.MarkAttentionRead(1); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := reopened.Attention(); !attentionMatches(got, Attention{UnreadCount: 1, HighWaterSeq: 2, ReadThroughSeq: 1}) {
		t.Fatalf("reopened attention=%+v", got)
	}
}

func TestAttentionCreationFailureRollsBackRowsAndCounters(t *testing.T) {
	for _, stage := range []string{"write", "rename"} {
		t.Run("canary/"+stage, func(t *testing.T) {
			dir := t.TempDir()
			store, err := Open(dir)
			if err != nil {
				t.Fatal(err)
			}
			if err := store.RecordAlert(AlertRecord{ID: "baseline"}); err != nil {
				t.Fatal(err)
			}
			if _, err := store.MarkAttentionRead(1); err != nil {
				t.Fatal(err)
			}
			store.saveHook = func(got string) error {
				if got == stage {
					return errors.New("injected alert save failure")
				}
				return nil
			}
			if err := store.RecordAlert(AlertRecord{ID: "failed"}); err == nil {
				t.Fatal("RecordAlert succeeded")
			}
			want := Attention{HighWaterSeq: 1, ReadThroughSeq: 1}
			if got := store.Attention(); !attentionMatches(got, want) || len(store.AlertHistory(10)) != 1 {
				t.Fatalf("failed alert state: attention=%+v history=%+v", got, store.AlertHistory(10))
			}
			reopened, err := Open(dir)
			if err != nil {
				t.Fatal(err)
			}
			if !attentionMatches(reopened.Attention(), want) || len(reopened.AlertHistory(10)) != 1 {
				t.Fatalf("reopened failed alert state: attention=%+v history=%+v", reopened.Attention(), reopened.AlertHistory(10))
			}
		})
		t.Run("governance/"+stage, func(t *testing.T) {
			dir := t.TempDir()
			store, err := Open(dir)
			if err != nil {
				t.Fatal(err)
			}
			if err := store.RecordAlert(AlertRecord{ID: "baseline"}); err != nil {
				t.Fatal(err)
			}
			if _, err := store.MarkAttentionRead(1); err != nil {
				t.Fatal(err)
			}
			store.saveHook = func(got string) error {
				if got == stage {
					return errors.New("injected governance save failure")
				}
				return nil
			}
			if _, _, err := store.UpsertGovernanceOccurrence(GovernanceOccurrence{Fingerprint: "failed", Kind: "policy_drift"}, time.Now().UTC()); err == nil {
				t.Fatal("governance creation succeeded")
			}
			want := Attention{HighWaterSeq: 1, ReadThroughSeq: 1}
			if got := store.Attention(); !attentionMatches(got, want) || len(store.Governance(time.Now().UTC()).Occurrences) != 0 {
				t.Fatalf("failed governance state: attention=%+v governance=%+v", got, store.Governance(time.Now().UTC()))
			}
			reopened, err := Open(dir)
			if err != nil {
				t.Fatal(err)
			}
			if !attentionMatches(reopened.Attention(), want) || len(reopened.Governance(time.Now().UTC()).Occurrences) != 0 {
				t.Fatalf("reopened failed governance state: attention=%+v governance=%+v", reopened.Attention(), reopened.Governance(time.Now().UTC()))
			}
		})
	}
}

func TestAlertRetentionRejectsUnreadAndEvictsOldestRead(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for i := range 100 {
		if err := store.RecordAlert(AlertRecord{ID: fmt.Sprintf("alert-%03d", i+1)}); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.RecordAlert(AlertRecord{ID: "overflow"}); !errors.Is(err, ErrAlertHistoryOverflow) {
		t.Fatalf("overflow err=%v", err)
	}
	if got := store.Attention(); !attentionMatches(got, Attention{UnreadCount: 100, HighWaterSeq: 100}) {
		t.Fatalf("overflow changed attention=%+v", got)
	}
	if _, err := store.MarkAttentionRead(1); err != nil {
		t.Fatal(err)
	}
	if err := store.RecordAlert(AlertRecord{ID: "fresh"}); err != nil {
		t.Fatal(err)
	}
	history := store.AlertHistory(0)
	if len(history) != 100 || history[0].ID != "fresh" {
		t.Fatalf("retained history len=%d newest=%+v", len(history), history[0])
	}
	for _, record := range history {
		if record.AttentionSeq == 1 {
			t.Fatal("oldest read row was not evicted")
		}
	}
	if got := store.Attention(); !attentionMatches(got, Attention{UnreadCount: 100, HighWaterSeq: 101, ReadThroughSeq: 1}) {
		t.Fatalf("attention after read eviction=%+v", got)
	}
}

func TestAlertRetentionEvictsLegacyRows(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	legacy := make([]AlertRecord, 100)
	for i := range legacy {
		legacy[i].ID = fmt.Sprintf("legacy-%03d", i)
	}
	raw, err := json.Marshal(Data{AlertSettings: AlertSettings{Mode: AlertModeWatchAndAct}, AlertHistory: legacy})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "state.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.RecordAlert(AlertRecord{ID: "fresh"}); err != nil {
		t.Fatal(err)
	}
	history := store.AlertHistory(0)
	if len(history) != 100 || history[0].AttentionSeq != 1 || history[len(history)-1].ID != "legacy-098" {
		t.Fatalf("legacy retention=%+v", history)
	}
	if got := store.Attention(); !attentionMatches(got, Attention{UnreadCount: 1, HighWaterSeq: 1}) {
		t.Fatalf("attention=%+v", got)
	}
}

func TestClearAlertHistoryRemovesOnlyLegacyAndReadRows(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := store.RecordAlert(AlertRecord{ID: "read"}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.MarkAttentionRead(1); err != nil {
		t.Fatal(err)
	}
	store.mu.Lock()
	store.data.AlertHistory = append(store.data.AlertHistory, AlertRecord{ID: "legacy"})
	store.mu.Unlock()
	if err := store.RecordAlert(AlertRecord{ID: "unread"}); err != nil {
		t.Fatal(err)
	}
	cleared, err := store.ClearAlertHistory()
	if err != nil {
		t.Fatal(err)
	}
	if cleared != 2 {
		t.Fatalf("cleared=%d, want legacy plus read row", cleared)
	}
	history := store.AlertHistory(0)
	if len(history) != 1 || history[0].ID != "unread" {
		t.Fatalf("retained history=%+v", history)
	}
	want := Attention{UnreadCount: 1, HighWaterSeq: 2, ReadThroughSeq: 1, UnreadRefs: []AttentionRef{{Kind: AttentionKindStress, ID: "unread"}}}
	if got := store.Attention(); !reflect.DeepEqual(got, want) {
		t.Fatalf("attention=%+v want=%+v", got, want)
	}
}

func TestClearAlertHistorySaveFailureRollsBackEverything(t *testing.T) {
	for _, stage := range []string{"write", "rename"} {
		t.Run(stage, func(t *testing.T) {
			dir := t.TempDir()
			store, err := Open(dir)
			if err != nil {
				t.Fatal(err)
			}
			if err := store.RecordAlert(AlertRecord{ID: "read"}); err != nil {
				t.Fatal(err)
			}
			if _, err := store.MarkAttentionRead(1); err != nil {
				t.Fatal(err)
			}
			if err := store.RecordAlert(AlertRecord{ID: "unread"}); err != nil {
				t.Fatal(err)
			}
			beforeHistory := store.AlertHistory(0)
			beforeAttention := store.Attention()
			store.saveHook = func(got string) error {
				if got == stage {
					return errors.New("injected clear failure")
				}
				return nil
			}
			cleared, err := store.ClearAlertHistory()
			if err == nil || cleared != 0 {
				t.Fatalf("cleared=%d err=%v", cleared, err)
			}
			if got := store.AlertHistory(0); !reflect.DeepEqual(got, beforeHistory) || !reflect.DeepEqual(store.Attention(), beforeAttention) {
				t.Fatalf("in-memory rollback history=%+v attention=%+v", got, store.Attention())
			}
			reopened, err := Open(dir)
			if err != nil {
				t.Fatal(err)
			}
			if got := reopened.AlertHistory(0); !reflect.DeepEqual(got, beforeHistory) || !reflect.DeepEqual(reopened.Attention(), beforeAttention) {
				t.Fatalf("reopened rollback history=%+v attention=%+v", got, reopened.Attention())
			}
		})
	}
}

func TestAttentionRefsCoverCappedCanaryAndGovernanceHistory(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for i := range 100 {
		if err := store.RecordAlert(AlertRecord{ID: fmt.Sprintf("canary-%03d", i)}); err != nil {
			t.Fatal(err)
		}
	}
	governance, _, err := store.UpsertGovernanceOccurrence(GovernanceOccurrence{Fingerprint: "governance", Kind: "policy_drift"}, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	attention := store.Attention()
	if attention.UnreadCount != 101 || attention.UnreadCount != len(attention.UnreadRefs) || attention.HighWaterSeq != 101 {
		t.Fatalf("attention=%+v", attention)
	}
	if attention.UnreadRefs[0] != (AttentionRef{Kind: AttentionKindStress, ID: "canary-000"}) || attention.UnreadRefs[100] != (AttentionRef{Kind: AttentionKindGovernance, ID: governance.DisplayID}) {
		t.Fatalf("ordered refs first=%+v last=%+v", attention.UnreadRefs[0], attention.UnreadRefs[100])
	}
	if got := store.AlertHistory(0); len(got) != 100 {
		t.Fatalf("full retained Canary history len=%d", len(got))
	}
}

func TestAttentionConcurrentCreationSnapshotIsCoherentAndOrdered(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	const pairs = 24
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range pairs {
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			if err := store.RecordAlert(AlertRecord{ID: fmt.Sprintf("canary-%02d", i)}); err != nil {
				t.Errorf("RecordAlert: %v", err)
			}
		}()
		go func() {
			defer wg.Done()
			<-start
			if _, _, err := store.UpsertGovernanceOccurrence(GovernanceOccurrence{Fingerprint: fmt.Sprintf("governance-%02d", i), Kind: "policy_drift"}, time.Now().UTC()); err != nil {
				t.Errorf("UpsertGovernanceOccurrence: %v", err)
			}
		}()
	}
	close(start)
	wg.Wait()

	attention := store.Attention()
	if attention.UnreadCount != pairs*2 || attention.UnreadCount != len(attention.UnreadRefs) || attention.HighWaterSeq != pairs*2 {
		t.Fatalf("attention=%+v", attention)
	}
	seqByRef := make(map[AttentionRef]uint64, pairs*2)
	store.mu.Lock()
	for _, record := range store.data.AlertHistory {
		seqByRef[AttentionRef{Kind: AttentionKindStress, ID: record.ID}] = record.AttentionSeq
	}
	for _, occurrence := range store.data.GovernanceOccurrences {
		seqByRef[AttentionRef{Kind: AttentionKindGovernance, ID: occurrence.DisplayID}] = occurrence.AttentionSeq
	}
	store.mu.Unlock()
	var prior uint64
	for _, ref := range attention.UnreadRefs {
		seq := seqByRef[ref]
		if seq <= prior {
			t.Fatalf("refs are not ordered by immutable sequence: prior=%d seq=%d ref=%+v", prior, seq, ref)
		}
		prior = seq
	}
}

func TestAttentionReaderOverlappingLockedCreationGetsCoherentSnapshot(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	creationLocked := make(chan struct{})
	releaseCreation := make(chan struct{})
	store.saveObserver = func() {
		close(creationLocked)
		<-releaseCreation
	}
	creationDone := make(chan error, 1)
	go func() {
		creationDone <- store.RecordAlert(AlertRecord{ID: "overlap"})
	}()
	<-creationLocked
	readerStarted := make(chan struct{})
	readerDone := make(chan Attention, 1)
	go func() {
		close(readerStarted)
		readerDone <- store.Attention()
	}()
	<-readerStarted
	close(releaseCreation)
	if err := <-creationDone; err != nil {
		t.Fatal(err)
	}
	got := <-readerDone
	want := Attention{UnreadCount: 1, HighWaterSeq: 1, UnreadRefs: []AttentionRef{{Kind: AttentionKindStress, ID: "overlap"}}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("overlapping reader saw mixed snapshot: got=%+v want=%+v", got, want)
	}
}

func TestAttentionMissingReferenceFailsClosed(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := store.RecordAlert(AlertRecord{ID: "first"}); err != nil {
		t.Fatal(err)
	}
	if err := store.RecordAlert(AlertRecord{ID: "second"}); err != nil {
		t.Fatal(err)
	}
	store.mu.Lock()
	store.data.AlertHistory = store.data.AlertHistory[:1]
	store.mu.Unlock()
	if _, err := store.MarkAttentionRead(2); !errors.Is(err, ErrAttentionReferencesIncomplete) {
		t.Fatalf("missing-ref mark err=%v", err)
	}
	attention := store.Attention()
	if attention.ReadThroughSeq != 0 || attention.UnreadCount != 1 || len(attention.UnreadRefs) != 1 || attention.UnreadRefs[0].ID != "second" {
		t.Fatalf("missing ref advanced or fabricated attention: %+v", attention)
	}
}

func TestAttentionSequenceExhaustionRollsBackSingleAndPartialBatch(t *testing.T) {
	t.Run("canary", func(t *testing.T) {
		store, err := Open(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		store.data.AttentionHighWaterSeq = ^uint64(0)
		store.data.AttentionReadThroughSeq = ^uint64(0)
		if err := store.RecordAlert(AlertRecord{ID: "overflow"}); !errors.Is(err, ErrAttentionSequenceExhausted) {
			t.Fatalf("RecordAlert err=%v", err)
		}
		if len(store.data.AlertHistory) != 0 || store.data.AttentionHighWaterSeq != ^uint64(0) {
			t.Fatalf("exhausted Canary mutation survived: %+v", store.data)
		}
	})
	t.Run("governance partial batch", func(t *testing.T) {
		store, err := Open(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		store.data.AttentionHighWaterSeq = ^uint64(0) - 1
		store.data.AttentionReadThroughSeq = ^uint64(0) - 1
		_, err = store.ObserveGovernanceOccurrences([]GovernanceOccurrence{
			{Fingerprint: "first", Kind: "policy_drift"},
			{Fingerprint: "second", Kind: "reconcile_exception"},
		}, true, time.Now().UTC())
		if !errors.Is(err, ErrAttentionSequenceExhausted) {
			t.Fatalf("ObserveGovernanceOccurrences err=%v", err)
		}
		if len(store.data.GovernanceOccurrences) != 0 || store.data.AttentionHighWaterSeq != ^uint64(0)-1 || store.data.AttentionReadThroughSeq != ^uint64(0)-1 {
			t.Fatalf("partial exhausted batch survived: %+v", store.data)
		}
	})
}

func TestRelayRoutePersistsAndFiltersByRemoteURL(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	store, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	now := time.Date(2026, 6, 16, 9, 0, 0, 0, time.UTC)
	route := RelayRoute{
		RemoteURL:      "https://remote.example",
		RouteID:        "r_route",
		ConnectorToken: "tok_route",
		PublicURL:      "https://remote.example",
		ConnectorURL:   "wss://remote.example/api/connect?route_id=r_route",
		ExpiresAt:      now.Add(-time.Hour),
	}
	if err := store.SetRelayRoute(route); err != nil {
		t.Fatalf("SetRelayRoute: %v", err)
	}

	reopened, err := Open(dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	// The route is returned even past its ExpiresAt: the relay revives a
	// token-matched resume, so a locally expired route must still resume
	// instead of being abandoned for a fresh route id.
	got, ok := reopened.RelayRoute("https://remote.example")
	if !ok {
		t.Fatalf("RelayRoute not returned")
	}
	if got.RouteID != route.RouteID || got.ConnectorToken != route.ConnectorToken || got.UpdatedAt.IsZero() {
		t.Fatalf("RelayRoute = %#v, want persisted route/token with UpdatedAt", got)
	}
	if _, ok := reopened.RelayRoute("https://other.example"); ok {
		t.Fatalf("RelayRoute returned for a different remote URL")
	}
}

func TestPruneDevicesRemovesStaleGrantsAndTheirPushSubscriptions(t *testing.T) {
	t.Parallel()
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	now := time.Date(2026, 7, 15, 8, 0, 0, 0, time.UTC)
	stale := DeviceGrant{ID: "dev-stale", Name: "old", CreatedAt: now.AddDate(0, 0, -40), LastSeenAt: now.AddDate(0, 0, -30)}
	// Freshly paired but never used: activity is the later of created/seen.
	freshUnused := DeviceGrant{ID: "dev-fresh", Name: "new", CreatedAt: now.AddDate(0, 0, -1)}
	active := DeviceGrant{ID: "dev-active", Name: "iPhone", CreatedAt: now.AddDate(0, 0, -60), LastSeenAt: now.AddDate(0, 0, -2)}
	for _, d := range []DeviceGrant{stale, freshUnused, active} {
		if err := store.AddDevice(d); err != nil {
			t.Fatalf("AddDevice: %v", err)
		}
	}
	if err := store.AddPushSubscription(PushSubscription{ID: "s1", DeviceID: "dev-stale", Endpoint: "https://push/stale"}); err != nil {
		t.Fatalf("AddPushSubscription: %v", err)
	}
	if err := store.AddPushSubscription(PushSubscription{ID: "s2", DeviceID: "dev-active", Endpoint: "https://push/active"}); err != nil {
		t.Fatalf("AddPushSubscription: %v", err)
	}

	removed, err := store.PruneDevices(now.AddDate(0, 0, -7))
	if err != nil {
		t.Fatalf("PruneDevices: %v", err)
	}
	if removed != 1 {
		t.Fatalf("removed = %d, want 1", removed)
	}
	if _, ok := store.Device("dev-stale"); ok {
		t.Fatalf("stale device survived the prune")
	}
	for _, id := range []string{"dev-fresh", "dev-active"} {
		if _, ok := store.Device(id); !ok {
			t.Fatalf("device %s should have survived the prune", id)
		}
	}
	subs := store.PushSubscriptions()
	if len(subs) != 1 || subs[0].DeviceID != "dev-active" {
		t.Fatalf("push subscriptions = %#v, want only the active device's", subs)
	}
}

func TestSetRelayRouteKeepsCreatedAtForSameRoute(t *testing.T) {
	t.Parallel()
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	route := RelayRoute{
		RemoteURL:      "https://remote.example",
		RouteID:        "r_route",
		ConnectorToken: "tok_route",
	}
	if err := store.SetRelayRoute(route); err != nil {
		t.Fatalf("SetRelayRoute: %v", err)
	}
	first, _ := store.RelayRoute("https://remote.example")
	if first.CreatedAt.IsZero() {
		t.Fatalf("CreatedAt not stamped on first persist")
	}
	// A route extension re-persists the same route id with a fresh token
	// expiry; the birth time must survive so route age stays observable.
	route.ConnectorToken = "tok_rotated"
	if err := store.SetRelayRoute(route); err != nil {
		t.Fatalf("SetRelayRoute extension: %v", err)
	}
	extended, _ := store.RelayRoute("https://remote.example")
	if !extended.CreatedAt.Equal(first.CreatedAt) {
		t.Fatalf("CreatedAt changed on extension: %v -> %v", first.CreatedAt, extended.CreatedAt)
	}
	// A different route id is a new route and gets a new birth time.
	fresh := RelayRoute{RemoteURL: "https://remote.example", RouteID: "r_new", ConnectorToken: "tok_new"}
	if err := store.SetRelayRoute(fresh); err != nil {
		t.Fatalf("SetRelayRoute fresh: %v", err)
	}
	got, _ := store.RelayRoute("https://remote.example")
	if got.CreatedAt.Before(first.CreatedAt) {
		t.Fatalf("fresh route CreatedAt %v predates previous route %v", got.CreatedAt, first.CreatedAt)
	}
}

func TestGovernanceReceiptPersistsPerTargetAcrossRestart(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	store, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 7, 18, 9, 0, 0, 0, time.UTC)
	occ, _, err := store.UpsertGovernanceOccurrence(GovernanceOccurrence{
		Fingerprint: "sha256:" + strings.Repeat("a", 64), Kind: "policy_drift", State: "open", Severity: "act",
		Title: "Policy pins need review", Body: "Review the policy pin status.", Destination: "alerts", OccurredAt: now,
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	targetOne := GovernanceTargetRef("device-one", "subscription-one")
	targetTwo := GovernanceTargetRef("device-two", "subscription-two")
	if err := store.RecordGovernanceAttempt(GovernanceAttempt{OccurrenceID: occ.DisplayID, TargetRef: targetOne, ReceiptKey: GovernanceReceiptKey(occ.Fingerprint, targetOne), At: now, Class: GovernanceTransportAccepted}, true); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !reopened.HasGovernanceReceipt(GovernanceReceiptKey(occ.Fingerprint, targetOne)) {
		t.Fatal("accepted target receipt did not survive restart")
	}
	if reopened.HasGovernanceReceipt(GovernanceReceiptKey(occ.Fingerprint, targetTwo)) {
		t.Fatal("one target receipt suppressed a different target")
	}
	view := reopened.Governance(now)
	if len(view.Attempts) != 1 || view.Attempts[0].TargetRef != targetOne || strings.Contains(view.Attempts[0].TargetRef, "device-one") {
		t.Fatalf("attempt view=%+v, want one opaque per-target receipt", view.Attempts)
	}
}

func TestGovernanceRetentionNeverEvictsBindingTruthAndFailsLoudOnOverflow(t *testing.T) {
	t.Parallel()
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	store.governanceMaxAttempts = 2
	now := time.Date(2026, 7, 18, 9, 0, 0, 0, time.UTC)
	for i := range 2 {
		err := store.RecordGovernanceAttempt(GovernanceAttempt{
			OccurrenceID: "gov-active", TargetRef: GovernanceTargetRef("d", string(rune('a'+i))),
			ReceiptKey: GovernanceReceiptKey("active", string(rune('a'+i))), At: now, Class: GovernanceTransportTimeoutRetry,
		}, false)
		if err != nil {
			t.Fatal(err)
		}
	}
	err = store.RecordGovernanceAttempt(GovernanceAttempt{OccurrenceID: "gov-active", TargetRef: "third", ReceiptKey: "third", At: now, Class: GovernanceTransportTimeoutRetry}, false)
	if !errors.Is(err, ErrGovernanceOverflow) {
		t.Fatalf("overflow err=%v, want ErrGovernanceOverflow", err)
	}
	view := store.Governance(now)
	if view.DeliveryHealth.State != GovernanceDeliveryOverflow || view.DeliveryHealth.Class != GovernanceTransportOverflow || len(view.Attempts) != 2 || view.AttemptTotals.CumulativeAttempts != 2 || view.HealthTotals.Overflows != 1 {
		t.Fatalf("view=%+v, want fail-loud overflow without eviction", view)
	}
}

func TestGovernanceOccurrenceOverflowRollsBackPartialBatchAttention(t *testing.T) {
	t.Parallel()
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	store.governanceMaxItems = 1
	now := time.Date(2026, 7, 19, 9, 0, 0, 0, time.UTC)
	_, err = store.ObserveGovernanceOccurrences([]GovernanceOccurrence{
		{Fingerprint: "first", Kind: "policy_drift"},
		{Fingerprint: "second", Kind: "reconcile_exception"},
	}, true, now)
	if !errors.Is(err, ErrGovernanceOverflow) {
		t.Fatalf("overflow err=%v", err)
	}
	if got := store.Attention(); !attentionMatches(got, Attention{}) {
		t.Fatalf("partial batch advanced attention=%+v", got)
	}
	view := store.Governance(now)
	if len(view.Occurrences) != 0 || view.DeliveryHealth.State != GovernanceDeliveryOverflow {
		t.Fatalf("partial batch survived overflow: %+v", view)
	}
}

func TestGovernanceResolvedDetailRetainsNinetyDaysThenCompacts(t *testing.T) {
	t.Parallel()
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 7, 18, 9, 0, 0, 0, time.UTC)
	occ, _, err := store.UpsertGovernanceOccurrence(GovernanceOccurrence{Fingerprint: "old", Kind: "policy_drift", DisplayID: "gov-old", OccurredAt: now.AddDate(0, 0, -100)}, now.AddDate(0, 0, -100))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.RecordGovernanceAttempt(GovernanceAttempt{OccurrenceID: occ.DisplayID, ReceiptKey: "old-receipt", TargetRef: "target", At: now.AddDate(0, 0, -100), Class: GovernanceTransportAccepted}, true); err != nil {
		t.Fatal(err)
	}
	if err := store.ResolveGovernanceOccurrences(nil, now.AddDate(0, 0, -99)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.MarkAttentionRead(store.Attention().HighWaterSeq); err != nil {
		t.Fatal(err)
	}
	if err := store.CompactGovernance(now); err != nil {
		t.Fatal(err)
	}
	view := store.Governance(now)
	if len(view.Occurrences) != 0 || len(view.Attempts) != 0 || store.HasGovernanceReceipt("old-receipt") || view.AttemptTotals.Accepted != 1 {
		t.Fatalf("expired resolved detail survived compaction: %+v", view)
	}
}

func TestCompactGovernanceRetainsUnreadUntilMarkedRead(t *testing.T) {
	t.Parallel()
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 7, 19, 9, 0, 0, 0, time.UTC)
	old := now.Add(-100 * 24 * time.Hour)
	occurrence, _, err := store.UpsertGovernanceOccurrence(GovernanceOccurrence{Fingerprint: "unread-old", Kind: "policy_drift", OccurredAt: old}, old)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.ResolveGovernanceOccurrences(nil, old.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := store.CompactGovernance(now); err != nil {
		t.Fatal(err)
	}
	if got := store.Governance(now).Occurrences; len(got) != 1 || got[0].DisplayID != occurrence.DisplayID {
		t.Fatalf("unread old occurrence compacted: %+v", got)
	}
	if _, err := store.MarkAttentionRead(occurrence.AttentionSeq); err != nil {
		t.Fatal(err)
	}
	if err := store.CompactGovernance(now); err != nil {
		t.Fatal(err)
	}
	if got := store.Governance(now).Occurrences; len(got) != 0 {
		t.Fatalf("read old occurrence survived compaction: %+v", got)
	}
}

func TestGovernanceStateWriteFailureStaysVisibleUntilPersistedRecovery(t *testing.T) {
	dir := t.TempDir()
	store, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 7, 18, 9, 0, 0, 0, time.UTC)
	if err := os.Remove(dir); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dir, []byte("blocks state directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(dir); _ = os.MkdirAll(dir, 0o700) })
	_, _, err = store.UpsertGovernanceOccurrence(GovernanceOccurrence{Fingerprint: "write-failure", Kind: "policy_drift", OccurredAt: now}, now)
	if err == nil {
		t.Fatal("governance write unexpectedly succeeded in read-only state dir")
	}
	if got := store.Governance(now).DeliveryHealth; got.State != GovernanceDeliveryUnavailable || got.Class != GovernanceTransportStateWrite {
		t.Fatalf("failure health=%+v", got)
	}
	if err := os.Remove(dir); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := store.SetGovernanceDeliveryHealth(GovernanceDeliveryHealth{State: GovernanceDeliveryHealthy, Class: GovernanceTransportAccepted, UpdatedAt: now.Add(time.Minute), LastAcceptedAt: now.Add(time.Minute)}); err != nil {
		t.Fatal(err)
	}
	view := store.Governance(now.Add(time.Minute))
	if view.DeliveryHealth.Class != GovernanceTransportAccepted || len(view.Attempts) != 0 || view.AttemptTotals.CumulativeAttempts != 0 || view.HealthTotals.StateFailures != 1 || view.HealthTotals.Recoveries != 1 {
		t.Fatalf("recovery evidence=%+v", view)
	}
}

func TestGovernanceRetryBackoffCapsAtFifteenMinutesWithoutTerminalAttemptLimit(t *testing.T) {
	t.Parallel()
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 7, 18, 9, 0, 0, 0, time.UTC)
	occ, _, err := store.UpsertGovernanceOccurrence(GovernanceOccurrence{Fingerprint: "retry", Kind: "policy_drift", OccurredAt: now}, now)
	if err != nil {
		t.Fatal(err)
	}
	target := GovernanceTargetRef("device", "subscription")
	wantBackoff := []time.Duration{time.Minute, 5 * time.Minute, 15 * time.Minute, 15 * time.Minute, 15 * time.Minute}
	for i, backoff := range wantBackoff {
		reservation, send, err := store.ReserveGovernanceAttempt(occ.DisplayID, target, now)
		if err != nil || !send {
			t.Fatalf("reserve %d: send=%v err=%v", i, send, err)
		}
		if _, err := store.CompleteGovernanceAttempt(reservation.ID, GovernanceTransportHTTPRetry, false, now); err != nil {
			t.Fatal(err)
		}
		view := store.Governance(now)
		latest := view.Attempts[len(view.Attempts)-1]
		if latest.RetryAt.IsZero() || !latest.RetryAt.Equal(now.Add(backoff)) {
			t.Fatalf("attempt %d retry_at=%v, want %v", i+1, latest.RetryAt, now.Add(backoff))
		}
		now = latest.RetryAt
	}
}

func TestGovernanceTerminalRejectionHasNoRetryObligation(t *testing.T) {
	t.Parallel()
	for _, class := range []string{GovernanceTransportHTTPRejected, GovernanceTransportRejected} {
		t.Run(class, func(t *testing.T) {
			store, err := Open(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			now := time.Date(2026, 7, 19, 11, 0, 0, 0, time.UTC)
			occurrence, _, err := store.UpsertGovernanceOccurrence(GovernanceOccurrence{Fingerprint: "terminal-" + class, Kind: "policy_drift", OccurredAt: now}, now)
			if err != nil {
				t.Fatal(err)
			}
			target := GovernanceTargetRef("device", "subscription")
			reservation, send, err := store.ReserveGovernanceAttempt(occurrence.DisplayID, target, now)
			if err != nil || !send {
				t.Fatalf("initial reserve send=%v err=%v", send, err)
			}
			if _, err := store.CompleteGovernanceAttempt(reservation.ID, class, false, now); err != nil {
				t.Fatal(err)
			}
			view := store.Governance(now)
			if len(view.Attempts) != 1 || view.Attempts[0].Class != class || !view.Attempts[0].RetryAt.IsZero() || view.AttemptTotals.RetryPending != 0 || view.AttemptTotals.Rejected != 1 {
				t.Fatalf("terminal rejection evidence=%+v totals=%+v", view.Attempts, view.AttemptTotals)
			}
			if retry, send, err := store.ReserveGovernanceAttempt(occurrence.DisplayID, target, now.Add(24*time.Hour)); err != nil || send || retry.Class != class {
				t.Fatalf("terminal rejection retried: reservation=%+v send=%v err=%v", retry, send, err)
			}
		})
	}
}

func TestGovernanceExpiryResolvesEpisodeAndStopsRetry(t *testing.T) {
	t.Parallel()
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 7, 18, 9, 0, 0, 0, time.UTC)
	record := GovernanceOccurrence{Fingerprint: "expires", Kind: "policy_drift", OccurredAt: now, ExpiresAt: now.Add(time.Minute)}
	occ, _, err := store.UpsertGovernanceOccurrence(record, now)
	if err != nil {
		t.Fatal(err)
	}
	target := GovernanceTargetRef("device", "subscription")
	reservation, send, err := store.ReserveGovernanceAttempt(occ.DisplayID, target, now)
	if err != nil || !send {
		t.Fatalf("initial reserve send=%v err=%v", send, err)
	}
	if _, err := store.CompleteGovernanceAttempt(reservation.ID, GovernanceTransportHTTPRetry, false, now); err != nil {
		t.Fatal(err)
	}
	if observed, err := store.ObserveGovernanceOccurrences([]GovernanceOccurrence{record}, false, now.Add(time.Minute)); err != nil || len(observed) != 0 {
		t.Fatalf("expired observation=%+v err=%v", observed, err)
	}
	if _, send, err := store.ReserveGovernanceAttempt(occ.DisplayID, target, now.Add(time.Minute)); err != nil || send {
		t.Fatalf("expired retry send=%v err=%v", send, err)
	}
	view := store.Governance(now.Add(time.Minute))
	if len(view.Occurrences) != 1 || view.Occurrences[0].ResolvedAt.IsZero() {
		t.Fatalf("expired occurrence=%+v", view.Occurrences)
	}
}

func TestGovernanceReactivatedFingerprintCreatesNewEpisodeAndReceiptNamespace(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	store, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 7, 18, 9, 0, 0, 0, time.UTC)
	fingerprint := "sha256:" + strings.Repeat("b", 64)
	first, _, err := store.UpsertGovernanceOccurrence(GovernanceOccurrence{Fingerprint: fingerprint, Kind: "policy_drift", State: "open", OccurredAt: now}, now)
	if err != nil {
		t.Fatal(err)
	}
	target := GovernanceTargetRef("device", "subscription")
	reservation, send, err := store.ReserveGovernanceAttempt(first.DisplayID, target, now)
	if err != nil || !send {
		t.Fatalf("reserve first: send=%v err=%v", send, err)
	}
	if _, err := store.CompleteGovernanceAttempt(reservation.ID, GovernanceTransportAccepted, true, now); err != nil {
		t.Fatal(err)
	}
	if err := store.ResolveGovernanceOccurrences(nil, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	second, created, err := reopened.UpsertGovernanceOccurrence(GovernanceOccurrence{Fingerprint: fingerprint, Kind: "policy_drift", State: "open", OccurredAt: now.Add(2 * time.Minute)}, now.Add(2*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if !created || second.DisplayID == first.DisplayID {
		t.Fatalf("reactivation created=%v first=%s second=%s", created, first.DisplayID, second.DisplayID)
	}
	if GovernanceReceiptKey(first.DisplayID, target) == GovernanceReceiptKey(second.DisplayID, target) {
		t.Fatal("reactivated episode reused receipt namespace")
	}
	if _, send, err := reopened.ReserveGovernanceAttempt(second.DisplayID, target, now.Add(2*time.Minute)); err != nil || !send {
		t.Fatalf("reactivated episode suppressed by resolved receipt: send=%v err=%v", send, err)
	}
	if got := reopened.Governance(now); len(got.Occurrences) != 2 || got.Occurrences[0].ResolvedAt.IsZero() || !got.Occurrences[1].ResolvedAt.IsZero() {
		t.Fatalf("episodes=%+v", got.Occurrences)
	}
}

func TestGovernanceReservationReservesCompletionCapacityAndInterruptedRetry(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	store, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	store.governanceMaxAttempts = 1
	store.governanceMaxItems = 1
	now := time.Date(2026, 7, 18, 9, 0, 0, 0, time.UTC)
	occ, _, err := store.UpsertGovernanceOccurrence(GovernanceOccurrence{Fingerprint: "reserve", Kind: "policy_drift", OccurredAt: now}, now)
	if err != nil {
		t.Fatal(err)
	}
	reservation, send, err := store.ReserveGovernanceAttempt(occ.DisplayID, GovernanceTargetRef("device", "sub"), now)
	if err != nil || !send || reservation.Class != GovernanceTransportReserved {
		t.Fatalf("reservation=%+v send=%v err=%v", reservation, send, err)
	}
	if _, err := store.CompleteGovernanceAttempt(reservation.ID, GovernanceTransportAccepted, true, now.Add(time.Second)); err != nil {
		t.Fatalf("completion required unreserved capacity: %v", err)
	}
	if len(store.Governance(now).Receipts) != 1 {
		t.Fatal("accepted completion was not durably recorded")
	}

	interruptedDir := t.TempDir()
	interrupted, err := Open(interruptedDir)
	if err != nil {
		t.Fatal(err)
	}
	occ, _, _ = interrupted.UpsertGovernanceOccurrence(GovernanceOccurrence{Fingerprint: "interrupted", Kind: "policy_drift", OccurredAt: now}, now)
	pending, send, err := interrupted.ReserveGovernanceAttempt(occ.DisplayID, GovernanceTargetRef("device", "sub"), now)
	if err != nil || !send {
		t.Fatalf("pending reserve: send=%v err=%v", send, err)
	}
	reopened, err := Open(interruptedDir)
	if err != nil {
		t.Fatal(err)
	}
	if _, send, err := reopened.ReserveGovernanceAttempt(occ.DisplayID, GovernanceTargetRef("device", "sub"), now.Add(30*time.Second)); err != nil || send {
		t.Fatalf("interrupted reservation retried early: send=%v err=%v", send, err)
	}
	retry, send, err := reopened.ReserveGovernanceAttempt(occ.DisplayID, GovernanceTargetRef("device", "sub"), now.Add(time.Minute))
	if err != nil || !send || retry.ID == pending.ID || retry.TransportCount != 1 {
		t.Fatalf("interrupted retry=%+v send=%v err=%v", retry, send, err)
	}
	view := reopened.Governance(now.Add(time.Minute))
	if len(view.Attempts) != 2 || view.Attempts[0].Class != GovernanceTransportInterrupted || !view.Attempts[0].At.Equal(now) || !view.Attempts[0].RetryAt.IsZero() || view.Attempts[1].Class != GovernanceTransportReserved {
		t.Fatalf("immutable interrupted evidence=%+v", view.Attempts)
	}
}

func TestGovernanceTargetLifecycleRetiresBindingDetailButPreservesTotals(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		retire func(*Store, time.Time) error
	}{
		{name: "subscription deletion", retire: func(store *Store, _ time.Time) error { return store.RemovePushSubscription("sub") }},
		{name: "device pruning", retire: func(store *Store, now time.Time) error { _, err := store.PruneDevices(now.Add(-time.Hour)); return err }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store, err := Open(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			now := time.Date(2026, 7, 18, 9, 0, 0, 0, time.UTC)
			if err := store.AddDevice(DeviceGrant{ID: "device", CreatedAt: now.Add(-24 * time.Hour)}); err != nil {
				t.Fatal(err)
			}
			if err := store.AddPushSubscription(PushSubscription{ID: "sub", DeviceID: "device", Endpoint: "https://push/sub", P256DH: "key", Auth: "auth"}); err != nil {
				t.Fatal(err)
			}
			occ, _, _ := store.UpsertGovernanceOccurrence(GovernanceOccurrence{Fingerprint: "long-lived", Kind: "policy_drift", OccurredAt: now}, now)
			reservation, _, _ := store.ReserveGovernanceAttempt(occ.DisplayID, GovernanceTargetRef("device", "sub"), now)
			if _, err := store.CompleteGovernanceAttempt(reservation.ID, GovernanceTransportNetworkRetry, false, now); err != nil {
				t.Fatal(err)
			}
			before := store.Governance(now).AttemptTotals
			if err := tc.retire(store, now); err != nil {
				t.Fatal(err)
			}
			view := store.Governance(now)
			if len(view.Attempts) != 1 || view.Attempts[0].RetiredAt.IsZero() || !view.Attempts[0].RetryAt.IsZero() || len(view.Receipts) != 0 || view.AttemptTotals.CumulativeAttempts != before.CumulativeAttempts || view.AttemptTotals.RetryPending != 0 {
				t.Fatalf("retired view=%+v totals before=%+v", view, before)
			}
		})
	}
}

func TestGovernanceCompletedTargetEvidenceRetainsReceiptUntilCompaction(t *testing.T) {
	t.Parallel()
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 7, 19, 9, 0, 0, 0, time.UTC)
	if err := store.AddDevice(DeviceGrant{ID: "device", CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := store.AddPushSubscription(PushSubscription{ID: "sub", DeviceID: "device", Endpoint: "https://push/sub", P256DH: "key", Auth: "auth"}); err != nil {
		t.Fatal(err)
	}
	occ, _, _ := store.UpsertGovernanceOccurrence(GovernanceOccurrence{Fingerprint: "accepted", Kind: "policy_drift", OccurredAt: now}, now)
	reservation, _, _ := store.ReserveGovernanceAttempt(occ.DisplayID, GovernanceTargetRef("device", "sub"), now)
	if _, err := store.CompleteGovernanceAttempt(reservation.ID, GovernanceTransportAccepted, true, now); err != nil {
		t.Fatal(err)
	}
	if err := store.RemovePushSubscription("sub"); err != nil {
		t.Fatal(err)
	}
	view := store.Governance(now)
	if len(view.Attempts) != 1 || view.Attempts[0].RetiredAt.IsZero() || len(view.Receipts) != 1 || view.Receipts[0].RetiredAt.IsZero() || view.AttemptTotals.Accepted != 1 {
		t.Fatalf("retained completed target evidence=%+v", view)
	}
	if err := store.CompactGovernance(view.Attempts[0].RetiredAt.Add(governanceRetention + time.Second)); err != nil {
		t.Fatal(err)
	}
	unread := store.Governance(now)
	if len(unread.Attempts) != 1 || len(unread.Receipts) != 1 {
		t.Fatalf("unread governance evidence compacted: %+v", unread)
	}
	if _, err := store.MarkAttentionRead(occ.AttentionSeq); err != nil {
		t.Fatal(err)
	}
	if err := store.CompactGovernance(view.Attempts[0].RetiredAt.Add(governanceRetention + time.Second)); err != nil {
		t.Fatal(err)
	}
	compacted := store.Governance(now)
	if len(compacted.Occurrences) != 1 || len(compacted.Attempts) != 0 || len(compacted.Receipts) != 0 || compacted.AttemptTotals.Accepted != 1 {
		t.Fatalf("retired target detail did not compact independently: %+v", compacted)
	}
}

func TestGovernancePendingReservationBecomesRetiredNonDueEvidence(t *testing.T) {
	t.Parallel()
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 7, 19, 9, 0, 0, 0, time.UTC)
	if err := store.AddDevice(DeviceGrant{ID: "device", CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := store.AddPushSubscription(PushSubscription{ID: "sub", DeviceID: "device", Endpoint: "https://push/pending", P256DH: "key", Auth: "auth"}); err != nil {
		t.Fatal(err)
	}
	occ, _, _ := store.UpsertGovernanceOccurrence(GovernanceOccurrence{Fingerprint: "pending-retire", Kind: "policy_drift", OccurredAt: now}, now)
	target := GovernanceTargetRef("device", "sub")
	reserved, send, err := store.ReserveGovernanceAttempt(occ.DisplayID, target, now)
	if err != nil || !send {
		t.Fatalf("reserve send=%v err=%v", send, err)
	}
	if err := store.RemovePushSubscriptionAt("sub", now.Add(30*time.Second)); err != nil {
		t.Fatal(err)
	}
	view := store.Governance(now.Add(time.Minute))
	if len(view.Attempts) != 1 || view.Attempts[0].Class != GovernanceTransportTargetRetired || !view.Attempts[0].At.Equal(reserved.At) || view.Attempts[0].RetiredAt.IsZero() || !view.Attempts[0].RetryAt.IsZero() || view.AttemptTotals.RetryPending != 0 || view.AttemptTotals.TargetRetired != 1 {
		t.Fatalf("pending retirement evidence=%+v totals=%+v", view.Attempts, view.AttemptTotals)
	}
}

func TestGovernanceRetiredInFlightCompletionPersistsActualOutcome(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		class    string
		accepted bool
	}{
		{name: "accepted", class: GovernanceTransportAccepted, accepted: true},
		{name: "failed", class: GovernanceTransportNetworkRetry},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store, err := Open(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			now := time.Date(2026, 7, 19, 10, 0, 0, 0, time.UTC)
			if err := store.AddDevice(DeviceGrant{ID: "device", CreatedAt: now}); err != nil {
				t.Fatal(err)
			}
			if err := store.AddPushSubscription(PushSubscription{ID: "sub", DeviceID: "device", Endpoint: "https://push/in-flight", P256DH: "key", Auth: "auth"}); err != nil {
				t.Fatal(err)
			}
			occ, _, _ := store.UpsertGovernanceOccurrence(GovernanceOccurrence{Fingerprint: "in-flight-" + tc.name, Kind: "policy_drift", OccurredAt: now}, now)
			reservation, _, _ := store.ReserveGovernanceAttempt(occ.DisplayID, GovernanceTargetRef("device", "sub"), now)
			if !store.BeginGovernanceTransport(reservation.ID) {
				t.Fatal("reserved transport did not enter in-flight state")
			}
			retiredAt := now.Add(30 * time.Second)
			if err := store.RemovePushSubscriptionAt("sub", retiredAt); err != nil {
				t.Fatal(err)
			}
			outcome, err := store.CompleteGovernanceAttempt(reservation.ID, tc.class, tc.accepted, now.Add(time.Minute))
			if err != nil {
				t.Fatal(err)
			}
			if outcome.Disposition != GovernanceCompletionRetired {
				t.Fatalf("completion outcome=%+v", outcome)
			}
			view := store.Governance(now.Add(time.Minute))
			if len(view.Attempts) != 1 || view.Attempts[0].Class != tc.class || !view.Attempts[0].RetiredAt.Equal(retiredAt) || !view.Attempts[0].RetryAt.IsZero() || view.AttemptTotals.RetryPending != 0 || view.AttemptTotals.TargetRetired != 0 {
				t.Fatalf("retired completion evidence=%+v totals=%+v", view.Attempts, view.AttemptTotals)
			}
			if tc.accepted {
				if len(view.Receipts) != 1 || !view.Receipts[0].RetiredAt.Equal(retiredAt) || view.AttemptTotals.Accepted != 1 {
					t.Fatalf("retired acceptance evidence=%+v totals=%+v", view.Receipts, view.AttemptTotals)
				}
			} else if len(view.Receipts) != 0 || view.AttemptTotals.RetryableFailures != 1 {
				t.Fatalf("retired failure evidence=%+v totals=%+v", view.Receipts, view.AttemptTotals)
			}
		})
	}
}

func TestGovernanceEndpointReassignmentRetiresOldTargetBeforeMove(t *testing.T) {
	t.Parallel()
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 7, 19, 9, 0, 0, 0, time.UTC)
	for _, deviceID := range []string{"old-device", "new-device"} {
		if err := store.AddDevice(DeviceGrant{ID: deviceID, CreatedAt: now}); err != nil {
			t.Fatal(err)
		}
	}
	sub := PushSubscription{ID: "sub", DeviceID: "old-device", Endpoint: "https://push/same", P256DH: "key", Auth: "auth", LastSeenAt: now}
	if err := store.AddPushSubscription(sub); err != nil {
		t.Fatal(err)
	}
	occ, _, _ := store.UpsertGovernanceOccurrence(GovernanceOccurrence{Fingerprint: "reassign", Kind: "policy_drift", OccurredAt: now}, now)
	oldTarget := GovernanceTargetRef("old-device", "sub")
	reservation, _, _ := store.ReserveGovernanceAttempt(occ.DisplayID, oldTarget, now)
	if _, err := store.CompleteGovernanceAttempt(reservation.ID, GovernanceTransportNetworkRetry, false, now); err != nil {
		t.Fatal(err)
	}
	sub.DeviceID = "new-device"
	sub.ID = "sub-new"
	sub.LastSeenAt = now.Add(time.Minute)
	if err := store.AddPushSubscription(sub); err != nil {
		t.Fatal(err)
	}
	view := store.Governance(now.Add(time.Minute))
	if len(view.Attempts) != 1 || view.Attempts[0].RetiredAt.IsZero() || view.AttemptTotals.RetryPending != 0 {
		t.Fatalf("old target remained binding: %+v", view)
	}
	active := store.ActivePushSubscriptionsForDevice("new-device")
	if len(active) != 1 || active[0].ID != "sub-new" {
		t.Fatalf("subscription was not reassigned after retirement: %+v", active)
	}
	if _, send, err := store.ReserveGovernanceAttempt(occ.DisplayID, GovernanceTargetRef("new-device", "sub-new"), now.Add(time.Minute)); err != nil || !send {
		t.Fatalf("new target reserve send=%v err=%v", send, err)
	}
}

func TestGovernanceExpiryResolvesOmittedOccurrenceWithoutAbsenceAuthority(t *testing.T) {
	t.Parallel()
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 7, 19, 9, 0, 0, 0, time.UTC)
	occ, _, _ := store.UpsertGovernanceOccurrence(GovernanceOccurrence{Fingerprint: "omitted-expiry", Kind: "policy_drift", OccurredAt: now, ExpiresAt: now.Add(time.Minute)}, now)
	target := GovernanceTargetRef("device", "sub")
	reservation, _, _ := store.ReserveGovernanceAttempt(occ.DisplayID, target, now)
	if _, err := store.CompleteGovernanceAttempt(reservation.ID, GovernanceTransportNetworkRetry, false, now); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ObserveGovernanceOccurrences(nil, false, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	view := store.Governance(now.Add(time.Minute))
	if view.Occurrences[0].ResolvedAt.IsZero() || view.AttemptTotals.RetryPending != 0 {
		t.Fatalf("expired omitted occurrence remained binding: %+v", view)
	}
}

func TestGovernanceAttemptAggregatesSeparateCumulativeAndCurrentState(t *testing.T) {
	t.Parallel()
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 7, 19, 9, 0, 0, 0, time.UTC)
	occ, _, _ := store.UpsertGovernanceOccurrence(GovernanceOccurrence{Fingerprint: "aggregate", Kind: "policy_drift", OccurredAt: now}, now)
	target := GovernanceTargetRef("device", "sub")
	reservation, _, _ := store.ReserveGovernanceAttempt(occ.DisplayID, target, now)
	if _, err := store.CompleteGovernanceAttempt(reservation.ID, GovernanceTransportNetworkRetry, false, now); err != nil {
		t.Fatal(err)
	}
	failed := store.Governance(now)
	if failed.AttemptTotals.CumulativeAttempts != 1 || failed.AttemptTotals.RetryPending != 1 || failed.AttemptTotals.RetryableFailures != 1 {
		t.Fatalf("failed aggregate=%+v", failed.AttemptTotals)
	}
	retry, send, err := store.ReserveGovernanceAttempt(occ.DisplayID, target, now.Add(time.Minute))
	if err != nil || !send {
		t.Fatalf("retry reserve send=%v err=%v", send, err)
	}
	if _, err := store.CompleteGovernanceAttempt(retry.ID, GovernanceTransportAccepted, true, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	succeeded := store.Governance(now.Add(time.Minute))
	if succeeded.AttemptTotals.CumulativeAttempts != 2 || succeeded.AttemptTotals.RetryPending != 0 || succeeded.AttemptTotals.Accepted != 1 || succeeded.AttemptTotals.RetryableFailures != 1 {
		t.Fatalf("success aggregate=%+v", succeeded.AttemptTotals)
	}
	before := succeeded.AttemptTotals.CumulativeAttempts
	if err := store.SetGovernanceDeliveryHealth(GovernanceDeliveryHealth{State: GovernanceDeliveryDegraded, Class: GovernanceTransportPartial, UpdatedAt: now.Add(2 * time.Minute)}); err != nil {
		t.Fatal(err)
	}
	if got := store.Governance(now).AttemptTotals.CumulativeAttempts; got != before {
		t.Fatalf("partial health inflated attempts: got=%d want=%d", got, before)
	}
}

func TestGovernanceResolvedReservationDoesNotBindReceiptCapacity(t *testing.T) {
	t.Parallel()
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 7, 19, 10, 0, 0, 0, time.UTC)
	old, _, _ := store.UpsertGovernanceOccurrence(GovernanceOccurrence{Fingerprint: "old-crash", Kind: "policy_drift", OccurredAt: now}, now)
	oldReservation, send, err := store.ReserveGovernanceAttempt(old.DisplayID, GovernanceTargetRef("device", "old"), now)
	if err != nil || !send {
		t.Fatalf("old reservation send=%v err=%v", send, err)
	}
	if err := store.ResolveGovernanceOccurrences(nil, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	newOccurrence, _, _ := store.UpsertGovernanceOccurrence(GovernanceOccurrence{Fingerprint: "new-active", Kind: "policy_drift", OccurredAt: now.Add(2 * time.Minute)}, now.Add(2*time.Minute))
	store.governanceMaxItems = 1
	newReservation, send, err := store.ReserveGovernanceAttempt(newOccurrence.DisplayID, GovernanceTargetRef("device", "new"), now.Add(2*time.Minute))
	if err != nil || !send {
		t.Fatalf("resolved reservation consumed receipt capacity: send=%v err=%v", send, err)
	}
	view := store.Governance(now.Add(2 * time.Minute))
	if len(view.Attempts) != 2 || view.Attempts[0].OccurrenceID != old.DisplayID || view.Attempts[0].Class != GovernanceTransportReserved || view.Attempts[1].OccurrenceID != newOccurrence.DisplayID || newReservation.ID == oldReservation.ID {
		t.Fatalf("reservation audit detail=%+v", view.Attempts)
	}
}

func TestGovernanceStateWriteRecoveryPreservesRequestedPosture(t *testing.T) {
	for _, tc := range []struct {
		name   string
		health GovernanceDeliveryHealth
	}{
		{name: "suppressed", health: GovernanceDeliveryHealth{State: GovernanceDeliverySuppressed, Class: GovernanceTransportSuppressed}},
		{name: "partial", health: GovernanceDeliveryHealth{State: GovernanceDeliveryDegraded, Class: GovernanceTransportPartial}},
		{name: "unavailable", health: GovernanceDeliveryHealth{State: GovernanceDeliveryUnavailable, Class: GovernanceTransportNoSubscription}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			store, err := Open(dir)
			if err != nil {
				t.Fatal(err)
			}
			now := time.Date(2026, 7, 19, 10, 0, 0, 0, time.UTC)
			if err := os.Remove(dir); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(dir, []byte("block state"), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, _, err := store.UpsertGovernanceOccurrence(GovernanceOccurrence{Fingerprint: "write-" + tc.name, Kind: "policy_drift", OccurredAt: now}, now); err == nil {
				t.Fatal("expected injected state write failure")
			}
			if err := os.Remove(dir); err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			tc.health.UpdatedAt = now.Add(time.Minute)
			if err := store.SetGovernanceDeliveryHealth(tc.health); err != nil {
				t.Fatal(err)
			}
			view := store.Governance(now.Add(time.Minute))
			if view.DeliveryHealth.State != tc.health.State || view.DeliveryHealth.Class != tc.health.Class || view.HealthTotals.StateFailures != 1 || view.HealthTotals.Recoveries != 1 {
				t.Fatalf("requested posture/recovery=%+v health totals=%+v", view.DeliveryHealth, view.HealthTotals)
			}
			reopened, err := Open(dir)
			if err != nil {
				t.Fatal(err)
			}
			persisted := reopened.Governance(now.Add(time.Minute))
			if persisted.DeliveryHealth.State != tc.health.State || persisted.DeliveryHealth.Class != tc.health.Class || persisted.HealthTotals.StateFailures != 1 || persisted.HealthTotals.Recoveries != 1 {
				t.Fatalf("persisted requested posture/recovery=%+v totals=%+v", persisted.DeliveryHealth, persisted.HealthTotals)
			}
		})
	}
}

func TestGovernanceLegacyTotalsLoadIntoAttemptAndHealthSplit(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	raw := `{"alert_settings":{"mode":"watch_and_act"},"governance_attempt_totals":{"total":8,"push_service_accepted":1,"rejected":1,"retry_pending":2,"partial":1,"state_write_failure":1,"recovery":1,"overflow":1}}`
	if err := os.WriteFile(filepath.Join(dir, "state.json"), []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	view := store.Governance(time.Now().UTC())
	if view.AttemptTotals.CumulativeAttempts != 4 || view.AttemptTotals.RetryableFailures != 2 || view.AttemptTotals.Accepted != 1 || view.AttemptTotals.Rejected != 1 {
		t.Fatalf("migrated attempt totals=%+v", view.AttemptTotals)
	}
	if view.HealthTotals != (GovernanceHealthEventTotals{PartialEpisodes: 1, StateFailures: 1, Recoveries: 1, Overflows: 1}) {
		t.Fatalf("migrated health totals=%+v", view.HealthTotals)
	}
}

func TestCompactGovernanceFailureRestoresDeepCopiedEvidence(t *testing.T) {
	dir := t.TempDir()
	store, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 7, 19, 9, 0, 0, 0, time.UTC)
	old := now.Add(-100 * 24 * time.Hour)
	occ, _, _ := store.UpsertGovernanceOccurrence(GovernanceOccurrence{Fingerprint: "compact-rollback", Kind: "policy_drift", OccurredAt: old}, old)
	reservation, _, _ := store.ReserveGovernanceAttempt(occ.DisplayID, GovernanceTargetRef("device", "sub"), old)
	if _, err := store.CompleteGovernanceAttempt(reservation.ID, GovernanceTransportAccepted, true, old); err != nil {
		t.Fatal(err)
	}
	if err := store.ResolveGovernanceOccurrences(nil, old.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.MarkAttentionRead(store.Attention().HighWaterSeq); err != nil {
		t.Fatal(err)
	}
	before := store.Governance(now)
	beforeAttention := store.Attention()
	store.saveHook = func(stage string) error {
		if stage == "rename" {
			return errors.New("injected compact rename failure")
		}
		return nil
	}
	if err := store.CompactGovernance(now); err == nil {
		t.Fatal("compaction persistence failure was ignored")
	}
	store.saveHook = nil
	after := store.Governance(now)
	if !reflect.DeepEqual(after.Occurrences, before.Occurrences) || !reflect.DeepEqual(after.Attempts, before.Attempts) || !reflect.DeepEqual(after.Receipts, before.Receipts) || after.AttemptTotals != before.AttemptTotals || !reflect.DeepEqual(store.Attention(), beforeAttention) {
		t.Fatalf("visible state changed after failed compaction:\nbefore=%+v\nafter=%+v", before, after)
	}
	reopened, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := reopened.Governance(now); !reflect.DeepEqual(got.Occurrences, before.Occurrences) || !reflect.DeepEqual(got.Attempts, before.Attempts) || !reflect.DeepEqual(got.Receipts, before.Receipts) || got.AttemptTotals != before.AttemptTotals {
		t.Fatalf("reopened evidence changed after failed compaction:\nbefore=%+v\nafter=%+v", before, got)
	}
}

func TestAtomicSavePreservesReadablePriorStateAndCleansTemporaryFiles(t *testing.T) {
	for _, stage := range []string{"write", "rename"} {
		t.Run(stage, func(t *testing.T) {
			dir := t.TempDir()
			store, err := Open(dir)
			if err != nil {
				t.Fatal(err)
			}
			if err := store.AddDevice(DeviceGrant{ID: "prior", CreatedAt: time.Now().UTC()}); err != nil {
				t.Fatal(err)
			}
			store.saveHook = func(got string) error {
				if got == stage {
					return errors.New("injected " + stage)
				}
				return nil
			}
			if err := store.AddDevice(DeviceGrant{ID: "new", CreatedAt: time.Now().UTC()}); err == nil {
				t.Fatal("injected save failure was ignored")
			}
			reopened, err := Open(dir)
			if err != nil {
				t.Fatalf("reopen after %s failure: %v", stage, err)
			}
			if _, ok := reopened.Device("prior"); !ok {
				t.Fatal("prior state was lost")
			}
			if _, ok := reopened.Device("new"); ok {
				t.Fatal("failed write replaced prior state")
			}
			temps, err := filepath.Glob(filepath.Join(dir, ".state-*.tmp"))
			if err != nil || len(temps) != 0 {
				t.Fatalf("temporary files=%v err=%v", temps, err)
			}
		})
	}
}

func TestAddPushSubscriptionRollsBackNewTargetOnPersistenceFailure(t *testing.T) {
	for _, stage := range []string{"write", "rename"} {
		t.Run(stage, func(t *testing.T) {
			dir := t.TempDir()
			store, err := Open(dir)
			if err != nil {
				t.Fatal(err)
			}
			now := time.Date(2026, 7, 19, 11, 0, 0, 0, time.UTC)
			if err := store.AddDevice(DeviceGrant{ID: "device", CreatedAt: now}); err != nil {
				t.Fatal(err)
			}
			before := store.PushSubscriptions()
			store.saveHook = func(got string) error {
				if got == stage {
					return errors.New("injected new-subscription " + stage + " failure")
				}
				return nil
			}
			failed := PushSubscription{ID: "failed", DeviceID: "device", Endpoint: "https://push.example/failed", P256DH: "key", Auth: "auth", CreatedAt: now}
			if err := store.AddPushSubscription(failed); err == nil {
				t.Fatal("new subscription persistence failure was ignored")
			}
			if got := store.PushSubscriptions(); !reflect.DeepEqual(got, before) {
				t.Fatalf("in-memory subscriptions changed after failed save: got=%+v want=%+v", got, before)
			}
			if got := store.ActivePushSubscriptions(); len(got) != 0 {
				t.Fatalf("failed target became active: %+v", got)
			}

			store.saveHook = nil
			if _, _, err := store.UpsertGovernanceOccurrence(GovernanceOccurrence{Fingerprint: "later-" + stage, Kind: "policy_drift", OccurredAt: now.Add(time.Minute)}, now.Add(time.Minute)); err != nil {
				t.Fatal(err)
			}
			reopened, err := Open(dir)
			if err != nil {
				t.Fatal(err)
			}
			if got := reopened.PushSubscriptions(); !reflect.DeepEqual(got, before) {
				t.Fatalf("later governance save persisted failed target: got=%+v want=%+v", got, before)
			}
		})
	}
}

func TestPushTargetRetirementIsAtomicAcrossEveryTopologyMutation(t *testing.T) {
	mutations := []string{"remove subscription", "prune device", "revoke device", "transfer endpoint"}
	for _, mutation := range mutations {
		for _, stage := range []string{"write", "rename"} {
			t.Run(mutation+"_"+stage, func(t *testing.T) {
				store, err := Open(t.TempDir())
				if err != nil {
					t.Fatal(err)
				}
				base := time.Date(2026, 7, 20, 18, 0, 0, 0, time.UTC)
				oldDevice := DeviceGrant{ID: "old-device", CreatedAt: base.Add(-24 * time.Hour)}
				if err := store.AddDevice(oldDevice); err != nil {
					t.Fatal(err)
				}
				if mutation == "transfer endpoint" {
					if err := store.AddDevice(DeviceGrant{ID: "new-device", CreatedAt: base}); err != nil {
						t.Fatal(err)
					}
				}
				subscription := PushSubscription{ID: "subscription", DeviceID: oldDevice.ID, Endpoint: "https://push.example/atomic", P256DH: "key", Auth: "auth", CreatedAt: base}
				if err := store.AddPushSubscription(subscription); err != nil {
					t.Fatal(err)
				}

				enableTestAlertDelivery(t, store)
				candidate := testAlertCandidate(t, rpc.AlertSourceDelivery, rpc.AlertKindDeliveryHealth, "delivery", strings.ReplaceAll(mutation, " ", "-"), base)
				if _, err := store.ObserveAlertSnapshot(testAlertSnapshot(base, []rpc.AlertSource{candidate.Source}, []rpc.AlertSource{candidate.Source}, rpc.AlertCoverageCurrent, candidate)); err != nil {
					t.Fatal(err)
				}
				alertTarget := AlertDeliveryTargetRef(oldDevice.ID, subscription.ID)
				alertReservation, send, err := store.BeginAlertDelivery(candidate.OccurrenceKey, alertTarget, base.Add(time.Second))
				if err != nil || !send {
					t.Fatalf("alert reservation send=%v err=%v", send, err)
				}
				governanceOccurrence, _, err := store.UpsertGovernanceOccurrence(GovernanceOccurrence{Fingerprint: "atomic-" + mutation, Kind: "policy_drift", OccurredAt: base}, base)
				if err != nil {
					t.Fatal(err)
				}
				governanceReservation, send, err := store.ReserveGovernanceAttempt(governanceOccurrence.DisplayID, GovernanceTargetRef(oldDevice.ID, subscription.ID), base)
				if err != nil || !send {
					t.Fatalf("governance reservation send=%v err=%v", send, err)
				}
				if _, err := store.CompleteGovernanceAttempt(governanceReservation.ID, GovernanceTransportNetworkRetry, false, base); err != nil {
					t.Fatal(err)
				}

				mutate := func() error {
					switch mutation {
					case "remove subscription":
						return store.RemovePushSubscriptionAt(subscription.ID, base.Add(2*time.Second))
					case "prune device":
						_, err := store.PruneDevices(base.Add(-time.Hour))
						return err
					case "revoke device":
						revoked := oldDevice
						revoked.RevokedAt = base.Add(2 * time.Second)
						return store.AddDevice(revoked)
					case "transfer endpoint":
						transferred := subscription
						transferred.ID = "transferred-subscription"
						transferred.DeviceID = "new-device"
						transferred.LastSeenAt = base.Add(2 * time.Second)
						return store.AddPushSubscription(transferred)
					default:
						return errors.New("unknown test mutation")
					}
				}
				store.saveHook = func(got string) error {
					if got == stage {
						return errors.New("injected atomic " + stage + " failure")
					}
					return nil
				}
				if err := mutate(); err == nil {
					t.Fatal("injected topology save failure was ignored")
				}
				if len(store.ActivePushSubscriptionsForDevice(oldDevice.ID)) != 1 {
					t.Fatalf("failed mutation changed active topology: %+v", store.PushSubscriptions())
				}
				if !store.data.AlertDelivery.RetiredTargets[alertTarget].IsZero() || store.data.AlertDelivery.Attempts[0].Class != AlertDeliveryAttemptReserved || !store.alertDeliveryInFlight[alertReservation.AttemptID] {
					t.Fatalf("failed mutation partially retired alert target: %+v", store.data.AlertDelivery)
				}
				governanceAfterFailure := store.Governance(base)
				if len(governanceAfterFailure.Attempts) != 1 || !governanceAfterFailure.Attempts[0].RetiredAt.IsZero() || governanceAfterFailure.AttemptTotals.RetryPending != 1 {
					t.Fatalf("failed mutation partially retired governance target: %+v", governanceAfterFailure)
				}

				store.saveHook = nil
				if err := mutate(); err != nil {
					t.Fatal(err)
				}
				if store.data.AlertDelivery.RetiredTargets[alertTarget].IsZero() || store.data.AlertDelivery.Attempts[0].Class != AlertDeliveryAttemptRetired || store.alertDeliveryInFlight[alertReservation.AttemptID] {
					t.Fatalf("successful mutation did not retire alert target: %+v", store.data.AlertDelivery)
				}
				governanceAfterSuccess := store.Governance(base)
				if len(governanceAfterSuccess.Attempts) != 1 || governanceAfterSuccess.Attempts[0].RetiredAt.IsZero() || governanceAfterSuccess.AttemptTotals.RetryPending != 0 {
					t.Fatalf("successful mutation did not retire governance target: %+v", governanceAfterSuccess)
				}
				switch mutation {
				case "transfer endpoint":
					if len(store.ActivePushSubscriptionsForDevice(oldDevice.ID)) != 0 || len(store.ActivePushSubscriptionsForDevice("new-device")) != 1 {
						t.Fatalf("endpoint transfer topology=%+v", store.PushSubscriptions())
					}
				default:
					if len(store.ActivePushSubscriptionsForDevice(oldDevice.ID)) != 0 {
						t.Fatalf("retired topology remained active: %+v", store.PushSubscriptions())
					}
				}
			})
		}
	}
}

func TestRevokedDeviceIdentityCannotBeReactivated(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	base := time.Date(2026, 7, 20, 18, 30, 0, 0, time.UTC)
	revoked := DeviceGrant{ID: "revoked-device", CreatedAt: base, RevokedAt: base.Add(time.Minute)}
	if err := store.AddDevice(revoked); err != nil {
		t.Fatal(err)
	}
	activeReuse := revoked
	activeReuse.RevokedAt = time.Time{}
	if err := store.AddDevice(activeReuse); err == nil {
		t.Fatal("revoked device identity was reactivated")
	}
	if _, ok := store.Device(revoked.ID); ok {
		t.Fatal("failed reactivation changed persisted revocation")
	}
	if err := store.AddDevice(DeviceGrant{ID: "fresh-device", CreatedAt: base.Add(2 * time.Minute)}); err != nil {
		t.Fatalf("fresh pairing identity was rejected: %v", err)
	}
}

func TestCrossDeviceEndpointTransferRotatesIdentityAndRejectsTransferBackReplay(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	base := time.Date(2026, 7, 20, 18, 45, 0, 0, time.UTC)
	for _, deviceID := range []string{"device-a", "device-b"} {
		if err := store.AddDevice(DeviceGrant{ID: deviceID, CreatedAt: base}); err != nil {
			t.Fatal(err)
		}
	}
	endpoint := "https://push.example/rotating"
	if err := store.AddPushSubscription(PushSubscription{ID: "sub-a", DeviceID: "device-a", Endpoint: endpoint, P256DH: "key-a", Auth: "auth-a", CreatedAt: base}); err != nil {
		t.Fatal(err)
	}
	enableTestAlertDelivery(t, store)
	candidate := testAlertCandidate(t, rpc.AlertSourceDelivery, rpc.AlertKindDeliveryHealth, "delivery", "transfer-back", base)
	if _, err := store.ObserveAlertSnapshot(testAlertSnapshot(base, []rpc.AlertSource{candidate.Source}, []rpc.AlertSource{candidate.Source}, rpc.AlertCoverageCurrent, candidate)); err != nil {
		t.Fatal(err)
	}
	if err := store.AddPushSubscription(PushSubscription{ID: "sub-b", DeviceID: "device-b", Endpoint: endpoint, P256DH: "key-b", Auth: "auth-b", CreatedAt: base.Add(time.Second), LastSeenAt: base.Add(time.Second)}); err != nil {
		t.Fatal(err)
	}
	activeB := store.ActivePushSubscriptionsForDevice("device-b")
	if len(activeB) != 1 || activeB[0].ID != "sub-b" {
		t.Fatalf("cross-device transfer reused prior identity: %+v", activeB)
	}
	if store.data.AlertDelivery.RetiredTargets[AlertDeliveryTargetRef("device-a", "sub-a")].IsZero() {
		t.Fatal("old target was not retired")
	}
	replay := PushSubscription{ID: "sub-a", DeviceID: "device-a", Endpoint: endpoint, P256DH: "key-a2", Auth: "auth-a2", CreatedAt: base.Add(2 * time.Second), LastSeenAt: base.Add(2 * time.Second)}
	if err := store.AddPushSubscription(replay); err == nil {
		t.Fatal("transfer-back reused a retired target identity")
	}
	if active := store.ActivePushSubscriptionsForDevice("device-b"); len(active) != 1 || active[0].ID != "sub-b" {
		t.Fatalf("rejected transfer-back changed topology: %+v", store.PushSubscriptions())
	}
	fresh := replay
	fresh.ID = "sub-a-fresh"
	if err := store.AddPushSubscription(fresh); err != nil {
		t.Fatal(err)
	}
	if _, send, err := store.BeginAlertDelivery(candidate.OccurrenceKey, AlertDeliveryTargetRef("device-a", fresh.ID), base.Add(3*time.Second)); err != nil || !send {
		t.Fatalf("fresh transfer-back target send=%v err=%v", send, err)
	}
	refresh := fresh
	refresh.ID = "ignored-refresh-id"
	refresh.LastSeenAt = base.Add(4 * time.Second)
	if err := store.AddPushSubscription(refresh); err != nil {
		t.Fatal(err)
	}
	activeA := store.ActivePushSubscriptionsForDevice("device-a")
	if len(activeA) != 1 || activeA[0].ID != fresh.ID {
		t.Fatalf("same-device refresh rotated stable identity: %+v", activeA)
	}
}

func TestPushSubscriptionRejectsDuplicateAndRetiredTargetIdentity(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	base := time.Date(2026, 7, 20, 18, 50, 0, 0, time.UTC)
	if err := store.AddDevice(DeviceGrant{ID: "device", CreatedAt: base}); err != nil {
		t.Fatal(err)
	}
	enableTestAlertDelivery(t, store)
	candidate := testAlertCandidate(t, rpc.AlertSourceDelivery, rpc.AlertKindDeliveryHealth, "delivery", "target-identity", base)
	if _, err := store.ObserveAlertSnapshot(testAlertSnapshot(base, []rpc.AlertSource{candidate.Source}, []rpc.AlertSource{candidate.Source}, rpc.AlertCoverageCurrent, candidate)); err != nil {
		t.Fatal(err)
	}
	first := PushSubscription{ID: "subscription", DeviceID: "device", Endpoint: "https://push.example/first", P256DH: "key", Auth: "auth", CreatedAt: base}
	if err := store.AddPushSubscription(first); err != nil {
		t.Fatal(err)
	}
	duplicate := first
	duplicate.Endpoint = "https://push.example/duplicate"
	if err := store.AddPushSubscription(duplicate); err == nil {
		t.Fatal("second endpoint reused an active target identity")
	}
	if err := store.RemovePushSubscriptionAt(first.ID, base.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	reused := first
	reused.Endpoint = "https://push.example/reused"
	if err := store.AddPushSubscription(reused); err == nil {
		t.Fatal("new endpoint reused a retired target identity")
	}
	if err := store.AddPushSubscription(PushSubscription{DeviceID: "device", Endpoint: "https://push.example/empty", P256DH: "key", Auth: "auth"}); err == nil {
		t.Fatal("empty subscription identity was accepted")
	}
	fresh := reused
	fresh.ID = "fresh-subscription"
	if err := store.AddPushSubscription(fresh); err != nil {
		t.Fatalf("fresh target identity rejected: %v", err)
	}
	if active := store.ActivePushSubscriptionsForDevice("device"); len(active) != 1 || active[0].ID != fresh.ID {
		t.Fatalf("active subscriptions=%+v", active)
	}
}

func TestGovernanceQuietPersistenceRecoversStateWriteFailure(t *testing.T) {
	for _, tc := range []struct {
		name         string
		failureStage string
		recover      func(*Store, time.Time) error
	}{
		{
			name:         "empty authoritative observation",
			failureStage: "write",
			recover: func(store *Store, now time.Time) error {
				_, err := store.ObserveGovernanceOccurrences(nil, true, now)
				return err
			},
		},
		{
			name:         "no-op compaction",
			failureStage: "rename",
			recover: func(store *Store, now time.Time) error {
				return store.CompactGovernance(now)
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			store, err := Open(dir)
			if err != nil {
				t.Fatal(err)
			}
			now := time.Date(2026, 7, 19, 11, 0, 0, 0, time.UTC)
			quiet := GovernanceDeliveryHealth{State: GovernanceDeliveryUnavailable, Class: GovernanceTransportNoSubscription, UpdatedAt: now}
			if err := store.SetGovernanceDeliveryHealth(quiet); err != nil {
				t.Fatal(err)
			}
			store.saveHook = func(stage string) error {
				if stage == tc.failureStage {
					return errors.New("injected quiet " + tc.failureStage + " failure")
				}
				return nil
			}
			if _, _, err := store.UpsertGovernanceOccurrence(GovernanceOccurrence{Fingerprint: "quiet-failure-" + tc.failureStage, Kind: "policy_drift", OccurredAt: now}, now); err == nil {
				t.Fatal("governance persistence failure was ignored")
			}
			if got := store.Governance(now).DeliveryHealth; got.Class != GovernanceTransportStateWrite {
				t.Fatalf("volatile failure not visible: %+v", got)
			}

			store.saveHook = nil
			if err := tc.recover(store, now.Add(time.Minute)); err != nil {
				t.Fatal(err)
			}
			view := store.Governance(now.Add(time.Minute))
			if view.DeliveryHealth.State != quiet.State || view.DeliveryHealth.Class != quiet.Class || view.HealthTotals.StateFailures != 1 || view.HealthTotals.Recoveries != 1 {
				t.Fatalf("quiet recovery view=%+v totals=%+v", view.DeliveryHealth, view.HealthTotals)
			}
			reopened, err := Open(dir)
			if err != nil {
				t.Fatal(err)
			}
			persisted := reopened.Governance(now.Add(time.Minute))
			if persisted.DeliveryHealth.State != quiet.State || persisted.DeliveryHealth.Class != quiet.Class || persisted.HealthTotals.StateFailures != 1 || persisted.HealthTotals.Recoveries != 1 {
				t.Fatalf("reopened quiet recovery=%+v totals=%+v", persisted.DeliveryHealth, persisted.HealthTotals)
			}
		})
	}
}

func TestGovernanceCompletionFailureReleasesRetiredInFlightCapacity(t *testing.T) {
	for _, stage := range []string{"write", "rename"} {
		for _, outcome := range []struct {
			name     string
			class    string
			accepted bool
		}{
			{name: "accepted", class: GovernanceTransportAccepted, accepted: true},
			{name: "failed", class: GovernanceTransportNetworkRetry},
		} {
			t.Run(stage+"/"+outcome.name, func(t *testing.T) {
				store, err := Open(t.TempDir())
				if err != nil {
					t.Fatal(err)
				}
				now := time.Date(2026, 7, 19, 11, 0, 0, 0, time.UTC)
				quiet := GovernanceDeliveryHealth{State: GovernanceDeliveryUnavailable, Class: GovernanceTransportNoSubscription, UpdatedAt: now}
				if err := store.SetGovernanceDeliveryHealth(quiet); err != nil {
					t.Fatal(err)
				}
				if err := store.AddDevice(DeviceGrant{ID: "retired-device", CreatedAt: now}); err != nil {
					t.Fatal(err)
				}
				if err := store.AddPushSubscription(PushSubscription{ID: "retired-sub", DeviceID: "retired-device", Endpoint: "https://push.example/retired", P256DH: "key", Auth: "auth", CreatedAt: now}); err != nil {
					t.Fatal(err)
				}
				occurrence, _, err := store.UpsertGovernanceOccurrence(GovernanceOccurrence{Fingerprint: "completion-capacity-" + stage + "-" + outcome.name, Kind: "policy_drift", OccurredAt: now}, now)
				if err != nil {
					t.Fatal(err)
				}
				oldTarget := GovernanceTargetRef("retired-device", "retired-sub")
				reservation, send, err := store.ReserveGovernanceAttempt(occurrence.DisplayID, oldTarget, now)
				if err != nil || !send || !store.BeginGovernanceTransport(reservation.ID) {
					t.Fatalf("initial reservation=%+v send=%v err=%v", reservation, send, err)
				}
				if err := store.RemovePushSubscriptionAt("retired-sub", now.Add(time.Second)); err != nil {
					t.Fatal(err)
				}
				store.governanceMaxItems = 1
				store.saveHook = func(got string) error {
					if got == stage {
						return errors.New("injected completion " + stage + " failure")
					}
					return nil
				}
				if _, err := store.CompleteGovernanceAttempt(reservation.ID, outcome.class, outcome.accepted, now.Add(2*time.Second)); err == nil {
					t.Fatal("completion persistence failure was ignored")
				}
				failed := store.Governance(now.Add(2 * time.Second))
				if len(failed.Attempts) != 1 || failed.Attempts[0].Class != GovernanceTransportTargetRetired || len(failed.Receipts) != 0 || failed.DeliveryHealth.Class != GovernanceTransportStateWrite {
					t.Fatalf("failed completion claimed durable outcome: %+v", failed)
				}

				store.saveHook = nil
				newTarget := GovernanceTargetRef("active-device", "active-sub")
				fresh, send, err := store.ReserveGovernanceAttempt(occurrence.DisplayID, newTarget, now.Add(time.Minute))
				if err != nil || !send || fresh.TargetRef != newTarget {
					t.Fatalf("retired failed completion retained capacity: reservation=%+v send=%v err=%v", fresh, send, err)
				}
				recovered := store.Governance(now.Add(time.Minute))
				if recovered.DeliveryHealth.State != quiet.State || recovered.DeliveryHealth.Class != quiet.Class || recovered.HealthTotals.StateFailures != 1 || recovered.HealthTotals.Recoveries != 1 {
					t.Fatalf("completion failure recovery=%+v totals=%+v", recovered.DeliveryHealth, recovered.HealthTotals)
				}
			})
		}
	}
}

func TestGovernanceNoOpObservationResolutionCompactionAndHealthSkipSaves(t *testing.T) {
	t.Parallel()
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	saves := 0
	store.saveObserver = func() { saves++ }
	now := time.Date(2026, 7, 18, 9, 0, 0, 0, time.UTC)
	record := GovernanceOccurrence{Fingerprint: "stable", Kind: "policy_drift", State: "open", OccurredAt: now}
	if _, err := store.ObserveGovernanceOccurrences([]GovernanceOccurrence{record}, true, now); err != nil {
		t.Fatal(err)
	}
	baseline := saves
	if _, err := store.ObserveGovernanceOccurrences([]GovernanceOccurrence{record}, true, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := store.ResolveGovernanceOccurrences([]string{"stable"}, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := store.CompactGovernance(now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	health := GovernanceDeliveryHealth{State: GovernanceDeliverySuppressed, Class: GovernanceTransportSuppressed, UpdatedAt: now}
	if err := store.SetGovernanceDeliveryHealth(health); err != nil {
		t.Fatal(err)
	}
	baseline++
	health.UpdatedAt = now.Add(time.Minute)
	if err := store.SetGovernanceDeliveryHealth(health); err != nil {
		t.Fatal(err)
	}
	if saves != baseline {
		t.Fatalf("no-op operations wrote state: saves=%d baseline=%d", saves, baseline)
	}
}

func TestStateJSONNeverStoresRawGovernanceTransportError(t *testing.T) {
	t.Parallel()
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(store.data)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "raw_error") {
		t.Fatal("state schema grew a raw governance error field")
	}
}
