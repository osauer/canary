package daemon

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/daemon/corestore"
)

func openMarketTestCoreStore(t *testing.T) *corestore.Store {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatalf("secure test corestore dir: %v", err)
	}
	store, err := corestore.Open(context.Background(), corestore.Options{
		Path: filepath.Join(dir, "daemon.db"),
	})
	if err != nil {
		t.Fatalf("open test corestore: %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("close test corestore: %v", err)
		}
	})
	return store
}

func TestSaveMarketStatePublishesCurrentWithoutObservation(t *testing.T) {
	store := openMarketTestCoreStore(t)
	at := time.Date(2026, 7, 20, 10, 0, 0, 0, time.UTC)
	if err := saveMarketState(store, "market/test", "current.v1", corestore.ObservationInput{
		ScopeKey: "market/test", Source: "current-code", Kind: "measurement.v1",
		ObservedAt: at, ContentType: "application/json", Payload: []byte(`{"v":1}`),
	}); err != nil {
		t.Fatal(err)
	}
	observation, ok, err := store.LatestDecisionEligibleObservation(t.Context(), "market/test", "current-code", "measurement.v1")
	if err != nil || ok {
		t.Fatalf("unexpected decision observation = %+v ok=%v err=%v", observation, ok, err)
	}
	doc, ok, err := store.GetStateDocument(t.Context(), "market/test", "current.v1")
	if err != nil || !ok || string(doc.JSON) != `{"v":1}` {
		t.Fatalf("current state = %+v ok=%v err=%v", doc, ok, err)
	}
}

func TestLiveCodeCannotUseGenericObservationReaders(t *testing.T) {
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	repoRoot := filepath.Clean(filepath.Join(wd, "..", ".."))
	for _, relativeRoot := range []string{"internal", "pkg"} {
		root := filepath.Join(repoRoot, relativeRoot)
		err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if entry.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			if path == filepath.Join(repoRoot, "internal", "daemon", "corestore", "observations.go") {
				return nil
			}
			raw, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			text := string(raw)
			if strings.Contains(text, ".LatestObservation(") || strings.Contains(text, ".ListObservations(") {
				t.Errorf("generic observation history reader is forbidden in live code: %s", path)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
}
