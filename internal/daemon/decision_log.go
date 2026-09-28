package daemon

import (
	"encoding/json"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/osauer/canary/v2/internal/logrotate"
	"github.com/osauer/canary/v2/internal/rpc"
)

// decisionLogFile sits beside daemon.db. daemon.db stays the record of
// authority; this file is diagnostic evidence and never feeds a decision.
const decisionLogFile = "events.jsonl"

// decisionLogMaxBytes bounds the file; one previous generation is kept.
const decisionLogMaxBytes = 32 << 20

// decisionReasonRunes bounds the reason carried on one line.
const decisionReasonRunes = 500

// decisionLog appends one JSON line per proposal preview, prepare and submit
// decision, whatever the log level. Writes are best effort: a failed append
// never changes the decision it describes.
type decisionLog struct {
	mu     sync.Mutex
	path   string
	w      *logrotate.Writer
	warned bool
}

func newDecisionLog(databasePath string) *decisionLog {
	if strings.TrimSpace(databasePath) == "" {
		return nil
	}
	return &decisionLog{path: filepath.Join(filepath.Dir(databasePath), decisionLogFile)}
}

// decisionEvent is one line of events.jsonl. It never carries a preview token,
// a prepared reference or an account number.
type decisionEvent struct {
	TS      time.Time   `json:"ts"`
	Svc     string      `json:"svc"`
	Event   string      `json:"event"`
	Outcome string      `json:"outcome"`
	Code    string      `json:"code,omitempty"`
	Codes   []string    `json:"codes,omitempty"`
	Reason  string      `json:"reason,omitempty"`
	IDs     decisionIDs `json:"ids"`
	Bucket  string      `json:"bucket,omitempty"`
	Mode    string      `json:"mode,omitempty"`
	Market  string      `json:"market,omitempty"`
	Session string      `json:"session_state,omitempty"`
	OpensAt *time.Time  `json:"opens_at,omitempty"`
	Ms      int64       `json:"ms"`
}

type decisionIDs struct {
	Key         string `json:"key,omitempty"`
	Rev         string `json:"rev,omitempty"`
	Preparation string `json:"preparation,omitempty"`
	Queue       string `json:"queue,omitempty"`
	TokenID     string `json:"token_id,omitempty"`
	OrderRef    string `json:"order_ref,omitempty"`
}

// Decision outcomes: an accepted step, a refusal with blockers, and a
// refusal the RPC returned as an error. A queued authorisation's steps and
// its executor's outcomes have their own.
const (
	decisionPreviewed = "previewed"
	decisionPrepared  = "prepared"
	decisionSubmitted = "submitted"
	decisionBlocked   = "blocked"
	decisionRefused   = "refused"

	decisionQueued    = "queued"
	decisionArmed     = "armed"
	decisionCancelled = "cancelled"
	decisionHeld      = "held"
	decisionSent      = "sent"
	decisionExpired   = "expired"
	decisionFailed    = "failed"
)

func (l *decisionLog) append(ev decisionEvent) error {
	if l == nil {
		return nil
	}
	raw, err := json.Marshal(ev)
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.w == nil {
		w, err := logrotate.Open(l.path, decisionLogMaxBytes)
		if err != nil {
			return err
		}
		l.w = w
	}
	_, err = l.w.Write(raw)
	return err
}

// Close releases the file; a later append reopens it.
func (l *decisionLog) Close() error {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.w == nil {
		return nil
	}
	err := l.w.Close()
	l.w = nil
	return err
}

// proposalDecision is what one preview, prepare or submit call decided.
type proposalDecision struct {
	event       string
	prop        rpc.TradeProposal
	key, rev    string
	accepted    bool
	accept      string
	blockers    []rpc.TradingBlocker
	message     string
	err         error
	readiness   *rpc.TradeProposalReadiness
	preparation string
	tokenID     string
	orderRef    string
	mode        string
	started     time.Time
	// queue names a queued authorisation; code and note carry an executor
	// outcome's reason code and reason.
	queue string
	code  string
	note  string
}

// recordDecision appends d to the decision log. It warns once per process
// when the file cannot be written and otherwise stays quiet: the log level
// governs the daemon log, never this record.
func (e *proposalEngine) recordDecision(d proposalDecision) {
	if e == nil || e.server == nil || e.server.decisions == nil {
		return
	}
	ev := decisionEvent{TS: e.clock(), Svc: "canary", Event: "proposal." + d.event, Bucket: d.prop.Bucket, Mode: d.mode,
		IDs: decisionIDs{Key: nonEmptyString(d.prop.Key, strings.TrimSpace(d.key)), Rev: nonEmptyString(d.prop.Revision, strings.TrimSpace(d.rev)),
			Preparation: d.preparation, Queue: d.queue, TokenID: d.tokenID, OrderRef: d.orderRef}, Code: d.code, Reason: d.note}
	if !d.started.IsZero() {
		ev.Ms = max(time.Since(d.started).Milliseconds(), 0)
	}
	for _, b := range d.blockers {
		if code := strings.TrimSpace(b.Code); code != "" && !slices.Contains(ev.Codes, code) {
			ev.Codes = append(ev.Codes, code)
		}
	}
	switch {
	case d.accepted:
		ev.Outcome = d.accept
	case len(d.blockers) > 0:
		ev.Outcome, ev.Reason = decisionBlocked, d.blockers[0].Message
	default:
		ev.Outcome, ev.Reason = decisionRefused, d.message
		if d.err != nil {
			ev.Reason = d.err.Error()
		}
	}
	if r := d.readiness; r != nil {
		ev.Code, ev.Market, ev.Session, ev.OpensAt = r.Code, r.Market, r.SessionState, r.OpensAt
		for _, code := range r.CanaryCodes {
			if !slices.Contains(ev.Codes, code) {
				ev.Codes = append(ev.Codes, code)
			}
		}
		if ev.Reason == "" {
			ev.Reason = r.Message
		}
	}
	ev.Reason = boundedDecisionReason(ev.Reason)
	if err := e.server.decisions.append(ev); err != nil {
		l := e.server.decisions
		l.mu.Lock()
		first := !l.warned
		l.warned = true
		l.mu.Unlock()
		if first {
			e.server.warnf("proposal decisions: append %s: %v", decisionLogFile, err)
		}
	}
}

// finishSubmit classifies a refused submission and records the decision; it
// runs once, deferred, for every manual, prepared and automatic submission.
func (e *proposalEngine) finishSubmit(event string, p rpc.TradeProposalSubmitParams, out *rpc.TradeProposalSubmitResult, err error, started time.Time) {
	accepted := err == nil && out.Accepted && len(out.Blockers) == 0
	if !accepted {
		out.Readiness = e.refusalReadiness(out.Proposal, out.Blockers, err)
	}
	d := proposalDecision{event: event, prop: out.Proposal, key: p.Key, rev: p.Revision, accepted: accepted, accept: decisionSubmitted,
		blockers: out.Blockers, message: out.Message, err: err, readiness: out.Readiness, tokenID: out.PreviewTokenID, orderRef: out.OrderRef,
		mode: e.decisionMode(out.Preview), started: started}
	if out.Preparation != nil {
		d.preparation = out.Preparation.ID
	}
	e.recordDecision(d)
}

// decisionMode is the paper/live mode a decision ran under: the preview's,
// else the served snapshot's.
func (e *proposalEngine) decisionMode(preview *rpc.TradeProposalOrderPreview) string {
	if preview != nil && preview.Mode != "" {
		return preview.Mode
	}
	if e == nil {
		return ""
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.snapshot.AccountMode
}

// decisionAccountPattern is Desk's monitor pattern for IBKR account codes; a
// blocker message that quotes a session's account is masked before it is kept.
var decisionAccountPattern = regexp.MustCompile(`\b(?:DU|U|F)[0-9]{5,12}\b`)

func boundedDecisionReason(s string) string {
	s = decisionAccountPattern.ReplaceAllString(strings.Join(strings.Fields(s), " "), "[account]")
	if utf8.RuneCountInString(s) <= decisionReasonRunes {
		return s
	}
	return string([]rune(s)[:decisionReasonRunes-1]) + "…"
}
