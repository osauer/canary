package daemon

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/breadth/spx"
	"github.com/osauer/canary/v2/internal/daemon/corestore"
	"github.com/osauer/canary/v2/internal/rpc"
)

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
	newRegimeHistoryCache(hmdsDir, nil).put("USD.JPY", USDJPYLookbackDays, makeBars(10, 150), now)
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
	if manifest.ImportedFiles != 10 || manifest.StateDocuments != 0 || manifest.Observations != 1 {
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

func TestImportLegacyResidualMarketFilesAreValidatedWithoutRetention(t *testing.T) {
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
	authority := openMarketTestCoreStore(t)
	manifest, err := importLegacyMarketObservations(context.Background(), authority)
	if err != nil {
		t.Fatalf("import residual observations: %v\nmanifest=%+v", err, manifest)
	}
	if manifest.ImportedFiles != 3 || manifest.Observations != 0 || manifest.StateDocuments != 0 {
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
		if _, ok, err := authority.LatestObservation(context.Background(), check.scope, check.source, check.kind); err != nil || ok {
			t.Fatalf("legacy %s observation retained: ok=%v err=%v", check.name, ok, err)
		}
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
	stressLine := stressDecisionLine{
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
	stressJournalPath, _ := stressDecisionsDefaultPath()
	writeLegacyJSONLines(t, regimePath, regimeLine)
	writeLegacyJSONLines(t, stressJournalPath, stressLine)
	rotatedDir := filepath.Join(filepath.Dir(regimePath), "rotated")
	olderRegime := regimeLine
	olderRegime.TS = now.AddDate(0, -1, 0)
	olderRegime.SessionKey = "2026-05-02"
	olderStress := stressLine
	olderStress.TS = olderRegime.TS
	olderStress.SessionKey = olderRegime.SessionKey
	writeLegacyGzipJSONLines(t, filepath.Join(rotatedDir, "regime-decisions-2026-05.jsonl.gz"), olderRegime)
	writeLegacyGzipJSONLines(t, filepath.Join(rotatedDir, "canary-decisions-2026-05.jsonl.gz"), olderStress)

	authority := openMarketTestCoreStore(t)
	manifest, err := importLegacyMarketObservations(context.Background(), authority)
	if err != nil {
		t.Fatalf("import: %v\nmanifest=%+v", err, manifest)
	}
	if manifest.ImportedFiles != 4 || manifest.StateDocuments != 0 || manifest.Observations != 0 {
		t.Fatalf("manifest counts = files:%d states:%d observations:%d", manifest.ImportedFiles, manifest.StateDocuments, manifest.Observations)
	}
	regimeObservations, err := authority.ListObservations(context.Background(), corestore.ObservationQuery{
		ScopeKey: legacyRegimeMeasurementScope, Source: legacyRegimeMeasurementSource, Kind: legacyRegimeMeasurementKind,
	})
	if err != nil || len(regimeObservations) != 0 {
		t.Fatalf("regime observations=%d err=%v", len(regimeObservations), err)
	}
	stressObservations, err := authority.ListObservations(context.Background(), corestore.ObservationQuery{
		ScopeKey: legacyStressMeasurementScope, Source: legacyStressMeasurementSource, Kind: legacyStressMeasurementKind,
	})
	if err != nil || len(stressObservations) != 0 {
		t.Fatalf("canary observations=%d err=%v", len(stressObservations), err)
	}
	var archives int
	for _, artifact := range manifest.Artifacts {
		if filepath.Ext(artifact.Path) == ".gz" && artifact.Status == "validated_discarded_beta" {
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
