package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/BurntSushi/toml"
)

var (
	reportingTableLine = regexp.MustCompile(`^\s*\[([^]]+)\]\s*(?:#.*)?$`)
	reportingFlexKey   = regexp.MustCompile(`^\s*(enabled|query_id|token_path)\s*=`)
)

// MergeFlexConfig updates only the three [flex] keys while preserving every
// unrelated line and comment. The returned document is parsed before use so
// unsupported formatting fails without touching the operator's file.
func MergeFlexConfig(data []byte, queryID, tokenPath string) ([]byte, error) {
	queryID = strings.TrimSpace(queryID)
	tokenPath = strings.TrimSpace(tokenPath)
	if queryID == "" || tokenPath == "" {
		return nil, errors.New("reporting query and token path are required")
	}
	if len(data) > 0 {
		if err := validateReportingConfig(data); err != nil {
			return nil, err
		}
	}
	lines := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
	if len(lines) == 1 && lines[0] == "" {
		lines = nil
	}
	start, end := -1, len(lines)
	for i, line := range lines {
		match := reportingTableLine.FindStringSubmatch(line)
		if len(match) != 2 {
			continue
		}
		if start >= 0 {
			end = i
			break
		}
		if strings.TrimSpace(match[1]) == "flex" {
			start = i
		}
	}
	values := map[string]string{
		"enabled":    "enabled = true",
		"query_id":   "query_id = " + strconv.Quote(queryID),
		"token_path": "token_path = " + strconv.Quote(tokenPath),
	}
	if start < 0 {
		if len(lines) > 0 && strings.TrimSpace(lines[len(lines)-1]) != "" {
			lines = append(lines, "")
		}
		lines = append(lines, "[flex]", values["enabled"], values["query_id"], values["token_path"])
	} else {
		seen := map[string]bool{}
		for i := start + 1; i < end; i++ {
			match := reportingFlexKey.FindStringSubmatch(lines[i])
			if len(match) != 2 {
				continue
			}
			key := match[1]
			if seen[key] {
				return nil, fmt.Errorf("duplicate [flex].%s key", key)
			}
			seen[key] = true
			lines[i] = values[key]
		}
		missing := make([]string, 0, 3)
		for _, key := range []string{"enabled", "query_id", "token_path"} {
			if !seen[key] {
				missing = append(missing, values[key])
			}
		}
		lines = append(lines[:end], append(missing, lines[end:]...)...)
	}
	out := []byte(strings.Join(lines, "\n") + "\n")
	if err := validateReportingConfig(out); err != nil {
		return nil, err
	}
	return out, nil
}

// UpdateFlexConfigAtomic promotes one already-validated candidate. Existing
// config is backed up to one bounded 0600 rollback file; the active file is
// then replaced atomically and also uses 0600.
func UpdateFlexConfigAtomic(path, queryID, tokenPath string) (string, error) {
	if strings.TrimSpace(path) == "" {
		path = DefaultPath()
	}
	var existing []byte
	info, err := os.Lstat(path)
	switch {
	case err == nil:
		if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
			return "", errors.New("reporting config must be a regular file")
		}
		existing, err = os.ReadFile(path)
		if err != nil {
			return "", errors.New("reporting config could not be read")
		}
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		return "", errors.New("reporting config could not be inspected")
	}
	updated, err := MergeFlexConfig(existing, queryID, tokenPath)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", errors.New("reporting config directory could not be created")
	}
	backup := ""
	if len(existing) > 0 {
		backup = path + ".reporting-backup"
		if err := writeReportingFileAtomic(backup, existing); err != nil {
			return "", errors.New("reporting config backup could not be written")
		}
	}
	if err := writeReportingFileAtomic(path, updated); err != nil {
		return "", errors.New("reporting config could not be replaced")
	}
	return backup, nil
}

func validateReportingConfig(data []byte) error {
	cfg := Config{}
	metadata, err := toml.Decode(string(data), &cfg)
	if err != nil {
		return fmt.Errorf("parse reporting config: %w", err)
	}
	if undecoded := metadata.Undecoded(); len(undecoded) > 0 {
		return fmt.Errorf("reporting config contains unsupported key %s", undecoded[0].String())
	}
	return nil
}

func writeReportingFileAtomic(path string, data []byte) (retErr error) {
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".tmp.*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer func() {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
	}()
	if err := tmp.Chmod(0o600); err != nil {
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		return err
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer func() {
		if err := dir.Close(); retErr == nil && err != nil {
			retErr = err
		}
	}()
	return dir.Sync()
}
