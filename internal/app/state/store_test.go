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
	if err := store.RecordAlert(AlertRecord{ID: "canary-2", Fingerprint: "private-canary-2", Account: "private-account"}); err != nil {
		t.Fatal(err)
	}
	got := store.Attention()
	wantRefs := []AttentionRef{{Kind: AttentionKindStress, ID: "canary-1"}, {Kind: AttentionKindStress, ID: "canary-2"}}
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
	for _, private := range []string{"private-canary", "private-account", "attention_seq", "fingerprint", "account"} {
		if strings.Contains(string(raw), private) {
			t.Fatalf("attention leaked %q: %s", private, raw)
		}
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

func TestOpenRejectsCorruptPersistedAuthority(t *testing.T) {
	t.Parallel()
	fixtures := map[string]string{
		"invalid mode":       `{"alert_settings":{"mode":"surprise"}}`,
		"cursor inversion":   `{"alert_settings":{"mode":"act_only"},"attention_high_water_seq":1,"attention_read_through_seq":2}`,
		"sequence gap":       `{"alert_settings":{"mode":"act_only"},"attention_high_water_seq":2,"alert_history":[{"id":"a","attention_seq":1}]}`,
		"duplicate sequence": `{"alert_settings":{"mode":"act_only"},"attention_high_water_seq":1,"alert_history":[{"id":"a","attention_seq":1},{"id":"b","attention_seq":1}]}`,
		"duplicate refs":     `{"alert_settings":{"mode":"act_only"},"attention_high_water_seq":2,"alert_history":[{"id":"same","attention_seq":1},{"id":"same","attention_seq":2}]}`,
		"out of range":       `{"alert_settings":{"mode":"act_only"},"attention_high_water_seq":1,"alert_history":[{"id":"a","attention_seq":2}]}`,
		"empty unread id":    `{"alert_settings":{"mode":"act_only"},"attention_high_water_seq":1,"alert_history":[{"id":"","attention_seq":1}]}`,
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
	if err := store.RecordAlert(AlertRecord{ID: "b"}); err != nil {
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
			if err := store.RecordAlert(AlertRecord{ID: fmt.Sprintf("canary-b-%02d", i)}); err != nil {
				t.Errorf("RecordAlert: %v", err)
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
			if err := store.RecordAlert(AlertRecord{ID: "later-" + stage, CreatedAt: now.Add(time.Minute)}); err != nil {
				t.Fatal(err)
			}
			reopened, err := Open(dir)
			if err != nil {
				t.Fatal(err)
			}
			if got := reopened.PushSubscriptions(); !reflect.DeepEqual(got, before) {
				t.Fatalf("later save persisted failed target: got=%+v want=%+v", got, before)
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
				store.saveHook = nil
				if err := mutate(); err != nil {
					t.Fatal(err)
				}
				if store.data.AlertDelivery.RetiredTargets[alertTarget].IsZero() || store.data.AlertDelivery.Attempts[0].Class != AlertDeliveryAttemptRetired || store.alertDeliveryInFlight[alertReservation.AttemptID] {
					t.Fatalf("successful mutation did not retire alert target: %+v", store.data.AlertDelivery)
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

// TestLoadMigratesLegacyGovernanceAttention pins the one-way decoder for the
// retired governance ledger: persisted occurrence rows vanish, their attention
// sequences compact out of the shared cursor space with order and read state
// preserved, and the next save drops the legacy keys permanently.
func TestLoadMigratesLegacyGovernanceAttention(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	legacy := `{
		"alert_settings": {"mode": "watch_and_act"},
		"alert_history": [
			{"id": "canary-read", "attention_seq": 1},
			{"id": "canary-unread", "attention_seq": 3}
		],
		"governance_occurrences": [
			{"fingerprint": "g-read", "display_id": "gov-1", "kind": "policy_drift", "state": "active", "delivery_disposition": "eligible", "attention_seq": 2},
			{"fingerprint": "g-unread", "display_id": "gov-2", "kind": "policy_drift", "state": "active", "delivery_disposition": "eligible", "attention_seq": 4}
		],
		"governance_attempt_totals": {"cumulative_attempts": 7},
		"governance_delivery_health": {"state": "healthy", "updated_at": "2026-07-20T10:00:00Z"},
		"attention_high_water_seq": 4,
		"attention_read_through_seq": 2
	}`
	if err := os.WriteFile(filepath.Join(dir, "state.json"), []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := Open(dir)
	if err != nil {
		t.Fatalf("Open with legacy governance rows: %v", err)
	}
	got := store.Attention()
	want := Attention{UnreadCount: 1, HighWaterSeq: 2, ReadThroughSeq: 1, UnreadRefs: []AttentionRef{{Kind: AttentionKindStress, ID: "canary-unread"}}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("migrated attention=%+v want=%+v", got, want)
	}
	if err := store.RecordAlert(AlertRecord{ID: "post-migration"}); err != nil {
		t.Fatalf("post-migration alert write: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, legacyKey := range []string{"governance_occurrences", "governance_attempts", "governance_receipts", "governance_attempt_totals", "governance_health_event_totals", "gov-1", "g-unread"} {
		if strings.Contains(string(raw), legacyKey) {
			t.Fatalf("legacy governance key %q survived the one-way migration: %s", legacyKey, raw)
		}
	}
	reopened, err := Open(dir)
	if err != nil {
		t.Fatalf("reopen after migration: %v", err)
	}
	if !attentionMatches(reopened.Attention(), Attention{UnreadCount: 2, HighWaterSeq: 3, ReadThroughSeq: 1}) {
		t.Fatalf("reopened attention=%+v", reopened.Attention())
	}
}
