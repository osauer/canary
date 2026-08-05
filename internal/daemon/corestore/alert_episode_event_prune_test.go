package corestore

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestAlertEpisodeEventPruneKeepsTransitionsAndUnknownPayloads(t *testing.T) {
	ctx := t.Context()
	dir := privateTempDir(t)
	sourcePath := filepath.Join(dir, "daemon.db")
	backupPath := filepath.Join(dir, "daemon-v4-backup.db")
	candidatePath := filepath.Join(dir, "daemon-v5-candidate.db")
	plan := currentMigrationPlan()
	if len(plan) != alertEpisodePruneMigrationVersionForTest {
		t.Fatalf("test fixture expects schema %d, got %d", alertEpisodePruneMigrationVersionForTest, len(plan))
	}

	store, err := openWithPlan(ctx, Options{Path: sourcePath}, plan[:4])
	if err != nil {
		t.Fatal(err)
	}
	registryJSON := []byte(`{"version":4,"scopes":[]}`)
	if _, err := store.CompareAndSwapStateDocument(ctx, StateDocumentCAS{
		ScopeKey: "daemon", Kind: "alert_episode_registry", JSON: registryJSON,
	}); err != nil {
		t.Fatal(err)
	}

	largePadding := strings.Repeat("x", 512*1024)
	events := []struct {
		key, scope, eventType, payload string
		remove                         bool
	}{
		{"opened", "daemon", "alert_episode_decision", alertDecisionPayload(4, "opened", ""), false},
		{"reopened", "daemon", "alert_episode_decision", alertDecisionPayload(4, "reopened", ""), false},
		{"escalated", "daemon", "alert_episode_decision", alertDecisionPayload(4, "escalated", ""), false},
		{"recovered", "daemon", "alert_episode_decision", alertDecisionPayload(4, "recovered", ""), false},
		{"refreshed-active", "daemon", "alert_episode_decision", alertDecisionPayload(4, "refreshed_active", largePadding), true},
		{"confirmed-recovered", "daemon", "alert_episode_decision", alertDecisionPayload(4, "confirmed_recovered", ""), true},
		{"negative-without-episode", "daemon", "alert_episode_decision", alertDecisionPayload(4, "negative_without_episode", ""), true},
		{"held-omitted", "daemon", "alert_episode_decision", alertDecisionPayload(4, "held_omitted", ""), true},
		{"held-partial", "daemon", "alert_episode_decision", alertDecisionPayload(4, "held_partial", ""), true},
		{"held-stale", "daemon", "alert_episode_decision", alertDecisionPayload(4, "held_stale", ""), true},
		{"held-unavailable", "daemon", "alert_episode_decision", alertDecisionPayload(4, "held_unavailable", largePadding), true},
		{"held-untrusted", "daemon", "alert_episode_decision", alertDecisionPayload(4, "held_untrusted_evidence", ""), true},
		{"v3-refreshed", "daemon", "alert_episode_decision", alertDecisionPayload(3, "refreshed_active", ""), true},
		{"v3-recovered", "daemon", "alert_episode_decision", alertDecisionPayload(3, "recovered", ""), false},
		{"mixed-transition", "daemon", "alert_episode_decision", alertDecisionPayloadActions(4, []string{"opened", "refreshed_active"}, ""), false},
		{"unknown-action", "daemon", "alert_episode_decision", alertDecisionPayload(4, "future_action", ""), false},
		{"unknown-version", "daemon", "alert_episode_decision", alertDecisionPayload(5, "refreshed_active", ""), false},
		{"primitive-string", "daemon", "alert_episode_decision", alertDecisionRawPayload(4, `"future"`), false},
		{"primitive-null", "daemon", "alert_episode_decision", alertDecisionRawPayload(4, `null`), false},
		{"primitive-number", "daemon", "alert_episode_decision", alertDecisionRawPayload(4, `7`), false},
		{"primitive-array", "daemon", "alert_episode_decision", alertDecisionRawPayload(4, `[]`), false},
		{"missing-action", "daemon", "alert_episode_decision", alertDecisionRawPayload(4, `{}`), false},
		{"nontext-action", "daemon", "alert_episode_decision", alertDecisionRawPayload(4, `{"action":7}`), false},
		{"empty-decisions", "daemon", "alert_episode_decision", alertDecisionRawPayload(4, ``), true},
		{"near-scope", "daemon/near", "alert_episode_decision", alertDecisionPayload(4, "refreshed_active", ""), false},
		{"near-type", "daemon", "alert_episode_decision.near", alertDecisionPayload(4, "refreshed_active", ""), false},
		// Keep removable rows at the source tail so the post-upgrade append proves
		// AUTOINCREMENT continuity rather than merely reusing a deleted sequence.
		{"tail-held", "daemon", "alert_episode_decision", alertDecisionPayload(3, "held_unavailable", ""), true},
	}

	var discarded []eventDiscardDigestRow
	var discardedBytes int64
	for i, event := range events {
		payload := []byte(event.payload)
		receipts, err := store.AppendEvents(ctx, []EventInput{{
			ScopeKey: event.scope, EventKey: event.key, Type: event.eventType,
			Action: "evaluate", Origin: "test", OccurredAt: time.Unix(1_700_000_000+int64(i), 0).UTC(), PayloadJSON: payload,
		}})
		if err != nil {
			t.Fatal(err)
		}
		if event.remove {
			discarded = append(discarded, eventDiscardDigestRow{id: receipts[0].EventSeq, digest: sha256.Sum256(payload)})
			discardedBytes += int64(len(payload))
		}
	}
	sourceHead, err := store.AuthorityHead(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	result, err := prepareUpgradeWithPlan(ctx, UpgradeOptions{
		SourcePath: sourcePath, BackupPath: backupPath, CandidatePath: candidatePath, MinimumHead: &sourceHead,
	}, plan)
	if err != nil {
		t.Fatal(err)
	}
	wantHead := sourceHead
	wantHead.HeadGeneration++
	if result.HeadTransition != UpgradeHeadTransitionAdvanceOnce || result.Candidate.Head != wantHead {
		t.Fatalf("maintenance head transition=%q candidate=%+v want=%+v", result.HeadTransition, result.Candidate.Head, wantHead)
	}
	if len(result.Maintenance.Discards) != 0 || len(result.Maintenance.EventDiscards) != 1 ||
		!result.Maintenance.Compacted || !result.Maintenance.SourceBackupRetirementRequired {
		t.Fatalf("maintenance result=%+v", result.Maintenance)
	}
	summary := result.Maintenance.EventDiscards[0]
	wantSelector := EventDiscardSelector{
		ScopeKey: "daemon", EventType: "alert_episode_decision", Predicate: alertEpisodeNonTransitionPredicate,
	}
	if summary.MigrationVersion != 5 || summary.MigrationName != "alert_episode_event_prune" ||
		summary.Selector != wantSelector || summary.RemovedRows != int64(len(discarded)) ||
		summary.PayloadBytes != discardedBytes || summary.OrderedDigestSHA256 != expectedEventDiscardDigest(wantSelector, discarded) {
		t.Fatalf("event discard summary=%+v", summary)
	}

	candidateDB := openReadOnlyTestDB(t, candidatePath)
	var state string
	if err := candidateDB.QueryRow(`SELECT document_json FROM state_documents WHERE scope_key='daemon' AND kind='alert_episode_registry'`).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != string(registryJSON) {
		t.Fatalf("registry state changed: %s", state)
	}
	rows, err := candidateDB.Query(`SELECT event_key FROM event_log ORDER BY event_seq`)
	if err != nil {
		t.Fatal(err)
	}
	var kept []string
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			t.Fatal(err)
		}
		kept = append(kept, key)
	}
	if err := rows.Close(); err != nil {
		t.Fatal(err)
	}
	wantKept := []string{
		"opened", "reopened", "escalated", "recovered", "v3-recovered", "mixed-transition",
		"unknown-action", "unknown-version", "primitive-string", "primitive-null", "primitive-number",
		"primitive-array", "missing-action", "nontext-action", "near-scope", "near-type",
	}
	if !slices.Equal(kept, wantKept) {
		t.Fatalf("kept event keys=%v want=%v", kept, wantKept)
	}
	var triggerSQL string
	if err := candidateDB.QueryRow(`SELECT sql FROM sqlite_schema WHERE type='trigger' AND name='event_log_no_delete'`).Scan(&triggerSQL); err != nil {
		t.Fatal(err)
	}
	if triggerSQL != appendOnlyDeleteTrigger("event_log") {
		t.Fatalf("delete trigger=%q want %q", triggerSQL, appendOnlyDeleteTrigger("event_log"))
	}
	if err := candidateDB.Close(); err != nil {
		t.Fatal(err)
	}

	guardedDB := rawDB(t, candidatePath)
	if _, err := guardedDB.Exec(`DELETE FROM event_log WHERE event_key='opened'`); err == nil || !strings.Contains(err.Error(), "append-only") {
		t.Fatalf("restored delete guard error=%v", err)
	}
	if err := guardedDB.Close(); err != nil {
		t.Fatal(err)
	}
	upgraded, err := Open(ctx, Options{Path: candidatePath, MinimumHead: &wantHead})
	if err != nil {
		t.Fatal(err)
	}
	receipts, err := upgraded.AppendEvents(ctx, []EventInput{{
		ScopeKey: "daemon", EventKey: "post-upgrade", Type: "control",
		Action: "append", Origin: "test", OccurredAt: time.Unix(1_800_000_000, 0).UTC(), PayloadJSON: []byte(`{"ok":true}`),
	}})
	if err != nil {
		t.Fatal(err)
	}
	if receipts[0].EventSeq <= sourceHead.LastEventSeq {
		t.Fatalf("post-upgrade event sequence=%d did not exceed source frontier=%d", receipts[0].EventSeq, sourceHead.LastEventSeq)
	}
	postAppendHead, err := upgraded.AuthorityHead(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if postAppendHead.HeadGeneration != wantHead.HeadGeneration+1 || postAppendHead.LastEventSeq != receipts[0].EventSeq {
		t.Fatalf("post-upgrade head=%+v receipt=%+v", postAppendHead, receipts[0])
	}
	if err := upgraded.Close(); err != nil {
		t.Fatal(err)
	}
	sourceInfo, err := os.Stat(backupPath)
	if err != nil {
		t.Fatal(err)
	}
	candidateInfo, err := os.Stat(candidatePath)
	if err != nil {
		t.Fatal(err)
	}
	if candidateInfo.Size() >= sourceInfo.Size() {
		t.Fatalf("compaction did not shrink candidate: candidate=%d old-backup=%d", candidateInfo.Size(), sourceInfo.Size())
	}
}

func TestAlertEpisodeEventPruneMaintenanceMetadataCannotWiden(t *testing.T) {
	valid := alertEpisodeEventPrune()
	if err := validateMigrationStatements(valid); err != nil {
		t.Fatalf("valid maintenance rejected: %v", err)
	}
	for _, tc := range []struct {
		name   string
		mutate func(*migration)
	}{
		{"selector", func(m *migration) { m.maintenance.EventDiscard.EventType += ".near" }},
		{"predicate", func(m *migration) { m.maintenance.EventDiscard.Predicate += ".near" }},
		{"extra statement", func(m *migration) { m.statements = append(m.statements, `SELECT 1`) }},
		{"preserve head", func(m *migration) { m.maintenance.PreserveAuthorityHead = true }},
		{"missing approval", func(m *migration) { m.destructive.statements = m.destructive.statements[:1] }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := cloneMigrationPlan([]migration{valid})[0]
			tc.mutate(&m)
			if err := validateMigrationStatements(m); err == nil {
				t.Fatal("widened maintenance metadata was accepted")
			}
		})
	}
}

func alertDecisionPayload(version int, action, padding string) string {
	return alertDecisionPayloadActions(version, []string{action}, padding)
}

func alertDecisionPayloadActions(version int, actions []string, padding string) string {
	decisions := make([]string, len(actions))
	for i, action := range actions {
		decisions[i] = fmt.Sprintf(`{"action":%q}`, action)
	}
	return fmt.Sprintf(`{"version":%d,"decisions":[%s],"padding":%q}`, version, strings.Join(decisions, ","), padding)
}

func alertDecisionRawPayload(version int, decisions string) string {
	return fmt.Sprintf(`{"version":%d,"decisions":[%s]}`, version, decisions)
}

type eventDiscardDigestRow struct {
	id     int64
	digest [sha256.Size]byte
}

func expectedEventDiscardDigest(selector EventDiscardSelector, rows []eventDiscardDigestRow) string {
	h := sha256.New()
	h.Write([]byte("canary.event-discard.v1\x00"))
	for _, value := range []string{selector.ScopeKey, selector.EventType, selector.Predicate} {
		var size [8]byte
		binary.BigEndian.PutUint64(size[:], uint64(len(value)))
		h.Write(size[:])
		h.Write([]byte(value))
	}
	for _, row := range rows {
		var identity [8]byte
		binary.BigEndian.PutUint64(identity[:], uint64(row.id))
		h.Write(identity[:])
		h.Write(row.digest[:])
	}
	return hex.EncodeToString(h.Sum(nil))
}

const alertEpisodePruneMigrationVersionForTest = 5
