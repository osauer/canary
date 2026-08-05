package daemon

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/daemon/corestore"
)

func TestGammaOpenInterestStoreRoundTripAndMergeRules(t *testing.T) {
	t.Parallel()

	store := newGammaOpenInterestStore(t.TempDir())
	observedAt := time.Date(2026, 6, 2, 14, 35, 0, 0, time.UTC)
	key := gammaOIKey("SPX", "SPXW", "20260605", 7600, "P")
	rec := gammaOIRecordForLeg("SPX", "SPXW", "20260605", 7600, "P", 12_345, observedAt)
	if err := store.SaveMerged(map[string]gammaOIRecord{key: rec}); err != nil {
		t.Fatalf("SaveMerged: %v", err)
	}

	got, err := store.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	roundTrip := got[key]
	if roundTrip.OpenInterest != 12_345 || roundTrip.SessionKey != "2026-06-02" ||
		roundTrip.SourceStatus != gammaOISourceLiveObserved || roundTrip.Expiry != "2026-06-05" {
		t.Fatalf("round-trip record = %+v", roundTrip)
	}

	older := gammaOIRecordForLeg("SPX", "SPXW", "20260605", 7600, "P", 99, observedAt.Add(-time.Hour))
	if err := store.SaveMerged(map[string]gammaOIRecord{key: older}); err != nil {
		t.Fatalf("SaveMerged older: %v", err)
	}
	got, err = store.Load()
	if err != nil {
		t.Fatalf("Load after older: %v", err)
	}
	if got[key].OpenInterest != 12_345 {
		t.Fatalf("older update overwrote valid OI: %+v", got[key])
	}

	zero := gammaOIRecordForLeg("SPX", "SPXW", "20260605", 7600, "P", 0, observedAt.Add(time.Hour))
	if err := store.SaveMerged(map[string]gammaOIRecord{key: zero}); err != nil {
		t.Fatalf("SaveMerged zero: %v", err)
	}
	got, err = store.Load()
	if err != nil {
		t.Fatalf("Load after zero: %v", err)
	}
	if got[key].OpenInterest != 0 || !got[key].ObservedAt.Equal(zero.ObservedAt) {
		t.Fatalf("newer observed zero did not replace positive OI: %+v", got[key])
	}
}

func TestGammaOpenInterestStoreUsesSQLiteWithoutFileFallback(t *testing.T) {
	legacyDir := t.TempDir()
	authority := openMarketTestCoreStore(t)
	store := newGammaOpenInterestStore(legacyDir)
	if err := store.UseCoreStore(authority); err != nil {
		t.Fatalf("UseCoreStore: %v", err)
	}
	observedAt := time.Date(2026, 6, 2, 14, 35, 0, 0, time.UTC)
	key := gammaOIKey("SPX", "SPXW", "20260605", 7600, "P")
	rec := gammaOIRecordForLeg("SPX", "SPXW", "20260605", 7600, "P", 12_345, observedAt)
	if err := store.SaveMerged(map[string]gammaOIRecord{key: rec}); err != nil {
		t.Fatalf("SaveMerged: %v", err)
	}
	if _, err := os.Stat(filepath.Join(legacyDir, gammaOIStateFilename)); !os.IsNotExist(err) {
		t.Fatalf("runtime save touched legacy file: %v", err)
	}

	restarted := newGammaOpenInterestStore(legacyDir)
	if err := restarted.UseCoreStore(authority); err != nil {
		t.Fatalf("restart UseCoreStore: %v", err)
	}
	got, err := restarted.Load()
	if err != nil || got[key].OpenInterest != rec.OpenInterest {
		t.Fatalf("SQLite round trip: got=%+v err=%v", got[key], err)
	}
	observations, err := authority.ListObservations(context.Background(), corestore.ObservationQuery{
		ScopeKey: gammaOIAuthorityScope, Source: gammaOISource, Kind: gammaOIObservationKind,
	})
	if err != nil || len(observations) != 0 {
		t.Fatalf("observations=%d err=%v", len(observations), err)
	}
}

func TestGammaOpenInterestKeySeparatesSPXAndSPXW(t *testing.T) {
	t.Parallel()

	spx := gammaOIKey("SPX", "SPX", "20260917", 7600, "C")
	spxw := gammaOIKey("SPX", "SPXW", "20260917", 7600, "C")
	if spx == spxw {
		t.Fatalf("SPX and SPXW OI keys collided: %s", spx)
	}
	if want := "SPX|SPX|20260917|7600.000000|C"; spx != want {
		t.Fatalf("SPX key = %q, want %q", spx, want)
	}
}

func TestValidCarriedGammaOIChecksAgeAndSettlement(t *testing.T) {
	t.Parallel()
	loc := newYorkLocation()
	rec := gammaOIRecordForLeg(
		"SPX", "SPX", "20260917", 7600, "C", 100,
		time.Date(2026, 9, 16, 10, 0, 0, 0, loc),
	)

	if !validCarriedGammaOI(rec, time.Date(2026, 9, 18, 9, 29, 0, 0, loc)) {
		t.Fatal("SPX AM Thursday-key carried OI should remain valid before Friday 09:30 settlement")
	}
	if validCarriedGammaOI(rec, time.Date(2026, 9, 18, 9, 46, 0, 0, loc)) {
		t.Fatal("SPX AM Thursday-key carried OI should expire after Friday 09:45 settlement buffer")
	}
	if validCarriedGammaOI(rec, rec.ObservedAt.Add(gammaOICarryMaxAge+time.Second)) {
		t.Fatal("too-old carried OI should be invalid")
	}
}

func TestGammaOpenInterestStoreCorruptJSONErrors(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	if err := os.WriteFile(dir+"/"+gammaOIStateFilename, []byte("{"), 0o644); err != nil {
		t.Fatalf("write corrupt fixture: %v", err)
	}
	if _, err := newGammaOpenInterestStore(dir).Load(); err == nil {
		t.Fatal("Load corrupt JSON returned nil error")
	}
}
