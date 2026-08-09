package daemon

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/daemon/corestore"
)

func TestInitializeFreshDaemonStateDoesNotReadLegacyDefaults(t *testing.T) {
	stateHome := t.TempDir()
	if err := os.Chmod(stateHome, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_STATE_HOME", stateHome)
	legacyFreeze := true
	settingsPath, _ := defaultPlatformSettingsPath()
	writeJSONFixture(t, settingsPath, platformSettingsData{Version: 1, Trading: platformTradingSettingsData{Freeze: &legacyFreeze}})
	core, err := corestore.Open(t.Context(), corestore.Options{Path: filepath.Join(stateHome, "offline.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer core.Close()
	if err := initializeFreshDaemonState(t.Context(), core); err != nil {
		t.Fatal(err)
	}
	before, ok, err := core.GetStateDocument(t.Context(), daemonStateScope, stateKindPlatformSettings)
	if err != nil || !ok {
		t.Fatal(err)
	}
	if err := initializeFreshDaemonState(t.Context(), core); err != nil {
		t.Fatal(err)
	}
	after, ok, err := core.GetStateDocument(t.Context(), daemonStateScope, stateKindPlatformSettings)
	if err != nil || !ok || before.Revision != after.Revision {
		t.Fatalf("fresh-state replay changed revision: before=%d after=%d err=%v", before.Revision, after.Revision, err)
	}
	s := &Server{now: time.Now}
	if err := s.bindAuthoritativeDaemonState(t.Context(), core); err != nil {
		t.Fatal(err)
	}
	if got := s.platformSettings.snapshot().Trading.Freeze; got != nil {
		t.Fatalf("fresh authority imported legacy freeze: %v", *got)
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
