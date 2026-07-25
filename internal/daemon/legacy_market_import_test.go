package daemon

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/osauer/ibkr/v2/internal/breadth/spx"
	"github.com/osauer/ibkr/v2/internal/daemon/corestore"
	"github.com/osauer/ibkr/v2/internal/rpc"
)

func TestOpenCoreStoreRecoversLegacyDecisionRotationBeforeImport(t *testing.T) {
	tests := []struct {
		name       string
		family     string
		crashStage string
	}{
		{name: "regime_archives_published_pre_swap", family: "regime", crashStage: "pre_swap"},
		{name: "regime_live_tail_swapped", family: "regime", crashStage: "post_swap"},
		{name: "canary_archives_published_pre_swap", family: "canary", crashStage: "pre_swap"},
		{name: "canary_live_tail_swapped", family: "canary", crashStage: "post_swap"},
		{name: "orphan_archive_temp_before_intent", family: "regime", crashStage: "temp_only"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("XDG_STATE_HOME", privateTestDir(t))
			t.Setenv("XDG_CACHE_HOME", privateTestDir(t))
			t.Setenv("XDG_CONFIG_HOME", privateTestDir(t))

			wantSessions, recoveryArtifacts := seedLegacyDecisionRotationCrash(t, tt.family, tt.crashStage)
			s := newCutoverTestServer(t, "")
			if err := s.openCoreStore(t.Context()); err != nil {
				t.Fatalf("open authority after %s rotation crash: %v", tt.crashStage, err)
			}
			t.Cleanup(func() { _ = s.closeCoreStore() })

			query := corestore.ObservationQuery{
				ScopeKey: legacyRegimeMeasurementScope,
				Source:   legacyRegimeMeasurementSource,
				Kind:     legacyRegimeMeasurementKind,
			}
			if tt.family == "canary" {
				query = corestore.ObservationQuery{
					ScopeKey: legacyCanaryMeasurementScope,
					Source:   legacyCanaryMeasurementSource,
					Kind:     legacyCanaryMeasurementKind,
				}
			}
			observations, err := s.coreStore.ListObservations(t.Context(), query)
			if err != nil {
				t.Fatalf("list imported %s measurements: %v", tt.family, err)
			}
			if len(observations) != len(wantSessions) {
				t.Fatalf("imported %s measurement count=%d, want %d", tt.family, len(observations), len(wantSessions))
			}
			gotSessions := make(map[string]int, len(observations))
			for _, observation := range observations {
				if observation.DecisionEligible {
					t.Fatal("recovered legacy measurement is decision-eligible")
				}
				var payload struct {
					SessionKey string `json:"session_key"`
				}
				if err := json.Unmarshal(observation.Payload, &payload); err != nil {
					t.Fatalf("decode imported %s measurement: %v", tt.family, err)
				}
				gotSessions[payload.SessionKey]++
			}
			for _, session := range wantSessions {
				if gotSessions[session] != 1 {
					t.Fatalf("session %s imported %d times after %s crash; want exactly once (all=%v)", session, gotSessions[session], tt.crashStage, gotSessions)
				}
			}
			for _, path := range recoveryArtifacts {
				if _, err := os.Lstat(path); !os.IsNotExist(err) {
					t.Fatalf("rotation recovery artifact remains live after cutover: %s (%v)", filepath.Base(path), err)
				}
			}
			manifest, _, err := loadCoreCutoverManifest(t.Context(), s.coreStore)
			if err != nil {
				t.Fatal(err)
			}
			for _, source := range manifest.Sources {
				if isLegacyRotationRecoveryArtifact(filepath.Base(source.Path)) {
					t.Fatalf("unresolved rotation artifact was sealed instead of recovered: %+v", source)
				}
			}
		})
	}
}

func TestOpenCoreStoreRefusesUnresolvedLegacyRotationIntent(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", privateTestDir(t))
	t.Setenv("XDG_CACHE_HOME", privateTestDir(t))
	t.Setenv("XDG_CONFIG_HOME", privateTestDir(t))
	regimePath, err := regimeDecisionsDefaultPath()
	if err != nil {
		t.Fatal(err)
	}
	rotatedDir := filepath.Join(filepath.Dir(regimePath), "rotated")
	if err := os.MkdirAll(rotatedDir, 0o700); err != nil {
		t.Fatal(err)
	}
	intentPath := filepath.Join(rotatedDir, ".rotation-intent-regime.json")
	if err := os.WriteFile(intentPath, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	s := newCutoverTestServer(t, "")
	if err := s.openCoreStore(t.Context()); err == nil {
		t.Fatal("cutover accepted an unresolved legacy rotation intent")
	}
	if _, err := os.Lstat(s.coreStorePath); !os.IsNotExist(err) {
		t.Fatalf("failed rotation recovery published daemon authority: %v", err)
	}
}

func TestImportLegacyMarketObservationsPreflightsAndPreservesExactBytes(t *testing.T) {
	cacheRoot := t.TempDir()
	stateRoot := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", cacheRoot)
	t.Setenv("XDG_STATE_HOME", stateRoot)
	now := time.Date(2026, 6, 2, 15, 0, 0, 0, time.UTC)

	gammaDir, _ := gammaZeroStoreDefaultDir()
	gamma := newGammaZeroStore(gammaDir)
	result := helperGammaResult(now)
	if err := gamma.Save(rpc.GammaZeroScopeCombined, nySessionKey(now), result); err != nil {
		t.Fatalf("seed gamma: %v", err)
	}
	legacyGammaRaw, err := os.ReadFile(filepath.Join(gammaDir, gammaZeroStoreFilename(rpc.GammaZeroScopeCombined)))
	if err != nil {
		t.Fatalf("read seeded gamma: %v", err)
	}
	oi := newGammaOpenInterestStore(gammaDir)
	oiKey := gammaOIKey("SPX", "SPXW", "20260605", 7600, "P")
	if err := oi.SaveMerged(map[string]gammaOIRecord{
		oiKey: gammaOIRecordForLeg("SPX", "SPXW", "20260605", 7600, "P", 123, now),
	}); err != nil {
		t.Fatalf("seed OI: %v", err)
	}
	grids := newExpiryGridStore(gammaDir)
	if err := grids.noteFetched("SPY", testClassedGrid("2026-06-03", "2026-06-05"), now); err != nil {
		t.Fatalf("seed grid: %v", err)
	}

	hmdsDir, _ := regimeHistoryCacheDefaultDir()
	newRegimeHistoryCache(hmdsDir).put("USD.JPY", USDJPYLookbackDays, makeBars(10, 150), now)
	seriesDir, _ := regimeSeriesCacheDefaultDir()
	newRegimeSeriesCache(seriesDir).put(fredSeriesHYOAS, makeSeries(21, 3.5), now)
	streakDir, _ := DefaultStreakStoreDir()
	NewStreakStore(streakDir).Tick(StreakKeyVIXTerm, 0.85, "green", now.In(newYorkLocation()))

	breadthDir, _ := spx.DefaultDir()
	breadth := spx.NewStore(breadthDir)
	if err := breadth.SaveSnapshot(spx.Snapshot{
		Value: 55, PctAbove50DMA: 55, AsOf: now, SessionKey: "2026-06-02",
		Method: spx.MethodConstituentFanout, MemberCount: 500, Coverage: 490,
	}); err != nil {
		t.Fatalf("seed breadth snapshot: %v", err)
	}
	if err := breadth.SaveWindows(map[string]spx.ConstituentWindow{
		"AAPL": {Symbol: "AAPL", Closes: []float64{100, 101}, LastBarAt: "2026-06-02"},
	}, now); err != nil {
		t.Fatalf("seed breadth windows: %v", err)
	}
	if err := breadth.SaveHistory([]spx.HistoryPoint{{Date: "2026-06-02", PctAbove50DMA: 55}}); err != nil {
		t.Fatalf("seed breadth history: %v", err)
	}
	skewPath, _ := gammaSkewDiagDefaultPath()
	if err := (&gammaSkewDiagJournal{path: skewPath}).append(now, rpc.GammaZeroScopeCombined, "2026-06-02", rankableCombinedGammaFixture(now)); err != nil {
		t.Fatalf("seed skew diagnostics: %v", err)
	}

	authority := openMarketTestCoreStore(t)
	manifest, err := importLegacyMarketObservations(context.Background(), authority)
	if err != nil {
		t.Fatalf("import: %v\nmanifest=%+v", err, manifest)
	}
	if manifest.ImportedFiles != 10 || manifest.StateDocuments != 0 || manifest.Observations != 12 {
		t.Fatalf("manifest counts = files:%d states:%d observations:%d", manifest.ImportedFiles, manifest.StateDocuments, manifest.Observations)
	}
	if _, ok, err := authority.GetStateDocument(context.Background(), gammaZeroAuthorityScope(rpc.GammaZeroScopeCombined), gammaZeroStateKind); err != nil || ok {
		t.Fatalf("legacy import seeded current gamma state: ok=%v err=%v", ok, err)
	}
	observation, ok, err := authority.LatestObservation(
		context.Background(), gammaZeroAuthorityScope(rpc.GammaZeroScopeCombined), gammaZeroSource, gammaZeroObservationKind,
	)
	if err != nil || !ok {
		t.Fatalf("latest imported gamma: ok=%v err=%v", ok, err)
	}
	if !bytes.Equal(observation.Payload, legacyGammaRaw) {
		t.Fatal("import did not preserve exact legacy gamma bytes")
	}
}

func TestImportLegacyMarketObservationsMalformedArtifactWritesNothing(t *testing.T) {
	cacheRoot := t.TempDir()
	stateRoot := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", cacheRoot)
	t.Setenv("XDG_STATE_HOME", stateRoot)
	gammaDir, _ := gammaZeroStoreDefaultDir()
	valid := helperGammaResult(time.Date(2026, 6, 2, 15, 0, 0, 0, time.UTC))
	if err := newGammaZeroStore(gammaDir).Save(rpc.GammaZeroScopeCombined, "2026-06-02", valid); err != nil {
		t.Fatalf("seed valid gamma before malformed artifact: %v", err)
	}
	if err := os.WriteFile(filepath.Join(gammaDir, gammaOIStateFilename), []byte("{"), 0o600); err != nil {
		t.Fatalf("seed malformed OI: %v", err)
	}
	authority := openMarketTestCoreStore(t)
	manifest, err := importLegacyMarketObservations(context.Background(), authority)
	if err == nil {
		t.Fatalf("malformed import succeeded: %+v", manifest)
	}
	head, headErr := authority.AuthorityHead(context.Background())
	if headErr != nil {
		t.Fatalf("AuthorityHead: %v", headErr)
	}
	if head.HeadGeneration != 0 || head.LastEventSeq != 0 {
		t.Fatalf("preflight failure mutated authority head: %+v", head)
	}
	if _, ok, stateErr := authority.GetStateDocument(context.Background(), gammaZeroAuthorityScope(rpc.GammaZeroScopeCombined), gammaZeroStateKind); stateErr != nil || ok {
		t.Fatalf("preflight failure wrote state: ok=%v err=%v", ok, stateErr)
	}
}

func TestImportLegacyResidualMarketFilesAsObservationsOnly(t *testing.T) {
	cacheRoot := t.TempDir()
	stateRoot := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", cacheRoot)
	t.Setenv("XDG_STATE_HOME", stateRoot)
	now := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)
	cacheDir, _ := fxRateStoreDefaultDir()
	if err := newFXRateStore(cacheDir).save(map[string]fxCachedRate{
		"EUR/USD": {rate: 0.88, at: now},
	}); err != nil {
		t.Fatalf("seed legacy FX: %v", err)
	}
	if err := (&earningsStore{dir: cacheDir}).save(map[string]earningsEntry{
		"AAPL": {Date: "2026-07-30", TimeOfDay: "amc", ObservedAt: now},
	}); err != nil {
		t.Fatalf("seed legacy earnings: %v", err)
	}
	membersPath, _ := spx.MembersDefaultPath()
	members, _ := spx.MemberList()
	if err := spx.SaveExternal(membersPath, members, now); err != nil {
		t.Fatalf("seed legacy SPX members: %v", err)
	}
	wantRaw := map[string][]byte{}
	for kind, path := range map[string]string{
		"fx":       filepath.Join(cacheDir, fxRateStoreFilename),
		"earnings": filepath.Join(cacheDir, earningsStoreFilename),
		"members":  membersPath,
	} {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s fixture: %v", kind, err)
		}
		wantRaw[kind] = raw
	}

	authority := openMarketTestCoreStore(t)
	manifest, err := importLegacyMarketObservations(context.Background(), authority)
	if err != nil {
		t.Fatalf("import residual observations: %v\nmanifest=%+v", err, manifest)
	}
	if manifest.ImportedFiles != 3 || manifest.Observations != 3 || manifest.StateDocuments != 0 {
		t.Fatalf("residual manifest counts: files=%d observations=%d states=%d", manifest.ImportedFiles, manifest.Observations, manifest.StateDocuments)
	}
	checks := []struct {
		name, scope, source, kind, stateKind string
	}{
		{"fx", fxAuthorityScope, fxObservationSource, fxObservationKind, fxStateKind},
		{"earnings", earningsAuthorityScope, earningsObservationSource, earningsObservationKind, earningsStateKind},
		{"members", "market/breadth/spx/members", "wikipedia.sp500_constituents", "spx_members.snapshot.v1", "spx_members.current.v1"},
	}
	for _, check := range checks {
		observation, ok, err := authority.LatestObservation(context.Background(), check.scope, check.source, check.kind)
		if err != nil || !ok {
			t.Fatalf("%s observation missing: ok=%v err=%v", check.name, ok, err)
		}
		if !bytes.Equal(observation.Payload, wantRaw[check.name]) {
			t.Fatalf("%s observation did not preserve exact bytes", check.name)
		}
		if observation.DecisionEligible {
			t.Fatalf("legacy %s observation is decision-eligible", check.name)
		}
		if _, ok, err := authority.LatestDecisionEligibleObservation(context.Background(), check.scope, check.source, check.kind); err != nil || ok {
			t.Fatalf("legacy %s crossed eligible reader: ok=%v err=%v", check.name, ok, err)
		}
		assertLegacyObservationMetadata(t, observation.MetadataJSON)
		if _, ok, err := authority.GetStateDocument(context.Background(), check.scope, check.stateKind); err != nil || ok {
			t.Fatalf("legacy %s seeded current state: ok=%v err=%v", check.name, ok, err)
		}
	}
}

func TestImportLegacyResidualMalformedRowFailsBeforeAnyWrite(t *testing.T) {
	cacheRoot := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", cacheRoot)
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	cacheDir, _ := fxRateStoreDefaultDir()
	if err := os.MkdirAll(cacheDir, 0o700); err != nil {
		t.Fatalf("mkdir cache dir: %v", err)
	}
	malformed := []byte(`{"version":1,"rates":{"EUR/USD":{"rate":-1,"at":"2026-07-20T12:00:00Z"}}}`)
	if err := os.WriteFile(filepath.Join(cacheDir, fxRateStoreFilename), malformed, 0o600); err != nil {
		t.Fatalf("seed malformed residual FX: %v", err)
	}
	authority := openMarketTestCoreStore(t)
	if _, err := importLegacyMarketObservations(context.Background(), authority); err == nil {
		t.Fatal("malformed residual observation imported")
	}
	head, err := authority.AuthorityHead(context.Background())
	if err != nil {
		t.Fatalf("AuthorityHead: %v", err)
	}
	if head.HeadGeneration != 0 || head.LastEventSeq != 0 {
		t.Fatalf("residual preflight failure mutated authority: %+v", head)
	}
}

func TestImportLegacyDecisionMeasurementsRedactsDecisionAndAccountData(t *testing.T) {
	cacheRoot := t.TempDir()
	stateRoot := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", cacheRoot)
	t.Setenv("XDG_STATE_HOME", stateRoot)
	now := time.Date(2026, 6, 2, 15, 0, 0, 0, time.UTC)
	value := 0.87
	regimeLine := regimeDecisionLine{
		V: 1, TS: now, SessionKey: "2026-06-02", TapeSession: rpc.TapeSessionTradingDate,
		Fingerprint: "decision-fingerprint-must-not-import", Stage: "confirmed_stress",
		Severity: "red", Verdict: "Stress signal present",
		Indicators: map[string]regimeDecisionIndicator{
			StreakKeyVIXTerm: {Status: "ready", Band: "green", Value: &value, Freshness: "fresh"},
		},
	}
	spy := 590.25
	vix := 21.4
	accountFingerprint := &rpc.Fingerprint{Version: "1", Key: "account-fingerprint-must-not-import"}
	regimeFingerprint := &rpc.Fingerprint{Version: "1", Key: "regime-source-ok"}
	canaryLine := canaryDecisionLine{
		V: 1, TS: now, SessionKey: "2026-06-02", Fingerprint: "canary-decision-must-not-import",
		Account: "SECRET-ACCOUNT", AccountMode: "live", Action: "defend", Summary: "decision summary",
		Market: rpc.StressMarketSummary{
			RegimeVerdict: "Stress signal present",
			RegimePosture: rpc.RegimePosture{Stage: "confirmed_stress", Severity: "red"},
			RedClusters:   2, EligibleRedClusters: 1, SPYPrice: &spy, VIX: &vix,
			TapeSessionState: rpc.TapeSessionTradingDate,
		},
		SourceAsOf: rpc.StressSourceAsOf{Account: now, Positions: now, Regime: now, MarketEvents: now},
		SourceFingerprints: rpc.StressSourceFingerprints{
			Account: accountFingerprint, Positions: accountFingerprint,
			Regime: regimeFingerprint, MarketEvents: regimeFingerprint,
		},
	}
	regimePath, _ := regimeDecisionsDefaultPath()
	canaryPath, _ := canaryDecisionsDefaultPath()
	writeLegacyJSONLines(t, regimePath, regimeLine)
	writeLegacyJSONLines(t, canaryPath, canaryLine)
	rotatedDir := filepath.Join(filepath.Dir(regimePath), "rotated")
	olderRegime := regimeLine
	olderRegime.TS = now.AddDate(0, -1, 0)
	olderRegime.SessionKey = "2026-05-02"
	olderCanary := canaryLine
	olderCanary.TS = olderRegime.TS
	olderCanary.SessionKey = olderRegime.SessionKey
	writeLegacyGzipJSONLines(t, filepath.Join(rotatedDir, "regime-decisions-2026-05.jsonl.gz"), olderRegime)
	writeLegacyGzipJSONLines(t, filepath.Join(rotatedDir, "canary-decisions-2026-05.jsonl.gz"), olderCanary)

	authority := openMarketTestCoreStore(t)
	manifest, err := importLegacyMarketObservations(context.Background(), authority)
	if err != nil {
		t.Fatalf("import: %v\nmanifest=%+v", err, manifest)
	}
	if manifest.ImportedFiles != 4 || manifest.StateDocuments != 0 || manifest.Observations != 4 {
		t.Fatalf("manifest counts = files:%d states:%d observations:%d", manifest.ImportedFiles, manifest.StateDocuments, manifest.Observations)
	}
	regimeObservations, err := authority.ListObservations(context.Background(), corestore.ObservationQuery{
		ScopeKey: legacyRegimeMeasurementScope, Source: legacyRegimeMeasurementSource, Kind: legacyRegimeMeasurementKind,
	})
	if err != nil || len(regimeObservations) != 2 {
		t.Fatalf("regime observations=%d err=%v", len(regimeObservations), err)
	}
	canaryObservations, err := authority.ListObservations(context.Background(), corestore.ObservationQuery{
		ScopeKey: legacyCanaryMeasurementScope, Source: legacyCanaryMeasurementSource, Kind: legacyCanaryMeasurementKind,
	})
	if err != nil || len(canaryObservations) != 2 {
		t.Fatalf("canary observations=%d err=%v", len(canaryObservations), err)
	}
	for _, observation := range regimeObservations {
		if observation.DecisionEligible {
			t.Fatal("legacy regime measurement is decision-eligible")
		}
		var payload map[string]any
		if err := json.Unmarshal(observation.Payload, &payload); err != nil {
			t.Fatalf("decode regime projection: %v", err)
		}
		for _, forbidden := range []string{"fingerprint", "stage", "severity", "readiness", "confidence", "verdict", "composite", "governors"} {
			if _, exists := payload[forbidden]; exists {
				t.Fatalf("regime projection retained forbidden %q: %s", forbidden, observation.Payload)
			}
		}
		assertLegacyObservationMetadata(t, observation.MetadataJSON)
	}
	for _, observation := range canaryObservations {
		if observation.DecisionEligible {
			t.Fatal("legacy canary measurement is decision-eligible")
		}
		var payload map[string]any
		if err := json.Unmarshal(observation.Payload, &payload); err != nil {
			t.Fatalf("decode canary projection: %v", err)
		}
		for _, forbidden := range []string{"fingerprint", "account", "account_mode", "action", "severity", "portfolio_fit", "held_stress", "rows", "summary"} {
			if _, exists := payload[forbidden]; exists {
				t.Fatalf("canary projection retained forbidden %q: %s", forbidden, observation.Payload)
			}
		}
		market := payload["market"].(map[string]any)
		if _, exists := market["regime_verdict"]; exists {
			t.Fatalf("canary market retained verdict: %s", observation.Payload)
		}
		if _, exists := market["regime_posture"]; exists {
			t.Fatalf("canary market retained stage/posture: %s", observation.Payload)
		}
		for _, key := range []string{"source_as_of", "source_fingerprints"} {
			sources := payload[key].(map[string]any)
			if _, exists := sources["account"]; exists {
				t.Fatalf("%s retained account data: %s", key, observation.Payload)
			}
			if _, exists := sources["positions"]; exists {
				t.Fatalf("%s retained portfolio data: %s", key, observation.Payload)
			}
		}
		assertLegacyObservationMetadata(t, observation.MetadataJSON)
	}
	var archives int
	for _, artifact := range manifest.Artifacts {
		if filepath.Ext(artifact.Path) == ".gz" && artifact.Status == "imported" {
			archives++
			if len(artifact.SHA256) != 64 || artifact.Records != 1 {
				t.Fatalf("archive manifest missing hash/count: %+v", artifact)
			}
		}
	}
	if archives != 2 {
		t.Fatalf("imported archives=%d, want 2", archives)
	}
}

func TestImportLegacyDecisionMeasurementsRejectsUnknownSchemaBeforeWrites(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	regimePath, _ := regimeDecisionsDefaultPath()
	if err := os.MkdirAll(filepath.Dir(regimePath), 0o700); err != nil {
		t.Fatalf("mkdir journal dir: %v", err)
	}
	if err := os.WriteFile(regimePath, []byte(`{"v":2,"ts":"2026-06-02T15:00:00Z","session_key":"2026-06-02","indicators":{}}`+"\n"), 0o600); err != nil {
		t.Fatalf("seed unknown schema: %v", err)
	}
	authority := openMarketTestCoreStore(t)
	if _, err := importLegacyMarketObservations(context.Background(), authority); err == nil {
		t.Fatal("unknown journal schema imported")
	}
	head, err := authority.AuthorityHead(context.Background())
	if err != nil {
		t.Fatalf("AuthorityHead: %v", err)
	}
	if head.HeadGeneration != 0 {
		t.Fatalf("failed preflight mutated authority: %+v", head)
	}
}

func writeLegacyJSONLines(t *testing.T, path string, values ...any) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("mkdir legacy journal dir: %v", err)
	}
	var data []byte
	for _, value := range values {
		line, err := json.Marshal(value)
		if err != nil {
			t.Fatalf("marshal legacy line: %v", err)
		}
		data = append(data, line...)
		data = append(data, '\n')
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write legacy journal: %v", err)
	}
}

func writeLegacyGzipJSONLines(t *testing.T, path string, values ...any) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("mkdir rotated dir: %v", err)
	}
	file, err := os.Create(path)
	if err != nil {
		t.Fatalf("create gzip archive: %v", err)
	}
	writer := gzip.NewWriter(file)
	for _, value := range values {
		line, err := json.Marshal(value)
		if err != nil {
			t.Fatalf("marshal rotated line: %v", err)
		}
		if _, err := writer.Write(append(line, '\n')); err != nil {
			t.Fatalf("write gzip line: %v", err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close gzip writer: %v", err)
	}
	if err := file.Close(); err != nil {
		t.Fatalf("close gzip file: %v", err)
	}
}

type legacyRotationArchiveFixture struct {
	Name     string `json:"name"`
	Months   string `json:"months"`
	GzBytes  int64  `json:"gz_bytes"`
	RawBytes int64  `json:"raw_bytes"`
	SHA256   string `json:"sha256"`
}

type legacyRotationManifestFixture struct {
	Version     int                            `json:"version"`
	Source      string                         `json:"source"`
	StartedAt   string                         `json:"started_at"`
	CutBytes    int64                          `json:"cut_bytes"`
	LiveSize    int64                          `json:"live_size"`
	BaseBefore  int64                          `json:"base_before"`
	PreGenesis  string                         `json:"pre_genesis"`
	PostGenesis string                         `json:"post_genesis"`
	PreSHA256   string                         `json:"pre_sha256"`
	PostSHA256  string                         `json:"post_sha256"`
	Archives    []legacyRotationArchiveFixture `json:"archives"`
}

// seedLegacyDecisionRotationCrash writes only states the retired rotation
// protocol can durably expose. Archives are final before the live-tail swap;
// a temp-only fixture represents the earlier pre-intent cleanup boundary.
func seedLegacyDecisionRotationCrash(t *testing.T, family, crashStage string) ([]string, []string) {
	t.Helper()
	regimePath, err := regimeDecisionsDefaultPath()
	if err != nil {
		t.Fatal(err)
	}
	livePath := regimePath
	if family == "canary" {
		livePath, err = canaryDecisionsDefaultPath()
		if err != nil {
			t.Fatal(err)
		}
	}
	rotatedDir := filepath.Join(filepath.Dir(regimePath), "rotated")
	if err := os.MkdirAll(rotatedDir, 0o700); err != nil {
		t.Fatal(err)
	}

	times := []time.Time{
		time.Date(2026, 4, 2, 15, 0, 0, 0, time.UTC),
		time.Date(2026, 4, 3, 15, 0, 0, 0, time.UTC),
		time.Date(2026, 7, 2, 15, 0, 0, 0, time.UTC),
	}
	sessions := []string{"2026-04-02", "2026-04-03", "2026-07-02"}
	lines := make([][]byte, 0, len(times))
	for i, at := range times {
		var value any
		switch family {
		case "regime":
			value = regimeDecisionLine{
				V: 1, TS: at, SessionKey: sessions[i], Fingerprint: "decision-only",
				Indicators: map[string]regimeDecisionIndicator{
					StreakKeyVIXTerm: {Status: "ready", Band: "green"},
				},
			}
		case "canary":
			value = canaryDecisionLine{
				V: 1, TS: at, SessionKey: sessions[i], Fingerprint: "decision-only",
				Market: rpc.StressMarketSummary{RedClusters: i + 1},
			}
		default:
			t.Fatalf("unknown fixture family %q", family)
		}
		line, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		lines = append(lines, line)
	}
	prefix := appendLegacyLines(nil, lines[:2]...)
	tail := appendLegacyLines(nil, lines[2:]...)
	full := append(append([]byte(nil), prefix...), tail...)
	archiveName := family + "-decisions-2026-04.jsonl.gz"
	archivePath := filepath.Join(rotatedDir, archiveName)
	if crashStage == "temp_only" {
		archivePath = filepath.Join(rotatedDir, ".tmp-"+archiveName)
	}
	gzBytes := writeLegacyGzipRaw(t, archivePath, prefix)

	live := full
	if crashStage == "post_swap" {
		live = tail
	}
	if err := os.MkdirAll(filepath.Dir(livePath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(livePath, live, 0o600); err != nil {
		t.Fatal(err)
	}

	artifacts := []string{archivePath}
	if crashStage != "temp_only" {
		intentPath := filepath.Join(rotatedDir, ".rotation-intent-"+family+".json")
		manifest := legacyRotationManifestFixture{
			Version: 1, Source: family, StartedAt: "2026-07-20T12:00:00Z",
			CutBytes: int64(len(prefix)), LiveSize: int64(len(full)), BaseBefore: 0,
			PreGenesis:  legacySHA256(lines[0]),
			PostGenesis: legacySHA256(lines[2]),
			PreSHA256:   legacySHA256(full),
			PostSHA256:  legacySHA256(tail),
			Archives: []legacyRotationArchiveFixture{{
				Name: archiveName, Months: "2026-04", GzBytes: gzBytes,
				RawBytes: int64(len(prefix)), SHA256: legacySHA256(prefix),
			}},
		}
		raw, err := json.Marshal(manifest)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(intentPath, append(raw, '\n'), 0o600); err != nil {
			t.Fatal(err)
		}
		artifacts = append(artifacts, intentPath)
	}
	return sessions, artifacts
}

func appendLegacyLines(dst []byte, lines ...[]byte) []byte {
	for _, line := range lines {
		dst = append(dst, line...)
		dst = append(dst, '\n')
	}
	return dst
}

func writeLegacyGzipRaw(t *testing.T, path string, raw []byte) int64 {
	t.Helper()
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	writer := gzip.NewWriter(file)
	if _, err := writer.Write(raw); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return info.Size()
}

func legacySHA256(raw []byte) string {
	digest := sha256.Sum256(raw)
	return hex.EncodeToString(digest[:])
}

func assertLegacyObservationMetadata(t *testing.T, raw []byte) {
	t.Helper()
	var metadata map[string]any
	if err := json.Unmarshal(raw, &metadata); err != nil {
		t.Fatalf("decode observation metadata: %v", err)
	}
	if eligible, ok := metadata["decision_eligible"].(bool); !ok || eligible {
		t.Fatalf("legacy observation eligibility = %#v", metadata["decision_eligible"])
	}
	if digest, _ := metadata["legacy_file_sha256"].(string); len(digest) != 64 {
		t.Fatalf("legacy observation missing file hash: %+v", metadata)
	}
}
