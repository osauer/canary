package daemon

import (
	"cmp"
	"context"
	"encoding/json"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/osauer/canary/v2/internal/daemon/corestore"
	"github.com/osauer/canary/v2/internal/rpc"
	"github.com/osauer/canary/v2/internal/setups"
)

// setupCoverageStore is where coverage persists; *corestore.Store
// implements it and daemon tests replace it.
type setupCoverageStore interface {
	LoadSetupCoverage(context.Context, string) (corestore.StateDocument, bool, error)
	SaveSetupCoverage(context.Context, string, []byte) (corestore.StateDocument, error)
	ListSetupCoverageSessions(context.Context) ([]string, error)
}

const (
	// setupCoverageContractLimit bounds contracts recorded per session.
	setupCoverageContractLimit = 128
	// setupCoverageStateLimit bounds state keys per contract; further
	// unavailable reasons count as unavailable:other.
	setupCoverageStateLimit = 24
	// setupCoverageWriteTimeout bounds one background read or write.
	setupCoverageWriteTimeout = 5 * time.Second
	// setupCoverageDrainTimeout bounds how long shutdown waits for coverage.
	setupCoverageDrainTimeout = 2 * time.Second
)

// setupCoverageRecorder counts live setup evaluations per contract and
// session. An evaluation only updates memory; one background flusher writes
// changed sessions to daemon.db, merging a session's persisted document once
// (after a restart) before its first write, so an evaluation never waits on
// SQLite. A failed write leaves memory authoritative until the next
// evaluation of that session marks it for writing again.
type setupCoverageRecorder struct {
	mu        sync.Mutex
	sessions  map[string]*setupCoverageSession
	dirty     map[string]bool
	flushing  bool
	flushDone chan struct{}
	closed    bool
	failing   bool
	// io serializes persisted loads and writes; it is never held by an
	// evaluation.
	io sync.Mutex
	// storeForTest replaces daemon.db.
	storeForTest setupCoverageStore
}

// setupCoverageSession is one session's coverage, as persisted.
type setupCoverageSession struct {
	Version   int                 `json:"version"`
	Date      string              `json:"session_date"`
	Open      time.Time           `json:"open"`
	Close     time.Time           `json:"close"`
	Truncated bool                `json:"truncated,omitempty"`
	Contracts []*setupCoverageRow `json:"contracts"`
	// loaded reports that the persisted document has been merged in; saved
	// that memory has been written since it last changed.
	loaded, saved bool
}

// setupCoverageRow is one contract's evaluations in a session. Slots lists
// the completed-bar slots that received an evaluation, ascending.
type setupCoverageRow struct {
	Symbol          string         `json:"symbol"`
	ConID           int            `json:"con_id,omitempty"`
	Evaluations     int            `json:"evaluations"`
	HistoryRequests int            `json:"history_requests"`
	States          map[string]int `json:"states"`
	Slots           []int          `json:"slots"`
	First           time.Time      `json:"first_evaluated_at"`
	Last            time.Time      `json:"last_evaluated_at"`
}

// setupCoverageEvent is one served evaluation. Only live evaluations inside
// a regular session are recorded; replays reconstruct other clocks.
type setupCoverageEvent struct {
	live     bool
	session  setups.Session
	contract rpc.ContractParams
	at       time.Time
	requests int
	state    string
}

// setupSlot is the setup bar interval.
const setupSlot = 5 * time.Minute

// setupLastSlot is the last completed-bar slot a session evaluation can
// observe: slot k completes at open + 5k minutes, and the bar ending at the
// close completes outside the regular session.
func setupLastSlot(open, close time.Time) int {
	return max(0, int(close.Sub(open)/setupSlot)-1)
}

// setupSlotAt is the latest completed-bar slot at t, zero before the first.
func setupSlotAt(open, close, t time.Time) int {
	if t.Before(open) {
		return 0
	}
	return min(int(t.Sub(open)/setupSlot), setupLastSlot(open, close))
}

// setupCoverageState keys an evaluation by state, and an unavailable one by
// the stable code that starts its first reason.
func setupCoverageState(r *rpc.SetupResult) string {
	if r.State != "unavailable" {
		return r.State
	}
	code := ""
	if len(r.Reasons) > 0 {
		code = r.Reasons[0]
		if i := strings.IndexFunc(code, func(c rune) bool { return (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '_' }); i >= 0 {
			code = code[:i]
		}
	}
	if code == "" {
		code = "unknown"
	}
	return "unavailable:" + code
}

func (s *Server) setupCoverageStore() setupCoverageStore {
	if s.setupCoverage.storeForTest != nil {
		return s.setupCoverage.storeForTest
	}
	if s.coreStore != nil {
		return s.coreStore
	}
	return nil
}

// recordSetupCoverage counts ev in memory and, when daemon.db is attached,
// makes sure a background flusher will write it. It never blocks on I/O.
func (s *Server) recordSetupCoverage(ev setupCoverageEvent) {
	store := s.setupCoverageStore()
	if s.setupCoverage.record(ev, store != nil) {
		go s.flushSetupCoverage(store)
	}
}

// record counts ev and reports whether the caller must start the flusher.
func (c *setupCoverageRecorder) record(ev setupCoverageEvent, persistent bool) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.sessions == nil {
		c.sessions, c.dirty = map[string]*setupCoverageSession{}, map[string]bool{}
	}
	sess := c.sessions[ev.session.Date]
	if sess == nil {
		sess = &setupCoverageSession{Version: 1, Date: ev.session.Date, Open: ev.session.Open, Close: ev.session.Close, Contracts: []*setupCoverageRow{}}
		c.sessions[ev.session.Date] = sess
		// Written sessions leave memory after their write; this bound holds
		// without daemon.db or while writes fail.
		if len(c.sessions) > corestore.SetupCoverageSessions {
			oldest := slices.Min(slices.Collect(maps.Keys(c.sessions)))
			delete(c.sessions, oldest)
			delete(c.dirty, oldest)
		}
	}
	sess.saved = false
	if row := sess.row(ev.contract.Symbol, ev.contract.ConID); row != nil {
		row.Evaluations++
		row.HistoryRequests += ev.requests
		row.addState(ev.state, 1)
		if k := setupSlotAt(sess.Open, sess.Close, ev.at); k > 0 {
			row.addSlot(k)
		}
		if row.First.IsZero() || ev.at.Before(row.First) {
			row.First = ev.at
		}
		if ev.at.After(row.Last) {
			row.Last = ev.at
		}
	}
	if !persistent {
		return false
	}
	c.dirty[ev.session.Date] = true
	if c.flushing || c.closed {
		return false
	}
	c.flushing, c.flushDone = true, make(chan struct{})
	return true
}

// row returns the contract's row, adding it within the per-session bound.
func (sess *setupCoverageSession) row(symbol string, conID int) *setupCoverageRow {
	for _, row := range sess.Contracts {
		if row.Symbol == symbol && row.ConID == conID {
			return row
		}
	}
	if len(sess.Contracts) >= setupCoverageContractLimit {
		sess.Truncated = true
		return nil
	}
	row := &setupCoverageRow{Symbol: symbol, ConID: conID, States: map[string]int{}, Slots: []int{}}
	sess.Contracts = append(sess.Contracts, row)
	return row
}

func (row *setupCoverageRow) addState(key string, n int) {
	if row.States == nil {
		row.States = map[string]int{}
	}
	const other = "unavailable:other"
	if _, ok := row.States[key]; !ok && key != other && len(row.States) >= setupCoverageStateLimit-1 {
		key = other
	}
	row.States[key] += n
}

func (row *setupCoverageRow) addSlot(k int) {
	if i, found := slices.BinarySearch(row.Slots, k); !found {
		row.Slots = slices.Insert(row.Slots, i, k)
	}
}

// merge adds a persisted document's counts to this session's.
func (sess *setupCoverageSession) merge(persisted *setupCoverageSession) {
	if sess.Open.IsZero() {
		sess.Open, sess.Close = persisted.Open, persisted.Close
	}
	sess.Truncated = sess.Truncated || persisted.Truncated
	for _, p := range persisted.Contracts {
		row := sess.row(p.Symbol, p.ConID)
		if row == nil {
			continue
		}
		row.Evaluations += p.Evaluations
		row.HistoryRequests += p.HistoryRequests
		for _, k := range slices.Sorted(maps.Keys(p.States)) {
			row.addState(k, p.States[k])
		}
		for _, k := range p.Slots {
			row.addSlot(k)
		}
		if !p.First.IsZero() && (row.First.IsZero() || p.First.Before(row.First)) {
			row.First = p.First
		}
		if p.Last.After(row.Last) {
			row.Last = p.Last
		}
	}
}

func (sess *setupCoverageSession) clone() *setupCoverageSession {
	out := *sess
	out.Contracts = make([]*setupCoverageRow, len(sess.Contracts))
	for i, row := range sess.Contracts {
		r := *row
		r.States, r.Slots = maps.Clone(row.States), slices.Clone(row.Slots)
		out.Contracts[i] = &r
	}
	return &out
}

func decodeSetupCoverage(doc corestore.StateDocument, date string) *setupCoverageSession {
	var sess setupCoverageSession
	if err := json.Unmarshal(doc.JSON, &sess); err != nil || sess.Version != 1 || sess.Date != date {
		return nil
	}
	return &sess
}

// flushSetupCoverage writes changed sessions until none are left.
func (s *Server) flushSetupCoverage(store setupCoverageStore) {
	c := &s.setupCoverage
	for {
		c.mu.Lock()
		dates := slices.Sorted(maps.Keys(c.dirty))
		clear(c.dirty)
		if len(dates) == 0 {
			c.flushing = false
			close(c.flushDone)
			c.mu.Unlock()
			return
		}
		c.mu.Unlock()
		for _, date := range dates {
			s.noteSetupCoverageWrite(date, c.persist(store, date, setupCoverageWriteTimeout))
		}
	}
}

// persist writes one session, first merging its persisted document, and then
// forgets older sessions that are already written.
func (c *setupCoverageRecorder) persist(store setupCoverageStore, date string, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	c.io.Lock()
	defer c.io.Unlock()
	if err := c.loadLocked(ctx, store, date); err != nil {
		return err
	}
	c.mu.Lock()
	sess := c.sessions[date]
	if sess == nil {
		c.mu.Unlock()
		return nil
	}
	payload, err := json.Marshal(sess)
	c.mu.Unlock()
	if err != nil {
		return err
	}
	if _, err := store.SaveSetupCoverage(ctx, date, payload); err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.dirty[date] {
		sess.saved = true
	}
	newest := slices.Max(slices.Collect(maps.Keys(c.sessions)))
	for d, old := range c.sessions {
		if d != newest && old.saved && !c.dirty[d] {
			delete(c.sessions, d)
		}
	}
	return nil
}

// loadLocked merges date's persisted document into memory once. io is held.
func (c *setupCoverageRecorder) loadLocked(ctx context.Context, store setupCoverageStore, date string) error {
	c.mu.Lock()
	sess := c.sessions[date]
	loaded := sess == nil || sess.loaded
	c.mu.Unlock()
	if loaded {
		return nil
	}
	doc, found, err := store.LoadSetupCoverage(ctx, date)
	if err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	// An unreadable document is replaced rather than blocking coverage.
	if persisted := decodeSetupCoverage(doc, date); found && persisted != nil {
		sess.merge(persisted)
	}
	sess.loaded = true
	return nil
}

func (s *Server) noteSetupCoverageWrite(date string, err error) {
	c := &s.setupCoverage
	c.mu.Lock()
	was := c.failing
	c.failing = err != nil
	c.mu.Unlock()
	if s.logger == nil {
		return
	}
	switch {
	case err != nil && !was:
		s.logger.Warnf("setups coverage: persist session %s: %v", date, err)
	case err == nil && was:
		s.logger.Infof("setups coverage: persistence recovered for session %s", date)
	}
}

// drainSetupCoverage stops background coverage writes and persists what is
// left before daemon.db closes. It is bounded: coverage never holds up
// shutdown for long.
func (s *Server) drainSetupCoverage() {
	c := &s.setupCoverage
	c.mu.Lock()
	c.closed = true
	flushing, done := c.flushing, c.flushDone
	c.mu.Unlock()
	if flushing {
		select {
		case <-done:
		case <-time.After(setupCoverageDrainTimeout):
			return
		}
	}
	store := s.setupCoverageStore()
	if store == nil {
		return
	}
	c.mu.Lock()
	dates := slices.Sorted(maps.Keys(c.dirty))
	clear(c.dirty)
	c.mu.Unlock()
	for _, date := range dates {
		s.noteSetupCoverageWrite(date, c.persist(store, date, setupCoverageDrainTimeout))
	}
}

// retained lists persisted and in-memory session dates, newest first.
func (c *setupCoverageRecorder) retained(ctx context.Context, store setupCoverageStore) ([]string, error) {
	dates := []string{}
	if store != nil {
		persisted, err := store.ListSetupCoverageSessions(ctx)
		if err != nil {
			return nil, err
		}
		dates = append(dates, persisted...)
	}
	c.mu.Lock()
	for d := range c.sessions {
		dates = append(dates, d)
	}
	c.mu.Unlock()
	slices.Sort(dates)
	dates = slices.Compact(dates)
	slices.Reverse(dates)
	return dates[:min(len(dates), corestore.SetupCoverageSessions)], nil
}

// snapshot returns a copy of date's coverage, persisted counts included.
func (c *setupCoverageRecorder) snapshot(ctx context.Context, store setupCoverageStore, date string) (*setupCoverageSession, error) {
	c.io.Lock()
	defer c.io.Unlock()
	c.mu.Lock()
	inMemory := c.sessions[date] != nil
	c.mu.Unlock()
	if inMemory {
		if store != nil {
			if err := c.loadLocked(ctx, store, date); err != nil {
				return nil, err
			}
		}
		c.mu.Lock()
		defer c.mu.Unlock()
		return c.sessions[date].clone(), nil
	}
	if store == nil {
		return nil, nil
	}
	doc, found, err := store.LoadSetupCoverage(ctx, date)
	if err != nil || !found {
		return nil, err
	}
	return decodeSetupCoverage(doc, date), nil
}

// handleSetupsCoverage reads retained coverage. It needs no gateway and
// changes nothing.
func (s *Server) handleSetupsCoverage(ctx context.Context, req *rpc.Request) (*rpc.SetupCoverageResult, error) {
	var p rpc.SetupCoverageParams
	if err := decodeParams(req.Params, &p); err != nil {
		return nil, err
	}
	p, err := rpc.NormalizeSetupCoverageParams(p)
	if err != nil {
		return nil, errBadRequest(err.Error())
	}
	now := s.setupClock()
	store := s.setupCoverageStore()
	sessions, err := s.setupCoverage.retained(ctx, store)
	if err != nil {
		return nil, err
	}
	out := &rpc.SetupCoverageResult{Version: 1, AsOf: now, SessionDate: p.Session, Sessions: sessions, Contracts: []rpc.SetupCoverageContract{}}
	if out.SessionDate == "" && len(sessions) > 0 {
		out.SessionDate = sessions[0]
	}
	if out.SessionDate == "" {
		return out, nil
	}
	sess, err := s.setupCoverage.snapshot(ctx, store, out.SessionDate)
	if err != nil || sess == nil {
		return out, err
	}
	completed := setupSlotAt(sess.Open, sess.Close, now)
	out.SessionOpen, out.SessionClose, out.SlotsCompleted, out.Truncated = sess.Open, sess.Close, completed, sess.Truncated
	for _, row := range sess.Contracts {
		if p.Symbol != "" && row.Symbol != p.Symbol {
			continue
		}
		first := max(1, setupSlotAt(sess.Open, sess.Close, row.First))
		states := row.States
		if states == nil {
			states = map[string]int{}
		}
		out.Contracts = append(out.Contracts, rpc.SetupCoverageContract{
			Symbol: row.Symbol, ConID: row.ConID,
			SlotsScheduled: max(0, completed-first+1), SlotsCovered: len(row.Slots),
			Evaluations: row.Evaluations, HistoryRequests: row.HistoryRequests, States: states,
			FirstEvaluatedAt: row.First, LastEvaluatedAt: row.Last,
		})
	}
	slices.SortFunc(out.Contracts, func(a, b rpc.SetupCoverageContract) int {
		return cmp.Or(cmp.Compare(a.Symbol, b.Symbol), cmp.Compare(a.ConID, b.ConID))
	})
	return out, nil
}
