package rpc

import "time"

// Answer-path lanes: the daemon's main client and the bulk-history client.
const (
	AnswerPathLanePrimary = "primary"
	AnswerPathLaneBreadth = "breadth"
)

// Answer-path states.
const (
	// AnswerPathAnswering means the Gateway answers this connection's history.
	AnswerPathAnswering = "answering"
	// AnswerPathStalled means the Gateway holds history requests unanswered on
	// the current session; the daemon redials it after ten minutes.
	AnswerPathStalled = "stalled"
	// AnswerPathNoSession means the lane has no connected session to judge.
	AnswerPathNoSession = "no_session"
)

// ConnectionAnswerPath is one broker connection's request-answer health:
// whether the Gateway answers what the connection asks. A connected socket
// alone does not establish it. Clocks are the daemon's receipt times.
type ConnectionAnswerPath struct {
	Lane     string `json:"lane"`
	ClientID int    `json:"client_id,omitempty"`
	State    string `json:"state"`
	// LastInboundAt is the newest frame of any kind on the current socket;
	// the heartbeat drops a session silent for two heartbeat intervals.
	LastInboundAt time.Time `json:"last_inbound_at,omitzero"`
	// InFlight counts history and contract-details requests awaiting an
	// answer; OldestSentAt dates the oldest of them.
	InFlight     int       `json:"in_flight"`
	OldestSentAt time.Time `json:"oldest_sent_at,omitzero"`
	// LastAnswerAt is the newest answer to a history or contract-details
	// request: data, an end marker or a coded broker refusal.
	LastAnswerAt time.Time `json:"last_answer_at,omitzero"`
	// Timeouts15m counts requests that waited out their budget unanswered
	// during the last 15 minutes.
	Timeouts15m  int `json:"timeouts_15m"`
	LimiterQueue int `json:"limiter_queue"`
	// StalledSince is set while State is stalled. RedialDue is when the
	// daemon drops the stalled session so it is redialled; RedialedAt is the
	// last time it did so.
	StalledSince time.Time `json:"stalled_since,omitzero"`
	RedialDue    time.Time `json:"redial_due,omitzero"`
	RedialedAt   time.Time `json:"redialed_at,omitzero"`
}
