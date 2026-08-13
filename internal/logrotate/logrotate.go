// Package logrotate provides the size-capped log file writer shared by the
// daemon and the app host. Both logs are diagnostic streams (trade audit lives
// in the order journal), so the policy is deliberately simple: when the file
// passes the cap it rolls aside to <path>.1, keeping exactly one previous
// generation and bounding on-disk use to ~2x the cap. Rotation happens at open
// and again at runtime whenever a write crosses the cap — a long-lived process
// must not sail past the cap until its next restart.
package logrotate

import (
	"os"
	"path/filepath"
	"sync"
)

// DefaultMaxBytes caps a log file at rotation time.
const DefaultMaxBytes = 64 << 20 // 64 MiB

// Writer is an io.WriteCloser appending to a 0600 log file, rotating it to
// path+".1" when its size passes maxBytes. Safe for concurrent use.
type Writer struct {
	mu       sync.Mutex
	f        *os.File
	path     string
	maxBytes int64
	size     int64
}

// Open creates (0700 dir, 0600 file) and opens path for appending, rotating
// first when the existing file already exceeds maxBytes. maxBytes <= 0 uses
// DefaultMaxBytes.
func Open(path string, maxBytes int64) (*Writer, error) {
	if maxBytes <= 0 {
		maxBytes = DefaultMaxBytes
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return nil, err
	}
	if info, err := os.Stat(path); err == nil && info.Size() >= maxBytes {
		// Best-effort: a rename failure leaves the oversized file in place
		// for the append open below rather than blocking process start.
		_ = os.Rename(path, path+".1")
	}
	w := &Writer{path: path, maxBytes: maxBytes}
	if err := w.open(); err != nil {
		return nil, err
	}
	return w, nil
}

func (w *Writer) open() error {
	f, err := os.OpenFile(w.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if err := f.Chmod(0o600); err != nil {
		_ = f.Close()
		return err
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return err
	}
	w.f = f
	w.size = info.Size()
	return nil
}

// Write appends p, rotating first when the file has already reached the cap.
// A whole record therefore stays in one generation. Rotation failures degrade
// to appending past the cap instead of dropping log output.
func (w *Writer) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.f == nil {
		if err := w.open(); err != nil {
			return 0, err
		}
	}
	if w.size >= w.maxBytes {
		if err := os.Rename(w.path, w.path+".1"); err == nil {
			_ = w.f.Close()
			if err := w.open(); err != nil {
				w.f = nil
				return 0, err
			}
		}
	}
	n, err := w.f.Write(p)
	w.size += int64(n)
	return n, err
}

// Close closes the underlying file. Later writes reopen it.
func (w *Writer) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.f == nil {
		return nil
	}
	err := w.f.Close()
	w.f = nil
	return err
}
