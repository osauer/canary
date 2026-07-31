package daemon

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/daemon/corestore"
)

const (
	historicalV171Commit = "57e130a06381583730afcaff822e512d01395158"
	historicalV221Commit = "f1b5647e39edef3a4bf88ba6bf0bb7a9cd4a1c4a"
	historicalV230Commit = "22375a798272130d476d25b1c66ab1c84cf55e99"
	historicalV254Commit = "3b548f6d63286448ac132ca4ade66484952612f5"
)

type historicalUpgradeManifest struct {
	Version         int                        `json:"version"`
	DigestAlgorithm string                     `json:"digest_algorithm"`
	Fixtures        []historicalUpgradeFixture `json:"fixtures"`
}

type historicalUpgradeFixture struct {
	ID             string                         `json:"id"`
	Classification string                         `json:"classification"`
	Source         historicalUpgradeSource        `json:"source"`
	ArtifactSHA256 string                         `json:"artifact_sha256"`
	Files          []historicalUpgradeFixtureFile `json:"files"`
	Expectations   map[string]json.RawMessage     `json:"synthetic_expectations"`
}

type historicalUpgradeSource struct {
	Tag            string   `json:"tag"`
	PeeledCommit   string   `json:"peeled_commit"`
	GeneratorPaths []string `json:"generator_paths"`
}

type historicalUpgradeFixtureFile struct {
	Path        string `json:"path"`
	SHA256      string `json:"sha256"`
	SourceMode  string `json:"source_mode"`
	InstallMode string `json:"install_mode"`
}

type historicalObservationRecord struct {
	ID               int64
	ScopeKey         string
	Source           string
	Kind             string
	ObservedAt       string
	ObservedAtMS     int64
	RecordedAt       string
	ContentType      string
	PayloadHex       string
	PayloadSHA256Hex string
	MetadataIsNull   int64
	MetadataJSONHex  string
	DecisionEligible int64
}

type historicalMaintenanceArtifacts struct {
	UpgradeID    string
	TargetBackup string
	Receipt      string
}

func TestHistoricalUpgradeFixtureManifestIntegrity(t *testing.T) {
	manifest := loadHistoricalUpgradeManifest(t)
	if manifest.Version != 1 {
		t.Fatalf("manifest version=%d, want 1", manifest.Version)
	}
	const digestContract = "sha256(path NUL source_mode NUL file_sha256 LF)"
	if manifest.DigestAlgorithm != digestContract {
		t.Fatalf("manifest digest contract=%q, want %q", manifest.DigestAlgorithm, digestContract)
	}
	wantSources := map[string]historicalUpgradeSource{
		"v1.7.1-file-authority": {
			Tag: "v1.7.1", PeeledCommit: historicalV171Commit,
			GeneratorPaths: []string{
				"scripts/upgrade-fixtures/generators/v1_7_1_fixture_test.go.txt",
			},
		},
		"v2.2.1-file-authority": {
			Tag: "v2.2.1", PeeledCommit: historicalV221Commit,
			GeneratorPaths: []string{
				"scripts/upgrade-fixtures/generators/v2_2_1_fixture_test.go.txt",
			},
		},
		"v2.3.0-schema-v1-authority": {
			Tag: "v2.3.0", PeeledCommit: historicalV230Commit,
			GeneratorPaths: []string{
				"scripts/upgrade-fixtures/generators/v2_3_0_core_fixture_test.go.txt",
				"scripts/upgrade-fixtures/generators/v2_3_0_head_fixture_test.go.txt",
			},
		},
		"v2.5.4-schema-v3-authority": {
			Tag: "v2.5.4", PeeledCommit: historicalV254Commit,
			GeneratorPaths: []string{
				"scripts/upgrade-fixtures/generators/v2_5_4_core_fixture_test.go.txt",
				"scripts/upgrade-fixtures/generators/v2_5_4_head_fixture_test.go.txt",
			},
		},
	}
	if len(manifest.Fixtures) != len(wantSources) {
		t.Fatalf("manifest fixtures=%d, want %d", len(manifest.Fixtures), len(wantSources))
	}

	manifested := map[string]bool{}
	for _, fixture := range manifest.Fixtures {
		wantSource, ok := wantSources[fixture.ID]
		if !ok {
			t.Fatalf("unknown fixture id %q", fixture.ID)
		}
		if fixture.Source.Tag != wantSource.Tag ||
			fixture.Source.PeeledCommit != wantSource.PeeledCommit ||
			!slices.Equal(fixture.Source.GeneratorPaths, wantSource.GeneratorPaths) {
			t.Fatalf("fixture %s source=%+v, want %+v", fixture.ID, fixture.Source, wantSource)
		}
		switch fixture.Classification {
		case "must_succeed", "fail_closed":
		default:
			t.Fatalf("fixture %s classification=%q", fixture.ID, fixture.Classification)
		}
		if fixture.Classification != "must_succeed" {
			t.Fatalf("fixture %s must be a supported upgrade, got %q", fixture.ID, fixture.Classification)
		}
		verifyHistoricalUpgradeFixture(t, fixture, manifested)
		delete(wantSources, fixture.ID)
	}
	if len(wantSources) != 0 {
		t.Fatalf("missing fixture sources: %+v", wantSources)
	}

	root := historicalUpgradeFixtureRoot(t)
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		relative = filepath.ToSlash(relative)
		if relative == "manifest.json" {
			return nil
		}
		if !manifested[relative] {
			return fmt.Errorf("unmanifested historical fixture file %s", relative)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestHistoricalV171FileAuthorityCutover(t *testing.T) {
	fixture := historicalUpgradeFixtureByID(t, "v1.7.1-file-authority")
	wantPurgeRef := historicalExpectation[string](t, fixture, "purge_order_ref")
	wantRestoreRef := historicalExpectation[string](t, fixture, "restore_order_ref")
	wantOrderFloor := historicalExpectation[int64](t, fixture, "global_order_id_floor")
	wantPurgeLeg := historicalExpectation[string](t, fixture, "purge_leg_id")
	wantPurged := historicalExpectation[float64](t, fixture, "purged_quantity")
	wantRestored := historicalExpectation[float64](t, fixture, "restored_quantity")
	wantRemaining := historicalExpectation[float64](t, fixture, "purge_remaining_quantity")
	wantFillCursors := historicalExpectation[int](t, fixture, "purge_fill_cursor_count")
	wantEndpoint := historicalExpectation[string](t, fixture, "route_endpoint")
	wantClientID := historicalExpectation[int](t, fixture, "route_client_id")
	wantAccount := historicalExpectation[string](t, fixture, "route_account")
	wantMode := historicalExpectation[string](t, fixture, "route_mode")

	root := materializeHistoricalFileAuthority(t, fixture)
	server := newCutoverTestServer(t, "")
	if err := server.openCoreStore(t.Context()); err != nil {
		t.Fatalf("cut over authentic v1.7.1 file authority: %v", err)
	}
	defer server.closeCoreStore()

	wantRoute, err := coreBrokerScopeFromEvent(orderJournalEvent{
		Endpoint: wantEndpoint, ClientID: wantClientID, Account: wantAccount, Mode: wantMode,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, orderRef := range []string{wantPurgeRef, wantRestoreRef} {
		events, err := server.coreStore.LoadOrderEvents(t.Context(), corestore.OrderQuery{
			OrderRef: orderRef, Limit: 20,
		})
		if err != nil {
			t.Fatal(err)
		}
		if len(events) != 2 {
			t.Fatalf("migrated v1.7.1 order %q events=%d, want 2", orderRef, len(events))
		}
		for _, event := range events {
			if event.Scope != wantRoute {
				t.Fatalf("migrated v1.7.1 order %q route=%+v, want %+v", orderRef, event.Scope, wantRoute)
			}
		}
	}
	if floor, err := server.coreStore.GlobalOrderIDFloor(t.Context()); err != nil || floor != wantOrderFloor {
		t.Fatalf("migrated v1.7.1 global order id floor=%d error=%v, want %d", floor, err, wantOrderFloor)
	}

	purgeDoc, ok, err := server.coreStore.GetStateDocument(t.Context(), purgeLedgerStateScope, purgeLedgerStateKind)
	if err != nil || !ok {
		t.Fatalf("migrated v1.7.1 purge authority found=%v error=%v", ok, err)
	}
	var purge purgeLedgerFile
	if err := json.Unmarshal(purgeDoc.JSON, &purge); err != nil {
		t.Fatal(err)
	}
	if purge.SchemaVersion != purgeLedgerSchemaVersion || len(purge.Rows) != 1 {
		t.Fatalf("migrated v1.7.1 purge authority=%+v", purge)
	}
	row := purge.Rows[0]
	if row.LegID != wantPurgeLeg ||
		row.Status != purgeLedgerStatusActive ||
		row.PurgedQuantity != wantPurged ||
		row.RestoredQuantity != wantRestored ||
		row.RemainingQuantity != wantRemaining ||
		row.Endpoint != wantEndpoint ||
		row.ClientID != wantClientID ||
		row.Account != wantAccount ||
		row.Mode != wantMode ||
		len(row.OrderFills) != wantFillCursors ||
		row.OrderFills[wantPurgeRef].Filled != wantPurged ||
		row.OrderFills[wantRestoreRef].Filled != wantRestored {
		t.Fatalf("migrated v1.7.1 purge row=%+v", row)
	}

	cutover := assertHistoricalFileCutoverArtifacts(t, fixture, root, server)
	if cutover.Counts.RetainedOrderEvents != 4 ||
		cutover.Counts.RetainedOrderChains != 2 ||
		cutover.Counts.PurgeRows != 1 ||
		cutover.Counts.PurgeFillCursors != wantFillCursors {
		t.Fatalf("v1.7.1 cutover counts=%+v", cutover.Counts)
	}

	if err := server.closeCoreStore(); err != nil {
		t.Fatal(err)
	}
	restarted := newCutoverTestServer(t, "")
	if err := restarted.openCoreStore(t.Context()); err != nil {
		t.Fatalf("restart migrated v1.7.1 authority: %v", err)
	}
	restartedDoc, ok, err := restarted.coreStore.GetStateDocument(
		t.Context(), purgeLedgerStateScope, purgeLedgerStateKind,
	)
	if err != nil || !ok || !bytes.Equal(restartedDoc.JSON, purgeDoc.JSON) {
		_ = restarted.closeCoreStore()
		t.Fatalf(
			"restarted v1.7.1 purge continuity found=%v document=%s error=%v",
			ok, restartedDoc.JSON, err,
		)
	}
	if err := restarted.closeCoreStore(); err != nil {
		t.Fatal(err)
	}
}

func TestHistoricalV221FileAuthorityCutover(t *testing.T) {
	fixture := historicalUpgradeFixtureByID(t, "v2.2.1-file-authority")
	wantFreeze := historicalExpectation[bool](t, fixture, "trading_freeze")
	wantPeak := historicalExpectation[float64](t, fixture, "capital_adjusted_peak_base")
	wantFlows := historicalExpectation[float64](t, fixture, "declared_capital_flow_base")
	wantOrderRef := historicalExpectation[string](t, fixture, "retained_order_ref")
	wantOrderFloor := historicalExpectation[int64](t, fixture, "global_order_id_floor")
	wantPurgeLeg := historicalExpectation[string](t, fixture, "purge_leg_id")
	wantPurgeRemaining := historicalExpectation[float64](t, fixture, "purge_remaining_quantity")
	root := privateTestDir(t)
	materializeHistoricalUpgradeFixture(t, fixture, root)
	stateHome := filepath.Join(root, "state")
	cacheHome := filepath.Join(root, "cache")
	configHome := filepath.Join(root, "config")
	for _, path := range []string{stateHome, cacheHome, configHome} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("XDG_STATE_HOME", stateHome)
	t.Setenv("XDG_CACHE_HOME", cacheHome)
	t.Setenv("XDG_CONFIG_HOME", configHome)

	server := newCutoverTestServer(t, "")
	if err := server.openCoreStore(t.Context()); err != nil {
		t.Fatalf("cut over authentic v2.2.1 file authority: %v", err)
	}
	defer server.closeCoreStore()

	if got := server.platformSettings.snapshot().Trading.Freeze; got == nil || *got != wantFreeze {
		t.Fatalf("migrated trading freeze=%v, want %v", got, wantFreeze)
	}
	server.riskCapital.mu.Lock()
	adjustedPeak := server.riskCapital.state.AdjustedPeakBase
	declaredFlows := server.riskCapital.cumFlowsBase
	blockLatched := server.riskCapital.state.BlockLatched
	server.riskCapital.mu.Unlock()
	if adjustedPeak != wantPeak || declaredFlows != wantFlows || !blockLatched {
		t.Fatalf("migrated capital continuity peak=%v flows=%v latched=%v", adjustedPeak, declaredFlows, blockLatched)
	}

	events, err := server.coreStore.LoadOrderEvents(t.Context(), corestore.OrderQuery{
		OrderRef: wantOrderRef, Limit: 20,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 {
		t.Fatalf("migrated retained order events=%d, want 2", len(events))
	}
	if floor, err := server.coreStore.GlobalOrderIDFloor(t.Context()); err != nil || floor != wantOrderFloor {
		t.Fatalf("migrated global order id floor=%d error=%v, want %d", floor, err, wantOrderFloor)
	}

	purgeDoc, ok, err := server.coreStore.GetStateDocument(t.Context(), purgeLedgerStateScope, purgeLedgerStateKind)
	if err != nil || !ok {
		t.Fatalf("migrated purge authority found=%v error=%v", ok, err)
	}
	var purge purgeLedgerFile
	if err := json.Unmarshal(purgeDoc.JSON, &purge); err != nil {
		t.Fatal(err)
	}
	if len(purge.Rows) != 1 || purge.Rows[0].LegID != wantPurgeLeg ||
		purge.Rows[0].RemainingQuantity != wantPurgeRemaining || purge.Rows[0].Endpoint != "127.0.0.1:4002" ||
		purge.Rows[0].ClientID != 41 || len(purge.Rows[0].OrderFills) != 1 {
		t.Fatalf("migrated purge authority=%+v", purge.Rows)
	}

	cutover, _, err := loadCoreCutoverManifest(t.Context(), server.coreStore)
	if err != nil {
		t.Fatal(err)
	}
	if cutover.Status != coreCutoverStatusSealed || !cutover.ImportedLegacy {
		t.Fatalf("cutover manifest=%+v", cutover)
	}
	sources := make(map[string]coreCutoverSource, len(cutover.Sources))
	for _, source := range cutover.Sources {
		sources[source.Path] = source
	}
	for _, file := range fixture.Files {
		relative := strings.TrimPrefix(file.Path, fixture.ID+"/")
		original := filepath.Join(root, filepath.FromSlash(relative))
		source, ok := sources[original]
		if !ok || source.Status != "sealed" {
			t.Fatalf("legacy source %s not sealed: %+v", original, source)
		}
		sealedRaw, err := os.ReadFile(source.Destination)
		if err != nil {
			t.Fatalf("read sealed %s: %v", source.Destination, err)
		}
		if historicalSHA256(sealedRaw) != file.SHA256 {
			t.Fatalf("sealed source %s digest changed", source.Destination)
		}
		info, err := os.Lstat(original)
		if strings.HasSuffix(original, "order-journal.jsonl") {
			if err != nil || !info.IsDir() {
				t.Fatalf("legacy order journal blocker info=%v error=%v", info, err)
			}
		} else if !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("legacy source remains live at %s: %v", original, err)
		}
	}
	for _, backup := range []string{cutover.PrepublishBackupPath, cutover.FinalBackupPath} {
		if info, err := os.Stat(backup); err != nil || !info.Mode().IsRegular() {
			t.Fatalf("cutover backup %s info=%v error=%v", backup, info, err)
		}
	}
	head, err := server.coreStore.AuthorityHead(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	watermark, err := loadAuthorityWatermark(server.coreStorePath + ".head")
	if err != nil || watermark == nil || *watermark != head {
		t.Fatalf("cutover watermark=%+v head=%+v error=%v", watermark, head, err)
	}

	if err := server.closeCoreStore(); err != nil {
		t.Fatal(err)
	}
	restarted := newCutoverTestServer(t, "")
	if err := restarted.openCoreStore(t.Context()); err != nil {
		t.Fatalf("restart migrated v2.2.1 authority: %v", err)
	}
	if got := restarted.platformSettings.snapshot().Trading.Freeze; got == nil || *got != wantFreeze {
		t.Fatalf("restarted trading freeze=%v, want %v", got, wantFreeze)
	}
	if err := restarted.closeCoreStore(); err != nil {
		t.Fatal(err)
	}
}

func materializeHistoricalFileAuthority(t *testing.T, fixture historicalUpgradeFixture) string {
	t.Helper()
	root := privateTestDir(t)
	materializeHistoricalUpgradeFixture(t, fixture, root)
	stateHome := filepath.Join(root, "state")
	cacheHome := filepath.Join(root, "cache")
	configHome := filepath.Join(root, "config")
	for _, path := range []string{stateHome, cacheHome, configHome} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("XDG_STATE_HOME", stateHome)
	t.Setenv("XDG_CACHE_HOME", cacheHome)
	t.Setenv("XDG_CONFIG_HOME", configHome)
	return root
}

func assertHistoricalFileCutoverArtifacts(
	t *testing.T,
	fixture historicalUpgradeFixture,
	root string,
	server *Server,
) coreCutoverManifest {
	t.Helper()
	cutover, _, err := loadCoreCutoverManifest(t.Context(), server.coreStore)
	if err != nil {
		t.Fatal(err)
	}
	if cutover.Status != coreCutoverStatusSealed || !cutover.ImportedLegacy {
		t.Fatalf("cutover manifest=%+v", cutover)
	}
	sources := make(map[string]coreCutoverSource, len(cutover.Sources))
	for _, source := range cutover.Sources {
		sources[source.Path] = source
	}
	for _, file := range fixture.Files {
		relative := strings.TrimPrefix(file.Path, fixture.ID+"/")
		original := filepath.Join(root, filepath.FromSlash(relative))
		source, ok := sources[original]
		if !ok || source.Status != "sealed" {
			t.Fatalf("legacy source %s not sealed: %+v", original, source)
		}
		sealedRaw, err := os.ReadFile(source.Destination)
		if err != nil {
			t.Fatalf("read sealed %s: %v", source.Destination, err)
		}
		if historicalSHA256(sealedRaw) != file.SHA256 {
			t.Fatalf("sealed source %s digest changed", source.Destination)
		}
		info, err := os.Lstat(original)
		if strings.HasSuffix(original, "order-journal.jsonl") {
			if err != nil || !info.IsDir() {
				t.Fatalf("legacy order journal blocker info=%v error=%v", info, err)
			}
		} else if !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("legacy source remains live at %s: %v", original, err)
		}
	}
	for _, backup := range []string{cutover.PrepublishBackupPath, cutover.FinalBackupPath} {
		info, err := os.Lstat(backup)
		if err != nil ||
			info.Mode()&os.ModeSymlink != 0 ||
			!info.Mode().IsRegular() ||
			info.Mode().Perm() != 0o600 {
			t.Fatalf("cutover backup %s info=%v error=%v", backup, info, err)
		}
	}
	head, err := server.coreStore.AuthorityHead(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	watermark, err := loadAuthorityWatermark(server.coreStorePath + ".head")
	if err != nil || watermark == nil || *watermark != head {
		t.Fatalf("cutover watermark=%+v head=%+v error=%v", watermark, head, err)
	}
	return cutover
}

func TestHistoricalV230SQLiteAuthorityUpgrade(t *testing.T) {
	fixture := historicalUpgradeFixtureByID(t, "v2.3.0-schema-v1-authority")
	wantSchema := historicalExpectation[int](t, fixture, "schema_version")
	wantEpoch := historicalExpectation[string](t, fixture, "authority_epoch")
	wantHeadGeneration := historicalExpectation[int64](t, fixture, "head_generation")
	wantStateScope := historicalExpectation[string](t, fixture, "state_scope")
	wantStateKind := historicalExpectation[string](t, fixture, "state_kind")
	wantStateJSON := historicalExpectation[string](t, fixture, "state_json")
	wantObservationScope := historicalExpectation[string](t, fixture, "defective_observation_scope")
	wantObservationSource := historicalExpectation[string](t, fixture, "defective_observation_source")
	wantDefectiveKind := historicalExpectation[string](t, fixture, "defective_observation_kind")
	wantDefectivePayload := historicalExpectation[string](t, fixture, "defective_observation_payload")
	wantControlKind := historicalExpectation[string](t, fixture, "control_observation_kind")
	wantControlPayload := historicalExpectation[string](t, fixture, "control_observation_payload")
	root := privateTestDir(t)
	materializeHistoricalUpgradeFixture(t, fixture, root)
	databasePath := filepath.Join(root, "daemon.db")
	watermarkPath := databasePath + ".head"

	minimum, err := loadAuthorityWatermark(watermarkPath)
	if err != nil || minimum == nil {
		t.Fatalf("load historical watermark=%+v error=%v", minimum, err)
	}
	source, err := corestore.Inspect(t.Context(), corestore.InspectOptions{
		Path: databasePath, MinimumHead: minimum,
	})
	if err != nil {
		t.Fatal(err)
	}
	if source.SchemaVersion != wantSchema || source.Status != corestore.InspectionUpgradeRequired ||
		source.Head.AuthorityEpoch != wantEpoch ||
		source.Head.HeadGeneration != wantHeadGeneration {
		t.Fatalf("historical source inspection=%+v", source)
	}
	beforeState := readHistoricalStateDocument(t, databasePath, wantStateScope, wantStateKind)
	if string(beforeState) != wantStateJSON {
		t.Fatalf("historical source state=%s", beforeState)
	}
	beforeDefective := readHistoricalObservations(
		t, databasePath, wantObservationScope, wantObservationSource, wantDefectiveKind,
	)
	beforeControl := readHistoricalObservations(
		t, databasePath, wantObservationScope, wantObservationSource, wantControlKind,
	)
	beforeDefectiveEvidence := readHistoricalObservationRecords(
		t, databasePath, wantObservationScope, wantObservationSource, wantDefectiveKind,
	)
	beforeControlEvidence := readHistoricalObservationRecords(
		t, databasePath, wantObservationScope, wantObservationSource, wantControlKind,
	)
	if len(beforeDefective) != 1 || string(beforeDefective[0]) != wantDefectivePayload {
		t.Fatalf("historical defective observations=%q", beforeDefective)
	}
	if len(beforeControl) != 1 || string(beforeControl[0]) != wantControlPayload {
		t.Fatalf("historical control observations=%q", beforeControl)
	}
	if len(beforeDefectiveEvidence) != 1 || len(beforeControlEvidence) != 1 {
		t.Fatalf(
			"historical observation evidence defective=%+v control=%+v",
			beforeDefectiveEvidence, beforeControlEvidence,
		)
	}

	upgradedMinimum, err := ensureCoreStoreSchemaCurrent(
		t.Context(), databasePath, minimum,
		time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC),
	)
	if err != nil {
		t.Fatalf("upgrade authentic v2.3.0 authority: %v", err)
	}
	wantHead := source.Head
	wantHead.HeadGeneration++
	if upgradedMinimum == nil || *upgradedMinimum != wantHead {
		t.Fatalf("upgraded minimum=%+v, want %+v", upgradedMinimum, wantHead)
	}
	current, err := corestore.Inspect(t.Context(), corestore.InspectOptions{
		Path: databasePath, MinimumHead: upgradedMinimum,
	})
	if err != nil {
		t.Fatal(err)
	}
	if current.Status != corestore.InspectionCurrent || current.SchemaVersion != source.TargetVersion ||
		current.Head != wantHead {
		t.Fatalf("upgraded authority=%+v", current)
	}
	afterState := readHistoricalStateDocument(t, databasePath, wantStateScope, wantStateKind)
	if !bytes.Equal(afterState, beforeState) {
		t.Fatalf("state continuity changed: before=%s after=%s", beforeState, afterState)
	}
	afterDefective := readHistoricalObservations(
		t, databasePath, wantObservationScope, wantObservationSource, wantDefectiveKind,
	)
	afterControl := readHistoricalObservations(
		t, databasePath, wantObservationScope, wantObservationSource, wantControlKind,
	)
	afterDefectiveEvidence := readHistoricalObservationRecords(
		t, databasePath, wantObservationScope, wantObservationSource, wantDefectiveKind,
	)
	afterControlEvidence := readHistoricalObservationRecords(
		t, databasePath, wantObservationScope, wantObservationSource, wantControlKind,
	)
	if len(afterDefective) != 0 {
		t.Fatalf("exact defective observations survived upgrade: %q", afterDefective)
	}
	if len(afterDefectiveEvidence) != 0 {
		t.Fatalf("exact defective evidence survived upgrade: %+v", afterDefectiveEvidence)
	}
	if len(afterControl) != 1 || !bytes.Equal(afterControl[0], beforeControl[0]) ||
		!slices.Equal(afterControlEvidence, beforeControlEvidence) {
		t.Fatalf(
			"near-control evidence changed: before=%+v after=%+v",
			beforeControlEvidence, afterControlEvidence,
		)
	}
	publishedWatermark, err := loadAuthorityWatermark(watermarkPath)
	if err != nil || publishedWatermark == nil || *publishedWatermark != wantHead {
		t.Fatalf("published watermark=%+v error=%v", publishedWatermark, err)
	}
	if pending, err := coreSchemaUpgradePending(databasePath); err != nil || pending {
		t.Fatalf("completed upgrade pending=%v error=%v", pending, err)
	}

	artifacts := findHistoricalMaintenanceArtifacts(
		t, databasePath, source.SchemaVersion, source.TargetVersion,
	)
	receipt, ok, err := loadCoreSchemaMaintenanceReceipt(artifacts.Receipt)
	if err != nil || !ok {
		t.Fatalf("load historical maintenance receipt found=%v error=%v", ok, err)
	}
	if receipt.UpgradeID != artifacts.UpgradeID {
		t.Fatalf("maintenance receipt upgrade id=%q, want %q", receipt.UpgradeID, artifacts.UpgradeID)
	}
	wantSelector := corestore.ObservationDiscardSelector{
		ScopeKey: wantObservationScope,
		Source:   wantObservationSource,
		Kind:     wantDefectiveKind,
	}
	wantDiscardDigest := historicalObservationDiscardDigest(
		t, wantSelector, beforeDefectiveEvidence,
	)
	if receipt.Discard.MigrationVersion != contractCachePruneMigrationVersion ||
		receipt.Discard.MigrationName != "contract_cache_observation_prune" ||
		receipt.Discard.Selector != wantSelector ||
		receipt.Discard.RemovedRows != int64(len(beforeDefectiveEvidence)) ||
		receipt.Discard.PayloadBytes != int64(len(beforeDefective[0])) ||
		receipt.Discard.OrderedDigestSHA256 != wantDiscardDigest {
		t.Fatalf("maintenance receipt discard=%+v", receipt.Discard)
	}
	if receipt.Source.SchemaVersion != source.SchemaVersion ||
		receipt.Source.Head != source.Head ||
		!validSHA256Hex(receipt.Source.SHA256) ||
		receipt.Source.Bytes <= 0 {
		t.Fatalf("maintenance receipt source fingerprint=%+v", receipt.Source)
	}
	targetDigest, targetBytes, err := hashPrivateUpgradeArtifact(artifacts.TargetBackup)
	if err != nil {
		t.Fatal(err)
	}
	if receipt.Target.SchemaVersion != current.SchemaVersion ||
		receipt.Target.Head != current.Head ||
		receipt.Target.SHA256 != targetDigest ||
		receipt.Target.Bytes != targetBytes {
		t.Fatalf(
			"maintenance receipt target=%+v digest=%s bytes=%d",
			receipt.Target, targetDigest, targetBytes,
		)
	}
	target, err := corestore.Inspect(t.Context(), corestore.InspectOptions{
		Path: artifacts.TargetBackup, MinimumHead: &wantHead,
	})
	if err != nil {
		t.Fatal(err)
	}
	if target.Status != corestore.InspectionCurrent ||
		target.SchemaVersion != current.SchemaVersion ||
		target.Head != current.Head {
		t.Fatalf("compact target-head backup=%+v", target)
	}
	targetState := readHistoricalStateDocument(
		t, artifacts.TargetBackup, wantStateScope, wantStateKind,
	)
	targetDefectiveEvidence := readHistoricalObservationRecords(
		t, artifacts.TargetBackup, wantObservationScope, wantObservationSource, wantDefectiveKind,
	)
	targetControlEvidence := readHistoricalObservationRecords(
		t, artifacts.TargetBackup, wantObservationScope, wantObservationSource, wantControlKind,
	)
	if !bytes.Equal(targetState, beforeState) ||
		len(targetDefectiveEvidence) != 0 ||
		!slices.Equal(targetControlEvidence, beforeControlEvidence) {
		t.Fatalf(
			"compact target backup continuity state=%s defective=%+v control=%+v",
			targetState, targetDefectiveEvidence, targetControlEvidence,
		)
	}

	store, err := corestore.Open(t.Context(), corestore.Options{
		Path: databasePath, MinimumHead: &wantHead,
	})
	if err != nil {
		t.Fatal(err)
	}
	doc, ok, err := store.GetStateDocument(t.Context(), wantStateScope, wantStateKind)
	if err != nil || !ok || !bytes.Equal(doc.JSON, beforeState) {
		_ = store.Close()
		t.Fatalf("opened upgraded state found=%v document=%s error=%v", ok, doc.JSON, err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestHistoricalV254SchemaV3MaintenancePreservesHeadAndWatermark(t *testing.T) {
	fixture := historicalUpgradeFixtureByID(t, "v2.5.4-schema-v3-authority")
	wantSchema := historicalExpectation[int](t, fixture, "schema_version")
	wantEpoch := historicalExpectation[string](t, fixture, "authority_epoch")
	wantHeadGeneration := historicalExpectation[int64](t, fixture, "head_generation")
	wantStateScope := historicalExpectation[string](t, fixture, "state_scope")
	wantStateKind := historicalExpectation[string](t, fixture, "state_kind")
	wantStateJSON := historicalExpectation[string](t, fixture, "state_json")
	wantObservationScope := historicalExpectation[string](t, fixture, "defective_observation_scope")
	wantObservationSource := historicalExpectation[string](t, fixture, "defective_observation_source")
	wantDefectiveKind := historicalExpectation[string](t, fixture, "defective_observation_kind")
	wantDefectivePayload := historicalExpectation[string](t, fixture, "defective_observation_payload")
	wantControlKind := historicalExpectation[string](t, fixture, "control_observation_kind")
	wantControlPayload := historicalExpectation[string](t, fixture, "control_observation_payload")

	root := privateTestDir(t)
	materializeHistoricalUpgradeFixture(t, fixture, root)
	databasePath := filepath.Join(root, "daemon.db")
	watermarkPath := databasePath + ".head"
	frozenWatermarkTime := time.Date(2001, 2, 3, 4, 5, 6, 0, time.UTC)
	if err := os.Chtimes(watermarkPath, frozenWatermarkTime, frozenWatermarkTime); err != nil {
		t.Fatal(err)
	}
	watermarkBytesBefore, err := os.ReadFile(watermarkPath)
	if err != nil {
		t.Fatal(err)
	}
	watermarkInfoBefore, err := os.Stat(watermarkPath)
	if err != nil {
		t.Fatal(err)
	}
	if !watermarkInfoBefore.ModTime().Equal(frozenWatermarkTime) {
		t.Fatalf(
			"failed to freeze schema-v3 watermark mtime: got=%s want=%s",
			watermarkInfoBefore.ModTime(), frozenWatermarkTime,
		)
	}
	minimum, err := loadAuthorityWatermark(watermarkPath)
	if err != nil || minimum == nil {
		t.Fatalf("load schema-v3 watermark=%+v error=%v", minimum, err)
	}
	source, err := corestore.Inspect(t.Context(), corestore.InspectOptions{
		Path: databasePath, MinimumHead: minimum,
	})
	if err != nil {
		t.Fatal(err)
	}
	if source.SchemaVersion != wantSchema || source.Status != corestore.InspectionUpgradeRequired ||
		source.Head.AuthorityEpoch != wantEpoch || source.Head.HeadGeneration != wantHeadGeneration {
		t.Fatalf("schema-v3 source inspection=%+v", source)
	}
	beforeState := readHistoricalStateDocument(t, databasePath, wantStateScope, wantStateKind)
	if string(beforeState) != wantStateJSON {
		t.Fatalf("schema-v3 source state=%s", beforeState)
	}
	beforeDefective := readHistoricalObservations(
		t, databasePath, wantObservationScope, wantObservationSource, wantDefectiveKind,
	)
	beforeControl := readHistoricalObservations(
		t, databasePath, wantObservationScope, wantObservationSource, wantControlKind,
	)
	beforeDefectiveEvidence := readHistoricalObservationRecords(
		t, databasePath, wantObservationScope, wantObservationSource, wantDefectiveKind,
	)
	beforeControlEvidence := readHistoricalObservationRecords(
		t, databasePath, wantObservationScope, wantObservationSource, wantControlKind,
	)
	if len(beforeDefective) != 1 || string(beforeDefective[0]) != wantDefectivePayload {
		t.Fatalf("schema-v3 defective observations=%q", beforeDefective)
	}
	if len(beforeControl) != 1 || string(beforeControl[0]) != wantControlPayload {
		t.Fatalf("schema-v3 control observations=%q", beforeControl)
	}
	if len(beforeDefectiveEvidence) != 1 || len(beforeControlEvidence) != 1 {
		t.Fatalf(
			"schema-v3 observation evidence defective=%+v control=%+v",
			beforeDefectiveEvidence, beforeControlEvidence,
		)
	}

	sourceBytes, err := coreSchemaUpgradeSourceFootprint(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	var maintenanceLogs bytes.Buffer
	upgradeTime := time.Date(2026, 7, 31, 12, 30, 0, 0, time.UTC)
	server := &Server{
		coreStorePath: databasePath,
		logger:        NewLogger(&maintenanceLogs, "info"),
		now:           func() time.Time { return upgradeTime },
	}
	upgradedMinimum, err := server.upgradeCoreStoreSchema(t.Context(), minimum)
	if err != nil {
		t.Fatalf("upgrade authentic schema-v3 authority: %v", err)
	}
	targetBytes, err := coreSchemaUpgradeSourceFootprint(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	logs := maintenanceLogs.String()
	wantStart := fmt.Sprintf(
		"daemon authority: one-time database maintenance starting; pruning unused contract-cache history and compacting %s before broker connection; do not interrupt",
		formatStorageBytes(sourceBytes),
	)
	wantCompletionTail := fmt.Sprintf(
		"; database %s -> %s; recovery artifacts verified",
		formatStorageBytes(sourceBytes),
		formatStorageBytes(targetBytes),
	)
	if !strings.Contains(logs, wantStart) ||
		!strings.Contains(logs, "daemon authority: one-time database maintenance completed in ") ||
		!strings.Contains(logs, wantCompletionTail) ||
		strings.Count(logs, "daemon authority: one-time database maintenance") != 2 {
		t.Fatalf("schema maintenance logs=%q", logs)
	}
	if upgradedMinimum == nil || *upgradedMinimum != source.Head {
		t.Fatalf("maintenance-only minimum=%+v, want unchanged %+v", upgradedMinimum, source.Head)
	}
	current, err := corestore.Inspect(t.Context(), corestore.InspectOptions{
		Path: databasePath, MinimumHead: upgradedMinimum,
	})
	if err != nil {
		t.Fatal(err)
	}
	if current.Status != corestore.InspectionCurrent || current.SchemaVersion != source.TargetVersion ||
		current.Head != source.Head {
		t.Fatalf("maintenance-only target=%+v", current)
	}
	afterState := readHistoricalStateDocument(t, databasePath, wantStateScope, wantStateKind)
	if !bytes.Equal(afterState, beforeState) {
		t.Fatalf("schema-v3 state continuity changed: before=%s after=%s", beforeState, afterState)
	}
	afterDefective := readHistoricalObservations(
		t, databasePath, wantObservationScope, wantObservationSource, wantDefectiveKind,
	)
	afterControl := readHistoricalObservations(
		t, databasePath, wantObservationScope, wantObservationSource, wantControlKind,
	)
	afterDefectiveEvidence := readHistoricalObservationRecords(
		t, databasePath, wantObservationScope, wantObservationSource, wantDefectiveKind,
	)
	afterControlEvidence := readHistoricalObservationRecords(
		t, databasePath, wantObservationScope, wantObservationSource, wantControlKind,
	)
	if len(afterDefective) != 0 {
		t.Fatalf("schema-v3 exact defective observations survived maintenance: %q", afterDefective)
	}
	if len(afterDefectiveEvidence) != 0 {
		t.Fatalf("schema-v3 exact defective evidence survived maintenance: %+v", afterDefectiveEvidence)
	}
	if len(afterControl) != 1 || !bytes.Equal(afterControl[0], beforeControl[0]) ||
		!slices.Equal(afterControlEvidence, beforeControlEvidence) {
		t.Fatalf(
			"schema-v3 near-control evidence changed: before=%+v after=%+v",
			beforeControlEvidence, afterControlEvidence,
		)
	}
	watermarkBytesAfter, err := os.ReadFile(watermarkPath)
	if err != nil {
		t.Fatal(err)
	}
	watermarkInfoAfter, err := os.Stat(watermarkPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(watermarkBytesAfter, watermarkBytesBefore) {
		t.Fatalf("maintenance-only upgrade rewrote external watermark bytes")
	}
	if !watermarkInfoAfter.ModTime().Equal(watermarkInfoBefore.ModTime()) {
		t.Fatalf(
			"maintenance-only upgrade changed external watermark mtime: before=%s after=%s",
			watermarkInfoBefore.ModTime(), watermarkInfoAfter.ModTime(),
		)
	}
	if !os.SameFile(watermarkInfoBefore, watermarkInfoAfter) {
		t.Fatalf("maintenance-only upgrade replaced the external watermark file")
	}
	publishedWatermark, err := loadAuthorityWatermark(watermarkPath)
	if err != nil || publishedWatermark == nil || *publishedWatermark != source.Head {
		t.Fatalf("maintenance-only watermark=%+v error=%v", publishedWatermark, err)
	}
	if pending, err := coreSchemaUpgradePending(databasePath); err != nil || pending {
		t.Fatalf("maintenance-only upgrade pending=%v error=%v", pending, err)
	}
}

func loadHistoricalUpgradeManifest(t *testing.T) historicalUpgradeManifest {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(historicalUpgradeFixtureRoot(t), "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	var manifest historicalUpgradeManifest
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&manifest); err != nil {
		t.Fatal(err)
	}
	if err := requireHistoricalJSONEOF(decoder); err != nil {
		t.Fatal(err)
	}
	return manifest
}

func historicalUpgradeFixtureByID(t *testing.T, id string) historicalUpgradeFixture {
	t.Helper()
	manifest := loadHistoricalUpgradeManifest(t)
	for _, fixture := range manifest.Fixtures {
		if fixture.ID == id {
			if fixture.Classification != "must_succeed" {
				t.Fatalf("fixture %s classification=%q, want must_succeed", id, fixture.Classification)
			}
			verifyHistoricalUpgradeFixture(t, fixture, nil)
			return fixture
		}
	}
	t.Fatalf("historical fixture %s is missing", id)
	return historicalUpgradeFixture{}
}

func historicalExpectation[T any](t *testing.T, fixture historicalUpgradeFixture, key string) T {
	t.Helper()
	var value T
	raw, ok := fixture.Expectations[key]
	if !ok {
		t.Fatalf("fixture %s expectation %q is missing", fixture.ID, key)
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&value); err != nil {
		t.Fatalf("fixture %s expectation %q: %v", fixture.ID, key, err)
	}
	if err := requireHistoricalJSONEOF(decoder); err != nil {
		t.Fatalf("fixture %s expectation %q: %v", fixture.ID, key, err)
	}
	return value
}

func verifyHistoricalUpgradeFixture(t *testing.T, fixture historicalUpgradeFixture, manifested map[string]bool) {
	t.Helper()
	files := append([]historicalUpgradeFixtureFile(nil), fixture.Files...)
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	if len(files) == 0 {
		t.Fatalf("fixture %s has no files", fixture.ID)
	}
	for _, file := range files {
		if file.SourceMode != "0600" || file.InstallMode != "0600" {
			t.Fatalf("fixture %s file %s modes=%s/%s", fixture.ID, file.Path, file.SourceMode, file.InstallMode)
		}
		if filepath.IsAbs(file.Path) || filepath.Clean(file.Path) != filepath.FromSlash(file.Path) ||
			file.Path == "." || strings.HasPrefix(file.Path, "../") ||
			!strings.HasPrefix(file.Path, fixture.ID+"/") {
			t.Fatalf("fixture %s unsafe file path %q", fixture.ID, file.Path)
		}
		path := filepath.Join(historicalUpgradeFixtureRoot(t), filepath.FromSlash(file.Path))
		info, err := os.Lstat(path)
		if err != nil {
			t.Fatal(err)
		}
		if !info.Mode().IsRegular() {
			t.Fatalf("fixture file %s mode=%v", path, info.Mode())
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if historicalSHA256(raw) != strings.ToLower(file.SHA256) {
			t.Fatalf("fixture file %s digest changed", file.Path)
		}
		if manifested != nil {
			if manifested[file.Path] {
				t.Fatalf("fixture file %s is listed more than once", file.Path)
			}
			manifested[file.Path] = true
		}
	}
	if historicalArtifactSHA256(files) != strings.ToLower(fixture.ArtifactSHA256) {
		t.Fatalf("fixture %s artifact digest changed", fixture.ID)
	}
	if len(fixture.Expectations) == 0 {
		t.Fatalf("fixture %s has no synthetic expectations", fixture.ID)
	}
}

func materializeHistoricalUpgradeFixture(t *testing.T, fixture historicalUpgradeFixture, root string) {
	t.Helper()
	for _, file := range fixture.Files {
		relative := strings.TrimPrefix(file.Path, fixture.ID+"/")
		if relative == file.Path {
			t.Fatalf("fixture file %s is outside fixture root %s", file.Path, fixture.ID)
		}
		source := filepath.Join(historicalUpgradeFixtureRoot(t), filepath.FromSlash(file.Path))
		destination := filepath.Join(root, filepath.FromSlash(relative))
		if err := os.MkdirAll(filepath.Dir(destination), 0o700); err != nil {
			t.Fatal(err)
		}
		input, err := os.Open(source)
		if err != nil {
			t.Fatal(err)
		}
		output, err := os.OpenFile(destination, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err != nil {
			_ = input.Close()
			t.Fatal(err)
		}
		_, copyErr := io.Copy(output, input)
		closeErr := errors.Join(input.Close(), output.Close())
		if copyErr != nil || closeErr != nil {
			t.Fatal(errors.Join(copyErr, closeErr))
		}
		info, err := os.Stat(destination)
		if err != nil || info.Mode().Perm() != 0o600 {
			t.Fatalf("materialized fixture %s mode=%v error=%v", destination, info.Mode(), err)
		}
	}
}

func historicalUpgradeFixtureRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("testdata", "upgrades"))
	if err != nil {
		t.Fatal(err)
	}
	return root
}

func historicalArtifactSHA256(files []historicalUpgradeFixtureFile) string {
	hash := sha256.New()
	for _, file := range files {
		_, _ = hash.Write([]byte(file.Path))
		_, _ = hash.Write([]byte{0})
		_, _ = hash.Write([]byte(file.SourceMode))
		_, _ = hash.Write([]byte{0})
		_, _ = hash.Write([]byte(strings.ToLower(file.SHA256)))
		_, _ = hash.Write([]byte{'\n'})
	}
	return hex.EncodeToString(hash.Sum(nil))
}

func historicalSHA256(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func requireHistoricalJSONEOF(decoder *json.Decoder) error {
	var extra any
	err := decoder.Decode(&extra)
	if errors.Is(err, io.EOF) {
		return nil
	}
	if err == nil {
		return fmt.Errorf("unexpected trailing JSON value")
	}
	return err
}

func readHistoricalStateDocument(t *testing.T, path, scope, kind string) []byte {
	t.Helper()
	dsn := "file:" + filepath.ToSlash(path) + "?mode=ro&_pragma=foreign_keys(ON)&_dqs=0"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var raw, digest []byte
	if err := db.QueryRowContext(
		context.Background(),
		`SELECT document_json, document_sha256 FROM state_documents WHERE scope_key=? AND kind=?`,
		scope, kind,
	).Scan(&raw, &digest); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(raw)
	if !bytes.Equal(digest, sum[:]) {
		t.Fatalf("state document %s/%s digest mismatch", scope, kind)
	}
	return raw
}

func readHistoricalObservations(t *testing.T, path, scope, source, kind string) [][]byte {
	t.Helper()
	dsn := "file:" + filepath.ToSlash(path) + "?mode=ro&_pragma=foreign_keys(ON)&_dqs=0"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	rows, err := db.QueryContext(
		context.Background(),
		`SELECT payload, payload_sha256
FROM observations
WHERE scope_key=? AND source=? AND kind=?
ORDER BY observation_id`,
		scope, source, kind,
	)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out [][]byte
	for rows.Next() {
		var payload, digest []byte
		if err := rows.Scan(&payload, &digest); err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(payload)
		if !bytes.Equal(digest, sum[:]) {
			t.Fatalf("observation %s/%s/%s digest mismatch", scope, source, kind)
		}
		out = append(out, payload)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func readHistoricalObservationRecords(
	t *testing.T,
	path, scope, source, kind string,
) []historicalObservationRecord {
	t.Helper()
	dsn := "file:" + filepath.ToSlash(path) + "?mode=ro&_pragma=foreign_keys(ON)&_dqs=0"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	rows, err := db.QueryContext(
		context.Background(),
		`SELECT observation_id,
       scope_key,
       source,
       kind,
       observed_at,
       observed_at_ms,
       recorded_at,
       content_type,
       hex(payload),
       lower(hex(payload_sha256)),
       metadata_json IS NULL,
       coalesce(hex(metadata_json), ''),
       decision_eligible
FROM observations
WHERE scope_key=? AND source=? AND kind=?
ORDER BY observation_id`,
		scope, source, kind,
	)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []historicalObservationRecord
	for rows.Next() {
		var record historicalObservationRecord
		if err := rows.Scan(
			&record.ID,
			&record.ScopeKey,
			&record.Source,
			&record.Kind,
			&record.ObservedAt,
			&record.ObservedAtMS,
			&record.RecordedAt,
			&record.ContentType,
			&record.PayloadHex,
			&record.PayloadSHA256Hex,
			&record.MetadataIsNull,
			&record.MetadataJSONHex,
			&record.DecisionEligible,
		); err != nil {
			t.Fatal(err)
		}
		payload, err := hex.DecodeString(record.PayloadHex)
		if err != nil {
			t.Fatal(err)
		}
		if historicalSHA256(payload) != record.PayloadSHA256Hex {
			t.Fatalf("observation %s/%s/%s digest mismatch", scope, source, kind)
		}
		out = append(out, record)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func historicalObservationDiscardDigest(
	t *testing.T,
	selector corestore.ObservationDiscardSelector,
	records []historicalObservationRecord,
) string {
	t.Helper()
	hash := sha256.New()
	_, _ = hash.Write([]byte("canary.observation-discard.v1\x00"))
	for _, value := range []string{selector.ScopeKey, selector.Source, selector.Kind} {
		var size [8]byte
		binary.BigEndian.PutUint64(size[:], uint64(len(value)))
		_, _ = hash.Write(size[:])
		_, _ = hash.Write([]byte(value))
	}
	for _, record := range records {
		digest, err := hex.DecodeString(record.PayloadSHA256Hex)
		if err != nil {
			t.Fatal(err)
		}
		if record.ID <= 0 || len(digest) != sha256.Size {
			t.Fatalf("invalid historical observation identity or digest: %+v", record)
		}
		var identity [8]byte
		binary.BigEndian.PutUint64(identity[:], uint64(record.ID))
		_, _ = hash.Write(identity[:])
		_, _ = hash.Write(digest)
	}
	return hex.EncodeToString(hash.Sum(nil))
}

func findHistoricalMaintenanceArtifacts(
	t *testing.T,
	databasePath string,
	sourceVersion, targetVersion int,
) historicalMaintenanceArtifacts {
	t.Helper()
	backupDirectory := filepath.Join(filepath.Dir(databasePath), "backups")
	entries, err := os.ReadDir(backupDirectory)
	if err != nil {
		t.Fatal(err)
	}
	prefix := fmt.Sprintf(
		"%s-schema-v%d-to-v%d-",
		filepath.Base(databasePath), sourceVersion, targetVersion,
	)
	var sourceBackups, targetBackups, receipts []string
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasPrefix(entry.Name(), prefix) {
			continue
		}
		path := filepath.Join(backupDirectory, entry.Name())
		switch {
		case strings.HasSuffix(entry.Name(), ".target.db"):
			targetBackups = append(targetBackups, path)
		case strings.HasSuffix(entry.Name(), ".maintenance.json"):
			receipts = append(receipts, path)
		case strings.HasSuffix(entry.Name(), ".db"):
			sourceBackups = append(sourceBackups, path)
		}
	}
	if len(sourceBackups) != 0 || len(targetBackups) != 1 || len(receipts) != 1 {
		t.Fatalf(
			"completed maintenance artifacts source=%v target=%v receipts=%v",
			sourceBackups, targetBackups, receipts,
		)
	}
	targetName := filepath.Base(targetBackups[0])
	receiptName := filepath.Base(receipts[0])
	targetID := strings.TrimSuffix(strings.TrimPrefix(targetName, prefix), ".target.db")
	receiptID := strings.TrimSuffix(strings.TrimPrefix(receiptName, prefix), ".maintenance.json")
	if targetID != receiptID || !validCoreSchemaUpgradeID(targetID) {
		t.Fatalf("maintenance artifact identities target=%q receipt=%q", targetID, receiptID)
	}
	for _, path := range []string{targetBackups[0], receipts[0]} {
		info, err := os.Lstat(path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode()&os.ModeSymlink != 0 ||
			!info.Mode().IsRegular() ||
			info.Mode().Perm() != 0o600 {
			t.Fatalf("maintenance artifact %s mode=%v", path, info.Mode())
		}
	}
	return historicalMaintenanceArtifacts{
		UpgradeID:    targetID,
		TargetBackup: targetBackups[0],
		Receipt:      receipts[0],
	}
}
