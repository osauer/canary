package dial

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestExplicitStopBlocksAutospawnUntilRestart(t *testing.T) {
	socket := filepath.Join(t.TempDir(), "daemon.sock")
	if err := InhibitAutostart(socket); err != nil {
		t.Fatal(err)
	}
	if err := InhibitAutostart(socket); err != nil {
		t.Fatal(err)
	}
	if _, err := AutospawnAndConnectContextFromExecutable(t.Context(), socket, "/must-not-execute"); !errors.Is(err, ErrStopped) {
		t.Fatalf("stop not respected: %v", err)
	}
	if err := CheckAutostart(socket + "-other"); err != nil {
		t.Fatalf("stop crossed socket scope: %v", err)
	}
	info, err := os.Stat(stopIntentPath(socket))
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("private marker: %v %v", info, err)
	}
	if err := AllowAutostart(socket); err != nil {
		t.Fatal(err)
	}
	if err := CheckAutostart(socket); err != nil {
		t.Fatal(err)
	}
}
