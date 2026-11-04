package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/osauer/canary/v2/internal/daemon/corestore"
	"github.com/osauer/canary/v2/internal/discover"
	"github.com/osauer/canary/v2/internal/rpc"
	ibkrlib "github.com/osauer/canary/v2/pkg/ibkr"
)

// Fill quotes: the exact-contract quote retained at every order event and
// fill, so the half-spread the desk pays is measured instead of estimated.
// Design note: internal-docs/design/fill-quotes.md.
const (
	fillQuoteScopeKey = "fill-quotes"
	fillQuoteKind     = "fill_quote"
	fillQuoteVersion  = 1
	// fillQuotePollEvery is the reqExecutions cadence while the broker session
	// is up; the poll runs on the background pacing lane.
	fillQuotePollEvery  = time.Minute
	fillQuoteSweepEvery = time.Second
	// fillQuoteReadBudget bounds one exact-contract quote read. A quote that
	// does not arrive in time is recorded as unavailable, never retried.
	fillQuoteReadBudget = 4 * time.Second
	fillQuotePollBudget = 15 * time.Second
	fillQuoteFXBudget   = 3 * time.Second
	fillQuoteQueueSize  = 256
	// fillQuoteExecTimeSkew bounds how far a broker execution time may sit
	// after its local receipt before the receipt time is used instead.
	fillQuoteExecTimeSkew = time.Minute
	fillQuoteStorePage    = 10_000
	// fillQuoteBackfillDays is the lastNDays the first poll of each broker
	// session asks for, so fills that landed while the daemon was down are
	// retained rather than waiting for the next statement. Seven is the most
	// TWS answered on 2026-11-04 (server version 203): eight and above came
	// back empty and complete without a notice, so the request is one try per
	// session beside the unchanged current-day poll, never in its place.
	fillQuoteBackfillDays = 7
	// fillQuoteReasonBackfilled marks a backfilled fill's quote: none was read,
	// because a quote taken days after the fill measures nothing.
	fillQuoteReasonBackfilled = "backfilled"
)

// fillQuoteStored is the daemon.db observation payload: the record plus the
// account it belongs to. The account stays in daemon.db; no RPC echoes it.
type fillQuoteStored struct {
	Account string              `json:"account"`
	Record  rpc.FillQuoteRecord `json:"record"`
}

// fillQuoteFill is one execution waiting for its quote read. A backfilled
// fill is retained without a read.
type fillQuoteFill struct {
	account    string
	record     rpc.FillQuoteRecord
	backfilled bool
}

// fillQuoteFetch is the executions request the poll sends; the connector's
// FetchExecutionsFiltered in the daemon, a stub in tests.
type fillQuoteFetch func(ctx context.Context, account string, filter ibkrlib.ExecutionsFilter, timeout time.Duration) (ibkrlib.ExecutionsSnapshot, error)

// fillQuotePoll is the last reqExecutions outcome; otherClientsSeen is sticky
// for the same account, broker mode and API client once another client
// appears, and otherClientsSeenAt is the poll that first saw one.
type fillQuotePoll struct {
	scope              brokerStateScope
	clientID           int
	at                 time.Time
	complete           bool
	executions         int
	err                string
	otherClientsSeen   bool
	otherClientsSeenAt time.Time
}

// fillQuoteRead is what one exact-contract quote read returned.
type fillQuoteRead struct {
	Bid, Ask, Last   *float64
	BidSize, AskSize *int
	DataType         string
	QuoteAt          time.Time
}

// fillQuoteRuntime keeps the retained fill quotes (daemon.db observations,
// mirrored in memory for lookups), the capture queue and the poll state.
type fillQuoteRuntime struct {
	core *corestore.Store

	mu      sync.Mutex
	records map[string]fillQuoteStored
	claimed map[string]bool
	// pending retains the originally captured payload until its durable commit is confirmed.
	pending map[string]fillQuoteStored
	queue   []fillQuoteFill
	poll    fillQuotePoll
	// backfill names the broker session (account, mode, client id and
	// session epoch) whose earlier-days request has completed; a new process
	// starts blank, so a daemon restart backfills again.
	backfill fillQuoteBackfillMark

	// quote and fx replace the broker reads in tests.
	quote func(context.Context, rpc.ContractParams, time.Duration) (fillQuoteRead, error)
	fx    func(context.Context, string) (base string, rate *float64, source, reason string)
}

// bindFillQuoteRuntime loads every retained fill quote from daemon.db. Rows
// are paged by execution time; a page boundary re-reads its last millisecond
// and the exec-id map absorbs the overlap.
func bindFillQuoteRuntime(ctx context.Context, core *corestore.Store) (*fillQuoteRuntime, error) {
	if core == nil {
		return nil, fmt.Errorf("fill quote SQLite authority is unavailable")
	}
	rt := &fillQuoteRuntime{core: core, records: map[string]fillQuoteStored{}, claimed: map[string]bool{}}
	var from int64
	for {
		rows, err := core.ListObservations(ctx, corestore.ObservationQuery{ScopeKey: fillQuoteScopeKey, Kind: fillQuoteKind, FromObservedAtMS: from, Limit: fillQuoteStorePage})
		if err != nil {
			return nil, fmt.Errorf("load fill quotes: %w", err)
		}
		for _, row := range rows {
			var stored fillQuoteStored
			if err := json.Unmarshal(row.Payload, &stored); err != nil || stored.Record.ExecID == "" {
				continue
			}
			rt.records[stored.Record.ExecID] = stored
			rt.claimed[stored.Record.ExecID] = true
		}
		if len(rows) < fillQuoteStorePage {
			break
		}
		next := rows[len(rows)-1].ObservedAt.UnixMilli()
		if next <= from {
			break
		}
		from = next
	}
	return rt, nil
}

// enqueue claims an execution for capture. A duplicate exec id (the poll
// re-finding a callback fill, or a repeated callback) is ignored. A full
// queue releases the claim so the next poll finds the fill again.
func (rt *fillQuoteRuntime) enqueue(f fillQuoteFill) bool {
	if rt == nil || strings.TrimSpace(f.record.ExecID) == "" {
		return false
	}
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if rt.claimed[f.record.ExecID] {
		return false
	}
	if len(rt.queue)+len(rt.pending) >= fillQuoteQueueSize {
		return false
	}
	rt.claimed[f.record.ExecID] = true
	rt.queue = append(rt.queue, f)
	return true
}

func (rt *fillQuoteRuntime) pop() (fillQuoteFill, bool) {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if len(rt.queue) == 0 {
		return fillQuoteFill{}, false
	}
	f := rt.queue[0]
	rt.queue = slices.Delete(rt.queue, 0, 1)
	return f, true
}

func (rt *fillQuoteRuntime) recordPoll(p fillQuotePoll) {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if sameBrokerScope(p.scope, rt.poll.scope) && p.clientID == rt.poll.clientID && rt.poll.otherClientsSeen {
		p.otherClientsSeen, p.otherClientsSeenAt = true, rt.poll.otherClientsSeenAt
	}
	rt.poll = p
}

// pollFor is the retained poll state when it belongs to this account, broker
// mode and client scope, and a blank poll otherwise.
func (rt *fillQuoteRuntime) pollFor(status rpc.TradingStatus) fillQuotePoll {
	poll := rt.pollStatus()
	if !sameBrokerScope(poll.scope, brokerStateScope{Account: status.Account, Mode: status.Mode}) || poll.clientID != status.ClientID {
		return fillQuotePoll{}
	}
	return poll
}

func (rt *fillQuoteRuntime) pollStatus() fillQuotePoll {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	return rt.poll
}

// append writes one record durably, then publishes it to the in-memory
// mirror. The write uses its own short context so a daemon shutdown during
// a capture still lands the record.
func (rt *fillQuoteRuntime) append(account string, rec rpc.FillQuoteRecord) error {
	if rt == nil || rt.core == nil {
		return fmt.Errorf("fill quote store is unavailable")
	}
	stored := fillQuoteStored{Account: strings.ToUpper(strings.TrimSpace(account)), Record: rec}
	rt.mu.Lock()
	if rt.pending == nil {
		rt.pending = map[string]fillQuoteStored{}
	}
	if original, ok := rt.pending[rec.ExecID]; ok {
		stored = original
	} else {
		rt.pending[rec.ExecID] = stored
	}
	rt.mu.Unlock()
	return rt.persistCaptured(stored)
}

// persistCaptured reconciles an ambiguous commit before retrying the original
// observation. A watermark failure can follow a successful SQLite commit.
// Never recapture a later quote or publish an unconfirmed observation.
func (rt *fillQuoteRuntime) persistCaptured(stored fillQuoteStored) error {
	if !rt.core.Health().Ready {
		return fmt.Errorf("fill quote store is not ready")
	}
	payload, err := json.Marshal(stored)
	if err != nil {
		return fmt.Errorf("encode fill quote: %w", err)
	}
	rec := stored.Record
	observedAt := rec.ExecTime
	if observedAt.IsZero() {
		observedAt = rec.ObservedAt
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	query := corestore.ObservationQuery{ScopeKey: fillQuoteScopeKey, Source: rec.Source, Kind: fillQuoteKind,
		FromObservedAtMS: observedAt.UnixMilli(), ToObservedAtMS: observedAt.UnixMilli(), Limit: fillQuoteStorePage}
	committed := false
	for {
		rows, err := rt.core.ListObservations(ctx, query)
		if err != nil {
			return fmt.Errorf("reconcile fill quote: %w", err)
		}
		for _, row := range rows {
			if bytes.Equal(row.Payload, payload) {
				committed = true
				break
			}
		}
		if committed || len(rows) < fillQuoteStorePage {
			break
		}
		query.AfterObservationID = rows[len(rows)-1].ID
	}
	if !committed {
		if _, err := rt.core.AppendObservation(ctx, corestore.ObservationInput{
			ScopeKey: fillQuoteScopeKey, Source: rec.Source, Kind: fillQuoteKind, ObservedAt: observedAt.UTC(),
			ContentType: "application/json", Payload: payload,
		}); err != nil {
			return fmt.Errorf("append fill quote: %w", err)
		}
	}
	rt.mu.Lock()
	rt.records[rec.ExecID] = stored
	rt.claimed[rec.ExecID] = true
	delete(rt.pending, rec.ExecID)
	rt.mu.Unlock()
	return nil
}

func (rt *fillQuoteRuntime) retryCaptured() {
	rt.mu.Lock()
	pending := make([]fillQuoteStored, 0, len(rt.pending))
	for _, stored := range rt.pending {
		pending = append(pending, stored)
	}
	rt.mu.Unlock()
	for _, stored := range pending {
		if err := rt.persistCaptured(stored); err != nil {
			return
		}
	}
}

// list returns the retained records of one account and mode, ascending by
// execution time.
func (rt *fillQuoteRuntime) list(account, mode string) []rpc.FillQuoteRecord {
	if rt == nil || !brokerScopeAccountConcrete(account) {
		return nil
	}
	account = strings.ToUpper(strings.TrimSpace(account))
	mode = strings.ToLower(strings.TrimSpace(mode))
	rt.mu.Lock()
	out := make([]rpc.FillQuoteRecord, 0, len(rt.records))
	for _, stored := range rt.records {
		if account != "" && stored.Account != account {
			continue
		}
		record := stored.Record
		// All older rows used order-entry authority, including contradictory
		// paper/live settings. The concrete stored account owns its identity.
		record.Mode = accountModeForStatus(0, stored.Account)
		if mode != "" && !strings.EqualFold(record.Mode, mode) {
			continue
		}
		out = append(out, record)
	}
	rt.mu.Unlock()
	sort.Slice(out, func(i, j int) bool {
		if !out[i].ExecTime.Equal(out[j].ExecTime) {
			return out[i].ExecTime.Before(out[j].ExecTime)
		}
		return out[i].ExecID < out[j].ExecID
	})
	return out
}

// fillsBetween returns the retained executions of one account with an
// execution time in (from, to], ascending, and whether they are all of
// them: the last poll of that account and mode completed without error, it
// started after to, and no execution it found still waits for its capture.
// The mandate's flow check reads it (mandateTradesExplain).
func (rt *fillQuoteRuntime) fillsBetween(scope brokerStateScope, from, to time.Time) ([]rpc.FillQuoteRecord, bool) {
	if rt == nil || !brokerScopeAccountConcrete(scope.Account) {
		return nil, false
	}
	account := strings.ToUpper(strings.TrimSpace(scope.Account))
	rt.mu.Lock()
	defer rt.mu.Unlock()
	p := rt.poll
	if !sameBrokerScope(p.scope, scope) || !p.complete || p.err != "" || !p.at.After(to) || len(rt.queue) > 0 || len(rt.pending) > 0 {
		return nil, false
	}
	var out []rpc.FillQuoteRecord
	for _, stored := range rt.records {
		at := stored.Record.ExecTime
		if at.IsZero() {
			at = stored.Record.ObservedAt
		}
		if stored.Account != account || !at.After(from) || at.After(to) {
			continue
		}
		out = append(out, stored.Record)
	}
	slices.SortFunc(out, func(a, b rpc.FillQuoteRecord) int {
		if c := a.ExecTime.Compare(b.ExecTime); c != 0 {
			return c
		}
		return strings.Compare(a.ExecID, b.ExecID)
	})
	return out, true
}

// FillQuoteByExecID returns the retained quote for one execution. It is the
// lookup the scorecard can use in place of its no_fill_quote reason.
func (s *Server) FillQuoteByExecID(execID string) (rpc.FillQuoteRecord, bool) {
	if s == nil || s.fillQuotes == nil {
		return rpc.FillQuoteRecord{}, false
	}
	s.fillQuotes.mu.Lock()
	defer s.fillQuotes.mu.Unlock()
	stored, ok := s.fillQuotes.records[strings.TrimSpace(execID)]
	return stored.Record, ok
}

// FillQuoteNear returns the retained quote of the fill on conID whose
// execution time is closest to at, within tolerance, skipping the execution
// ids in exclude (records another match already claimed).
func (s *Server) FillQuoteNear(conID int, at time.Time, tolerance time.Duration, exclude map[string]bool) (rpc.FillQuoteRecord, bool) {
	if s == nil || s.fillQuotes == nil || conID <= 0 || at.IsZero() || tolerance < 0 {
		return rpc.FillQuoteRecord{}, false
	}
	s.fillQuotes.mu.Lock()
	defer s.fillQuotes.mu.Unlock()
	var best rpc.FillQuoteRecord
	found := false
	for _, stored := range s.fillQuotes.records {
		rec := stored.Record
		if rec.Contract.ConID != conID || rec.ExecTime.IsZero() || exclude[rec.ExecID] {
			continue
		}
		gap := rec.ExecTime.Sub(at).Abs()
		if gap > tolerance {
			continue
		}
		if !found || gap < best.ExecTime.Sub(at).Abs() {
			best, found = rec, true
		}
	}
	return best, found
}

// fillQuoteMid is the mid of a two-sided, non-crossed quote; nil otherwise.
func fillQuoteMid(bid, ask *float64) *float64 {
	if bid == nil || ask == nil || *bid <= 0 || *ask < *bid || math.IsNaN(*bid) || math.IsNaN(*ask) || math.IsInf(*bid, 0) || math.IsInf(*ask, 0) {
		return nil
	}
	return new((*bid + *ask) / 2)
}

func fillQuoteGap(quoteAt, eventAt time.Time) *int64 {
	if quoteAt.IsZero() || eventAt.IsZero() {
		return nil
	}
	return new(quoteAt.Sub(eventAt).Milliseconds())
}

// fillQuoteAtEvent describes the quote an order event was priced on: the
// preview quote at the previewed and send-attempted events, with the gap
// between the quote's own time and the event.
func fillQuoteAtEvent(q rpc.OrderQuoteSnapshot, eventAt time.Time) *rpc.FillQuote {
	if q.AsOf.IsZero() && q.Bid == nil && q.Ask == nil && q.Last == nil {
		return nil
	}
	quoteAt := q.PriceAt
	if quoteAt.IsZero() {
		quoteAt = q.AsOf
	}
	out := &rpc.FillQuote{Bid: q.Bid, Ask: q.Ask, Last: q.Last, BidSize: q.BidSize, AskSize: q.AskSize, DataType: q.DataType, QuoteAt: quoteAt.UTC(), ReadAt: q.AsOf.UTC()}
	out.Mid = fillQuoteMid(q.Bid, q.Ask)
	out.GapMS = fillQuoteGap(quoteAt, eventAt)
	if out.Mid != nil {
		out.Status = rpc.FillQuoteQuoted
	} else {
		out.Status, out.Reason = rpc.FillQuoteUnavailable, fillQuoteSidesReason(q.Bid, q.Ask)
	}
	return out
}

func fillQuoteSidesReason(bid, ask *float64) string {
	switch {
	case bid == nil && ask == nil:
		return "quote_unavailable"
	case bid == nil || ask == nil:
		return "quote_not_two_sided"
	default:
		return "quote_crossed"
	}
}

// offerFillQuoteCapture is called after a lifecycle event is durably
// journaled. It never blocks and never fails the journal path: a full queue
// leaves the fill to the next poll. A fill whose journal row Canary owns
// (order reference or preview token) is a Canary fill; any other execution
// reaching this client's callback was placed elsewhere.
func (s *Server) offerFillQuoteCapture(ev orderJournalEvent) {
	if s == nil || s.fillQuotes == nil || !fillQuoteEligibleJournalEvent(ev) {
		return
	}
	source := rpc.FillQuoteSourceBroker
	if strings.TrimSpace(ev.OrderRef) != "" || strings.TrimSpace(ev.PreviewTokenID) != "" {
		source = rpc.FillQuoteSourceCanary
	}
	s.fillQuotes.enqueue(fillQuoteFillFromJournal(ev, source))
}

func fillQuoteEligibleJournalEvent(ev orderJournalEvent) bool {
	return ev.Type == orderJournalEventStatusUpdated && strings.TrimSpace(ev.ExecID) != "" && ev.ExecShares > 0
}

func fillQuoteFillFromJournal(ev orderJournalEvent, source string) fillQuoteFill {
	execAt, execSource := fillQuoteExecTime(ev.ExecTime, ev.At.UTC())
	rec := rpc.FillQuoteRecord{
		Version: fillQuoteVersion, ExecID: strings.TrimSpace(ev.ExecID), Source: source, Seen: rpc.FillQuoteSeenCallback,
		Mode: accountModeForStatus(0, ev.Account), OrderRef: strings.TrimSpace(ev.OrderRef), PermID: ev.PermID,
		Contract: fillQuoteContract(ev.ConID, ev.Symbol, ev.SecType, ev.Currency, ev.LocalSymbol, ev.TradingClass, ev.Expiry, ev.Strike, ev.Right, ev.Multiplier),
		Venue:    strings.TrimSpace(ev.Exchange), Side: fillQuoteSide(ev.ExecSide, ev.Action), Quantity: ev.ExecShares, Price: ev.LastFillPrice,
		ExecTime: execAt, ExecTimeSource: execSource, ObservedAt: ev.At.UTC(),
	}
	if ev.clientIDPresent {
		rec.ClientID = new(ev.ClientID)
	}
	return fillQuoteFill{account: ev.Account, record: rec}
}

// fillQuoteFillFromExecution shapes one reqExecutions row. An execution placed
// by this daemon's client id is a Canary fill; a legacy row without a client
// id is Canary's only when it carries an order reference (TWS hand orders
// carry none).
func fillQuoteFillFromExecution(ev ibkrlib.OrderLifecycleEvent, status rpc.TradingStatus, now time.Time) fillQuoteFill {
	source := rpc.FillQuoteSourceBroker
	if (ev.ClientIDPresent && ev.ClientID == status.ClientID) || (!ev.ClientIDPresent && strings.TrimSpace(ev.OrderRef) != "") {
		source = rpc.FillQuoteSourceCanary
	}
	account := strings.TrimSpace(ev.Account)
	if account == "" {
		account = status.Account
	}
	execAt, execSource := fillQuoteExecTime(ev.ExecTime, now)
	rec := rpc.FillQuoteRecord{
		Version: fillQuoteVersion, ExecID: strings.TrimSpace(ev.ExecID), Source: source, Seen: rpc.FillQuoteSeenPoll,
		Mode: accountModeForStatus(status.GatewayPort, account), OrderRef: strings.TrimSpace(ev.OrderRef), PermID: ev.PermID,
		Contract: fillQuoteContract(ev.ConID, ev.Symbol, ev.SecType, ev.Currency, ev.LocalSymbol, ev.TradingClass, ev.Expiry, ev.Strike, ev.Right, ev.Multiplier),
		Venue:    strings.TrimSpace(ev.Exchange), Side: fillQuoteSide(ev.ExecutionSide, ""), Quantity: ev.Shares, Price: ev.Price,
		ExecTime: execAt, ExecTimeSource: execSource, ObservedAt: now.UTC(),
	}
	if ev.ClientIDPresent {
		rec.ClientID = new(ev.ClientID)
	}

	return fillQuoteFill{account: account, record: rec}
}

// fillQuoteContract is the contract the quote is read for. The execution's
// exchange is the venue that filled, not a routing destination, so the quote
// routes through SMART (IDEALPRO for currency pairs) by contract id.
func fillQuoteContract(conID int, symbol, secType, currency, localSymbol, tradingClass, expiry string, strike float64, right string, multiplier int) rpc.ContractParams {
	secType = strings.ToUpper(strings.TrimSpace(secType))
	exchange := "SMART"
	if secType == "CASH" {
		exchange = "IDEALPRO"
	}
	out := rpc.ContractParams{
		ConID: conID, Symbol: strings.ToUpper(strings.TrimSpace(symbol)), SecType: secType, Exchange: exchange,
		Currency: normCcy(currency), LocalSymbol: strings.TrimSpace(localSymbol), TradingClass: strings.TrimSpace(tradingClass),
		Expiry: strings.TrimSpace(expiry), Strike: strike, Right: strings.ToUpper(strings.TrimSpace(right)), Multiplier: multiplier,
	}
	if secType == "STK" {
		out.Multiplier = 1
	}
	return out
}

// fillQuoteSide normalises the broker's execution side (BOT/SLD) or, failing
// that, the order action, to BUY/SELL; empty when neither is known.
func fillQuoteSide(executionSide, action string) string {
	switch strings.ToUpper(strings.TrimSpace(executionSide)) {
	case "BOT", "BUY":
		return rpc.OrderActionBuy
	case "SLD", "SELL":
		return rpc.OrderActionSell
	}
	switch strings.ToUpper(strings.TrimSpace(action)) {
	case rpc.OrderActionBuy:
		return rpc.OrderActionBuy
	case rpc.OrderActionSell:
		return rpc.OrderActionSell
	}
	return ""
}

// fillQuoteExecTime prefers the broker's execution time when it parses and
// does not sit implausibly after the local receipt; otherwise the receipt.
func fillQuoteExecTime(raw string, receipt time.Time) (time.Time, string) {
	if at, ok := parseIBKRExecTime(raw); ok && (receipt.IsZero() || !at.After(receipt.Add(fillQuoteExecTimeSkew))) {
		return at.UTC(), "broker_exec_time"
	}
	return receipt.UTC(), "receipt"
}

// runFillQuoteLoop captures queued fills every second and polls the account's
// executions every minute while the broker session is up.
func (s *Server) runFillQuoteLoop(ctx context.Context) {
	if s == nil || s.fillQuotes == nil {
		return
	}
	sweep := time.NewTicker(fillQuoteSweepEvery)
	defer sweep.Stop()
	poll := time.NewTicker(fillQuotePollEvery)
	defer poll.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-sweep.C:
			s.captureQueuedFillQuotes(ctx)
		case <-poll.C:
			s.pollAccountExecutions(ctx)
			s.captureQueuedFillQuotes(ctx)
		}
	}
}

func (s *Server) captureQueuedFillQuotes(ctx context.Context) {
	s.fillQuotes.retryCaptured()
	for ctx.Err() == nil {
		f, ok := s.fillQuotes.pop()
		if !ok {
			return
		}
		s.captureFillQuote(ctx, f)
	}
}

// fillQuoteAccountStatus separates broker account identity from order-entry
// authority. Auto-discovered accounts use the same scope rule as broker state.
func (s *Server) fillQuoteAccountStatus(ep discover.Endpoint) rpc.TradingStatus {
	status := s.tradingStatus(ep)
	scope := brokerStateScopeFromSnapshot(status.Account, ep.Account, status.GatewayPort, s.connectedGatewayAccount())
	status.Account, status.Mode = scope.Account, scope.Mode
	return status
}

// pollAccountExecutions asks the broker for the day's executions and queues
// every one not yet retained. Which clients' executions come back is TWS's
// decision; the report says what has been proven.
func (s *Server) pollAccountExecutions(ctx context.Context) {
	rt := s.fillQuotes
	if rt == nil {
		return
	}
	now := s.orderNow()
	s.mu.Lock()
	ep := s.endpoint
	s.mu.Unlock()
	status := s.fillQuoteAccountStatus(ep)
	outcome := fillQuotePoll{at: now, scope: brokerStateScope{Account: status.Account, Mode: status.Mode}, clientID: status.ClientID}
	if !brokerScopeAccountConcrete(status.Account) {
		outcome.err = "account_unknown"
		rt.recordPoll(outcome)
		return
	}
	authority, err := s.setupMarkoutBrokerAuthority()
	if err != nil {
		outcome.err = "broker_unavailable"
		rt.recordPoll(outcome)
		return
	}
	s.pollExecutionsFrom(ctx, status, authority.session.Epoch(), authority.connector.FetchExecutionsFiltered, outcome)
}

// fillQuoteBackfillMark is the broker session whose backfill completed.
type fillQuoteBackfillMark struct {
	scope    brokerStateScope
	clientID int
	epoch    uint64
	done     bool
}

// backfillDue reports whether this broker session still owes its
// earlier-days request.
func (rt *fillQuoteRuntime) backfillDue(scope brokerStateScope, clientID int, epoch uint64) bool {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	m := rt.backfill
	return !(m.done && sameBrokerScope(m.scope, scope) && m.clientID == clientID && m.epoch == epoch)
}

func (rt *fillQuoteRuntime) markBackfilled(scope brokerStateScope, clientID int, epoch uint64) {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	rt.backfill = fillQuoteBackfillMark{scope: scope, clientID: clientID, epoch: epoch, done: true}
}

// pollExecutionsFrom sends the current-day request that every poll sends,
// then, once per broker session, the earlier-days request that finds fills
// the daemon missed while it was down. The current-day request's outcome is
// the poll's; the backfill only adds fills and other-client evidence. A
// backfill that did not complete is owed again next poll; an empty complete
// answer (a server that returns nothing for the field) settles it.
func (s *Server) pollExecutionsFrom(ctx context.Context, status rpc.TradingStatus, epoch uint64, fetch fillQuoteFetch, outcome fillQuotePoll) {
	rt := s.fillQuotes
	now := outcome.at
	pollCtx, cancel := context.WithTimeout(ibkrlib.WithRequestPriority(ctx, ibkrlib.PriorityBackground), fillQuotePollBudget)
	snap, err := fetch(pollCtx, status.Account, ibkrlib.ExecutionsFilter{}, fillQuotePollBudget)
	cancel()
	outcome.complete, outcome.executions = snap.Complete, len(snap.Executions)
	switch {
	case err == nil:
	case errors.Is(err, context.DeadlineExceeded):
		outcome.err = "timeout"
	case errors.Is(err, ibkrlib.ErrIBKRUnavailable):
		outcome.err = "broker_unavailable"
	default:
		outcome.err = "request_failed"
		s.debugf("fill quotes: executions poll: %v", err)
	}
	for _, ev := range snap.Executions {
		if ev.Account != "" && !strings.EqualFold(strings.TrimSpace(ev.Account), strings.TrimSpace(status.Account)) {
			continue
		}
		if ev.ClientIDPresent && ev.ClientID != status.ClientID {
			outcome.otherClientsSeen, outcome.otherClientsSeenAt = true, now
		}
		rt.enqueue(fillQuoteFillFromExecution(ev, status, now))
	}

	if err == nil && rt.backfillDue(outcome.scope, status.ClientID, epoch) {
		backCtx, cancelBack := context.WithTimeout(ibkrlib.WithRequestPriority(ctx, ibkrlib.PriorityBackground), fillQuotePollBudget)
		back, backErr := fetch(backCtx, status.Account, ibkrlib.ExecutionsFilter{LastNDays: fillQuoteBackfillDays}, fillQuotePollBudget)
		cancelBack()
		added := 0
		for _, ev := range back.Executions {
			if ev.Account != "" && !strings.EqualFold(strings.TrimSpace(ev.Account), strings.TrimSpace(status.Account)) {
				continue
			}
			if ev.ClientIDPresent && ev.ClientID != status.ClientID {
				outcome.otherClientsSeen, outcome.otherClientsSeenAt = true, now
			}
			f := fillQuoteFillFromExecution(ev, status, now)
			f.record.Seen, f.backfilled = rpc.FillQuoteSeenBackfill, true
			if rt.enqueue(f) {
				added++
			}
		}
		switch {
		case backErr == nil && back.Complete:
			rt.markBackfilled(outcome.scope, status.ClientID, epoch)
			s.infof("fill quotes: backfill of the last %d days found %d executions, %d not yet retained (broker session %d)", fillQuoteBackfillDays, len(back.Executions), added, epoch)
		default:
			s.debugf("fill quotes: backfill incomplete (complete=%v err=%v); owed again next poll", back.Complete, backErr)
		}
	}
	rt.recordPoll(outcome)
}

// captureFillQuote reads the exact contract once, converts to base at this
// moment's FX, and retains the record whatever the read returned. A
// backfilled fill is retained with no read: its quote is unavailable with
// the reason backfilled, so it counts in the record and the visibility
// proof but never in a cost.
func (s *Server) captureFillQuote(ctx context.Context, f fillQuoteFill) {
	rec := f.record
	started := s.orderNow()
	if f.backfilled {
		rec.Quote = rpc.FillQuote{Status: rpc.FillQuoteUnavailable, Reason: fillQuoteReasonBackfilled, ReadStartedAt: started.UTC(), ReadAt: started.UTC()}
		if err := s.fillQuotes.append(f.account, rec); err != nil {
			s.warnf("fill quote %s: %v", rec.ExecID, err)
		}
		return
	}
	read, err := s.readFillQuote(ctx, rec.Contract)
	rec.Quote = fillQuoteFromRead(read, err, rec.ExecTime, started, s.orderNow())
	rec.BaseCurrency, rec.FXRate, rec.FXSource, rec.FXReason = s.fillQuoteFX(ctx, rec.Contract.Currency)
	if err := s.fillQuotes.append(f.account, rec); err != nil {
		s.warnf("fill quote %s: %v", rec.ExecID, err)
	}
}

func (s *Server) readFillQuote(ctx context.Context, contract rpc.ContractParams) (fillQuoteRead, error) {
	if rt := s.fillQuotes; rt != nil && rt.quote != nil {
		return rt.quote(ctx, contract, fillQuoteReadBudget)
	}
	if contract.ConID <= 0 {
		return fillQuoteRead{}, errFillQuoteContractUnknown
	}
	authority, err := s.setupMarkoutBrokerAuthority()
	if err != nil {
		return fillQuoteRead{}, ibkrlib.ErrIBKRUnavailable
	}
	readCtx, cancel := context.WithTimeout(ibkrlib.WithRequestPriority(ctx, ibkrlib.PriorityBackground), fillQuoteReadBudget)
	defer cancel()
	started := time.Now()
	snap, sizes, err := s.exactSessionContractQuote(readCtx, authority, contract, fillQuoteReadBudget, false, true)
	if err != nil {
		switch {
		case !s.orderPreviewBrokerAuthorityCurrent(authority):
			return fillQuoteRead{}, ibkrlib.ErrIBKRUnavailable
		case readCtx.Err() != nil || time.Since(started) >= fillQuoteReadBudget:
			// The exact read wraps its deadline as text; the budget's own
			// clock says whether the quote simply did not arrive in time.
			return fillQuoteRead{}, context.DeadlineExceeded
		}
		return fillQuoteRead{}, err
	}
	quoteAt := snap.PriceAt
	if quoteAt.IsZero() {
		quoteAt = snap.AsOf
	}
	return fillQuoteRead{Bid: snap.Bid, Ask: snap.Ask, Last: snap.Last, BidSize: sizes.Bid, AskSize: sizes.Ask, DataType: snap.DataType, QuoteAt: quoteAt}, nil
}

var errFillQuoteContractUnknown = errors.New("fill quote: contract id unknown")

// fillQuoteFromRead turns one read into the retained quote. A failed read is
// unavailable with its reason; a one-sided read keeps the side it has.
func fillQuoteFromRead(read fillQuoteRead, err error, eventAt, started, finished time.Time) rpc.FillQuote {
	out := rpc.FillQuote{ReadStartedAt: started.UTC(), ReadAt: finished.UTC()}
	if err != nil {
		out.Status = rpc.FillQuoteUnavailable
		switch {
		case errors.Is(err, errFillQuoteContractUnknown):
			out.Reason = "contract_unknown"
		case errors.Is(err, ibkrlib.ErrIBKRUnavailable):
			out.Reason = "broker_unavailable"
		case errors.Is(err, context.DeadlineExceeded):
			out.Reason = "timeout"
		default:
			out.Reason = "quote_read_failed"
		}
		return out
	}
	out.Bid, out.Ask, out.Last, out.BidSize, out.AskSize, out.DataType = read.Bid, read.Ask, read.Last, read.BidSize, read.AskSize, read.DataType
	out.QuoteAt = read.QuoteAt.UTC()
	out.GapMS = fillQuoteGap(read.QuoteAt, eventAt)
	out.Mid = fillQuoteMid(read.Bid, read.Ask)
	if out.Mid != nil {
		out.Status = rpc.FillQuoteQuoted
		return out
	}
	out.Status, out.Reason = rpc.FillQuoteUnavailable, fillQuoteSidesReason(read.Bid, read.Ask)
	return out
}

// fillQuoteFX returns the base currency and the base-per-contract-currency
// rate at this moment: a fresh cached rate, else a live read, else the last
// known rate within the cache lifetime.
func (s *Server) fillQuoteFX(ctx context.Context, currency string) (base string, rate *float64, source, reason string) {
	if rt := s.fillQuotes; rt != nil && rt.fx != nil {
		return rt.fx(ctx, currency)
	}
	ccy := normCcy(currency)
	if ccy == "" {
		return "", nil, "", "currency_unknown"
	}
	s.mu.Lock()
	c := s.connector
	s.mu.Unlock()
	if c != nil {
		if summary := c.CachedAccountSummary(); summary != nil && summary.BaseCurrencyProvenance.Proven() {
			base = normCcy(summary.BaseCurrency)
		}
	}
	if base == "" {
		return "", nil, "", "base_currency_unknown"
	}
	if base == ccy {
		return base, new(1.0), "same_currency", ""
	}
	if s.fxRates != nil {
		if r, _, ok := s.fxRates.get(base, ccy, fxCacheFreshWindow); ok {
			return base, &r, "cache", ""
		}
	}
	if c != nil && c.IsReady() {
		fxCtx, cancel := context.WithTimeout(ibkrlib.WithRequestPriority(ctx, ibkrlib.PriorityBackground), fillQuoteFXBudget)
		defer cancel()
		if r, ok := resolveBasePerCurrencyFXRate(fxCtx, c, base, ccy, fillQuoteFXBudget); ok && r > 0 {
			if s.fxRates != nil {
				s.fxRates.put(base, ccy, r)
			}
			return base, &r, "live", ""
		}
	}
	if s.fxRates != nil {
		if r, _, ok := s.fxRates.get(base, ccy, fxCacheTTL); ok {
			return base, &r, "cache_stale", ""
		}
	}
	return base, nil, "", "fx_unavailable"
}

// handleReportingCosts serves reporting.costs from the retained fill quotes
// of the current account and mode. It reads daemon.db's mirror only and
// changes nothing.
func (s *Server) handleReportingCosts(_ context.Context, req *rpc.Request) (*rpc.ReportingCostsResult, error) {
	var p rpc.ReportingCostsParams
	if err := decodeParams(req.Params, &p); err != nil {
		return nil, err
	}
	p.Month = strings.TrimSpace(p.Month)
	if p.Month != "" && !rpc.ValidCostsMonth(p.Month) {
		return nil, errBadRequest("month must be YYYY-MM")
	}
	now := s.orderNow()
	s.mu.Lock()
	ep := s.endpoint
	s.mu.Unlock()
	status := s.fillQuoteAccountStatus(ep)
	if !brokerScopeAccountConcrete(status.Account) {
		return nil, errBadRequest("execution costs unavailable: select one concrete broker account")
	}
	var records []rpc.FillQuoteRecord
	var poll fillQuotePoll
	polling := false
	if s.fillQuotes != nil {
		records = s.fillQuotes.list(status.Account, status.Mode)
		poll = s.fillQuotes.pollFor(status)
		polling = true
	}
	result := buildReportingCosts(records, p.Month, status.Mode, status.ClientID, poll, polling, now)
	if err := rpc.ValidateReportingCostsResult(result); err != nil {
		return nil, err
	}
	return &result, nil
}

// buildReportingCosts folds retained records into the report: per fill the
// half-spread against the quote's mid, totals by month, source and class,
// and the coverage of usable quotes.
func buildReportingCosts(records []rpc.FillQuoteRecord, month, mode string, clientID int, poll fillQuotePoll, polling bool, now time.Time) rpc.ReportingCostsResult {
	result := rpc.ReportingCostsResult{
		SchemaVersion: rpc.ReportingCostsSchemaVersion, AsOf: now.UTC(), Month: month, Mode: strings.ToLower(strings.TrimSpace(mode)),
		ByMonth: []rpc.CostTotal{}, BySource: []rpc.CostTotal{}, ByClass: []rpc.CostTotal{}, Fills: []rpc.CostFill{},
	}
	mixedBase := false
	byMonth, bySource, byClass := map[string]*rpc.CostTotal{}, map[string]*rpc.CostTotal{}, map[string]*rpc.CostTotal{}
	for _, rec := range records {
		key := rec.ExecTime.UTC().Format("2006-01")
		if rec.ExecTime.IsZero() {
			key = rec.ObservedAt.UTC().Format("2006-01")
		}
		if month != "" && key != month {
			continue
		}
		if base := strings.ToUpper(strings.TrimSpace(rec.BaseCurrency)); base != "" {
			if result.BaseCurrency != "" && result.BaseCurrency != base {
				mixedBase = true
			}
			result.BaseCurrency = base
		}
		fill := costFillFromRecord(rec)
		result.Fills = append(result.Fills, fill)
		result.Coverage.Fills++
		if fill.Quote.Status == rpc.FillQuoteQuoted {
			result.Coverage.Quoted++
		} else {
			result.Coverage.Unknown++
		}
		for _, bucket := range []struct {
			into map[string]*rpc.CostTotal
			key  string
		}{{byMonth, key}, {bySource, fill.Source}, {byClass, fill.Class}} {
			total := bucket.into[bucket.key]
			if total == nil {
				total = &rpc.CostTotal{Key: bucket.key}
				bucket.into[bucket.key] = total
			}
			total.Fills++
			if fill.Status != rpc.CostFillMeasured {
				total.Unknown++
				continue
			}
			total.Measured++
			if fill.HalfSpreadBase == nil {
				total.Unconverted++
				continue
			}
			found := false
			for i := range total.ByCurrency {
				if total.ByCurrency[i].Currency == fill.BaseCurrency {
					total.ByCurrency[i].HalfSpreadBase += *fill.HalfSpreadBase
					found = true
					break
				}
			}
			if !found {
				total.ByCurrency = append(total.ByCurrency, rpc.CostCurrencyAmount{Currency: fill.BaseCurrency, HalfSpreadBase: *fill.HalfSpreadBase})
			}
		}
	}
	if mixedBase {
		result.BaseCurrency = ""
	}
	for _, groups := range []map[string]*rpc.CostTotal{byMonth, bySource, byClass} {
		for _, total := range groups {
			sort.Slice(total.ByCurrency, func(i, j int) bool { return total.ByCurrency[i].Currency < total.ByCurrency[j].Currency })
			// Old scalar fields are populated only for an unambiguous report unit.
			if result.BaseCurrency != "" && len(total.ByCurrency) == 1 {
				total.HalfSpreadBase = new(total.ByCurrency[0].HalfSpreadBase)
			}
		}
	}
	sort.SliceStable(result.Fills, func(i, j int) bool {
		if !result.Fills[i].At.Equal(result.Fills[j].At) {
			return result.Fills[i].At.Before(result.Fills[j].At)
		}
		return result.Fills[i].ExecID < result.Fills[j].ExecID
	})
	if result.Coverage.Fills > 0 {
		result.Coverage.Ratio = new(float64(result.Coverage.Quoted) / float64(result.Coverage.Fills))
	}
	result.ByMonth, result.BySource, result.ByClass = sortedCostTotals(byMonth), sortedCostTotals(bySource), sortedCostTotals(byClass)
	result.Observation = rpc.ExecutionObservation{
		Polling: polling, IntervalSeconds: int(fillQuotePollEvery / time.Second),
		LastPollAt: poll.at.UTC(), LastPollComplete: poll.complete, LastPollExecutions: poll.executions, LastPollError: poll.err,
	}
	visibility := executionVisibility(clientID, poll)
	result.Observation.Visibility, result.Observation.ProvenAt, result.Observation.Action = visibility.Visibility, visibility.ProvenAt, visibility.Action
	return result
}

// executionVisibility is the verdict the costs report and `canary status`
// share: proven by an execution from another client id, never assumed.
func executionVisibility(clientID int, poll fillQuotePoll) rpc.ExecutionVisibilityHealth {
	v := rpc.ExecutionVisibilityHealth{Visibility: rpc.ExecutionVisibilityUnproven, ClientID: clientID, Setting: rpc.ExecutionVisibilitySetting}
	if poll.otherClientsSeen {
		v.Visibility = rpc.ExecutionVisibilityAllClients
		if !poll.otherClientsSeenAt.IsZero() {
			v.ProvenAt = poll.otherClientsSeenAt.UTC()
		}
		return v
	}
	v.Action = fmt.Sprintf("No fills from TWS or other API clients have been observed for this account and API client. To include them, check that %s is Canary's client id (%d); the owner decides that setting.", rpc.ExecutionVisibilitySetting, clientID)
	return v
}

// executionVisibilityHealth is the `canary status` row, read from the same
// poll state the costs report reads; nil while the store is not bound.
func (s *Server) executionVisibilityHealth(ep discover.Endpoint) *rpc.ExecutionVisibilityHealth {
	if s == nil || s.fillQuotes == nil {
		return nil
	}
	status := s.fillQuoteAccountStatus(ep)
	v := executionVisibility(status.ClientID, s.fillQuotes.pollFor(status))
	return &v
}

func sortedCostTotals(totals map[string]*rpc.CostTotal) []rpc.CostTotal {
	out := make([]rpc.CostTotal, 0, len(totals))
	for _, total := range totals {
		out = append(out, *total)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out
}

// costFillFromRecord measures one fill: half-spread paid = (fill − mid) ×
// side sign × quantity × multiplier, positive when the fill sat on the far
// side of the mid. Anything it cannot prove leaves the fill unknown with the
// first failing condition.
func costFillFromRecord(rec rpc.FillQuoteRecord) rpc.CostFill {
	fill := rpc.CostFill{
		ExecID: rec.ExecID, Source: rec.Source, At: rec.ExecTime.UTC(), Symbol: rec.Contract.Symbol, LocalSymbol: rec.Contract.LocalSymbol,
		SecType: rec.Contract.SecType, Class: costClass(rec.Contract.SecType), Side: rec.Side, Quantity: rec.Quantity, Price: rec.Price,
		Currency: rec.Contract.Currency, BaseCurrency: strings.ToUpper(strings.TrimSpace(rec.BaseCurrency)), Quote: rec.Quote, FXRate: rec.FXRate, Status: rpc.CostFillUnknown,
	}
	if rec.ExecTime.IsZero() {
		fill.At = rec.ObservedAt.UTC()
	}
	multiplier, multiplierKnown := costMultiplier(fill.Class, rec.Contract.Multiplier)
	if multiplierKnown {
		fill.Multiplier = multiplier
	}
	sign := 0.0
	switch rec.Side {
	case rpc.OrderActionBuy:
		sign = 1
	case rpc.OrderActionSell:
		sign = -1
	}
	switch {
	case rec.Quote.Mid == nil:
		fill.Reason = rec.Quote.Reason
		if fill.Reason == "" {
			fill.Reason = "quote_unavailable"
		}
		return fill
	case rec.Quote.DataType != rpc.MarketDataLive:
		fill.Reason = "quote_not_live: " + nonEmptyString(rec.Quote.DataType, "unknown")
	case !(rec.Price > 0) || math.IsInf(rec.Price, 0):
		fill.Reason = "fill_price_unknown"
	case !(rec.Quantity > 0) || math.IsInf(rec.Quantity, 0):
		fill.Reason = "fill_quantity_unknown"
	case sign == 0:
		fill.Reason = "fill_side_unknown"
	case !multiplierKnown:
		fill.Reason = "multiplier_unknown"
	}
	if fill.Reason != "" {
		return fill
	}
	half := (rec.Price - *rec.Quote.Mid) * sign * rec.Quantity * multiplier
	fill.HalfSpread = &half
	if rec.FXRate != nil && *rec.FXRate > 0 && fill.BaseCurrency != "" {
		fill.HalfSpreadBase = new(half * *rec.FXRate)
	} else {
		fill.FXRate = nil
	}
	fill.Status = rpc.CostFillMeasured
	return fill
}

func costClass(secType string) string {
	switch strings.ToUpper(strings.TrimSpace(secType)) {
	case "STK", "ETF":
		return rpc.CostClassStock
	case "OPT", "FOP":
		return rpc.CostClassOption
	case "BOND", "BILL":
		return rpc.CostClassBond
	case "CASH":
		return rpc.CostClassFX
	case "FUT":
		return rpc.CostClassFuture
	default:
		return rpc.CostClassOther
	}
}

// costMultiplier is the money per one price point and one unit: 1 for stock
// and currency, the contract multiplier for options and futures, and 0.01
// for bonds, whose IBKR prices are per 100 of face.
func costMultiplier(class string, multiplier int) (float64, bool) {
	switch class {
	case rpc.CostClassStock, rpc.CostClassFX:
		return 1, true
	case rpc.CostClassBond:
		return 0.01, true
	case rpc.CostClassOption, rpc.CostClassFuture:
		if multiplier > 0 {
			return float64(multiplier), true
		}
		return 0, false
	default:
		return 0, false
	}
}
