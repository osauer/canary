package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime/pprof"
	"sync"
	"time"

	"github.com/osauer/canary/v2/internal/rpc"
)

// Go's CPU profiler is process-wide. Allocation captures share this guard so
// diagnostics cannot overlap or distort each other's measurement window.
var processProfileGate sync.Mutex

// Bound diagnostic output as well as duration; no automatic periodic capture.
const profileFileMaxBytes = 32 << 20

type profileWriter struct {
	w         io.Writer
	remaining int
	err       error
}

func (w *profileWriter) Write(p []byte) (int, error) {
	if w.err != nil {
		return 0, w.err
	}
	if len(p) > w.remaining {
		w.err = fmt.Errorf("profile exceeded its file size budget")
		return 0, w.err
	}
	n, err := w.w.Write(p)
	w.remaining -= n
	if err == nil && n != len(p) {
		err = io.ErrShortWrite
	}
	w.err = err
	return n, err
}

func (s *Server) handleProfile(ctx context.Context, req *rpc.Request) (result rpc.ProfileResult, err error) {
	var p rpc.ProfileParams
	if err := json.Unmarshal(req.Params, &p); err != nil {
		return result, errBadRequest("invalid profile parameters")
	}
	if (p.Kind != "cpu" && p.Kind != "allocs") || p.Seconds < 1 || p.Seconds > int(rpc.ProfileMaxDuration/time.Second) {
		return result, errBadRequest("profile requires kind cpu or allocs and seconds between 1 and 120")
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	if !processProfileGate.TryLock() {
		return result, errBadRequest("a process profile is already running")
	}
	defer processProfileGate.Unlock()
	dir, err := os.MkdirTemp("", "canary-profile-")
	if err != nil {
		return result, err
	}
	defer func() {
		if err != nil {
			_ = os.RemoveAll(dir)
		}
	}()
	result = rpc.ProfileResult{Kind: p.Kind, PID: os.Getpid(), Directory: dir, StartedAt: time.Now().UTC()}
	writeAllocs := func(name string) error {
		f, err := os.OpenFile(filepath.Join(dir, name), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			return err
		}
		w := &profileWriter{w: f, remaining: profileFileMaxBytes}
		err = pprof.Lookup("allocs").WriteTo(w, 0)
		err = errors.Join(err, w.err, f.Close())
		if err == nil {
			result.Files = append(result.Files, name)
		}
		return err
	}
	var stopCPU func() error
	if p.Kind == "cpu" {
		f, openErr := os.OpenFile(filepath.Join(dir, "cpu.pprof"), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if openErr != nil {
			return result, openErr
		}
		w := &profileWriter{w: f, remaining: profileFileMaxBytes}
		if startErr := pprof.StartCPUProfile(w); startErr != nil {
			return result, errors.Join(startErr, f.Close())
		}
		stopCPU = func() error { pprof.StopCPUProfile(); return errors.Join(w.err, f.Close()) }
		defer func() {
			if stopCPU != nil {
				err = errors.Join(err, stopCPU())
			}
		}()
		result.Files = []string{"cpu.pprof"}
	} else if err = writeAllocs("allocs-start.pprof"); err != nil {
		return result, err
	}
	timer := time.NewTimer(time.Duration(p.Seconds) * time.Second)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return result, ctx.Err()
	case <-timer.C:
	}
	if stopCPU != nil {
		err = stopCPU()
		stopCPU = nil
	} else {
		err = writeAllocs("allocs-end.pprof")
	}
	result.FinishedAt = time.Now().UTC()
	return result, err
}
