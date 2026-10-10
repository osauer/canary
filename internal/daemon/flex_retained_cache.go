package daemon

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"os"
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
	rows map[[sha256.Size]byte][]flexstmt.Statement
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
	next := make(map[[sha256.Size]byte][]flexstmt.Statement)
	// Hash unchanged files with one reusable buffer, without allocating their
	// complete XML or trusting names, size, modification time or broker dates.
	buffer := make([]byte, 32*1024)
	statements, problems, err := loadRetainedFlexStatementsWith(ctx, checkpoint, s.flexEvidenceSelection(), func(ctx context.Context, path string) ([]flexstmt.Statement, error) {
		entry, err := c.read(ctx, path, buffer)
		if err != nil {
			return nil, err
		}
		next[entry.digest] = entry.statements
		return entry.statements, nil
	})
	if err == nil {
		c.rows = next
	}
	return statements, problems, err
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
	next := make(map[[sha256.Size]byte][]flexstmt.Statement)
	var out []flexstmt.Statement
	for i, file := range files {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		rows, ok := c.rows[file.digest]
		if !ok {
			var err error
			rows, err = flexstmt.ParseContext(ctx, file.data)
			if err != nil {
				return nil, err
			}
		}
		files[i].statements = rows
		next[file.digest] = rows
		out = append(out, rows...)
	}
	c.rows = next
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
	if rows, ok := c.rows[digest]; ok {
		return retainedFlexEntry{digest: digest, statements: rows}, ctx.Err()
	}
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
	return retainedFlexEntry{digest: digest, statements: statements}, nil
}
