package daemon

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"
)

// Owner decision 2026-10-29 10:59 CET: beside each policy file Canary keeps
// the newest five backups it wrote plus every backup younger than thirty
// days, and removes the rest once a daemon start has succeeded. A backup is
// recognised by the UTC stamp Canary puts at the end of its name; copies the
// owner made by hand carry no such stamp and are never touched.
const (
	policyBackupKeep   = 5
	policyBackupMaxAge = 30 * 24 * time.Hour
)

// policyBackupStamp matches the tail every Canary backup writer appends:
// `<file>.bak-<label>-<YYYYMMDDTHHMMSSZ>`, with `-<n>` after the stamp when a
// second save landed in the same second (writeCashPolicyBackup).
var policyBackupStamp = regexp.MustCompile(`-(\d{8}T\d{6}Z)(?:-\d+)?$`)

type policyBackup struct {
	path  string
	stamp time.Time
	size  int64
}

// listPolicyBackups returns the Canary-stamped backups beside the policy file
// at path, newest first. A file whose name does not end in a Canary stamp is
// not a backup Canary owns, whatever else it is called.
func listPolicyBackups(path string) ([]policyBackup, error) {
	dir, prefix := filepath.Dir(path), filepath.Base(path)+".bak-"
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var backups []policyBackup
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasPrefix(name, prefix) || !entry.Type().IsRegular() {
			continue
		}
		m := policyBackupStamp.FindStringSubmatch(name)
		if m == nil {
			continue
		}
		stamp, err := time.Parse("20060102T150405Z", m[1])
		if err != nil {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		backups = append(backups, policyBackup{path: filepath.Join(dir, name), stamp: stamp, size: info.Size()})
	}
	slices.SortFunc(backups, func(a, b policyBackup) int { return b.stamp.Compare(a.stamp) })
	return backups, nil
}

// prunePolicyBackups removes the backups beside path that fall outside the
// retention rule and reports how many went and their size. The newest
// policyBackupKeep always stay, as does anything younger than
// policyBackupMaxAge, so a backup written at this start is never a candidate.
func prunePolicyBackups(path string, now time.Time) (removed int, bytes int64, err error) {
	backups, err := listPolicyBackups(path)
	if err != nil {
		return 0, 0, err
	}
	var errs []error
	for i, b := range backups {
		if i < policyBackupKeep || now.Sub(b.stamp) <= policyBackupMaxAge {
			continue
		}
		if rmErr := os.Remove(b.path); rmErr != nil && !errors.Is(rmErr, os.ErrNotExist) {
			errs = append(errs, rmErr)
			continue
		}
		removed++
		bytes += b.size
	}
	return removed, bytes, errors.Join(errs...)
}

// prunePolicyBackupsAfterStart runs the retention rule for every policy file
// once the daemon is serving. It runs after the serving marker on purpose: a
// start that fails keeps every backup it might need. Only the daemon that
// materialises the policy files (ensurePolicyFiles) prunes them, so there is
// exactly one writer, the same one that made the backups.
func (s *Server) prunePolicyBackupsAfterStart() {
	if s == nil || !s.ensurePolicyFiles {
		return
	}
	set := PolicyFileSetFor(s.cfg)
	now := time.Now()
	var removed int
	var bytes int64
	for _, path := range []string{set.Rulebook, set.Protection, set.Opportunity, set.Constitution} {
		n, b, err := prunePolicyBackups(path, now)
		removed += n
		bytes += b
		if err != nil {
			s.warnf("policy backups: %s: %v", filepath.Base(path), err)
		}
	}
	if removed == 0 {
		return
	}
	s.logger.Infof("policy backups: removed %d older than %d days (%s); the newest %d of each file and every younger backup stay",
		removed, int(policyBackupMaxAge.Hours()/24), formatBackupBytes(bytes), policyBackupKeep)
}

func formatBackupBytes(n int64) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/float64(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%d KB", n>>10)
	}
	return fmt.Sprintf("%d B", n)
}
