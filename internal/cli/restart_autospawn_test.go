package cli

import (
	"bytes"
	"context"
	"errors"
	"io"
	"path/filepath"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/dial"
	"github.com/osauer/canary/v2/internal/rpc"
	"github.com/osauer/canary/v2/internal/update"
)

// A background reader arriving after the old daemon exits must wait for
// restart's chosen replacement, rather than spawn a competing executable.
func TestRestartExcludesAutospawnDuringHandover(t *testing.T) {
	root := t.TempDir()
	socket := filepath.Join(root, "daemon.sock")
	t.Setenv("CANARY_SOCKET", socket)
	t.Setenv("CANARY_LOG", filepath.Join(root, "daemon.log"))
	t.Setenv("XDG_STATE_HOME", root)
	var out, stderr bytes.Buffer
	opts := &restartOptions{jsonOut: true, timeout: time.Second, out: &out, err: &stderr}
	attempted := false
	exit := runRestartAllCore(context.Background(), opts, restartDeps{
		find: func(context.Context, string) (update.DaemonProcess, error) {
			return update.DaemonProcess{PID: 42, SocketPath: socket, LockPath: dial.LockPath(socket)}, nil
		},
		stop: func(int, time.Duration) error {
			attempted = true
			ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
			defer cancel()
			conn, err := dial.AutospawnAndConnectContextFromExecutable(ctx, socket, filepath.Join(root, "must-not-execute"))
			if conn != nil {
				_ = conn.Close()
			}
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Errorf("background autospawn escaped restart reservation: %v", err)
			}
			return nil
		},
		startAndHealth: func(context.Context, string, io.Writer, bool) (int, rpc.HealthResult, error) {
			return 43, rpc.HealthResult{DaemonVersion: "test"}, nil
		},
	}, appRestartDeps{})
	if exit != 0 || !attempted {
		t.Fatalf("restart exit=%d, attempted=%v, stderr=%s", exit, attempted, stderr.String())
	}
}
