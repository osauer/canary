package state

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"time"
)

const (
	// AlertDeliveryHealthClassInvalidPersistedState is the public, redacted
	// fail-closed posture for an isolated alert_delivery decode or semantic
	// validation failure. The raw state and its artifact identity stay private.
	AlertDeliveryHealthClassInvalidPersistedState = "invalid_persisted_state"

	// JavaScript's largest exactly representable integer is a stable sentinel
	// above every practical persisted generation. A reconnecting SPA must not
	// retain an older healthy generation after the ledger becomes unreadable.
	alertDeliveryQuarantineGeneration = uint64(1<<53 - 1)
	alertDeliveryQuarantinePrefix     = "alert-delivery-quarantine-sha256-"
)

type alertDeliveryQuarantine struct {
	raw json.RawMessage
}

// alertDeliveryQuarantinedLocked and alertDeliveryQuarantineGuardLocked are
// the shared integration seam for alert_delivery.go. Callers must already hold
// s.mu. Read projections remain available; every mutation/send path must use
// the guard and preserve the quarantined raw value unchanged.
func (s *Store) alertDeliveryQuarantinedLocked() bool {
	return s.alertDeliveryQuarantine != nil
}

func (s *Store) alertDeliveryQuarantineGuardLocked() error {
	if s.alertDeliveryQuarantinedLocked() {
		return ErrAlertDeliveryUnavailable
	}
	return nil
}

func (s *Store) alertDeliveryStateWriteFailure() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.alertDeliveryVolatile != nil &&
		s.alertDeliveryVolatile.State == AlertDeliveryHealthUnavailable &&
		s.alertDeliveryVolatile.Class == AlertDeliveryHealthClassStateWrite
}

func (s *Store) quarantineLoadedAlertDelivery(cause error) error {
	if cause == nil {
		return nil
	}
	raw := append(json.RawMessage(nil), s.loadedAlertDeliveryRaw...)
	if len(raw) == 0 || !json.Valid(raw) {
		return fmt.Errorf("%w: preserve invalid alert delivery state: raw value unavailable", ErrInvalidPersistedState)
	}
	updatedAt, err := preserveAlertDeliveryQuarantine(filepath.Dir(s.path), raw)
	if err != nil {
		return fmt.Errorf("%w: preserve invalid alert delivery state: %v", ErrInvalidPersistedState, err)
	}

	// The typed value is deliberately discarded only from memory. The exact raw
	// value remains authoritative in both the immutable artifact and every later
	// rewrite of state.json. There is intentionally no clear or repair method.
	s.data.AlertDelivery = nil
	s.alertDeliveryQuarantine = &alertDeliveryQuarantine{raw: raw}
	s.alertDeliveryVolatile = &AlertDeliveryHealth{
		State: AlertDeliveryHealthUnavailable, Class: AlertDeliveryHealthClassInvalidPersistedState,
		UpdatedAt: updatedAt,
	}
	s.alertDeliveryVolatileGeneration = alertDeliveryQuarantineGeneration
	s.loadedAlertDeliveryRaw = nil
	s.loadedAlertDeliveryDecodeErr = nil
	return nil
}

func preserveAlertDeliveryQuarantine(dir string, raw json.RawMessage) (time.Time, error) {
	if !json.Valid(raw) {
		return time.Time{}, errors.New("quarantine value is not valid JSON")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return time.Time{}, fmt.Errorf("create state directory: %w", err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return time.Time{}, fmt.Errorf("make state directory private: %w", err)
	}

	artifactPath := filepath.Join(dir, alertDeliveryQuarantineArtifactName(raw))
	if updatedAt, found, err := verifyAlertDeliveryQuarantine(artifactPath, raw); err != nil {
		return time.Time{}, err
	} else if found {
		if err := syncAlertDeliveryQuarantineDirectory(dir); err != nil {
			return time.Time{}, err
		}
		return updatedAt, nil
	}

	tmp, err := os.CreateTemp(dir, ".alert-delivery-quarantine-*.tmp")
	if err != nil {
		return time.Time{}, fmt.Errorf("create quarantine temporary file: %w", err)
	}
	tmpPath := tmp.Name()
	closed := false
	defer func() {
		if !closed {
			_ = tmp.Close()
		}
		_ = os.Remove(tmpPath)
	}()
	if err := tmp.Chmod(0o600); err != nil {
		return time.Time{}, fmt.Errorf("make quarantine artifact private: %w", err)
	}
	if _, err := tmp.Write(raw); err != nil {
		return time.Time{}, fmt.Errorf("write quarantine artifact: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		return time.Time{}, fmt.Errorf("sync quarantine artifact: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return time.Time{}, fmt.Errorf("close quarantine artifact: %w", err)
	}
	closed = true

	// Link publishes the fully-written inode without ever replacing a prior
	// artifact with the same evidence hash. A racing/existing artifact is
	// accepted only after exact-content and permission verification.
	if err := os.Link(tmpPath, artifactPath); err != nil {
		if !errors.Is(err, os.ErrExist) {
			return time.Time{}, fmt.Errorf("publish quarantine artifact: %w", err)
		}
		updatedAt, found, verifyErr := verifyAlertDeliveryQuarantine(artifactPath, raw)
		if verifyErr != nil {
			return time.Time{}, verifyErr
		}
		if !found {
			return time.Time{}, errors.New("quarantine artifact disappeared during publication")
		}
		if err := syncAlertDeliveryQuarantineDirectory(dir); err != nil {
			return time.Time{}, err
		}
		return updatedAt, nil
	}
	if err := syncAlertDeliveryQuarantineDirectory(dir); err != nil {
		return time.Time{}, err
	}

	updatedAt, found, err := verifyAlertDeliveryQuarantine(artifactPath, raw)
	if err != nil {
		return time.Time{}, err
	}
	if !found {
		return time.Time{}, errors.New("published quarantine artifact is missing")
	}
	return updatedAt, nil
}

func syncAlertDeliveryQuarantineDirectory(dir string) error {
	directory, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("open quarantine directory for sync: %w", err)
	}
	if err := directory.Sync(); err != nil {
		_ = directory.Close()
		return fmt.Errorf("sync quarantine directory: %w", err)
	}
	if err := directory.Close(); err != nil {
		return fmt.Errorf("close quarantine directory: %w", err)
	}
	return nil
}

func verifyAlertDeliveryQuarantine(path string, raw json.RawMessage) (time.Time, bool, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return time.Time{}, false, nil
	}
	if err != nil {
		return time.Time{}, false, fmt.Errorf("inspect quarantine artifact: %w", err)
	}
	if !info.Mode().IsRegular() {
		return time.Time{}, false, errors.New("quarantine artifact is not a regular file")
	}
	if info.Mode().Perm() != 0o600 {
		return time.Time{}, false, fmt.Errorf("quarantine artifact permissions are %04o, want 0600", info.Mode().Perm())
	}
	persisted, err := os.ReadFile(path)
	if err != nil {
		return time.Time{}, false, fmt.Errorf("read quarantine artifact: %w", err)
	}
	if !bytes.Equal(persisted, raw) {
		return time.Time{}, false, errors.New("quarantine artifact content does not match its evidence hash")
	}
	return info.ModTime().UTC(), true, nil
}

func alertDeliveryQuarantineArtifactName(raw json.RawMessage) string {
	sum := sha256.Sum256(raw)
	return alertDeliveryQuarantinePrefix + hex.EncodeToString(sum[:]) + ".json"
}

func (s *Store) marshalStateForSave() ([]byte, error) {
	if s.alertDeliveryQuarantine == nil {
		return json.MarshalIndent(s.data, "", "  ")
	}
	if s.data.AlertDelivery != nil {
		return nil, s.alertDeliveryQuarantineGuardLocked()
	}
	b, err := json.MarshalIndent(s.data, "", "  ")
	if err != nil {
		return nil, err
	}
	if len(b) == 0 || b[len(b)-1] != '}' {
		return nil, errors.New("encode app state: top-level JSON object required")
	}

	raw := s.alertDeliveryQuarantine.raw
	if len(raw) == 0 || !json.Valid(raw) {
		return nil, errors.New("encode app state: quarantined alert delivery value unavailable")
	}
	sep := []byte("\n")
	if len(b) > 2 {
		sep = []byte(",\n")
	}
	out := slices.Concat(b[:len(b)-1], sep, []byte("  \"alert_delivery\": "), raw, []byte("\n}"))
	if !json.Valid(out) {
		return nil, errors.New("encode app state: quarantined alert delivery reinsertion produced invalid JSON")
	}
	return out, nil
}
