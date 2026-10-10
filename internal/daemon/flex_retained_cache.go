package daemon

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"

	"github.com/osauer/canary/v2/internal/flexstmt"
)

// Only pure parsing is reused: reconciliation still evaluates current policy,
// capital events, account scope and time on every read. The cache belongs to
// this daemon and retains at most the currently selected file set; changing
// query generation or removing a file drops its parsed view on the next read.
type retainedFlexCache struct {
	once sync.Once
	gate chan struct{}
	rows map[string]retainedFlexEntry
}

type retainedFlexEntry struct {
	digest     [sha256.Size]byte
	statements []flexstmt.Statement
}

// The returned outer slice is caller-owned. Nested statement evidence is
// immutable and shared; all consumers filter, merge or project it without
// editing the parsed records.
func (s *Server) loadActiveRetainedFlexStatementsContext(ctx context.Context, checkpoint func(string) error) ([]flexstmt.Statement, []string, error) {
	c := &s.retainedFlex
	if err := c.lock(ctx); err != nil {
		return nil, nil, err
	}
	defer func() { <-c.gate }()
	// Hash unchanged files with one reusable buffer, without allocating their
	// complete XML or trusting names, size, modification time or broker dates.
	buffer := make([]byte, 32*1024)
	return loadRetainedFlexStatementsWith(ctx, checkpoint, s.flexEvidenceSelection(), c.retainFiles, func(ctx context.Context, path string) ([]flexstmt.Statement, error) {
		entry, err := c.read(ctx, path, buffer)
		if err != nil {
			return nil, err
		}
		return entry.statements, nil
	})
}

// Keep completed files even when a later parse is cancelled. Keying by the
// selected filename bounds retained progress to one verified version per file.
// The content digest, never the filename alone, determines whether it is reused.
func (c *retainedFlexCache) retainFiles(names []string) {
	if c.rows == nil {
		c.rows = make(map[string]retainedFlexEntry, len(names))
	}
	selected := make(map[string]bool, len(names))
	for _, name := range names {
		selected[name] = true
	}
	for name := range c.rows {
		if !selected[name] {
			delete(c.rows, name)
		}
	}
}

func (c *retainedFlexCache) lock(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	c.once.Do(func() { c.gate = make(chan struct{}, 1) })
	select {
	case c.gate <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// parse shares the same pure parser with readers that have already hashed the
// files and checked them against the accepted SQLite inventory. Those callers
// still recheck inventory, query and account authority before serving evidence.
func (c *retainedFlexCache) parse(ctx context.Context, files []statementProjectionFile) ([]flexstmt.Statement, error) {
	if err := c.lock(ctx); err != nil {
		return nil, err
	}
	defer func() { <-c.gate }()
	return c.parseLocked(ctx, files)
}

func (c *retainedFlexCache) parseLocked(ctx context.Context, files []statementProjectionFile) ([]flexstmt.Statement, error) {
	names := make([]string, len(files))
	for i, file := range files {
		names[i] = file.name
	}
	c.retainFiles(names)
	var out []flexstmt.Statement
	for i, file := range files {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		entry, ok := c.rows[file.name]
		if !ok || entry.digest != file.digest {
			delete(c.rows, file.name)
			rows, err := flexstmt.ParseContext(ctx, file.data)
			if err != nil {
				return nil, err
			}
			entry = retainedFlexEntry{digest: file.digest, statements: rows}
			c.rows[file.name] = entry
		}
		files[i].statements = entry.statements
		out = append(out, entry.statements...)
	}
	return out, nil
}

func (c *retainedFlexCache) read(ctx context.Context, path string, buffer []byte) (retainedFlexEntry, error) {
	var empty retainedFlexEntry
	info, err := os.Lstat(path)
	if err != nil {
		return empty, err
	}
	if !info.Mode().IsRegular() {
		return empty, fmt.Errorf("statement source is not a regular non-symlink file")
	}
	f, err := os.Open(path)
	if err != nil {
		return empty, err
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil {
		return empty, err
	}
	if !opened.Mode().IsRegular() || !os.SameFile(info, opened) {
		return empty, fmt.Errorf("statement source identity changed while opening")
	}
	hash := sha256.New()
	for {
		if err := ctx.Err(); err != nil {
			return empty, err
		}
		n, err := f.Read(buffer)
		_, _ = hash.Write(buffer[:n])
		if err == io.EOF {
			break
		}
		if err != nil {
			return empty, err
		}
	}
	var digest [sha256.Size]byte
	hash.Sum(digest[:0])
	name := filepath.Base(path)
	if entry, ok := c.rows[name]; ok && entry.digest == digest {
		return entry, ctx.Err()
	}
	delete(c.rows, name)
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return empty, err
	}
	data, err := io.ReadAll(f)
	if err != nil {
		return empty, err
	}
	if sha256.Sum256(data) != digest {
		return empty, fmt.Errorf("statement source changed while reading")
	}
	statements, err := flexstmt.ParseContext(ctx, data)
	if err != nil {
		return empty, err
	}
	entry := retainedFlexEntry{digest: digest, statements: statements}
	c.rows[name] = entry
	return entry, nil
}
