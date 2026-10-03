package daemon

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/config"
	"github.com/osauer/canary/v2/internal/dial"
)

func TestDaemonStartHonorsStopBeforeOpeningAuthority(t *testing.T) {
	dir := t.TempDir()
	socket := filepath.Join(dir, "daemon.sock")
	database := filepath.Join(dir, "must-not-create.db")
	if err := dial.InhibitAutostart(socket); err != nil {
		t.Fatal(err)
	}
	s := &Server{now: time.Now, cfg: &config.Resolved{}, logger: NewLogger(io.Discard, "error"), socketPath: socket, coreStorePath: database}
	if err := s.Start(t.Context()); !errors.Is(err, dial.ErrStopped) {
		t.Fatalf("start: %v", err)
	}
	if _, err := os.Stat(database); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("authority touched: %v", err)
	}
	lock, err := acquireInstanceLock(socket)
	if err != nil {
		t.Fatalf("stopped start leaked lock: %v", err)
	}
	lock.Release()
}
