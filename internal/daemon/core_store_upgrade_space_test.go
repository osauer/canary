package daemon

import (
	"bytes"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestCoreSchemaUpgradeSpaceRequirementIncludesSidecarsAndMargin(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "daemon.db")
	files := map[string]int{
		path:          16,
		path + "-wal": 32,
		path + "-shm": 8,
	}
	for candidate, size := range files {
		if err := os.WriteFile(candidate, bytes.Repeat([]byte{1}, size), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	footprint, err := coreSchemaUpgradeSourceFootprint(path)
	if err != nil {
		t.Fatal(err)
	}
	if footprint != 56 {
		t.Fatalf("source footprint = %d, want 56", footprint)
	}
	required, err := coreSchemaUpgradeRequiredFreeBytes(footprint)
	if err != nil {
		t.Fatal(err)
	}
	if required != footprint*2+coreSchemaUpgradeMinimumMarginBytes {
		t.Fatalf("required bytes = %d", required)
	}
}

func TestCoreSchemaUpgradeStatfsValueSupportsSignedAndUnsignedPlatforms(t *testing.T) {
	t.Parallel()

	if _, ok := coreSchemaUpgradeNonNegativeStatfsValue(int64(-1)); ok {
		t.Fatal("negative signed filesystem value was accepted")
	}
	if got, ok := coreSchemaUpgradeNonNegativeStatfsValue(uint64(42)); !ok || got != 42 {
		t.Fatalf("unsigned filesystem value = %d, ok=%v", got, ok)
	}
}

func TestCoreSchemaUpgradeInsufficientSpaceRefusesBeforeIntent(t *testing.T) {
	t.Parallel()

	databasePath, source := newFakeSchemaAuthority(t)
	before, err := os.ReadFile(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	ops := fakeCoreSchemaUpgradeOps()
	ops.availableBytes = func(string) (uint64, error) { return 0, nil }
	minimum := source.Head
	_, err = ensureCoreStoreSchemaCurrentWithOps(t.Context(), databasePath, &minimum, time.Now(), ops)
	var spaceErr *coreSchemaUpgradeSpaceError
	if !errors.As(err, &spaceErr) {
		t.Fatalf("upgrade error = %v, want typed space error", err)
	}
	if spaceErr.AvailableBytes != 0 || spaceErr.RequiredBytes == 0 || spaceErr.SourceBytes == 0 {
		t.Fatalf("space error = %+v", spaceErr)
	}
	if pending, pendingErr := coreSchemaUpgradePending(databasePath); pendingErr != nil || pending {
		t.Fatalf("upgrade intent pending=%v err=%v", pending, pendingErr)
	}
	if _, statErr := os.Lstat(coreSchemaUpgradeManifestPath(databasePath)); !errors.Is(statErr, fs.ErrNotExist) {
		t.Fatalf("upgrade manifest exists after refusal: %v", statErr)
	}
	after, err := os.ReadFile(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("space refusal changed source bytes")
	}
	watermark, err := loadAuthorityWatermark(databasePath + ".head")
	if err != nil || watermark == nil || *watermark != source.Head {
		t.Fatalf("space refusal watermark=%+v err=%v", watermark, err)
	}
}
