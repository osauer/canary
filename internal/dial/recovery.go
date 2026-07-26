package dial

import (
	"bytes"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/osauer/canary/v2/internal/productidentity"
)

// LockPath returns the canonical instance-lock path co-located with the
// socket. The daemon writes its PID here under flock; the CLI reads it
// during recovery to detect a stuck daemon.
func LockPath(socketPath string) string {
	return filepath.Join(filepath.Dir(socketPath), productidentity.DaemonLockName)
}

// LockHolderPID returns the PID written to the lock file, or 0 if the
// file is missing, unreadable, or malformed. Best-effort: a 0 result is
// indistinguishable from "no daemon" and callers should treat it that way.
func LockHolderPID(lockPath string) int {
	data, err := os.ReadFile(lockPath)
	if err != nil {
		return 0
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || pid <= 0 {
		return 0
	}
	return pid
}

// IsProcessAlive reports whether a PID is currently a live, non-zombie
// process. Uses signal 0 (kill -0) as the ownership/existence probe, then
// filters out defunct processes because zombies still satisfy kill -0 on Unix
// even though SIGTERM/SIGKILL cannot make them "more exited".
//
// EPERM (we know the process exists but cannot signal it — typically
// owned by another user) reports false here on purpose: the caller would
// otherwise try to SIGTERM a process it can't actually signal. For our
// recovery use case "not our daemon, leave it alone" is conservative
// and safe.
func IsProcessAlive(pid int) bool {
	if pid <= 0 || pid == os.Getpid() {
		return false
	}
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	if err := p.Signal(syscall.Signal(0)); err == nil {
		return !isProcessZombie(pid)
	} else if errors.Is(err, syscall.ESRCH) {
		return false
	}
	return false
}

var lookupProcessStatus = processStatus

func isProcessZombie(pid int) bool {
	status, err := lookupProcessStatus(pid)
	if err != nil {
		return false
	}
	status = strings.TrimSpace(status)
	return strings.HasPrefix(status, "Z")
}

func processStatus(pid int) (string, error) {
	if pid <= 0 {
		return "", errors.New("invalid PID")
	}
	cmd := exec.Command("ps", "-p", strconv.Itoa(pid), "-o", "stat=")
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// TailLastLine returns the last non-empty line of the file at path, capped
// at maxBytes from the tail. Returns "" if the file is missing, empty, or
// unreadable. Used by the CLI to surface the latest daemon log line in
// error messages when autospawn fails.
func TailLastLine(path string, maxBytes int) string {
	if maxBytes <= 0 {
		maxBytes = 64 << 10
	}
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()

	fi, err := f.Stat()
	if err != nil {
		return ""
	}
	size := fi.Size()
	off := int64(0)
	if size > int64(maxBytes) {
		off = size - int64(maxBytes)
	}
	if _, err := f.Seek(off, io.SeekStart); err != nil {
		return ""
	}
	buf, err := io.ReadAll(f)
	if err != nil {
		return ""
	}
	// Strip trailing newlines/whitespace, then take everything after the
	// last newline. Empty trailing lines are skipped.
	buf = bytes.TrimRight(buf, "\r\n\t ")
	if len(buf) == 0 {
		return ""
	}
	if i := bytes.LastIndexByte(buf, '\n'); i >= 0 {
		buf = buf[i+1:]
	}
	return string(buf)
}
