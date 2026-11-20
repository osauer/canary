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
// evidence write side from began for evidenceHoldWarnAfter or longer. On
// 2026-11-19 the reader stalled for eight minutes at the US open and the log
// could not say what held it. Other evidence and publication holders
// (protected sends, connector publication, the portfolio and PnL captures)
// are not timed here.
func (c *Connector) noteEvidenceHold(began time.Time) {
	held := time.Since(began)
	if held < evidenceHoldWarnAfter {
		return
	}
	c.logWarn("a commit held broker evidence for %s (%s); order, error and notice frames from IBKR waited behind it", held.Round(time.Millisecond), evidenceCommitter())
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
