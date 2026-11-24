package ibkr

import (
	"runtime"
	"strings"
	"time"
)

// evidenceHoldWarnAfter is how long a derived-state commit may hold the
// evidence write side before the log names it. Order, error and notice frames
// take the read side and account-summary frames the publication write side,
// so the reader waits out every such hold; a commit is expected to take
// milliseconds.
const evidenceHoldWarnAfter = time.Second

// noteEvidenceHold names a WithStableBrokerEvidence commit that held the
// evidence write side from acquired for evidenceHoldWarnAfter or longer, and
// how long it waited from began to take it. On 2026-11-19 the reader stalled
// for eight minutes at the US open and the log could not say what held it.
// Other evidence and publication holders (protected sends, connector
// publication, the portfolio and PnL captures) are not timed here.
func (c *Connector) noteEvidenceHold(began, acquired time.Time) {
	held := time.Since(acquired)
	if held < evidenceHoldWarnAfter {
		return
	}
	c.logWarn("a commit held broker evidence for %s after waiting %s to take it (%s); order, error and notice frames from IBKR waited behind it", held.Round(time.Millisecond), acquired.Sub(began).Round(time.Millisecond), evidenceCommitter())
}

// orderFrameWarnAfter is how long an order frame may wait for the evidence
// read side, or take under it, before the log names it. A frame is expected
// to take milliseconds; the journal append it carries is one store write.
const orderFrameWarnAfter = 250 * time.Millisecond

// rlockEvidenceForOrderFrame takes the evidence read side for one order frame
// on the reader and returns its release. A frame that waited for it, or held
// it, for orderFrameWarnAfter or longer is named with both times, so the log
// can tell a frame that queued behind a commit from one whose own handling
// (the daemon's journal append) was slow.
func (c *Connector) rlockEvidenceForOrderFrame(kind string) (release func()) {
	began := time.Now()
	c.evidenceBarrier.RLock()
	acquired := time.Now()
	return func() {
		c.evidenceBarrier.RUnlock()
		waited, handled := acquired.Sub(began), time.Since(acquired)
		if waited >= orderFrameWarnAfter || handled >= orderFrameWarnAfter {
			c.logWarn("an order frame (%s) waited %s for broker evidence and was handled in %s", kind, waited.Round(time.Millisecond), handled.Round(time.Millisecond))
		}
	}
}

// evidenceCommitter is the first caller outside the evidence wrappers.
func evidenceCommitter() string {
	pcs := make([]uintptr, 16)
	frames := runtime.CallersFrames(pcs[:runtime.Callers(3, pcs)])
	for {
		frame, more := frames.Next()
		name := frame.Function[strings.LastIndex(frame.Function, "/")+1:]
		if !strings.Contains(strings.ToLower(name), "brokerevidence") && !strings.Contains(name, "BrokerAndOrderEvidence") {
			return name
		}
		if !more {
			return "unknown caller"
		}
	}
}
