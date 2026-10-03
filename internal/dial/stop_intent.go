package dial

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// ErrStopped means only an explicit restart may resume this daemon scope.
var ErrStopped = errors.New("daemon was explicitly stopped; run canary restart to resume")

func stopIntentPath(socket string) string { return socket + ".stopped" }

// InhibitAutostart records operator stop intent before signalling any process.
func InhibitAutostart(socket string) error {
	if err := os.MkdirAll(filepath.Dir(socket), 0700); err != nil {
		return err
	}
	f, err := os.OpenFile(stopIntentPath(socket), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if errors.Is(err, os.ErrExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("record daemon stop: %w", err)
	}
	return f.Close()
}

// AllowAutostart is called only by an explicit restart, never ordinary reads.
func AllowAutostart(socket string) error {
	err := os.Remove(stopIntentPath(socket))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

// CheckAutostart rejects starts while operator stop intent is present.
func CheckAutostart(socket string) error {
	_, err := os.Lstat(stopIntentPath(socket))
	if err == nil {
		return ErrStopped
	}
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return fmt.Errorf("inspect daemon stop intent: %w", err)
}
