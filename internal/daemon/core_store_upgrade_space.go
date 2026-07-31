package daemon

import (
	"errors"
	"fmt"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"syscall"

	"golang.org/x/sys/unix"
)

const coreSchemaUpgradeMinimumMarginBytes = uint64(1 << 30)

type coreSchemaUpgradeSpaceError struct {
	Path           string
	Phase          string
	SourceBytes    uint64
	RequiredBytes  uint64
	AvailableBytes uint64
	Cause          error
}

func (e *coreSchemaUpgradeSpaceError) Error() string {
	if e == nil {
		return "daemon schema upgrade storage is unavailable"
	}
	message := fmt.Sprintf(
		"daemon schema upgrade needs %s free for %s (source footprint %s, available %s); published authority is unchanged",
		formatStorageBytes(e.RequiredBytes),
		e.Phase,
		formatStorageBytes(e.SourceBytes),
		formatStorageBytes(e.AvailableBytes),
	)
	if e.Path != "" {
		message += " at " + e.Path
	}
	if e.Cause != nil {
		message += ": " + e.Cause.Error()
	}
	return message
}

func (e *coreSchemaUpgradeSpaceError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Cause
}

func formatStorageBytes(value uint64) string {
	const (
		mib = uint64(1 << 20)
		gib = uint64(1 << 30)
	)
	if value >= gib {
		return fmt.Sprintf("%.1f GiB", float64(value)/float64(gib))
	}
	return fmt.Sprintf("%.1f MiB", float64(value)/float64(mib))
}

func coreSchemaUpgradeSourceFootprint(path string) (uint64, error) {
	var total uint64
	for _, candidate := range []string{path, path + "-wal", path + "-shm", path + "-journal"} {
		info, err := os.Lstat(candidate)
		if errors.Is(err, fs.ErrNotExist) {
			if candidate == path {
				return 0, fmt.Errorf("inspect schema upgrade source: %w", err)
			}
			continue
		}
		if err != nil {
			return 0, fmt.Errorf("inspect schema upgrade source artifact: %w", err)
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return 0, fmt.Errorf("schema upgrade source artifact must be a regular file")
		}
		size := info.Size()
		if size < 0 || uint64(size) > math.MaxUint64-total {
			return 0, fmt.Errorf("schema upgrade source footprint overflow")
		}
		total += uint64(size)
	}
	return total, nil
}

func coreSchemaUpgradeRequiredFreeBytes(sourceBytes uint64) (uint64, error) {
	margin := max(sourceBytes/20, coreSchemaUpgradeMinimumMarginBytes)
	if sourceBytes > (math.MaxUint64-margin)/2 {
		return 0, fmt.Errorf("schema upgrade storage requirement overflow")
	}
	return sourceBytes*2 + margin, nil
}

func coreSchemaUpgradeAvailableBytes(path string) (uint64, error) {
	var stat unix.Statfs_t
	if err := unix.Statfs(filepath.Dir(path), &stat); err != nil {
		return 0, fmt.Errorf("inspect schema upgrade filesystem: %w", err)
	}
	if stat.Bavail < 0 || stat.Bsize < 0 {
		return 0, fmt.Errorf("schema upgrade filesystem returned invalid available space")
	}
	availableBlocks := uint64(stat.Bavail)
	blockSize := uint64(stat.Bsize)
	if blockSize != 0 && availableBlocks > math.MaxUint64/blockSize {
		return 0, fmt.Errorf("schema upgrade available-space overflow")
	}
	return availableBlocks * blockSize, nil
}

func ensureCoreSchemaUpgradeSpace(path, phase string, availableProbe func(string) (uint64, error)) error {
	sourceBytes, err := coreSchemaUpgradeSourceFootprint(path)
	if err != nil {
		return err
	}
	requiredBytes, err := coreSchemaUpgradeRequiredFreeBytes(sourceBytes)
	if err != nil {
		return err
	}
	if availableProbe == nil {
		availableProbe = coreSchemaUpgradeAvailableBytes
	}
	availableBytes, err := availableProbe(path)
	if err != nil {
		return err
	}
	if availableBytes < requiredBytes {
		return &coreSchemaUpgradeSpaceError{
			Path:           path,
			Phase:          phase,
			SourceBytes:    sourceBytes,
			RequiredBytes:  requiredBytes,
			AvailableBytes: availableBytes,
		}
	}
	return nil
}

func coreSchemaUpgradeNoSpaceError(path, phase string, availableProbe func(string) (uint64, error), cause error) error {
	if !errors.Is(cause, syscall.ENOSPC) {
		return cause
	}
	sourceBytes, sizeErr := coreSchemaUpgradeSourceFootprint(path)
	if sizeErr != nil {
		return errors.Join(cause, sizeErr)
	}
	requiredBytes, requiredErr := coreSchemaUpgradeRequiredFreeBytes(sourceBytes)
	if requiredErr != nil {
		return errors.Join(cause, requiredErr)
	}
	if availableProbe == nil {
		availableProbe = coreSchemaUpgradeAvailableBytes
	}
	availableBytes, availableErr := availableProbe(path)
	if availableErr != nil {
		return errors.Join(cause, availableErr)
	}
	return &coreSchemaUpgradeSpaceError{
		Path:           path,
		Phase:          phase,
		SourceBytes:    sourceBytes,
		RequiredBytes:  requiredBytes,
		AvailableBytes: availableBytes,
		Cause:          cause,
	}
}
