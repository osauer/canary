package cli

import (
	"context"
	"errors"
	"io"
	"path/filepath"
	"testing"

	"github.com/osauer/canary/v2/internal/dial"
	"github.com/osauer/canary/v2/internal/rpc"
	"github.com/osauer/canary/v2/internal/update"
)

func TestStopRecordsIntentEvenWhenDaemonAlreadyExited(t *testing.T) {
	socket := filepath.Join(t.TempDir(), "daemon.sock")
	t.Setenv("CANARY_SOCKET", socket)
	opts := stopOptions{daemon: true, yes: true, out: io.Discard, err: io.Discard}
	deps := stopDeps{
		health:  func(context.Context, string) (rpc.HealthResult, bool, error) { return rpc.HealthResult{}, false, nil },
		inhibit: dial.InhibitAutostart,
		daemon: restartDeps{find: func(context.Context, string) (update.DaemonProcess, error) {
			if !errors.Is(dial.CheckAutostart(socket), dial.ErrStopped) {
				t.Error("stop intent must precede process lookup")
			}
			return update.DaemonProcess{}, update.ErrDaemonNotRunning
		}},
	}
	if code := runStopCore(t.Context(), &opts, deps); code != 0 {
		t.Fatalf("exit=%d", code)
	}
	if !errors.Is(dial.CheckAutostart(socket), dial.ErrStopped) {
		t.Fatal("background reads can restart an already stopped daemon")
	}
}
