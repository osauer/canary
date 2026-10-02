package ibkr

import (
	"context"
	"errors"
	"strconv"
	"sync"
	"time"
)

// ErrAccountSummaryProbeCancelUncertain holds a competing ordinary summary
// when an uncancelled diagnostic may still occupy one broker subscription.
var ErrAccountSummaryProbeCancelUncertain = errors.New("account summary diagnostic cancellation uncertain; only one ordinary subscription available")

// AccountSettlementProbe is a value-free result of a SettledCash-only request.
// It is never parsed into an account snapshot or admitted as cash authority.
type AccountSettlementProbe struct {
	Status          string
	CancelStatus    string
	BrokerErrorCode int
	Observation     *AccountSettlementObservation
}

type summaryProbeFlight struct {
	cancel  context.CancelFunc
	done    chan struct{}
	yielded bool
}
type accountSummarySchedule struct {
	mu             sync.Mutex
	ordinary       map[int]uint64
	pending        int
	uncertainProbe bool
	probe          *summaryProbeFlight
}

// Ordinary subscriptions retain their lease until their cancellation is sent.
// End marks a snapshot cutoff, but does not end an IBKR subscription.
func (s *accountSummarySchedule) enterOrdinary(ctx context.Context, reqID int, epoch uint64) error {
	s.mu.Lock()
	s.pending++
	for s.probe != nil {
		p := s.probe
		p.yielded = true
		p.cancel()
		s.mu.Unlock()
		select {
		case <-ctx.Done():
			s.mu.Lock()
			s.pending--
			s.mu.Unlock()
			return ctx.Err()
		case <-p.done:
		}
		s.mu.Lock()
	}
	s.pending--
	if err := ctx.Err(); err != nil {
		s.mu.Unlock()
		return err
	}
	if s.uncertainProbe && len(s.ordinary) >= 2 {
		s.mu.Unlock()
		return ErrAccountSummaryProbeCancelUncertain
	}
	if _, exists := s.ordinary[reqID]; exists {
		s.mu.Unlock()
		return errors.New("account summary request already active")
	}
	if s.ordinary == nil {
		s.ordinary = make(map[int]uint64)
	}
	s.ordinary[reqID] = epoch
	s.mu.Unlock()
	return nil
}
func (s *accountSummarySchedule) leaveOrdinary(reqID int, epoch uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if got, ok := s.ordinary[reqID]; ok && got == epoch {
		delete(s.ordinary, reqID)
	}
}
func (s *accountSummarySchedule) beginProbe(cancel context.CancelFunc) *summaryProbeFlight {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.pending != 0 || len(s.ordinary) != 0 || s.probe != nil {
		return nil
	}
	p := &summaryProbeFlight{cancel: cancel, done: make(chan struct{})}
	s.probe = p
	return p
}
func (s *accountSummarySchedule) finishProbe(p *summaryProbeFlight) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	yielded := p.yielded
	if s.probe == p {
		s.probe = nil
		close(p.done)
	}
	return yielded
}
func (s *accountSummarySchedule) reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	clear(s.ordinary)
	s.uncertainProbe = false
	if s.probe != nil {
		s.probe.cancel()
	}
}

// RequestSettlementCashProbe requests exactly SettledCash on the existing
// session. It skips ordinary subscriptions, yields to new authority reads,
// and returns only sanitized callback provenance after End. The response/send
// budget is at most two seconds, followed by at most one second to cancel.
func (c *Connector) RequestSettlementCashProbe(ctx context.Context, timeout time.Duration) (out AccountSettlementProbe) {
	out.Status = "unavailable"
	out.CancelStatus = "not_started"
	if ctx == nil || ctx.Err() != nil {
		out.Status = "cancelled"
		return
	}
	origin, ok := c.CaptureSession()
	if !ok {
		return
	}
	conn := origin.connection
	conn.accountMu.RLock()
	quarantineFull := len(conn.retiredSummaryProbes) >= 256
	conn.accountMu.RUnlock()
	if quarantineFull {
		out.Status = "skipped_quarantine_limit"
		return
	}
	expected := accountSummaryExpectedAccount(conn)
	if !expected.valid() {
		out.Status = "scope_conflict"
		return
	}
	if timeout <= 0 || timeout > 2*time.Second {
		timeout = 2 * time.Second
	}
	probeCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	flight := conn.summarySchedule.beginProbe(cancel)
	if flight == nil {
		out.Status = "skipped_busy"
		return
	}
	defer func() {
		if conn.summarySchedule.finishProbe(flight) && out.Status != "session_changed" && out.Status != "scope_conflict" {
			out.Status = "yielded"
			out.Observation = nil
		}
	}()
	reqID, err := conn.nextRequestIDForForwarding()
	if err != nil {
		return
	}
	defer conn.discardRequestIDReservation(reqID)
	if err = conn.claimRequestID(reqID); err != nil {
		return
	}
	conn.accountMu.Lock()
	if conn.summarySnapshots == nil {
		conn.summarySnapshots = make(map[int]*summarySnapshot)
	}
	snap := &summarySnapshot{expectedAccount: string(expected), done: make(chan struct{}), diagnosticOnly: true}
	conn.summarySnapshots[reqID] = snap
	conn.accountMu.Unlock()
	sentPossible := false
	// Quarantine before cancellation and before releasing the scheduling lane.
	// Unknown/late tags for this ID must never seed the account cache.
	defer func() {
		conn.accountMu.Lock()
		if conn.summarySnapshots[reqID] == snap {
			delete(conn.summarySnapshots, reqID)
		}
		if conn.BrokerSessionEpoch() == origin.epoch {
			if conn.retiredSummaryProbes == nil {
				conn.retiredSummaryProbes = make(map[int]uint64)
			}
			conn.retiredSummaryProbes[reqID] = origin.epoch
		}
		conn.accountMu.Unlock()
		if !c.SessionCurrent(origin) {
			out.Status = "session_changed"
			out.Observation = nil
			out.CancelStatus = "session_changed"
			return
		}
		if !expected.equal(accountSummaryExpectedAccount(conn)) {
			out.Status = "scope_conflict"
			out.Observation = nil
		}
		if !sentPossible {
			out.CancelStatus = "not_needed"
			return
		}
		cancelCtx, cancelSend := context.WithTimeout(context.Background(), time.Second)
		defer cancelSend()
		err := conn.cancelAccountSummaryForEpoch(cancelCtx, reqID, origin.epoch, func() error {
			if !c.SessionCurrent(origin) {
				return ErrIBKRUnavailable
			}
			return nil
		})
		if err != nil {
			if !c.SessionCurrent(origin) {
				out.Status = "session_changed"
				out.CancelStatus = "session_changed"
				out.Observation = nil
				return
			}
			out.CancelStatus = "failed"
			out.Observation = nil
			// An uncertain cancellation cannot admit another diagnostic subscription.
			conn.summarySchedule.mu.Lock()
			if conn.summarySchedule.ordinary == nil {
				conn.summarySchedule.ordinary = make(map[int]uint64)
			}
			if conn.BrokerSessionEpoch() == origin.epoch {
				conn.summarySchedule.ordinary[reqID] = origin.epoch
				conn.summarySchedule.uncertainProbe = true
			}
			conn.summarySchedule.mu.Unlock()
		} else {
			out.CancelStatus = "sent"
		}
	}()
	guard := func() error {
		if !c.SessionCurrent(origin) {
			return ErrIBKRUnavailable
		}
		if !expected.equal(accountSummaryExpectedAccount(conn)) {
			return ErrAccountSummaryScopeConflict
		}
		return nil
	}
	err = conn.sendMessageWithTypeContextForEpochGuarded(probeCtx, conn.encodeMsg(reqAccountSummary, "1", reqID, "All", "SettledCash"), RequestTypeGeneral, origin.epoch, true, guard)
	sentPossible = err == nil || SendDispositionOf(err) != SendDispositionDefinitelyUnsent
	if err != nil {
		out.Status = "send_error"
		if errors.Is(err, context.DeadlineExceeded) {
			out.Status = "timeout"
		}
		if errors.Is(err, context.Canceled) {
			out.Status = "cancelled"
		}
		return
	}
	select {
	case <-snap.done:
		conn.accountMu.RLock()
		conflict, code := snap.scopeConflict, snap.brokerErrorCode
		observation := snap.settlementObservation
		observation.Rows = append([]AccountSettlementRow(nil), observation.Rows...)
		conn.accountMu.RUnlock()
		if err := guard(); err != nil {
			out.Status = "session_changed"
			if errors.Is(err, ErrAccountSummaryScopeConflict) {
				out.Status = "scope_conflict"
			}
			return
		}
		if conflict {
			out.Status = "scope_conflict"
			return
		}
		if code != 0 {
			out.Status = "broker_error"
			out.BrokerErrorCode = code
			return
		}
		observation.AsOf = time.Now().UTC()
		out.Status = "completed"
		if observation.Callbacks == 0 {
			out.Status = "completed_empty"
		}
		out.Observation = &observation
	case <-probeCtx.Done():
		out.Status = "cancelled"
		if errors.Is(probeCtx.Err(), context.DeadlineExceeded) {
			out.Status = "timeout"
		}
	}
	return
}

// consumeSummaryProbeError never logs broker prose from a diagnostic request.
func (c *Connection) consumeSummaryProbeError(reqText string, code int, epoch uint64) bool {
	reqID, err := strconv.Atoi(reqText)
	if err != nil {
		return false
	}
	c.accountMu.Lock()
	defer c.accountMu.Unlock()
	if retired, ok := c.retiredSummaryProbes[reqID]; ok && retired == epoch {
		return true
	}
	snap := c.summarySnapshots[reqID]
	if snap == nil || !snap.diagnosticOnly {
		return false
	}
	select {
	case <-snap.done:
		return true
	default:
	}
	if code > 0 && code < 2000 {
		snap.brokerErrorCode = code
		close(snap.done)
	}
	return true
}
