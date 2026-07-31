package daemon

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/osauer/canary/v2/internal/dial"
)

func defaultTradingStatePath(filename string) (string, error) {
	if v := os.Getenv("XDG_STATE_HOME"); v != "" {
		return filepath.Join(v, "ibkr", filename), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve user home: %w", err)
	}
	return filepath.Join(home, ".local", "state", "ibkr", filename), nil
}

// defaultDaemonDatabasePath delegates to dial so the CLI, which sizes this
// file to budget how long it waits for the daemon to publish its socket,
// cannot drift from where the daemon actually opens it.
func defaultDaemonDatabasePath() (string, error) {
	return dial.DefaultAuthorityPath()
}

func ensurePrivateStateDir(path string) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("mkdir %s: %w", dir, err)
	}
	return nil
}

func writePrivateStateAtomic(path string, data []byte) error {
	if err := ensurePrivateStateDir(path); err != nil {
		return err
	}
	base := filepath.Base(path)
	tmp, err := os.CreateTemp(filepath.Dir(path), base+".tmp.*")
	if err != nil {
		return fmt.Errorf("create temp: %w", err)
	}
	tmpPath := tmp.Name()
	defer func() {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
	}()
	if _, err := tmp.Write(data); err != nil {
		return fmt.Errorf("write temp: %w", err)
	}
	if err := tmp.Chmod(0o600); err != nil {
		return fmt.Errorf("chmod temp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("rename %s: %w", path, err)
	}
	return nil
}
