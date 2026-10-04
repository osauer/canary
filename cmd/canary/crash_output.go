package main

import (
	"fmt"
	"os"

	"github.com/osauer/canary/v2/internal/logrotate"
)

// crashLogMaxBytes caps the crash log at open time. One goroutine dump is a
// few hundred kilobytes; a daemon that keeps dumping is itself the finding.
const crashLogMaxBytes = 8 << 20

// captureCrashOutput points the process's file descriptor 2 at path so the
// runtime's fatal output — a SIGQUIT goroutine dump, an unrecovered panic,
// a fatal runtime error — lands in a file of its own instead of the slog
// stream the supervisor redirected stderr into. The slog writer keeps its own
// descriptor and is unaffected. The returned file stays open for the life of
// the process; the caller never closes it.
func captureCrashOutput(path string) (*os.File, error) {
	f, err := logrotate.OpenFile(path, crashLogMaxBytes)
	if err != nil {
		return nil, fmt.Errorf("open crash log: %w", err)
	}
	if err := redirectStderr(f); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("redirect stderr to crash log: %w", err)
	}
	os.Stderr = f
	return f, nil
}
