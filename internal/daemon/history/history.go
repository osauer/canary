// Package history implements the retired history.db index for legacy import,
// rotation recovery, and isolated compatibility tests.
//
// Production history reads and mutable authority live in daemon.db through
// package corestore. This package must not be used as a current-state,
// freshness, submit-eligibility, freeze, or broker-write authority. Its SQLite
// database is derived from retained journals, archives, and statements and may
// be rebuilt after an open failure.
//
// Legacy rotation relocates only fully ingested decision-journal prefixes into
// immutable gzip archives. Archives plus the live tail reconstruct the source
// byte stream; the order journal is never rotated, truncated, or rewritten.
package history

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	_ "modernc.org/sqlite" // pure-Go database/sql driver, name "sqlite"
)

// Options locates the derived index file, its journal sources, the
// retained-statement directory, and the rotation archive directory.
type Options struct {
	// DBPath is the history.db file. Pre-created 0600 before the driver
	// touches it so WAL sidecars inherit a private mode.
	DBPath string
	// RegimeJournalPath is regime-decisions.jsonl. The file may not exist
	// yet; ingest treats a missing journal as zero lines.
	RegimeJournalPath string
	// RulesJournalPath is rules-decisions.jsonl. Same missing-file
	// semantics as the regime journal.
	RulesJournalPath string
	// CanaryJournalPath is canary-decisions.jsonl, the on-disk journal
	// feeding the "stress" ingest source. The file keeps its name: renaming
	// retained evidence and its rotated archives is not part of the sensor
	// rename.
	CanaryJournalPath string
	// CapitalJournalPath is capital-events.jsonl (never rotated).
	CapitalJournalPath string
	// RiskPolicyJournalPath is risk-policy-journal.jsonl (never rotated).
	RiskPolicyJournalPath string
	// ProposalOutcomesPath is trade-proposal-outcomes.jsonl (never rotated).
	ProposalOutcomesPath string
	// OrderJournalPath is order-journal.jsonl. Trading evidence: indexed in
	// place, never rotated, truncated, or rewritten by this package.
	OrderJournalPath string
	// ValidateOrderLine is the daemon's canonical full order-journal
	// decoder. The history package cannot import the daemon's writer type
	// without an import cycle, so the owner injects that authority here.
	// When nil, order rows are indexed for rebuildability but marked
	// unparseable and can never serve the indexed order-read fast path.
	ValidateOrderLine func([]byte) error
	// StatementsDir is the retained Flex statement directory; *.xml files
	// there derive statement_equity_days (file-set ingest, newest
	// whenGenerated wins per day).
	StatementsDir string
	// RotatedDir is where rotation writes its immutable gzip archives
	// (<journal>-YYYY-MM.jsonl.gz). Created 0700 on first rotation.
	RotatedDir string
	// Logf receives warnings (skipped corrupt lines, rebuilds, recovery,
	// rotation failures). Ingest failures never propagate to journaling or
	// snapshots — they are logged here and swallowed. nil discards.
	Logf func(format string, args ...any)
	// Infof receives the one-line rotation disclosures. nil falls back to
	// Logf.
	Infof func(format string, args ...any)
}

// Store is a legacy history.db handle. One SQLite connection serializes ingest
// batches and reads, while the kick channel lets legacy journal writers request
// another ingest pass without carrying source data through memory.
type Store struct {
	db   *sql.DB
	opts Options
	kick chan struct{}

	// ingestMu serializes mutation passes (ingestAll, rotation, recovery)
	// so the rotation live-file swap can never race a concurrent tail
	// ingest into a spurious truncation rebuild. Read queries do not take
	// it — the single SQLite connection already serializes statements.
	ingestMu sync.Mutex

	// wmMu guards the in-memory per-source logical ingest watermarks and
	// the cached orders parse-marker flag. Watermarks are refreshed after
	// every ingest batch commit; freshness checks (OrdersFresh) read them
	// without touching SQLite. Deliberately NOT seeded at Open: the first
	// serves after a restart fall back to the legacy journal scan until the
	// first ingest pass verifies size and genesis, so the fallback path is
	// exercised on every cold start.
	wmMu       sync.Mutex
	watermarks map[string]int64
	ordersBad  bool
	// orderGeneration is the file identity validated by the ingest pass
	// which produced the current orders watermark. Freshness requires the
	// live path to still name this same generation; size equality alone is
	// insufficient because an equal-length replacement can contain wholly
	// different evidence.
	orderGeneration   orderFileGeneration
	orderGenerationOK bool

	// rotateFailpoint, when set (tests only), is invoked at named rotation
	// and backfill stages and aborts the operation when it errors —
	// simulating a crash for the recovery matrix.
	rotateFailpoint func(stage string) error

	closeOnce sync.Once
	closeErr  error
}

// Open opens (creating and migrating as needed) history.db at
// opts.DBPath. A corrupt or future-versioned file is deleted and recreated
// once — the index is derived state, so recovery is rebuild, not repair.
// A second consecutive failure returns the error; the caller leaves the
// index disabled for this run and journaling continues unaffected.
func Open(opts Options) (*Store, error) {
	if opts.DBPath == "" {
		return nil, errors.New("history: DBPath is required")
	}
	if err := os.MkdirAll(filepath.Dir(opts.DBPath), 0o700); err != nil {
		return nil, fmt.Errorf("history: mkdir state dir: %w", err)
	}
	var lastErr error
	for attempt := range 2 {
		if attempt > 0 {
			logf(opts.Logf, "history: recreating %s after open failure: %v", opts.DBPath, lastErr)
			removeDBFiles(opts.DBPath)
		}
		db, err := openAndMigrate(opts.DBPath)
		if err == nil {
			s := &Store{db: db, opts: opts, kick: make(chan struct{}, 1), watermarks: map[string]int64{}}
			s.seedOrdersParseBad()
			// The schema migration renames the stress source inside the DB;
			// its pending rotation intent lives on disk and is carried over
			// here, before any caller can recover or start a rotation.
			s.adoptLegacyCanaryManifest()
			return s, nil
		}
		lastErr = err
	}
	return nil, fmt.Errorf("history: open %s: %w", opts.DBPath, lastErr)
}

// seedOrdersParseBad initializes the cached parse-marker flag from the
// reopened DB so a restart cannot serve indexed order reads over a file
// the previous run flagged as unparseable.
func (s *Store) seedOrdersParseBad() {
	var bad bool
	if err := s.db.QueryRow(`SELECT EXISTS(SELECT 1 FROM order_events WHERE parse_ok = 0)`).Scan(&bad); err == nil {
		s.setOrdersParseBad(bad)
	}
}

// Close releases the SQLite connection. Idempotent; safe on a nil Store.
func (s *Store) Close() error {
	if s == nil {
		return nil
	}
	s.closeOnce.Do(func() {
		s.closeErr = s.db.Close()
	})
	return s.closeErr
}

// Kick nudges the ingest goroutine without blocking or carrying data — the
// journal file itself is the only ingest input, which is what guarantees
// evidence-before-index ordering. Safe on a nil Store and from any
// goroutine; a kick while one is already pending coalesces.
func (s *Store) Kick() {
	if s == nil {
		return
	}
	select {
	case s.kick <- struct{}{}:
	default:
	}
}

// Run ingests every source once (this initial pass IS backfill and crash
// reconcile — first ingest simply starts from offset 0, streaming rotated
// archives before the live file where they exist) and then services kicks
// until ctx is cancelled. All ingest errors are logged and swallowed: the
// index must never fail or block journaling or snapshots.
func (s *Store) Run(ctx context.Context) {
	s.ingestAll(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.kick:
			s.ingestAll(ctx)
		}
	}
}

// setWatermark records a source's committed logical ingest offset for
// lock-free freshness checks.
func (s *Store) setWatermark(source string, offset int64) {
	if s == nil {
		return
	}
	s.wmMu.Lock()
	s.watermarks[source] = offset
	s.wmMu.Unlock()
}

func (s *Store) setOrdersParseBad(bad bool) {
	if s == nil {
		return
	}
	s.wmMu.Lock()
	s.ordersBad = bad
	s.wmMu.Unlock()
}

func (s *Store) ordersParseBad() bool {
	s.wmMu.Lock()
	defer s.wmMu.Unlock()
	return s.ordersBad
}

type orderFileGeneration struct {
	info             os.FileInfo
	genesis          string
	genesisOK        bool
	changeIdentity   orderChangeIdentity
	changeIdentityOK bool
}

func (s *Store) setOrderGeneration(info os.FileInfo, genesis string, genesisOK bool) {
	if s == nil {
		return
	}
	changeIdentity, changeIdentityOK := orderChangeIdentityFor(info)
	s.wmMu.Lock()
	s.orderGeneration = orderFileGeneration{
		info:             info,
		genesis:          genesis,
		genesisOK:        genesisOK,
		changeIdentity:   changeIdentity,
		changeIdentityOK: changeIdentityOK,
	}
	// Platforms where FileInfo exposes no non-user-settable change identity
	// fail closed to the journal scan. Size/mtime/genesis alone cannot prove
	// that a same-size in-place rewrite did not occur.
	s.orderGenerationOK = info != nil && changeIdentityOK
	s.wmMu.Unlock()
}

func (s *Store) clearOrderGeneration() {
	if s == nil {
		return
	}
	s.wmMu.Lock()
	s.orderGeneration = orderFileGeneration{}
	s.orderGenerationOK = false
	s.wmMu.Unlock()
}

func (s *Store) ordersFreshState() (int64, bool, bool, orderFileGeneration, bool) {
	s.wmMu.Lock()
	defer s.wmMu.Unlock()
	wm, watermarkOK := s.watermarks[sourceOrders]
	return wm, watermarkOK, s.ordersBad, s.orderGeneration, s.orderGenerationOK
}

func (s *Store) orderGenerationMatches(info os.FileInfo, genesis string, genesisOK bool) bool {
	if s == nil || info == nil {
		return false
	}
	s.wmMu.Lock()
	defer s.wmMu.Unlock()
	g := s.orderGeneration
	changeIdentity, changeIdentityOK := orderChangeIdentityFor(info)
	return s.orderGenerationOK && g.info != nil &&
		changeIdentityOK && g.changeIdentityOK && changeIdentity == g.changeIdentity &&
		info.Size() == g.info.Size() &&
		info.ModTime().Equal(g.info.ModTime()) &&
		os.SameFile(info, g.info) &&
		genesisOK == g.genesisOK && genesis == g.genesis
}

func sameOrderFileGeneration(a, b os.FileInfo) bool {
	if a == nil || b == nil {
		return false
	}
	aChange, aOK := orderChangeIdentityFor(a)
	bChange, bOK := orderChangeIdentityFor(b)
	return aOK && bOK && aChange == bChange &&
		a.Size() == b.Size() && a.ModTime().Equal(b.ModTime()) &&
		os.SameFile(a, b)
}

// warnf logs through Options.Logf when set.
func (s *Store) warnf(format string, args ...any) {
	logf(s.opts.Logf, format, args...)
}

// infof logs operational disclosures (rotation summaries) through
// Options.Infof, falling back to Logf.
func (s *Store) infof(format string, args ...any) {
	if s.opts.Infof != nil {
		s.opts.Infof(format, args...)
		return
	}
	logf(s.opts.Logf, format, args...)
}

func logf(fn func(format string, args ...any), format string, args ...any) {
	if fn != nil {
		fn(format, args...)
	}
}

// removeDBFiles deletes history.db and its WAL sidecars. Best-effort: the
// subsequent open attempt reports any file that could not be cleared.
func removeDBFiles(path string) {
	for _, p := range []string{path, path + "-wal", path + "-shm"} {
		_ = os.Remove(p)
	}
}
