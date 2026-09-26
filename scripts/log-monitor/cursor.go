package main

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// A cursor is private monitor state. Numeric v1 offsets are read but replayed:
// a line count alone cannot establish which file was actually consumed.
type logCursor struct {
	Version  int                       `json:"version"`
	Path     string                    `json:"path"`
	Identity string                    `json:"identity"`
	Lines    int                       `json:"lines"`
	Prefix   string                    `json:"prefix"`
	Boundary string                    `json:"boundary"`
	Days     map[string]map[string]int `json:"days,omitempty"`
	Starts   []time.Time               `json:"starts,omitempty"`
}

func readCursor(path string) (logCursor, error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return logCursor{}, nil
	}
	if err != nil {
		return logCursor{}, err
	}
	value := strings.TrimSpace(string(raw))
	if value == "" {
		return logCursor{}, nil
	}
	if n, err := strconv.Atoi(value); err == nil && n >= 0 {
		return logCursor{Version: 1, Lines: n}, nil
	}
	var c logCursor
	if err := json.Unmarshal(raw, &c); err != nil || c.Version != 2 || c.Lines < 0 {
		return c, fmt.Errorf("invalid log cursor")
	}
	return c, nil
}

func lineHash(lines []string) string {
	h := sha256.New()
	for _, line := range lines {
		h.Write([]byte(line))
		h.Write([]byte{'\n'})
	}
	return hex.EncodeToString(h.Sum(nil))
}

func cursorMatches(c logCursor, lines []string) bool {
	return c.Lines <= len(lines) && c.Prefix == lineHash(lines[:min(c.Lines, 32)]) &&
		c.Boundary == lineHash(lines[max(0, c.Lines-1):c.Lines])
}

func readLog(path string) ([]string, os.FileInfo, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, nil, err
	}
	// Snapshot the length. An incomplete final record remains unread next time.
	r := bufio.NewReader(io.NewSectionReader(f, 0, info.Size()))
	var lines []string
	var line strings.Builder
	for {
		part, err := r.ReadSlice('\n')
		if line.Len()+len(part) > maxScannerCapacity {
			return nil, nil, fmt.Errorf("log record exceeds size limit")
		}
		line.Write(part)
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, nil, err
		}
		lines = append(lines, strings.TrimSuffix(strings.TrimSuffix(line.String(), "\n"), "\r"))
		line.Reset()
	}
	return lines, info, nil
}

func scanIncremental(logPath, offsetPath string) (scannedLog, error) {
	previous, err := readCursor(offsetPath)
	if err != nil {
		return scannedLog{}, err
	}
	path, err := filepath.Abs(logPath)
	if err != nil {
		return scannedLog{}, err
	}
	all, info, err := readLog(path)
	if errors.Is(err, os.ErrNotExist) {
		return scannedLog{state: "missing", path: path}, nil
	}
	if err != nil {
		return scannedLog{}, err
	}
	identity := fileIdentity(info)
	next := logCursor{Version: 2, Path: path, Identity: identity, Lines: len(all), Prefix: lineHash(all[:min(len(all), 32)]), Boundary: lineHash(all[max(0, len(all)-1):]), Days: previous.Days, Starts: previous.Starts}
	scanned := scannedLog{state: "unchanged", path: path, modified: info.ModTime(), total: len(all), cursor: next}
	offset := 0
	if previous.Version == 2 && previous.Path == path && previous.Identity == identity && cursorMatches(previous, all) {
		offset = previous.Lines
	} else if previous.Version != 0 {
		scanned.offsetReset = true
		if previous.Version == 1 {
			// Replay both retained files once when migrating an unverifiable offset.
			older, _, readErr := readLog(path + ".1")
			if readErr != nil && !errors.Is(readErr, os.ErrNotExist) {
				return scannedLog{}, readErr
			}
			scanned.lines = append(scanned.lines, older...)
			scanned.coverage = "legacy line offset replayed; older rotated coverage cannot be verified"
		} else if previous.Path == path {
			older, oldInfo, readErr := readLog(path + ".1")
			if readErr != nil && !errors.Is(readErr, os.ErrNotExist) {
				return scannedLog{}, readErr
			}
			// The digest also handles copy/truncate rotation, whose backup has a new inode.
			if readErr == nil && cursorMatches(previous, older) && (previous.Identity == fileIdentity(oldInfo) || previous.Lines > 0) {
				scanned.lines = append(scanned.lines, older[previous.Lines:]...)
			} else {
				scanned.coverage = "log replaced or truncated; unread rotated tail is unavailable"
			}
		} else {
			scanned.coverage = "log source changed; previous source coverage cannot be verified"
			scanned.cursor.Days = nil
			scanned.cursor.Starts = nil
		}
	}
	scanned.lines = append(scanned.lines, all[offset:]...)
	if len(scanned.lines) > 0 {
		scanned.state = "scanned"
	}
	return scanned, nil
}

func writeCursor(path string, c logCursor) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close()
	if err := tmp.Chmod(0o600); err != nil {
		return err
	}
	if err := json.NewEncoder(tmp).Encode(c); err != nil {
		return err
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
