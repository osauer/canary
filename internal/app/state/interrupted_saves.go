package state

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// interruptedSaveAge is how old a `.state-*.tmp` file must be before it
// counts as the remains of an interrupted save rather than a save in flight.
// save() removes its own temp file on every error path, so one survives only
// a process killed between creating it and renaming it over state.json.
const interruptedSaveAge = time.Hour

// RemoveInterruptedSaves deletes the `.state-*.tmp` files under dir that are
// older than an hour and reports how many went. The caller holds the app lock
// for dir, so no other Canary app process is saving there; the age guard
// covers a save by this process that is still in flight.
func RemoveInterruptedSaves(dir string, now time.Time) (int, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return 0, nil
		}
		return 0, err
	}
	var removed int
	var errs []error
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasPrefix(name, ".state-") || !strings.HasSuffix(name, ".tmp") || !entry.Type().IsRegular() {
			continue
		}
		info, err := entry.Info()
		if err != nil || now.Sub(info.ModTime()) < interruptedSaveAge {
			continue
		}
		if err := os.Remove(filepath.Join(dir, name)); err != nil && !errors.Is(err, os.ErrNotExist) {
			errs = append(errs, err)
			continue
		}
		removed++
	}
	return removed, errors.Join(errs...)
}
