package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/daemon/corestore"
	"github.com/osauer/canary/v2/internal/rpc"
)

func TestDaemonStateCutoverAndCoreBinding(t *testing.T) {
	stateHome := t.TempDir()
	if err := os.Chmod(stateHome, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_STATE_HOME", stateHome)
	now := time.Date(2026, 7, 20, 10, 0, 0, 0, time.UTC)
	freeze := true
	settings := platformSettingsData{Version: 1, Trading: platformTradingSettingsData{Freeze: &freeze}}
	settingsPath, _ := defaultPlatformSettingsPath()
	writeJSONFixture(t, settingsPath, settings)
	legacySettings, err := os.ReadFile(settingsPath)
	if err != nil {
		t.Fatal(err)
	}

	capitalState := riskCapitalStateFileV1{
		Version: riskCapitalStateVer, GenesisAt: now.Add(-24 * time.Hour), Seeded: true,
		AccountID: "ACCOUNT", AccountMode: rpc.AccountModeLive,
		AdjustedPeakBase: 250000, PeakAsOf: now.Add(-time.Hour),
		LastEquityBase: 240000, LastEquityAsOf: now, BlockLatched: true,
		LatchedAt: now.Add(-30 * time.Minute), LatchEpisodeSeq: 2,
	}
	capitalPath, _ := defaultTradingStatePath(riskCapitalStateFile)
	writeJSONFixture(t, capitalPath, capitalState)
	capitalJournal, _ := defaultTradingStatePath(capitalEventsJournalFile)
	writeJSONLinesFixture(t, capitalJournal,
		capitalEventV1{Version: 1, At: now.Add(-2 * time.Hour), Type: "deposit", AmountBase: 1000, EffectiveAt: now.Add(-2 * time.Hour)},
		capitalEventV1{Version: 1, At: now.Add(-time.Hour), Type: "reconcile", ReportID: "report-1", CoverageTo: now.Add(-time.Hour)},
	)
	policyJournal, _ := defaultTradingStatePath(riskPolicyJournalFile)
	writeJSONLinesFixture(t, policyJournal,
		map[string]any{"version": 1, "at": now.Add(-3 * time.Hour), "kind": "capital_tier", "to": "warn"},
		map[string]any{"version": 1, "at": now.Add(-time.Minute), "kind": "recon_dismiss", "line_id": "line-1", "reason": "confirmed"},
	)
	dbPath := filepath.Join(stateHome, "daemon.db")
	core, err := corestore.Open(context.Background(), corestore.Options{Path: dbPath})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = core.Close() })
	report, err := prepareDaemonStateCutover(context.Background(), core)
	if err != nil {
		t.Fatal(err)
	}
	if report.CapitalEventsImported != 2 || report.GovernanceEventsImported != 1 || report.GovernanceEventsSkipped != 1 {
		t.Fatalf("cutover report = %+v", report)
	}
	if report.CapitalDeclaredFlowsBase != 1000 || report.LastReconcileReportID != "report-1" {
		t.Fatalf("capital continuity = %+v", report)
	}
	if len(report.Sources) != 6 {
		t.Fatalf("source manifest entries = %d, want 6: %+v", len(report.Sources), report.Sources)
	}

	s := &Server{now: func() time.Time { return now }}
	if err := s.bindAuthoritativeDaemonState(context.Background(), core); err != nil {
		t.Fatal(err)
	}
	if got := s.platformSettings.snapshot().Trading.Freeze; got == nil || !*got {
		t.Fatalf("bound freeze = %v, want true", got)
	}
	s.riskCapital.mu.Lock()
	if s.riskCapital.cumFlowsBase != 1000 || !s.riskCapital.state.BlockLatched || s.riskCapital.lastReconcileReportID != "report-1" {
		t.Fatalf("bound capital state = %+v flows=%v report=%q", s.riskCapital.state, s.riskCapital.cumFlowsBase, s.riskCapital.lastReconcileReportID)
	}
	s.riskCapital.mu.Unlock()
	for _, kind := range []string{coreEventRegimeDecision, coreEventRuleTransition, coreEventStressDecision, coreEventProposalOutcome} {
		events, err := loadAllCoreEvents(context.Background(), core, kind)
		if err != nil || len(events) != 0 {
			t.Fatalf("fresh decision stream %s = %d, err=%v", kind, len(events), err)
		}
	}

	if err := s.platformSettings.update(func(next *platformSettingsData) error {
		value := false
		next.Trading.Freeze = &value
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(settingsPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(legacySettings) {
		t.Fatal("core-bound settings update modified the legacy file")
	}
	s.latchRulesRegimeStage(&rpc.RegimeSnapshotResult{Lifecycle: rpc.LifecycleState{Stage: rpc.LifecycleConfirmedStress}})
	if got := s.rulesRegimeStageSnapshot().Stage; got != rpc.LifecycleConfirmedStress {
		t.Fatalf("persisted rules regime stage = %q", got)
	}

	if err := core.Close(); err != nil {
		t.Fatal(err)
	}
	if err := s.platformSettings.update(func(next *platformSettingsData) error {
		value := true
		next.Trading.Freeze = &value
		return nil
	}); err == nil {
		t.Fatal("closed SQLite authority accepted settings mutation")
	}
	if got := s.platformSettings.snapshot().Trading.Freeze; got == nil || *got {
		t.Fatalf("failed write published in-memory freeze = %v", got)
	}
	s.latchRulesRegimeStage(&rpc.RegimeSnapshotResult{Lifecycle: rpc.LifecycleState{Stage: rpc.LifecycleQuiet}})
	if got := s.rulesRegimeStageSnapshot().Stage; got != rpc.LifecycleConfirmedStress {
		t.Fatalf("failed write relaxed in-memory rules regime stage to %q", got)
	}
	if _, err := s.riskCapital.ApplyCapitalEvent(rpc.CapitalEventParams{Type: "deposit", AmountBase: 500}, rpc.OrderOriginHumanTTY); err == nil {
		t.Fatal("closed SQLite authority accepted capital event")
	}
	s.riskCapital.mu.Lock()
	defer s.riskCapital.mu.Unlock()
	if s.riskCapital.cumFlowsBase != 1000 || len(s.riskCapital.capitalEvents) != 2 {
		t.Fatalf("failed capital write escaped rollback: flows=%v events=%d", s.riskCapital.cumFlowsBase, len(s.riskCapital.capitalEvents))
	}
}

func TestDaemonStateCutoverRejectsSymlink(t *testing.T) {
	stateHome := t.TempDir()
	if err := os.Chmod(stateHome, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_STATE_HOME", stateHome)
	target := filepath.Join(t.TempDir(), "settings.json")
	writeJSONFixture(t, target, platformSettingsData{Version: 1})
	path, _ := defaultPlatformSettingsPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}
	core, err := corestore.Open(context.Background(), corestore.Options{Path: filepath.Join(stateHome, "daemon.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer core.Close()
	report, err := prepareDaemonStateCutover(context.Background(), core)
	if err == nil {
		t.Fatal("symlinked legacy state was accepted")
	}
	if len(report.Sources) != 1 || report.Sources[0].Status != "invalid" {
		t.Fatalf("source report = %+v", report.Sources)
	}
}

func TestDaemonStateCutoverRejectsLossySettingsDecode(t *testing.T) {
	for _, tc := range []struct {
		name string
		raw  string
	}{
		{name: "unknown_field", raw: `{"version":1,"unknown_guardrail":true}`},
		{name: "trailing_value", raw: `{"version":1}{"version":1}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stateHome := privateTestDir(t)
			t.Setenv("XDG_STATE_HOME", stateHome)
			settingsPath, _ := defaultPlatformSettingsPath()
			if err := writePrivateStateAtomic(settingsPath, []byte(tc.raw)); err != nil {
				t.Fatal(err)
			}
			core, err := corestore.Open(t.Context(), corestore.Options{Path: filepath.Join(stateHome, "daemon.db")})
			if err != nil {
				t.Fatal(err)
			}
			defer core.Close()
			report, err := prepareDaemonStateCutover(t.Context(), core)
			if err == nil {
				t.Fatal("lossy legacy settings decode was accepted")
			}
			if len(report.Sources) != 1 || report.Sources[0].Status != "invalid" {
				t.Fatalf("source report = %+v", report.Sources)
			}
		})
	}
}

func TestDaemonStateCutoverRejectsCapitalJournalsWithoutState(t *testing.T) {
	stateHome := privateTestDir(t)
	t.Setenv("XDG_STATE_HOME", stateHome)
	now := time.Date(2026, 7, 20, 10, 0, 0, 0, time.UTC)
	capitalJournal, _ := defaultTradingStatePath(capitalEventsJournalFile)
	writeJSONLinesFixture(t, capitalJournal, capitalEventV1{
		Version: 1, At: now, Type: "deposit", AmountBase: 1000, EffectiveAt: now,
	})
	core, err := corestore.Open(t.Context(), corestore.Options{Path: filepath.Join(stateHome, "daemon.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer core.Close()
	if _, err := prepareDaemonStateCutover(t.Context(), core); err == nil || !strings.Contains(err.Error(), "journals exist without") {
		t.Fatalf("partial risk-capital authority error = %v", err)
	}
}

func TestDaemonStateCutoverPreservesEverySafetyField(t *testing.T) {
	stateHome := privateTestDir(t)
	t.Setenv("XDG_STATE_HOME", stateHome)
	base := time.Date(2026, 7, 20, 10, 0, 0, 0, time.UTC)
	flag := true
	limit := 23
	notional := 123456.78
	keepMonths := 7
	settings := platformSettingsData{
		Version: platformSettingsDocVersion, TradingControlGeneration: 1,
		Features: platformFeatureSettingsData{
			StockProtection: platformStockProtectionSettingsData{Enabled: &flag},
			Rulebook: platformRulebookSettingsData{
				Enabled: &flag, EarningsOverrides: map[string]string{"TEST": "2026-08-01Tamc"},
			},
		},
		Trading: platformTradingSettingsData{
			MaxNotional: &notional, MaxOptionContracts: &limit, AllowStockShort: &flag,
			AllowOptionSellToOpen: &flag, Freeze: &flag,
		},
		Regime:  platformRegimeSettingsData{Journal: platformRegimeJournalSettingsData{Enabled: &flag}},
		Stress:  platformStressSettingsData{Journal: platformStressJournalSettingsData{Enabled: &flag}},
		History: platformHistorySettingsData{Rotation: platformHistoryRotationSettingsData{Enabled: &flag, KeepRawMonths: &keepMonths}},
	}
	capital := riskCapitalStateFileV1{
		Version: 1, GenesisAt: base.Add(-90 * 24 * time.Hour), Seeded: true,
		AccountID: "TEST-ACCOUNT", AccountMode: rpc.AccountModeLive,
		AdjustedPeakBase: 250000, PeakAsOf: base.Add(-48 * time.Hour),
		LastEquityBase: 240000, LastEquityAsOf: base.Add(-time.Hour),
		DailyEquity: map[string]float64{"2026-07-19": 239000, "2026-07-20": 240000},
		LastTier:    "warn", BlockLatched: true, LatchedAt: base.Add(-2 * time.Hour),
		LatchEpisodeSeq: 3, LatchConsumedPct: 0.82,
		Overrides: []rpc.OverrideRecord{{
			ID: "override-1", Control: "capital.max_unreconciled_days", Reason: "evidence pending",
			GrantedAt: base.Add(-time.Hour), ExpiresAt: base.Add(24 * time.Hour),
			PolicyFingerprint: "policy-fingerprint", Active: true,
		}},
		StatementFlowsBase: 12500, StatementCoverageTo: base.Add(-24 * time.Hour),
		StatementAuthorityActive:          true,
		IncorporatedStatementLineIDs:      []string{"statement-line-1"},
		AppliedStatementPeakCorrectionIDs: []string{"peak-correction-1"},
	}
	nudges := nudgeStateFileV1{
		Version: 1,
		Shadow: nudgeShadowEpisodeState{
			PolicyIdentity: "policy-v3", LatchEpisode: "episode-3", OccurredAt: base.Add(-3 * time.Hour), Count: 2,
		},
		ConfirmedCoverage: &nudgeConfirmedCoverageState{
			CoverageFrom: base.AddDate(0, -1, 0), ReportIdentity: "report-1", CoveredRowCount: 4,
			CurrentReportIdentity: "report-2", CurrentRowCount: 5, CurrentRowsObserved: true,
			KnownRows: []string{"row-1"}, CurrentRows: []string{"row-2"},
		},
		ConfirmedEvents: []nudgeConfirmedEventState{{ContentIdentity: "event-1", OccurredAt: base.Add(-time.Hour), Superseded: true}},
	}
	stage := rulesRegimeStageState{
		Version: rulesRegimeStageStateVer, Bucket: bucketRegimeStage(rpc.LifecycleConfirmedStress),
		Stage: rpc.LifecycleConfirmedStress, AsOf: base.Add(-30 * time.Minute),
	}
	assertFullyPopulated(t, "settings fixture", settings)
	assertFullyPopulated(t, "capital fixture", capital)
	assertFullyPopulated(t, "nudge fixture", nudges)
	assertFullyPopulated(t, "rules-stage fixture", stage)

	settingsPath, _ := defaultPlatformSettingsPath()
	capitalPath, _ := defaultTradingStatePath(riskCapitalStateFile)
	nudgePath, _ := defaultTradingStatePath(governanceNudgeStateFile)
	stagePath, _ := defaultTradingStatePath(rulesRegimeStageFile)
	// The legacy file predates the canary→stress rename, so it is written in the
	// version-1 shape and the import must land its journal preference in Stress.
	legacy := platformSettingsDocument{
		platformSettingsData: settings,
		LegacyCanary:         &platformStressSettingsData{Journal: platformStressJournalSettingsData{Enabled: &flag}},
	}
	legacy.Version = platformSettingsDocVersionCanary
	legacy.Stress = platformStressSettingsData{}
	writeJSONFixture(t, settingsPath, legacy)
	writeJSONFixture(t, capitalPath, capital)
	writeJSONFixture(t, nudgePath, nudges)
	writeJSONFixture(t, stagePath, stage)

	core, err := corestore.Open(t.Context(), corestore.Options{Path: filepath.Join(stateHome, "daemon.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer core.Close()
	if _, err := prepareDaemonStateCutover(t.Context(), core); err != nil {
		t.Fatal(err)
	}

	assertStateDocumentEquals(t, core, stateKindPlatformSettings, settings)
	assertStateDocumentEquals(t, core, stateKindRiskCapital, riskCapitalSQLiteDocument{
		Version: riskCapitalSQLiteDocVer, State: capital, OverrideSeq: len(capital.Overrides),
	})
	assertStateDocumentEquals(t, core, stateKindNudges, nudges)
	assertStateDocumentEquals(t, core, stateKindRulesRegimeStage, stage)
}

func assertStateDocumentEquals(t *testing.T, core *corestore.Store, kind string, want any) {
	t.Helper()
	got := reflect.New(reflect.TypeOf(want))
	loadStateDocument(t, core, kind, got.Interface())
	if !reflect.DeepEqual(got.Elem().Interface(), want) {
		t.Fatalf("state %s mismatch\n got: %#v\nwant: %#v", kind, got.Elem().Interface(), want)
	}
}

func loadStateDocument(t *testing.T, core *corestore.Store, kind string, dst any) {
	t.Helper()
	doc, ok, err := core.GetStateDocument(t.Context(), daemonStateScope, kind)
	if err != nil || !ok {
		t.Fatalf("load state %s: ok=%v err=%v", kind, ok, err)
	}
	if err := json.Unmarshal(doc.JSON, dst); err != nil {
		t.Fatalf("decode state %s: %v", kind, err)
	}
}

func assertFullyPopulated(t *testing.T, name string, value any) {
	t.Helper()
	var walk func(string, reflect.Value)
	timeType := reflect.TypeFor[time.Time]()
	walk = func(path string, current reflect.Value) {
		if current.Kind() == reflect.Interface {
			current = current.Elem()
		}
		if current.Kind() == reflect.Pointer {
			if current.IsNil() {
				t.Errorf("%s.%s is nil", name, path)
				return
			}
			walk(path, current.Elem())
			return
		}
		if current.Type() == timeType {
			if current.Interface().(time.Time).IsZero() {
				t.Errorf("%s.%s is zero", name, path)
			}
			return
		}
		switch current.Kind() {
		case reflect.Struct:
			for i := 0; i < current.NumField(); i++ {
				field := current.Type().Field(i)
				if field.PkgPath == "" {
					walk(path+"."+field.Name, current.Field(i))
				}
			}
		case reflect.Slice, reflect.Array:
			if current.Len() == 0 {
				t.Errorf("%s.%s is empty", name, path)
			}
			for i := 0; i < current.Len(); i++ {
				walk(fmt.Sprintf("%s[%d]", path, i), current.Index(i))
			}
		case reflect.Map:
			if current.Len() == 0 {
				t.Errorf("%s.%s is empty", name, path)
			}
		case reflect.Bool:
			if !current.Bool() {
				t.Errorf("%s.%s is false", name, path)
			}
		default:
			if current.IsZero() {
				t.Errorf("%s.%s is zero", name, path)
			}
		}
	}
	walk("value", reflect.ValueOf(value))
}

func TestInitializeFreshDaemonStateDoesNotReadLegacyDefaults(t *testing.T) {
	stateHome := t.TempDir()
	if err := os.Chmod(stateHome, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_STATE_HOME", stateHome)
	legacyFreeze := true
	settingsPath, _ := defaultPlatformSettingsPath()
	writeJSONFixture(t, settingsPath, platformSettingsData{
		Version: 1, Trading: platformTradingSettingsData{Freeze: &legacyFreeze},
	})
	core, err := corestore.Open(t.Context(), corestore.Options{Path: filepath.Join(stateHome, "offline.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer core.Close()
	if err := initializeFreshDaemonState(t.Context(), core); err != nil {
		t.Fatal(err)
	}
	// Restart-safe completion must not replace any document generation.
	before, ok, err := core.GetStateDocument(t.Context(), daemonStateScope, stateKindPlatformSettings)
	if err != nil || !ok {
		t.Fatal(err)
	}
	if err := initializeFreshDaemonState(t.Context(), core); err != nil {
		t.Fatal(err)
	}
	after, ok, err := core.GetStateDocument(t.Context(), daemonStateScope, stateKindPlatformSettings)
	if err != nil || !ok {
		t.Fatal(err)
	}
	if before.Revision != after.Revision {
		t.Fatalf("fresh-state reinitialization replaced settings revision %d with %d", before.Revision, after.Revision)
	}
	s := &Server{now: time.Now}
	if err := s.bindAuthoritativeDaemonState(t.Context(), core); err != nil {
		t.Fatal(err)
	}
	if got := s.platformSettings.snapshot().Trading.Freeze; got != nil {
		t.Fatalf("fresh authority imported legacy freeze: %v", *got)
	}
	s.riskCapital.mu.Lock()
	defer s.riskCapital.mu.Unlock()
	if s.riskCapital.state.Seeded || s.riskCapital.state.BlockLatched || len(s.riskCapital.capitalEvents) != 0 {
		t.Fatalf("fresh capital authority is not empty: state=%+v events=%d", s.riskCapital.state, len(s.riskCapital.capitalEvents))
	}
}

func writeJSONFixture(t *testing.T, path string, value any) {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if err := writePrivateStateAtomic(path, raw); err != nil {
		t.Fatal(err)
	}
}

func writeJSONLinesFixture(t *testing.T, path string, values ...any) {
	t.Helper()
	var raw []byte
	for _, value := range values {
		line, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		raw = append(raw, line...)
		raw = append(raw, '\n')
	}
	if err := writePrivateStateAtomic(path, raw); err != nil {
		t.Fatal(err)
	}
}
