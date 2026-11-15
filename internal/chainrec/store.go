package chainrec

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/osauer/canary/v3/internal/rpc"
	_ "modernc.org/sqlite" // registers the "sqlite" driver
)

// StoreFile is the recorder's database name beside daemon.db. It is a
// separate file so a growing research panel never enlarges, backs up with or
// slows an upgrade of the daemon's authority store.
const StoreFile = "option-chains.db"

const storeSchemaVersion = 1

var storeSchema = []string{
	`CREATE TABLE store_meta (
  key TEXT PRIMARY KEY,
  value TEXT NOT NULL
) STRICT`,
	`CREATE TABLE runs (
  run_id INTEGER PRIMARY KEY AUTOINCREMENT,
  session_date TEXT NOT NULL,
  session_close TEXT NOT NULL,
  scheduled_at TEXT NOT NULL,
  deadline TEXT NOT NULL,
  started_at TEXT NOT NULL,
  early_close INTEGER NOT NULL CHECK (early_close IN (0, 1)),
  settings_json TEXT NOT NULL CHECK (json_valid(settings_json))
) STRICT`,
	`CREATE INDEX runs_session ON runs(session_date, run_id)`,
	`CREATE TABLE run_symbols (
  run_id INTEGER NOT NULL REFERENCES runs(run_id),
  symbol TEXT NOT NULL,
  source TEXT NOT NULL,
  status TEXT NOT NULL,
  detail TEXT,
  spot REAL,
  spot_source TEXT,
  spot_data_type TEXT,
  spot_at TEXT,
  planned INTEGER NOT NULL CHECK (planned >= 0),
  expiries_json TEXT NOT NULL CHECK (json_valid(expiries_json)),
  discovered_at TEXT NOT NULL,
  PRIMARY KEY (run_id, symbol)
) STRICT`,
	`CREATE TABLE quotes (
  run_id INTEGER NOT NULL REFERENCES runs(run_id),
  symbol TEXT NOT NULL,
  trading_class TEXT NOT NULL,
  expiry TEXT NOT NULL,
  expiry_slot TEXT NOT NULL,
  strike REAL NOT NULL,
  option_right TEXT NOT NULL CHECK (option_right IN ('C', 'P')),
  status TEXT NOT NULL,
  detail TEXT,
  con_id INTEGER,
  bid REAL,
  ask REAL,
  last REAL,
  bid_size REAL,
  ask_size REAL,
  volume INTEGER,
  open_interest INTEGER,
  iv REAL,
  delta REAL,
  gamma REAL,
  vega REAL,
  theta REAL,
  underlying_price REAL,
  model_source TEXT,
  feed TEXT,
  requested_at_ms INTEGER,
  observed_at_ms INTEGER,
  quote_received_at_ms INTEGER,
  last_trade_at_ms INTEGER,
  UNIQUE (run_id, symbol, trading_class, expiry, strike, option_right)
) STRICT`,
	`CREATE TABLE run_outcomes (
  run_id INTEGER PRIMARY KEY REFERENCES runs(run_id),
  finished_at TEXT NOT NULL,
  status TEXT NOT NULL,
  stop_reason TEXT,
  planned INTEGER NOT NULL,
  attempted INTEGER NOT NULL,
  quoted INTEGER NOT NULL,
  held_unavailable INTEGER NOT NULL CHECK (held_unavailable IN (0, 1))
) STRICT`,
}

func init() {
	// Every table is append-only: a recorded observation is never rewritten
	// or removed by the daemon. Retention is unlimited.
	for _, table := range []string{"runs", "run_symbols", "quotes", "run_outcomes"} {
		for _, op := range []string{"UPDATE", "DELETE"} {
			storeSchema = append(storeSchema, fmt.Sprintf(
				`CREATE TRIGGER %s_no_%s BEFORE %s ON %s BEGIN SELECT RAISE(ABORT, 'option-chain records are append-only'); END`,
				table, strings.ToLower(op), op, table))
		}
	}
}

// Store is the recorder's SQLite file. Writes come only from the recorder;
// reads serve the chains.recorded RPCs.
type Store struct {
	db   *sql.DB
	path string
	mu   sync.Mutex
}

// Open opens or creates the store at path with private file modes.
func Open(ctx context.Context, path string) (*Store, error) {
	return open(ctx, path, true)
}

// OpenExisting opens the store only when its file exists; (nil, nil) means
// the recorder has never run.
func OpenExisting(ctx context.Context, path string) (*Store, error) {
	return open(ctx, path, false)
}

func open(ctx context.Context, path string, create bool) (*Store, error) {
	if strings.TrimSpace(path) == "" {
		return nil, errors.New("option-chain store path is required")
	}
	info, err := os.Lstat(path)
	switch {
	case err == nil:
		if !info.Mode().IsRegular() {
			return nil, errors.New("option-chain store path is not a regular file")
		}
	case errors.Is(err, os.ErrNotExist):
		if !create {
			return nil, nil
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return nil, fmt.Errorf("create option-chain store directory: %w", err)
		}
		f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
		if err != nil {
			return nil, fmt.Errorf("create option-chain store: %w", err)
		}
		_ = f.Close()
	default:
		return nil, fmt.Errorf("inspect option-chain store: %w", err)
	}
	u := &url.URL{Scheme: "file", Path: path}
	q := u.Query()
	q.Add("_pragma", "journal_mode(WAL)")
	q.Add("_pragma", "synchronous(NORMAL)")
	q.Add("_pragma", "foreign_keys(ON)")
	q.Add("_pragma", "busy_timeout(5000)")
	q.Set("_txlock", "immediate")
	u.RawQuery = q.Encode()
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		return nil, fmt.Errorf("open option-chain store: %w", err)
	}
	db.SetMaxOpenConns(1)
	s := &Store{db: db, path: path}
	if err := s.migrate(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	for _, p := range []string{path, path + "-wal", path + "-shm"} {
		if err := os.Chmod(p, 0o600); err != nil && !errors.Is(err, os.ErrNotExist) {
			_ = db.Close()
			return nil, fmt.Errorf("secure option-chain store: %w", err)
		}
	}
	return s, nil
}

func (s *Store) migrate(ctx context.Context) error {
	var version int
	if err := s.db.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&version); err != nil {
		return fmt.Errorf("read option-chain store version: %w", err)
	}
	switch version {
	case storeSchemaVersion:
		return nil
	case 0:
	default:
		return fmt.Errorf("option-chain store has unsupported schema version %d", version)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin option-chain store schema: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	for _, stmt := range storeSchema {
		if _, err := tx.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("create option-chain store schema: %w", err)
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO store_meta(key, value) VALUES ('created_at', ?)`, formatTime(time.Now())); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, fmt.Sprintf(`PRAGMA user_version = %d`, storeSchemaVersion)); err != nil {
		return err
	}
	return tx.Commit()
}

// Path is the store's file.
func (s *Store) Path() string { return s.path }

// Close releases the database.
func (s *Store) Close() error {
	if s == nil {
		return nil
	}
	return s.db.Close()
}

// BeginRun records a run's header and returns its id.
func (s *Store) BeginRun(ctx context.Context, start RunStart) (int64, error) {
	settings, err := json.Marshal(struct {
		Symbols             []string `json:"symbols"`
		StrikeWindowPct     float64  `json:"strike_window_pct"`
		MaxStrikesPerExpiry int      `json:"max_strikes_per_expiry"`
		MaxSymbols          int      `json:"max_symbols"`
		LeadMinutes         int      `json:"lead_minutes"`
		MaxLines            int      `json:"max_lines"`
		LineReserve         int      `json:"line_reserve"`
		ContractWindowMS    int64    `json:"contract_window_ms"`
	}{start.Settings.Symbols, StrikeWindowPct, MaxStrikesPerExpiry, MaxSymbols, int(SnapshotLead / time.Minute), MaxLines, LineReserve, ContractWindow.Milliseconds()})
	if err != nil {
		return 0, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	res, err := s.db.ExecContext(ctx, `INSERT INTO runs(session_date, session_close, scheduled_at, deadline, started_at, early_close, settings_json)
VALUES (?, ?, ?, ?, ?, ?, ?)`, start.Slot.SessionDate, formatTime(start.Slot.Close), formatTime(start.Slot.ScheduledAt),
		formatTime(start.Slot.Deadline), formatTime(start.StartedAt), boolInt(start.Slot.EarlyClose), string(settings))
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// RecordSymbol records one underlying's discovery.
func (s *Store) RecordSymbol(ctx context.Context, runID int64, plan SymbolPlan) error {
	expiries := plan.Expiries
	if expiries == nil {
		expiries = []PlannedExpiry{}
	}
	raw, err := json.Marshal(expiries)
	if err != nil {
		return err
	}
	var spot, spotSource, spotType, spotAt any
	if plan.Spot.Price > 0 {
		spot, spotSource, spotType = plan.Spot.Price, nullString(plan.Spot.Source), nullString(plan.Spot.DataType)
		if !plan.Spot.AsOf.IsZero() {
			spotAt = formatTime(plan.Spot.AsOf)
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err = s.db.ExecContext(ctx, `INSERT INTO run_symbols(run_id, symbol, source, status, detail, spot, spot_source, spot_data_type, spot_at, planned, expiries_json, discovered_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, runID, plan.Symbol, plan.Source, plan.Status, nullString(plan.Detail),
		spot, spotSource, spotType, spotAt, plan.Planned, string(raw), formatTime(plan.DiscoveredAt))
	return err
}

// RecordRows appends contract rows in one transaction.
func (s *Store) RecordRows(ctx context.Context, runID int64, rows []Row) error {
	if len(rows) == 0 {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	stmt, err := tx.PrepareContext(ctx, `INSERT INTO quotes(run_id, symbol, trading_class, expiry, expiry_slot, strike, option_right, status, detail, con_id,
  bid, ask, last, bid_size, ask_size, volume, open_interest, iv, delta, gamma, vega, theta, underlying_price, model_source, feed,
  requested_at_ms, observed_at_ms, quote_received_at_ms, last_trade_at_ms)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for _, r := range rows {
		var conID any
		if r.ConID > 0 {
			conID = r.ConID
		}
		if _, err := stmt.ExecContext(ctx, runID, r.Symbol, r.TradingClass, r.Expiry, r.Slot, r.Strike, r.Right, r.Status,
			nullString(r.Detail), conID, nullFloat(r.Bid), nullFloat(r.Ask), nullFloat(r.Last), nullFloat(r.BidSize), nullFloat(r.AskSize),
			nullInt(r.Volume), nullInt(r.OpenInterest), nullFloat(r.IV), nullFloat(r.Delta), nullFloat(r.Gamma), nullFloat(r.Vega),
			nullFloat(r.Theta), nullFloat(r.UnderlyingPrice), nullString(r.ModelSource), nullString(r.Feed),
			nullMillis(r.RequestedAt), nullMillis(r.ObservedAt), nullMillis(r.QuoteReceivedAt), nullMillis(r.LastTradeAt)); err != nil {
			return fmt.Errorf("record %s %s %g%s: %w", r.Symbol, r.Expiry, r.Strike, r.Right, err)
		}
	}
	return tx.Commit()
}

// FinishRun closes a run.
func (s *Store) FinishRun(ctx context.Context, runID int64, o Outcome) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.ExecContext(ctx, `INSERT INTO run_outcomes(run_id, finished_at, status, stop_reason, planned, attempted, quoted, held_unavailable)
VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, runID, formatTime(o.FinishedAt), o.Status, nullString(o.StopReason), o.Planned, o.Attempted, o.Quoted, boolInt(o.HeldUnavailable))
	return err
}

// HasRun reports whether any run began for the session date. A session is
// recorded at most once: an interrupted run is never retried.
func (s *Store) HasRun(ctx context.Context, sessionDate string) (bool, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM runs WHERE session_date = ?`, sessionDate).Scan(&n)
	return n > 0, err
}

// Stats summarises the store.
func (s *Store) Stats(ctx context.Context) (rpc.ChainRecorderStore, error) {
	out := rpc.ChainRecorderStore{Present: true}
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM runs`).Scan(&out.Runs); err != nil {
		return out, err
	}
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM quotes`).Scan(&out.Rows); err != nil {
		return out, err
	}
	for _, p := range []string{s.path, s.path + "-wal"} {
		if info, err := os.Stat(p); err == nil {
			out.Bytes += info.Size()
		}
	}
	return out, nil
}

// Runs lists runs newest first. active names a run this process is still
// executing; any other run without an outcome reads as interrupted.
func (s *Store) Runs(ctx context.Context, p rpc.ChainsRecordedParams, active int64) ([]rpc.ChainRecorderRun, error) {
	query := `SELECT r.run_id, r.session_date, r.session_close, r.scheduled_at, r.started_at, r.deadline, r.early_close,
  o.finished_at, o.status, o.stop_reason, o.held_unavailable
FROM runs r LEFT JOIN run_outcomes o ON o.run_id = r.run_id`
	var where []string
	var args []any
	if p.Date != "" {
		where = append(where, `r.session_date = ?`)
		args = append(args, p.Date)
	}
	if p.Symbol != "" {
		where = append(where, `EXISTS (SELECT 1 FROM run_symbols s WHERE s.run_id = r.run_id AND s.symbol = ?)`)
		args = append(args, p.Symbol)
	}
	if len(where) > 0 {
		query += " WHERE " + strings.Join(where, " AND ")
	}
	query += ` ORDER BY r.run_id DESC LIMIT ?`
	limit := p.Limit
	if limit <= 0 {
		limit = rpc.ChainsRecordedDefaultRuns
	}
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	var out []rpc.ChainRecorderRun
	for rows.Next() {
		var run rpc.ChainRecorderRun
		var closeAt, scheduled, started, deadline string
		var early int
		var finished, status, stop sql.NullString
		var held sql.NullInt64
		if err := rows.Scan(&run.RunID, &run.SessionDate, &closeAt, &scheduled, &started, &deadline, &early, &finished, &status, &stop, &held); err != nil {
			_ = rows.Close()
			return nil, err
		}
		run.SessionClose, run.ScheduledAt, run.StartedAt, run.Deadline = parseTime(closeAt), parseTime(scheduled), parseTime(started), parseTime(deadline)
		run.EarlyClose = early == 1
		switch {
		case status.Valid:
			run.Status, run.StopReason, run.FinishedAt = status.String, stop.String, parseTime(finished.String)
			run.HeldUnavailable = held.Int64 == 1
		case run.RunID == active:
			run.Status = RunRunning
		default:
			run.Status = RunInterrupted
		}
		out = append(out, run)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i := range out {
		if err := s.fillCoverage(ctx, &out[i], p.Symbol); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func (s *Store) fillCoverage(ctx context.Context, run *rpc.ChainRecorderRun, symbol string) error {
	symQuery := `SELECT symbol, source, status, detail, spot, spot_source, spot_data_type, planned, expiries_json FROM run_symbols WHERE run_id = ?`
	args := []any{run.RunID}
	if symbol != "" {
		symQuery += ` AND symbol = ?`
		args = append(args, symbol)
	}
	symQuery += ` ORDER BY rowid`
	rows, err := s.db.QueryContext(ctx, symQuery, args...)
	if err != nil {
		return err
	}
	index := map[string]int{}
	for rows.Next() {
		var sr rpc.ChainRecorderSymbolResult
		var detail, spotSource, spotType sql.NullString
		var spot sql.NullFloat64
		var planned int
		var expiries string
		if err := rows.Scan(&sr.Symbol, &sr.Source, &sr.Status, &detail, &spot, &spotSource, &spotType, &planned, &expiries); err != nil {
			_ = rows.Close()
			return err
		}
		sr.Detail, sr.Spot, sr.SpotSource, sr.SpotDataType = detail.String, spot.Float64, spotSource.String, spotType.String
		var planExp []PlannedExpiry
		if err := json.Unmarshal([]byte(expiries), &planExp); err != nil {
			_ = rows.Close()
			return fmt.Errorf("decode run %d %s expiries: %w", run.RunID, sr.Symbol, err)
		}
		for _, e := range planExp {
			sr.Expiries = append(sr.Expiries, rpc.ChainRecorderExpiry{Date: e.Date, TradingClass: e.TradingClass, Slot: e.Slot,
				InWindow: e.InWindow, Selected: e.Selected, Contracts: e.Contracts})
		}
		sr.Coverage.Planned = planned
		index[sr.Symbol] = len(run.Symbols)
		run.Symbols = append(run.Symbols, sr)
	}
	if err := rows.Close(); err != nil {
		return err
	}
	countQuery := `SELECT symbol, status, COUNT(*),
  SUM(bid IS NOT NULL AND ask IS NOT NULL), SUM(iv IS NOT NULL), SUM(delta IS NOT NULL), SUM(open_interest IS NOT NULL)
FROM quotes WHERE run_id = ?`
	args = []any{run.RunID}
	if symbol != "" {
		countQuery += ` AND symbol = ?`
		args = append(args, symbol)
	}
	countQuery += ` GROUP BY symbol, status`
	rows, err = s.db.QueryContext(ctx, countQuery, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var sym, status string
		var n, twoSided, iv, greeks, oi int
		if err := rows.Scan(&sym, &status, &n, &twoSided, &iv, &greeks, &oi); err != nil {
			return err
		}
		i, ok := index[sym]
		if !ok {
			index[sym] = len(run.Symbols)
			i = len(run.Symbols)
			run.Symbols = append(run.Symbols, rpc.ChainRecorderSymbolResult{Symbol: sym})
		}
		for _, c := range []*rpc.ChainRecorderCoverage{&run.Symbols[i].Coverage, &run.Coverage} {
			addCoverage(c, status, n, twoSided, iv, greeks, oi)
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	run.Coverage.Planned = 0
	for i := range run.Symbols {
		c := &run.Symbols[i].Coverage
		// A run that ended before writing its not-attempted rows (an
		// interruption) still counts its planned contracts as missing.
		if recorded := c.Attempted + c.NotAttempted; c.Planned > recorded {
			c.NotAttempted += c.Planned - recorded
			run.Coverage.NotAttempted += c.Planned - recorded
		}
		run.Coverage.Planned += c.Planned
	}
	return nil
}

func addCoverage(c *rpc.ChainRecorderCoverage, status string, n, twoSided, iv, greeks, oi int) {
	if c.ByStatus == nil {
		c.ByStatus = map[string]int{}
	}
	c.ByStatus[status] += n
	if status == StatusNotAttempted {
		c.NotAttempted += n
	} else {
		c.Attempted += n
	}
	if status == StatusQuoted {
		c.Quoted += n
	}
	c.TwoSided += twoSided
	c.WithIV += iv
	c.WithGreeks += greeks
	c.WithOI += oi
}

// Rows returns one page of rows in recording order and the next cursor.
func (s *Store) Rows(ctx context.Context, p rpc.ChainsRecordedRowsParams) ([]rpc.ChainRecordedRow, string, error) {
	query := `SELECT q.rowid, q.run_id, r.session_date, q.symbol, q.trading_class, q.expiry, q.expiry_slot, q.strike, q.option_right, q.status, q.detail,
  q.con_id, q.bid, q.ask, q.last, q.bid_size, q.ask_size, q.volume, q.open_interest, q.iv, q.delta, q.gamma, q.vega, q.theta,
  q.underlying_price, q.model_source, q.feed, q.requested_at_ms, q.observed_at_ms, q.quote_received_at_ms, q.last_trade_at_ms
FROM quotes q JOIN runs r ON r.run_id = q.run_id WHERE q.rowid > ?`
	after := int64(0)
	if p.After != "" {
		n, err := strconv.ParseInt(p.After, 10, 64)
		if err != nil {
			return nil, "", fmt.Errorf("invalid cursor")
		}
		after = n
	}
	args := []any{after}
	if p.RunID > 0 {
		query += ` AND q.run_id = ?`
		args = append(args, p.RunID)
	}
	if p.Date != "" {
		query += ` AND r.session_date = ?`
		args = append(args, p.Date)
	}
	if p.Symbol != "" {
		query += ` AND q.symbol = ?`
		args = append(args, p.Symbol)
	}
	limit := p.Limit
	if limit <= 0 {
		limit = rpc.ChainsRowsDefaultLimit
	}
	query += ` ORDER BY q.rowid LIMIT ?`
	args = append(args, limit+1)
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, "", err
	}
	defer rows.Close()
	out := []rpc.ChainRecordedRow{}
	var last int64
	more := false
	for rows.Next() {
		if len(out) == limit {
			more = true
			break
		}
		var r rpc.ChainRecordedRow
		var rowid int64
		var detail, model, feed sql.NullString
		var conID, volume, oi, requested, observed, received, lastTrade sql.NullInt64
		var bid, ask, lastPx, bidSize, askSize, iv, delta, gamma, vega, theta, under sql.NullFloat64
		if err := rows.Scan(&rowid, &r.RunID, &r.SessionDate, &r.Symbol, &r.TradingClass, &r.Expiry, &r.ExpirySlot, &r.Strike, &r.Right,
			&r.Status, &detail, &conID, &bid, &ask, &lastPx, &bidSize, &askSize, &volume, &oi, &iv, &delta, &gamma, &vega, &theta,
			&under, &model, &feed, &requested, &observed, &received, &lastTrade); err != nil {
			return nil, "", err
		}
		r.Detail, r.ModelSource, r.Feed, r.ConID = detail.String, model.String, feed.String, int(conID.Int64)
		r.Bid, r.Ask, r.Last, r.BidSize, r.AskSize = floatPtr(bid), floatPtr(ask), floatPtr(lastPx), floatPtr(bidSize), floatPtr(askSize)
		r.Volume, r.OpenInterest = intPtr(volume), intPtr(oi)
		r.IV, r.Delta, r.Gamma, r.Vega, r.Theta, r.UnderlyingPrice = floatPtr(iv), floatPtr(delta), floatPtr(gamma), floatPtr(vega), floatPtr(theta), floatPtr(under)
		r.RequestedAt, r.ObservedAt, r.QuoteReceivedAt, r.LastTradeAt = millisTime(requested), millisTime(observed), millisTime(received), millisTime(lastTrade)
		out = append(out, r)
		last = rowid
	}
	if err := rows.Err(); err != nil {
		return nil, "", err
	}
	next := ""
	if more {
		next = strconv.FormatInt(last, 10)
	}
	return out, next, nil
}

// TwoSidedQuote is one recorded quote with both a bid and an ask: the
// session date, the exact contract and its prices.
type TwoSidedQuote struct {
	SessionDate string
	Symbol      string
	Expiry      string
	Strike      float64
	Right       string
	ConID       int64
	Bid         float64
	Ask         float64
}

// TwoSidedQuotes returns the recorded two-sided quotes of the given
// underlyings on session dates from through to (YYYY-MM-DD), oldest first.
// The exit replay prices an exact contract on a past day from them.
func (s *Store) TwoSidedQuotes(ctx context.Context, from, to string, symbols []string) ([]TwoSidedQuote, error) {
	if len(symbols) == 0 {
		return nil, nil
	}
	args := []any{from, to}
	marks := make([]string, 0, len(symbols))
	for _, symbol := range symbols {
		marks = append(marks, "?")
		args = append(args, symbol)
	}
	rows, err := s.db.QueryContext(ctx, `SELECT r.session_date, q.symbol, q.expiry, q.strike, q.option_right, q.con_id, q.bid, q.ask
FROM runs r JOIN quotes q ON q.run_id = r.run_id
WHERE r.session_date BETWEEN ? AND ? AND q.symbol IN (`+strings.Join(marks, ",")+`) AND q.bid IS NOT NULL AND q.ask IS NOT NULL
ORDER BY r.session_date, q.rowid`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TwoSidedQuote
	for rows.Next() {
		var q TwoSidedQuote
		var conID sql.NullInt64
		if err := rows.Scan(&q.SessionDate, &q.Symbol, &q.Expiry, &q.Strike, &q.Right, &conID, &q.Bid, &q.Ask); err != nil {
			return nil, err
		}
		q.ConID = conID.Int64
		out = append(out, q)
	}
	return out, rows.Err()
}

func formatTime(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

func parseTime(s string) time.Time {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}
	}
	return t.UTC()
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func nullString(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func nullFloat(v *float64) any {
	if v == nil {
		return nil
	}
	return *v
}

func nullInt(v *int64) any {
	if v == nil {
		return nil
	}
	return *v
}

func nullMillis(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t.UnixMilli()
}

func floatPtr(v sql.NullFloat64) *float64 {
	if !v.Valid {
		return nil
	}
	return new(v.Float64)
}

func intPtr(v sql.NullInt64) *int64 {
	if !v.Valid {
		return nil
	}
	return new(v.Int64)
}

func millisTime(v sql.NullInt64) time.Time {
	if !v.Valid {
		return time.Time{}
	}
	return time.UnixMilli(v.Int64).UTC()
}
