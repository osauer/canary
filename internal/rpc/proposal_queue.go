package rpc

import "time"

// Queued-authorisation methods carry one owner-signed reduction to the next
// regular session (owner decisions #26–#31, 2026-09-28). Prepare and arm are
// private backend handoffs like prepared proposals: the queued reference
// travels on standard input only. None of these methods is part of the
// read-only MCP API.
const (
	MethodTradeProposalsQueuePrepare = "trade.proposals.queue_prepare"
	MethodTradeProposalsQueueArm     = "trade.proposals.queue_arm"
	MethodTradeProposalsQueueCancel  = "trade.proposals.queue_cancel"
	MethodTradeProposalsQueueList    = "trade.proposals.queue_list"
	MethodTradeProposalsQueueStatus  = "trade.proposals.queue_status"
)

// Queued-authorisation states. A prepared record waits for the owner's
// signature and expires unarmed after ten minutes. Armed and held records
// wait for their send window; held names what the record waits for. Sending
// is the intent persisted before the broker call. A sent order works at the
// broker as a DAY order until it fills or the session ends. The rest are
// final.
const (
	QueuedAuthPrepared        = "prepared"
	QueuedAuthArmed           = "armed"
	QueuedAuthHeld            = "held"
	QueuedAuthSending         = "sending"
	QueuedAuthSent            = "sent"
	QueuedAuthFilled          = "filled"
	QueuedAuthPartiallyFilled = "partially_filled"
	QueuedAuthExpiredUnfilled = "expired_unfilled"
	QueuedAuthCancelled       = "cancelled"
	QueuedAuthExpired         = "expired"
	QueuedAuthFailed          = "failed"
)

// QueuedAuthStyleBoundedLimit prices the order when it is sent, from the live
// quote: a limit Concession of the way from the mid toward the bid (toward
// the ask for a buy), never beyond WorstPrice, and sent only while the spread
// is within MaxSpreadPctOfMid.
const QueuedAuthStyleBoundedLimit = "bounded_limit"

// QueuedAuthTerms is what the owner signs: at most MaxQuantity of one exact
// contract, on one side, once, between NotBefore and NotAfter of one
// session, as a bounded DAY limit, and only while the row still asks for it
// with the position and policy as they were. Canary executes only these
// stored terms. TermsDigest is the lowercase hex SHA-256 of their JSON
// encoding as Canary serves it.
type QueuedAuthTerms struct {
	Version     int    `json:"version"`
	QueueID     string `json:"queue_id"`
	AccountID   string `json:"account_id"`
	AccountMode string `json:"account_mode"`
	Key         string `json:"key"`
	Bucket      string `json:"bucket"`
	// RevisionAtQueue is the proposal revision the owner reviewed. It is
	// audit only: the send revalidates by key and row terms, so snapshot
	// revision churn at the open never cancels the record.
	RevisionAtQueue string         `json:"revision_at_queue"`
	Contract        ContractParams `json:"contract"`
	Action          string         `json:"action"`
	PositionEffect  string         `json:"position_effect"`
	// PositionQuantity is the signed position the owner saw; any change
	// before the send cancels the record.
	PositionQuantity float64 `json:"position_quantity"`
	MaxQuantity      int     `json:"max_quantity"`
	Style            string  `json:"style"`
	// Concession is the fraction of the way from the mid toward the bid
	// (sell) or the ask (buy): 0.5 is halfway.
	Concession float64 `json:"concession"`
	// WorstPrice is the lowest price a sell (the highest a buy) may be sent
	// at; it defaults to the reference mark moved 25% against the order.
	WorstPrice        float64   `json:"worst_price"`
	ReferenceMark     float64   `json:"reference_mark"`
	ReferenceMarkAt   time.Time `json:"reference_mark_at"`
	MaxSpreadPctOfMid float64   `json:"max_spread_pct_of_mid"`
	Currency          string    `json:"currency,omitempty"`
	// Market and SessionDate name the regular session the send window
	// belongs to, in the market's own calendar.
	Market      string    `json:"market"`
	SessionDate string    `json:"session_date"`
	NotBefore   time.Time `json:"not_before"`
	NotAfter    time.Time `json:"not_after"`
	TIF         string    `json:"tif"`
	ArmDeadline time.Time `json:"arm_deadline"`
	// RowTermsDigest binds the row the send must still find under Key: its
	// bucket, exact contract, side and position effect. PolicyFingerprint and
	// RulebookFingerprint are the effective (semantic) fingerprints of the
	// protection policy and the Rulebook; a change to either cancels.
	RowTermsDigest      string      `json:"row_terms_digest"`
	PolicyFingerprint   Fingerprint `json:"policy_fingerprint"`
	RulebookFingerprint Fingerprint `json:"rulebook_fingerprint,omitzero"`
}

// QueuedAuthQuote is the quote an order was priced from when it was sent.
type QueuedAuthQuote struct {
	Bid       *float64  `json:"bid,omitempty"`
	Ask       *float64  `json:"ask,omitempty"`
	SpreadPct *float64  `json:"spread_pct,omitempty"`
	DataType  string    `json:"data_type,omitempty"`
	AsOf      time.Time `json:"as_of,omitzero"`
}

// QueuedAuth is one queued authorisation as read surfaces show it. It never
// carries the queued reference or its hash.
type QueuedAuth struct {
	Terms        QueuedAuthTerms `json:"terms"`
	TermsDigest  string          `json:"terms_digest"`
	State        string          `json:"state"`
	CreatedAt    time.Time       `json:"created_at"`
	ArmedAt      time.Time       `json:"armed_at,omitzero"`
	DeskActionID string          `json:"desk_action_id,omitempty"`
	Credential   string          `json:"credential,omitempty"`
	// HeldAt, HoldCode and HoldReason say what a held record waits for.
	HeldAt     time.Time `json:"held_at,omitzero"`
	HoldCode   string    `json:"hold_code,omitempty"`
	HoldReason string    `json:"hold_reason,omitempty"`
	SendingAt  time.Time `json:"sending_at,omitzero"`
	SentAt     time.Time `json:"sent_at,omitzero"`
	// Late marks a send that happened well after NotBefore, inside the
	// window, for example after a restart.
	Late           bool             `json:"late,omitempty"`
	PreviewTokenID string           `json:"preview_token_id,omitempty"`
	OrderRef       string           `json:"order_ref,omitempty"`
	PermID         int              `json:"perm_id,omitempty"`
	QuantitySent   int              `json:"quantity_sent,omitempty"`
	LimitPrice     float64          `json:"limit_price,omitempty"`
	SendQuote      *QueuedAuthQuote `json:"send_quote,omitempty"`
	FilledQuantity float64          `json:"filled_quantity,omitempty"`
	AvgFillPrice   float64          `json:"avg_fill_price,omitempty"`
	ResolvedAt     time.Time        `json:"resolved_at,omitzero"`
	// ReasonCode and Reason say why a record was cancelled, expired or
	// failed; CancelOrigin is the request origin of an owner cancel.
	ReasonCode   string `json:"reason_code,omitempty"`
	Reason       string `json:"reason,omitempty"`
	CancelOrigin string `json:"cancel_origin,omitempty"`
	// CancelRequested marks an owner cancel that arrived while the order was
	// being placed: if the attempt proves unsent, the record ends cancelled
	// instead of waiting again.
	CancelRequested bool `json:"cancel_requested,omitempty"`
}

// TradeProposalQueued is the live queued authorisation a served proposal row
// names: armed, held or sending, or sent until its order resolves, for the
// row's exact contract and side in the snapshot's account and mode.
type TradeProposalQueued struct {
	QueueID   string    `json:"queue_id"`
	Key       string    `json:"key"`
	State     string    `json:"state"`
	NotBefore time.Time `json:"not_before"`
	NotAfter  time.Time `json:"not_after"`
}

// TradeProposalQueuePrepareParams names the row to queue. Quantity lowers
// the signed maximum below the row's quantity; zero keeps it, and a negative
// quantity is refused. Prepare reads
// no quote: the price is set only when the order is sent.
type TradeProposalQueuePrepareParams struct {
	Key      string `json:"key"`
	Revision string `json:"revision"`
	Quantity int    `json:"quantity,omitempty"`
}

// TradeProposalQueuePrepareResult keeps the authorizing reference
// backend-only. QueuedRef must never be sent to a browser, logged or passed
// in argv. A refused prepare carries Readiness and Blockers and no record.
type TradeProposalQueuePrepareResult struct {
	Accepted  bool                    `json:"accepted"`
	QueuedRef string                  `json:"queued_ref,omitempty"`
	Queue     *QueuedAuth             `json:"queue,omitempty"`
	Proposal  *TradeProposal          `json:"proposal,omitempty"`
	Readiness *TradeProposalReadiness `json:"readiness,omitempty"`
	Blockers  []TradingBlocker        `json:"blockers,omitempty"`
	AsOf      time.Time               `json:"as_of"`
}

// TradeProposalQueueArmParams arms one prepared record once the owner has
// signed its terms. TermsDigest must equal the prepared digest. Envelope is
// the owner's signature envelope, kept for audit only; DeskActionID and
// Credential name Desk's action and the credential that signed it.
type TradeProposalQueueArmParams struct {
	QueuedRef    string `json:"queued_ref"`
	TermsDigest  string `json:"terms_digest"`
	DeskActionID string `json:"desk_action_id,omitempty"`
	Credential   string `json:"credential,omitempty"`
	Envelope     string `json:"envelope,omitempty"`
	Origin       string `json:"origin,omitempty"`
}

// TradeProposalQueueCancelParams cancels one record by QueueID, or with All
// every record that has not been sent. Cancelling only withdraws authority,
// so any origin may ask; the origin is recorded.
type TradeProposalQueueCancelParams struct {
	QueueID string `json:"queue_id,omitempty"`
	All     bool   `json:"all,omitempty"`
	Reason  string `json:"reason,omitempty"`
	Origin  string `json:"origin,omitempty"`
	// PreparedOnly cancels a record only while it is still prepared: the
	// owner closing an unconfirmed review never withdraws a queue confirmed
	// meanwhile.
	PreparedOnly bool `json:"prepared_only,omitempty"`
}

// TradeProposalQueueStatusParams names one record.
type TradeProposalQueueStatusParams struct {
	QueueID string `json:"queue_id"`
}

// TradeProposalQueueListParams selects records; the default lists every
// retained record, newest first.
type TradeProposalQueueListParams struct {
	LiveOnly bool `json:"live_only,omitempty"`
}

// TradeProposalQueueResult answers arm, cancel and status. Queues lists every
// record a cancel-all cancelled; InFlight lists what it could not cancel:
// records being placed at that moment (their cancel request is kept) and
// sent orders still working at the broker.
type TradeProposalQueueResult struct {
	Accepted bool             `json:"accepted"`
	Queue    *QueuedAuth      `json:"queue,omitempty"`
	Queues   []QueuedAuth     `json:"queues,omitempty"`
	InFlight []QueuedAuth     `json:"in_flight,omitempty"`
	Message  string           `json:"message,omitempty"`
	Blockers []TradingBlocker `json:"blockers,omitempty"`
	AsOf     time.Time        `json:"as_of"`
}

// TradeProposalQueueListResult lists records without their references.
// AccountID and AccountMode name the broker session Canary serves now, empty
// while it has no concrete scope, so a reader can keep the records of that
// account and mode.
type TradeProposalQueueListResult struct {
	Queues      []QueuedAuth `json:"queues"`
	AccountID   string       `json:"account_id,omitempty"`
	AccountMode string       `json:"account_mode,omitempty"`
	AsOf        time.Time    `json:"as_of"`
}
