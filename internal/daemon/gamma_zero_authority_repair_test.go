package daemon

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/daemon/corestore"
	"github.com/osauer/canary/v2/internal/rpc"
)

func TestRepairGammaLastGoodAuthorityPromotesValidatedObservationOnce(t *testing.T) {
	authority := openMarketTestCoreStore(t)
	at := time.Date(2026, 7, 20, 16, 7, 48, 0, newYorkLocation())
	result := helperCombinedGammaResult(at)
	payload := gammaPersistPayload(t, rpc.GammaZeroScopeCombined, at, result)
	if _, err := authority.AppendObservation(t.Context(), corestore.ObservationInput{
		ScopeKey: gammaZeroAuthorityScope(rpc.GammaZeroScopeCombined),
		Source:   gammaZeroSource, Kind: gammaZeroObservationKind,
		ObservedAt: at, ContentType: "application/json", Payload: payload,
		MetadataJSON: []byte(`{"imported_from_legacy":true}`),
	}); err != nil {
		t.Fatal(err)
	}
	before, err := authority.AuthorityHead(t.Context())
	if err != nil {
		t.Fatal(err)
	}

	server := &Server{}
	if err := server.repairGammaLastGoodAuthority(t.Context(), authority); err != nil {
		t.Fatal(err)
	}
	doc, ok, err := authority.GetStateDocument(t.Context(), gammaZeroAuthorityScope(rpc.GammaZeroScopeCombined), gammaZeroStateKind)
	if err != nil || !ok {
		t.Fatalf("promoted state ok=%v err=%v", ok, err)
	}
	if doc.Revision != 1 {
		t.Fatalf("promoted document revision=%d", doc.Revision)
	}
	var promoted gammaZeroPersistEnvelope
	if err := json.Unmarshal(doc.JSON, &promoted); err != nil || promoted.Result == nil ||
		promoted.Result.AuthorityProvenance != gammaAuthorityProvenanceRecoveredObservation {
		t.Fatalf("promoted provenance=%+v err=%v", promoted.Result, err)
	}
	observation, ok, err := authority.LatestObservation(t.Context(), gammaZeroAuthorityScope(rpc.GammaZeroScopeCombined), gammaZeroSource, gammaZeroObservationKind)
	if err != nil || !ok || observation.DecisionEligible {
		t.Fatalf("retained observation ok=%v eligible=%v err=%v", ok, observation.DecisionEligible, err)
	}
	if !bytes.Equal(observation.Payload, payload) {
		t.Fatal("repair changed immutable observation bytes")
	}

	store := newGammaZeroStore("")
	if err := store.UseCoreStore(authority); err != nil {
		t.Fatal(err)
	}
	loaded, err := store.LoadStale(rpc.GammaZeroScopeCombined)
	if err != nil || loaded == nil || !loaded.AsOf.Equal(at) {
		t.Fatalf("hydrated last-good=%+v err=%v", loaded, err)
	}
	annotateGammaQuality(loaded, at.Add(10*time.Minute))
	if loaded.Quality == nil || loaded.Quality.Rankability != rpc.GammaRankabilityContextOnly {
		t.Fatalf("same-session recovered gamma quality=%+v, want context_only", loaded.Quality)
	}
	afterFirst, err := authority.AuthorityHead(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if afterFirst.HeadGeneration != before.HeadGeneration+1 {
		t.Fatalf("repair head=%+v before=%+v", afterFirst, before)
	}
	if err := server.repairGammaLastGoodAuthority(t.Context(), authority); err != nil {
		t.Fatal(err)
	}
	afterSecond, err := authority.AuthorityHead(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if afterSecond != afterFirst {
		t.Fatalf("idempotent repair changed head: first=%+v second=%+v", afterFirst, afterSecond)
	}

	// A normal current-code compute replaces the quarantined state and does
	// not inherit recovery provenance from the last-good observation.
	freshAt := at.Add(5 * time.Minute)
	fresh := helperCombinedGammaResult(freshAt)
	if err := store.Save(rpc.GammaZeroScopeCombined, nySessionKey(freshAt), fresh); err != nil {
		t.Fatal(err)
	}
	replaced, err := store.LoadStale(rpc.GammaZeroScopeCombined)
	if err != nil || replaced == nil || replaced.AuthorityProvenance != "" {
		t.Fatalf("fresh compute did not supersede recovery provenance: result=%+v err=%v", replaced, err)
	}
	quality := buildGammaSignalQuality(replaced, freshAt.Add(time.Minute))
	for _, gate := range quality.Gates {
		if gate.Name == "authority_provenance" {
			t.Fatalf("fresh compute retained authority-provenance gate: %+v", quality)
		}
	}
}

func TestRepairGammaLastGoodAuthorityRejectsUncurrentOrMalformedEvidence(t *testing.T) {
	authority := openMarketTestCoreStore(t)
	at := time.Date(2026, 7, 20, 16, 7, 48, 0, newYorkLocation())
	result := helperCombinedGammaResult(at)
	result.Method = "retired-method"
	payload := gammaPersistPayload(t, rpc.GammaZeroScopeCombined, at, result)
	if _, err := authority.AppendObservation(t.Context(), corestore.ObservationInput{
		ScopeKey: gammaZeroAuthorityScope(rpc.GammaZeroScopeCombined),
		Source:   gammaZeroSource, Kind: gammaZeroObservationKind,
		ObservedAt: at, ContentType: "application/json", Payload: payload,
		MetadataJSON: []byte(`{"imported_from_legacy":true}`),
	}); err != nil {
		t.Fatal(err)
	}
	if err := (&Server{}).repairGammaLastGoodAuthority(t.Context(), authority); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := authority.GetStateDocument(t.Context(), gammaZeroAuthorityScope(rpc.GammaZeroScopeCombined), gammaZeroStateKind); err != nil || ok {
		t.Fatalf("invalid evidence promoted: ok=%v err=%v", ok, err)
	}
}

func TestValidatedGammaLastGoodStatePayloadRejectsShapeAndSessionMismatches(t *testing.T) {
	t.Parallel()
	baseAt := time.Date(2026, 7, 20, 16, 7, 48, 0, newYorkLocation())
	for _, test := range []struct {
		name   string
		at     time.Time
		mutate func(*rpc.GammaZeroComputed)
	}{
		{name: "result_scope_missing", at: baseAt, mutate: func(result *rpc.GammaZeroComputed) { result.Scope = "" }},
		{name: "combined_child_missing", at: baseAt, mutate: func(result *rpc.GammaZeroComputed) { delete(result.PerIndex, "SPX") }},
		{name: "nested_method_mismatch", at: baseAt, mutate: func(result *rpc.GammaZeroComputed) { result.PerIndex["SPX"].Method = "retired" }},
		{name: "nested_session_mismatch", at: baseAt, mutate: func(result *rpc.GammaZeroComputed) { result.PerIndex["SPX"].AsOf = baseAt.AddDate(0, 0, -1) }},
		{name: "future_timestamp", at: time.Now().Add(10 * time.Minute), mutate: func(*rpc.GammaZeroComputed) {}},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			result := helperCombinedGammaResult(test.at)
			test.mutate(result)
			payload := gammaPersistPayload(t, rpc.GammaZeroScopeCombined, test.at, result)
			_, err := validatedGammaLastGoodStatePayload(rpc.GammaZeroScopeCombined, corestore.Observation{
				ObservedAt: test.at, ContentType: "application/json", Payload: payload,
				MetadataJSON: []byte(`{"imported_from_legacy":true}`),
			})
			if err == nil {
				t.Fatal("malformed retained gamma evidence was accepted")
			}
		})
	}
}

func TestValidatedGammaLastGoodStatePayloadRejectsNonLegacyEvidence(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 7, 20, 16, 7, 48, 0, newYorkLocation())
	payload := gammaPersistPayload(t, rpc.GammaZeroScopeCombined, at, helperCombinedGammaResult(at))
	for _, test := range []struct {
		name             string
		metadata         []byte
		decisionEligible bool
	}{
		{name: "metadata_missing"},
		{name: "legacy_marker_false", metadata: []byte(`{"imported_from_legacy":false}`)},
		{name: "decision_eligible", metadata: []byte(`{"imported_from_legacy":true}`), decisionEligible: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			_, err := validatedGammaLastGoodStatePayload(rpc.GammaZeroScopeCombined, corestore.Observation{
				ObservedAt: at, ContentType: "application/json", Payload: payload,
				MetadataJSON: test.metadata, DecisionEligible: test.decisionEligible,
			})
			if err == nil {
				t.Fatal("non-legacy or decision-eligible gamma evidence was accepted")
			}
		})
	}
}

func TestOpenCoreStoreRepairsImportedGammaAcrossRestart(t *testing.T) {
	stateRoot := privateTestDir(t)
	cacheRoot := privateTestDir(t)
	t.Setenv("XDG_STATE_HOME", stateRoot)
	t.Setenv("XDG_CACHE_HOME", cacheRoot)
	t.Setenv("XDG_CONFIG_HOME", privateTestDir(t))
	at := time.Date(2026, 7, 20, 16, 7, 48, 0, newYorkLocation())
	result := helperCombinedGammaResult(at)
	gammaDir, err := gammaZeroStoreDefaultDir()
	if err != nil {
		t.Fatal(err)
	}
	if err := newGammaZeroStore(gammaDir).Save(rpc.GammaZeroScopeCombined, nySessionKey(at), result); err != nil {
		t.Fatal(err)
	}
	want, err := os.ReadFile(filepath.Join(gammaDir, gammaZeroStoreFilename(rpc.GammaZeroScopeCombined)))
	if err != nil {
		t.Fatal(err)
	}

	server := newCutoverTestServer(t, "")
	if err := server.openCoreStore(t.Context()); err != nil {
		t.Fatal(err)
	}
	doc, ok, err := server.coreStore.GetStateDocument(t.Context(), gammaZeroAuthorityScope(rpc.GammaZeroScopeCombined), gammaZeroStateKind)
	if err != nil || !ok {
		t.Fatalf("cutover gamma state ok=%v err=%v", ok, err)
	}
	var promoted gammaZeroPersistEnvelope
	if err := json.Unmarshal(doc.JSON, &promoted); err != nil || promoted.Result == nil ||
		promoted.Result.AuthorityProvenance != gammaAuthorityProvenanceRecoveredObservation {
		t.Fatalf("cutover gamma provenance=%+v err=%v", promoted.Result, err)
	}
	observation, ok, err := server.coreStore.LatestObservation(t.Context(), gammaZeroAuthorityScope(rpc.GammaZeroScopeCombined), gammaZeroSource, gammaZeroObservationKind)
	if err != nil || !ok || !bytes.Equal(observation.Payload, want) || observation.DecisionEligible {
		t.Fatalf("cutover observation ok=%v exact=%v eligible=%v err=%v", ok, bytes.Equal(observation.Payload, want), observation.DecisionEligible, err)
	}
	if err := server.closeCoreStore(); err != nil {
		t.Fatal(err)
	}

	restarted := newCutoverTestServer(t, "")
	if err := restarted.openCoreStore(t.Context()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = restarted.closeCoreStore() })
	loaded, err := restarted.zeroGamma.store.LoadStale(rpc.GammaZeroScopeCombined)
	if err != nil || loaded == nil || !loaded.AsOf.Equal(at) {
		t.Fatalf("restart last-good=%+v err=%v", loaded, err)
	}
	annotateGammaQuality(loaded, at.Add(10*time.Minute))
	if loaded.Quality == nil || loaded.Quality.Rankability != rpc.GammaRankabilityContextOnly {
		t.Fatalf("restart recovered gamma quality=%+v", loaded.Quality)
	}
}

func gammaPersistPayload(t *testing.T, scope string, at time.Time, result *rpc.GammaZeroComputed) []byte {
	t.Helper()
	payload, err := json.Marshal(gammaZeroPersistEnvelope{
		Version: currentGammaPersistVersion, SessionKey: nySessionKey(at),
		Scope: scope, Method: result.Method, Result: result,
	})
	if err != nil {
		t.Fatal(err)
	}
	return payload
}

func helperCombinedGammaResult(asOf time.Time) *rpc.GammaZeroComputed {
	spy := helperGammaResult(asOf)
	spy.Scope = rpc.GammaZeroScopeSPY
	spx := helperGammaResult(asOf)
	spx.Scope = rpc.GammaZeroScopeSPX
	return combineGammaResults(spy, spx)
}
