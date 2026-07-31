package daemon

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/osauer/canary/v2/internal/daemon/corestore"
)

const authorityWatermarkVersion = 1

type authorityWatermarkFile struct {
	Version int                     `json:"version"`
	Head    corestore.AuthorityHead `json:"head"`
}

func loadAuthorityWatermark(path string) (*corestore.AuthorityHead, error) {
	if strings.TrimSpace(path) == "" {
		return nil, fmt.Errorf("authority watermark path is empty")
	}
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("inspect authority watermark: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return nil, fmt.Errorf("authority watermark must be a regular private file")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read authority watermark: %w", err)
	}
	var file authorityWatermarkFile
	if err := json.Unmarshal(raw, &file); err != nil {
		return nil, fmt.Errorf("decode authority watermark: %w", err)
	}
	if file.Version != authorityWatermarkVersion || strings.TrimSpace(file.Head.AuthorityEpoch) == "" || file.Head.HeadGeneration < 0 || file.Head.LastEventSeq < 0 || file.Head.SignerGeneration < 1 {
		return nil, fmt.Errorf("authority watermark is invalid")
	}
	return &file.Head, nil
}

func writeAuthorityWatermark(path string, head corestore.AuthorityHead) error {
	if strings.TrimSpace(head.AuthorityEpoch) == "" || head.HeadGeneration < 0 || head.LastEventSeq < 0 || head.SignerGeneration < 1 {
		return fmt.Errorf("refuse invalid authority watermark")
	}
	raw, err := json.Marshal(authorityWatermarkFile{Version: authorityWatermarkVersion, Head: head})
	if err != nil {
		return fmt.Errorf("encode authority watermark: %w", err)
	}
	encoded := append(raw, '\n')
	// A maintenance-only schema upgrade can publish a new physical database
	// without changing the authority head. Preserve the already-durable
	// watermark byte-for-byte in that case rather than physically re-stamping
	// identical authority.
	unchanged := false
	if info, statErr := os.Lstat(path); statErr == nil &&
		info.Mode()&os.ModeSymlink == 0 && info.Mode().IsRegular() &&
		info.Mode().Perm()&0o077 == 0 {
		if existing, readErr := os.ReadFile(path); readErr == nil && bytes.Equal(existing, encoded) {
			unchanged = true
		}
	}
	if !unchanged {
		if err := writePrivateStateAtomic(path, encoded); err != nil {
			return fmt.Errorf("write authority watermark: %w", err)
		}
	}
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return fmt.Errorf("open authority watermark for sync: %w", err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return fmt.Errorf("sync authority watermark: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close authority watermark: %w", err)
	}
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return fmt.Errorf("open authority watermark directory: %w", err)
	}
	if err := dir.Sync(); err != nil {
		_ = dir.Close()
		return fmt.Errorf("sync authority watermark directory: %w", err)
	}
	if err := dir.Close(); err != nil {
		return fmt.Errorf("close authority watermark directory: %w", err)
	}
	return nil
}
