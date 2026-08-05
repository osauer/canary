package corestore

import (
	"fmt"
	"path/filepath"
	"testing"
	"time"
)

func TestBetaOperationalPruneUpgradesEveryPriorSchemaVersion(t *testing.T) {
	plan := currentMigrationPlan()
	if len(plan) != 6 {
		t.Fatalf("migration plan length=%d want 6", len(plan))
	}
	for sourceVersion := 1; sourceVersion < len(plan); sourceVersion++ {
		t.Run(fmt.Sprintf("v%d", sourceVersion), func(t *testing.T) {
			ctx := t.Context()
			dir := privateTempDir(t)
			sourcePath := filepath.Join(dir, "daemon.db")
			store, err := openWithPlan(ctx, Options{Path: sourcePath}, plan[:sourceVersion])
			if err != nil {
				t.Fatal(err)
			}
			stateJSON := fmt.Appendf(nil, `{"source_version":%d}`, sourceVersion)
			if _, err := store.CompareAndSwapStateDocument(ctx, StateDocumentCAS{
				ScopeKey: "daemon", Kind: "compatibility_probe", JSON: stateJSON,
			}); err != nil {
				t.Fatal(err)
			}
			at := time.Date(2026, 8, 5, 12, 0, 0, 0, time.UTC)
			if _, err := store.AppendObservations(ctx, []ObservationInput{
				{ScopeKey: "market/gamma/oi", Source: "ibkr.tws.option_chain", Kind: "gamma_open_interest.snapshot.v1", ObservedAt: at, ContentType: "application/json", Payload: []byte(`{"drop":true}`), DecisionEligible: true},
				{ScopeKey: "market/earnings", Source: "ibkr.tws.wsh", Kind: "earnings_dates.identity_outcome.v1", ObservedAt: at, ContentType: "application/json", Payload: []byte(`{"keep":true}`), DecisionEligible: true},
			}); err != nil {
				t.Fatal(err)
			}
			stressType := "stress_decision"
			if sourceVersion == 1 {
				stressType = "canary_decision"
			}
			events := []EventInput{{
				ScopeKey: "daemon", EventKey: "stress", Type: stressType,
				Action: "record", Origin: "test", OccurredAt: at, PayloadJSON: []byte(`{"drop":true}`),
			}, {
				ScopeKey: "daemon", EventKey: "control", Type: "control",
				Action: "record", Origin: "test", OccurredAt: at.Add(time.Second), PayloadJSON: []byte(`{"keep":true}`),
			}}
			for i := range 4 {
				events = append(events, EventInput{
					ScopeKey: "daemon", EventKey: fmt.Sprintf("regime-%d", i), Type: "regime_decision",
					Action: "record", Origin: "test", OccurredAt: at.Add(time.Duration(i+2) * time.Second),
					PayloadJSON: fmt.Appendf(nil, `{"ordinal":%d}`, i),
				})
			}
			if _, err := store.AppendEvents(ctx, events); err != nil {
				t.Fatal(err)
			}
			sourceHead, err := store.AuthorityHead(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if err := store.Close(); err != nil {
				t.Fatal(err)
			}

			result, err := prepareUpgradeWithPlan(ctx, UpgradeOptions{
				SourcePath: sourcePath, BackupPath: filepath.Join(dir, "source-backup.db"),
				CandidatePath: filepath.Join(dir, "candidate.db"), MinimumHead: &sourceHead,
			}, plan)
			if err != nil {
				t.Fatal(err)
			}
			wantHead := sourceHead
			wantHead.HeadGeneration++
			if result.Candidate.SchemaVersion != 6 || result.Candidate.Head != wantHead ||
				result.HeadTransition != UpgradeHeadTransitionAdvanceOnce || !result.Candidate.Integrity.OK() {
				t.Fatalf("upgrade result=%+v", result)
			}
			if len(result.Maintenance.OperationalPrunes) != 1 || !result.Maintenance.Compacted ||
				!result.Maintenance.SourceBackupRetirementRequired {
				t.Fatalf("maintenance=%+v", result.Maintenance)
			}

			candidate, err := Open(ctx, Options{Path: result.Candidate.Path, MinimumHead: &wantHead})
			if err != nil {
				t.Fatal(err)
			}
			doc, ok, err := candidate.GetStateDocument(ctx, "daemon", "compatibility_probe")
			if err != nil || !ok || string(doc.JSON) != string(stateJSON) {
				t.Fatalf("state after upgrade=%+v ok=%v err=%v", doc, ok, err)
			}
			if rows, err := candidate.ListObservations(ctx, ObservationQuery{ScopeKey: "market/gamma/oi", Kind: "gamma_open_interest.snapshot.v1"}); err != nil || len(rows) != 0 {
				t.Fatalf("discarded observations=%d err=%v", len(rows), err)
			}
			if rows, err := candidate.ListObservations(ctx, ObservationQuery{ScopeKey: "market/earnings", Kind: "earnings_dates.identity_outcome.v1"}); err != nil || len(rows) != 1 {
				t.Fatalf("identity observations=%d err=%v", len(rows), err)
			}
			if rows, err := candidate.LoadEvents(ctx, EventQuery{ScopeKey: "daemon", Type: "stress_decision"}); err != nil || len(rows) != 0 {
				t.Fatalf("stress events=%d err=%v", len(rows), err)
			}
			if rows, err := candidate.LoadEvents(ctx, EventQuery{ScopeKey: "daemon", Type: "regime_decision"}); err != nil || len(rows) != 2 || rows[0].EventKey != "regime-2" || rows[1].EventKey != "regime-3" {
				t.Fatalf("retained regime events=%+v err=%v", rows, err)
			}
			if rows, err := candidate.LoadEvents(ctx, EventQuery{ScopeKey: "daemon", Type: "control"}); err != nil || len(rows) != 1 {
				t.Fatalf("control events=%d err=%v", len(rows), err)
			}
			if err := candidate.Close(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestBetaOperationalPruneKeepsOnlyNamedOperationalEvidence(t *testing.T) {
	ctx := t.Context()
	dir := privateTempDir(t)
	plan := currentMigrationPlan()
	store, err := openWithPlan(ctx, Options{Path: filepath.Join(dir, "daemon.db")}, plan[:5])
	if err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 8, 5, 12, 0, 0, 0, time.UTC)
	knownKinds := []string{
		"contract_cache.snapshot.v3", "gamma_open_interest.snapshot.v1", "trading_halts.snapshot.v1",
		"breadth_spx.windows.v2", "borrow_inventory.snapshot.v1", "gamma_expiry_grid.snapshot.v1",
		"regime_streaks.snapshot.v1", "regime_official_series.snapshot.v1", "regime_hmds.snapshot.v1",
		"regime_measurement.v1", "gamma_skew_diagnostic.v1", "reg_sho.snapshot.v1",
		"stress_market_measurement.v1", "fx_rates.snapshot.v1", "earnings_dates.snapshot.v1",
		"borrow_fees.fetch_outcome.v2", "earnings_dates.provider_outcome.v2", "earnings_dates.provider_outcome.v3",
		"breadth_spx.history.v2", "breadth_spx.snapshot.v1", "spx_members.snapshot.v1", "borrow_fee_tws.fetch_outcome.v1",
	}
	inputs := make([]ObservationInput, 0, len(knownKinds)+5)
	for i, kind := range knownKinds {
		inputs = append(inputs, ObservationInput{ScopeKey: "known", Source: "known", Kind: kind, ObservedAt: at.Add(time.Duration(i) * time.Second), ContentType: "application/json", Payload: []byte(`{"drop":true}`), DecisionEligible: true})
	}
	inputs = append(inputs,
		ObservationInput{ScopeKey: "gamma", Source: "ibkr.tws.option_chain", Kind: "gamma_zero.compute.v1", ObservedAt: at, ContentType: "application/json", Payload: []byte(`{"drop":true}`), DecisionEligible: true},
		ObservationInput{ScopeKey: "gamma", Source: "ibkr.tws.option_chain", Kind: "gamma_zero.compute.v1", ObservedAt: at, ContentType: "application/json", Payload: []byte(`{"keep":"quarantine"}`), DecisionEligible: false},
		ObservationInput{ScopeKey: "gamma", Source: "future.source", Kind: "gamma_zero.compute.v1", ObservedAt: at, ContentType: "application/json", Payload: []byte(`{"keep":"near"}`), DecisionEligible: true},
		ObservationInput{ScopeKey: "earnings", Source: "ibkr.tws.wsh", Kind: "earnings_dates.identity_outcome.v1", ObservedAt: at, ContentType: "application/json", Payload: []byte(`{"keep":"identity"}`), DecisionEligible: true},
		ObservationInput{ScopeKey: "future", Source: "future", Kind: "future.snapshot.v1", ObservedAt: at, ContentType: "application/json", Payload: []byte(`{"keep":"unknown"}`), DecisionEligible: true},
	)
	if _, err := store.AppendObservations(ctx, inputs); err != nil {
		t.Fatal(err)
	}

	events := []EventInput{
		pruneEvent("rule", "daemon", "rule_transition", `{"version":1}`, at, EventProjection{RuleTransition: &RuleTransitionProjection{RuleID: "r", Status: "watch"}}),
		pruneEvent("stress", "daemon", "stress_decision", `{"version":1}`, at, EventProjection{StressTransition: &StressTransitionProjection{Action: "arm"}}),
		pruneEvent("alert", "daemon", "alert_episode_decision", `{"version":4,"decisions":[{"action":"opened"}]}`, at, EventProjection{}),
		pruneEvent("near-scope", "daemon/near", "stress_decision", `{"version":1}`, at, EventProjection{}),
		pruneEvent("unknown-type", "daemon", "future_decision", `{"version":1}`, at, EventProjection{}),
		pruneEvent("proposal-generated", "daemon", "trade_proposal_event", `{"version":1,"type":"generated"}`, at, EventProjection{}),
		pruneEvent("proposal-ignored", "daemon", "trade_proposal_event", `{"version":1,"type":"ignored"}`, at, EventProjection{}),
		pruneEvent("proposal-submitted", "daemon", "trade_proposal_event", `{"version":1,"type":"submitted"}`, at, EventProjection{}),
		pruneEvent("proposal-future", "daemon", "trade_proposal_event", `{"version":2,"type":"generated"}`, at, EventProjection{}),
		pruneEvent("opportunity-shown", "daemon", "opportunity_event", `{"version":1,"type":"shown"}`, at, EventProjection{}),
		pruneEvent("opportunity-ignored", "daemon", "opportunity_event", `{"version":1,"type":"ignored"}`, at, EventProjection{}),
		pruneEvent("opportunity-submitted", "daemon", "opportunity_event", `{"version":1,"type":"submitted"}`, at, EventProjection{}),
	}
	for i := range 4 {
		value := float64(i)
		events = append(events, pruneEvent(fmt.Sprintf("regime-%d", i), "daemon", "regime_decision", fmt.Sprintf(`{"ordinal":%d}`, i), at.Add(time.Duration(i)*time.Second), EventProjection{RegimeDecision: &RegimeDecisionProjection{DecisionKey: fmt.Sprintf("d-%d", i), Stage: "watch", Indicators: []RegimeIndicatorProjection{{Indicator: "breadth", Value: &value}}}}))
	}
	if _, err := store.AppendEvents(ctx, events); err != nil {
		t.Fatal(err)
	}
	head, err := store.AuthorityHead(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	result, err := prepareUpgradeWithPlan(ctx, UpgradeOptions{
		SourcePath: filepath.Join(dir, "daemon.db"), BackupPath: filepath.Join(dir, "backup.db"),
		CandidatePath: filepath.Join(dir, "candidate.db"), MinimumHead: &head,
	}, plan)
	if err != nil {
		t.Fatal(err)
	}
	db := openReadOnlyTestDB(t, result.Candidate.Path)
	defer db.Close()
	for _, table := range []string{"rule_transitions", "stress_transitions"} {
		var count int
		if err := db.QueryRow(`SELECT count(*) FROM ` + table).Scan(&count); err != nil || count != 0 {
			t.Fatalf("%s rows=%d err=%v", table, count, err)
		}
	}
	var regimeDecisions, regimeIndicators int
	if err := db.QueryRow(`SELECT count(*) FROM regime_decisions`).Scan(&regimeDecisions); err != nil || regimeDecisions != 2 {
		t.Fatalf("regime decisions=%d err=%v", regimeDecisions, err)
	}
	if err := db.QueryRow(`SELECT count(*) FROM regime_indicators`).Scan(&regimeIndicators); err != nil || regimeIndicators != 2 {
		t.Fatalf("regime indicators=%d err=%v", regimeIndicators, err)
	}
	wantEventKeys := []string{"near-scope", "unknown-type", "proposal-ignored", "proposal-submitted", "proposal-future", "opportunity-ignored", "opportunity-submitted", "regime-2", "regime-3"}
	rows, err := db.Query(`SELECT event_key FROM event_log ORDER BY event_seq`)
	if err != nil {
		t.Fatal(err)
	}
	var gotEventKeys []string
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			t.Fatal(err)
		}
		gotEventKeys = append(gotEventKeys, key)
	}
	if err := rows.Close(); err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(gotEventKeys) != fmt.Sprint(wantEventKeys) {
		t.Fatalf("retained event keys=%v want=%v", gotEventKeys, wantEventKeys)
	}
	var retainedObservations int
	if err := db.QueryRow(`SELECT count(*) FROM observations`).Scan(&retainedObservations); err != nil || retainedObservations != 4 {
		t.Fatalf("retained observations=%d err=%v", retainedObservations, err)
	}
}

func pruneEvent(key, scope, eventType, payload string, at time.Time, projection EventProjection) EventInput {
	return EventInput{ScopeKey: scope, EventKey: key, Type: eventType, Action: "record", Origin: "test", OccurredAt: at, PayloadJSON: []byte(payload), Projection: projection}
}
