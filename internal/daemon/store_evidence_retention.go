package daemon

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"time"
)

// Owner decision 2026-11-22: the daemon's own copies of daemon.db (about as
// large as the live file each) are kept in check after every successful
// start, the same way policy backups are (policy_backups.go):
//
//   - damaged/: the newest storeEvidenceKeepDamaged quarantines stay, and so
//     does any younger than storeEvidenceMinAge, so an incident still under
//     investigation keeps its evidence;
//   - backups/: the newest schema-upgrade set (<label>.db, .target.db,
//     .maintenance.json share one label) stays; older sets go. Nothing is
//     removed while a schema upgrade is pending.
//
// Only names Canary's own writers produce are candidates
// (preserveAuthorityEvidence, corestore.Salvage, coreSchemaUpgradeArtifactPaths);
// anything the owner put there by hand stays.
const (
	storeEvidenceKeepDamaged = 2
	storeEvidenceMinAge      = 7 * 24 * time.Hour
)

// damagedDirName matches `<code>-<stamp>` from preserveAuthorityEvidence and
// `damaged-<stamp>-<random>` from corestore.Salvage.
var damagedDirName = regexp.MustCompile(`^(?:[a-z]+(?:-[a-z]+)*-(\d{8}T\d{6}Z)|damaged-(\d{8}T\d{6}Z)-[0-9]+)$`)

// upgradeBackupName matches the three files of one schema-upgrade set.
var upgradeBackupName = regexp.MustCompile(`^(.+-schema-v(\d+)-to-v(\d+)-[0-9a-f]+)\.(?:db|target\.db|maintenance\.json)$`)

type storeEvidence struct {
	paths []string
	stamp time.Time
	rank  int // schema target version for backups; 0 for damaged copies
	size  int64
}

// listDamagedCopies returns Canary's quarantine folders under dir, newest first.
func listDamagedCopies(dir string) ([]storeEvidence, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var out []storeEvidence
	for _, entry := range entries {
		if !entry.IsDir() || entry.Type()&fs.ModeSymlink != 0 {
			continue
		}
		m := damagedDirName.FindStringSubmatch(entry.Name())
		if m == nil {
			continue
		}
		stamp, err := time.Parse("20060102T150405Z", m[1]+m[2])
		if err != nil {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		out = append(out, storeEvidence{paths: []string{path}, stamp: stamp, size: treeSize(path)})
	}
	slices.SortFunc(out, func(a, b storeEvidence) int { return b.stamp.Compare(a.stamp) })
	return out, nil
}

// listUpgradeBackups groups backups/ into upgrade sets, newest target schema first.
func listUpgradeBackups(dir string) ([]storeEvidence, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	sets := map[string]*storeEvidence{}
	for _, entry := range entries {
		if !entry.Type().IsRegular() {
			continue
		}
		m := upgradeBackupName.FindStringSubmatch(entry.Name())
		if m == nil {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		target, _ := strconv.Atoi(m[3])
		set := sets[m[1]]
		if set == nil {
			set = &storeEvidence{rank: target}
			sets[m[1]] = set
		}
		set.paths = append(set.paths, filepath.Join(dir, entry.Name()))
		set.size += info.Size()
		if info.ModTime().After(set.stamp) {
			set.stamp = info.ModTime()
		}
	}
	out := make([]storeEvidence, 0, len(sets))
	for _, set := range sets {
		out = append(out, *set)
	}
	slices.SortFunc(out, func(a, b storeEvidence) int {
		if a.rank != b.rank {
			return b.rank - a.rank
		}
		return b.stamp.Compare(a.stamp)
	})
	return out, nil
}

// pruneStoreEvidence applies the retention rule beside the store at
// databasePath and reports how many copies went and their size.
func pruneStoreEvidence(databasePath string, now time.Time) (removed int, bytes int64, err error) {
	parent := filepath.Dir(databasePath)
	var errs []error
	remove := func(e storeEvidence) {
		for _, p := range e.paths {
			if rmErr := os.RemoveAll(p); rmErr != nil {
				errs = append(errs, rmErr)
				return
			}
		}
		removed++
		bytes += e.size
	}

	damaged, err := listDamagedCopies(filepath.Join(parent, "damaged"))
	if err != nil {
		errs = append(errs, err)
	}
	for i, d := range damaged {
		if i < storeEvidenceKeepDamaged || now.Sub(d.stamp) <= storeEvidenceMinAge {
			continue
		}
		remove(d)
	}

	pending, err := coreSchemaUpgradePending(databasePath)
	switch {
	case err != nil:
		errs = append(errs, err)
	case !pending:
		sets, err := listUpgradeBackups(filepath.Join(parent, "backups"))
		if err != nil {
			errs = append(errs, err)
		}
		for i, set := range sets {
			if i == 0 {
				continue
			}
			remove(set)
		}
	}
	return removed, bytes, errors.Join(errs...)
}

// pruneStoreEvidenceAfterStart runs the rule once the daemon is serving, so a
// start that fails keeps every copy it might need.
func (s *Server) pruneStoreEvidenceAfterStart() {
	if s == nil || s.coreStorePath == "" {
		return
	}
	removed, bytes, err := pruneStoreEvidence(s.coreStorePath, time.Now())
	if err != nil {
		s.warnf("store copies: %v", err)
	}
	if removed == 0 {
		return
	}
	s.logger.Infof("store copies: removed %d old quarantine or upgrade copies (%s); the newest %d quarantines, any younger than %d days, and the newest upgrade backup stay",
		removed, formatBackupBytes(bytes), storeEvidenceKeepDamaged, int(storeEvidenceMinAge.Hours()/24))
}

func treeSize(root string) int64 {
	var n int64
	_ = filepath.WalkDir(root, func(_ string, d fs.DirEntry, err error) error {
		if err == nil && d.Type().IsRegular() {
			if info, err := d.Info(); err == nil {
				n += info.Size()
			}
		}
		return nil
	})
	return n
}
