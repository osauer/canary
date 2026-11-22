package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The installed binary never runs a simulated daemon: no source file of
// cmd/canary names daemon.Options.Simulator or installs a simulated exchange
// session (those belong to cmd/canary-sim only).
func TestInstalledBinaryNeverSimulates(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil || len(files) == 0 {
		t.Fatalf("source files: %v %v", files, err)
	}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, banned := range []string{"Simulator:", "Simulator =", "SimulateSession", "twssim"} {
			if strings.Contains(string(raw), banned) {
				t.Errorf("%s contains %q", f, banned)
			}
		}
	}
}
