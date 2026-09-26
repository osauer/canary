package rpc

import "time"

// Prepared-proposal methods bind an existing preview to its exact proposal.
// They are private CLI/RPC handoffs and are not part of the read-only MCP API.
const (
	MethodTradeProposalsPrepare        = "trade.proposals.prepare"
	MethodTradeProposalsPreparedStatus = "trade.proposals.prepared_status"
)

// TradeProposalPreparation is non-authorizing identity and durable use state.
// Prepared does not imply current submit eligibility; nil Consumed is unknown.
type TradeProposalPreparation struct {
	ID               string    `json:"id"`
	Key              string    `json:"key"`
	Revision         string    `json:"revision"`
	DraftFingerprint string    `json:"draft_fingerprint"`
	ExpiresAt        time.Time `json:"expires_at"`
	State            string    `json:"state"`
	Consumed         *bool     `json:"consumed"`
}

// TradeProposalPrepareResult keeps the authorizing reference backend-only.
// PreparedRef must never be sent to a browser, logged, or passed in argv.
type TradeProposalPrepareResult struct {
	TradeProposalPreviewResult
	PreparedRef string                    `json:"prepared_ref,omitempty"`
	Preparation *TradeProposalPreparation `json:"preparation,omitempty"`
}

// TradeProposalPreparedStatusParams names one private prepared reference.
type TradeProposalPreparedStatusParams struct {
	PreparedRef string `json:"prepared_ref"`
}

// TradeProposalPreparedStatusResult is a passive local receipt lookup.
// Order preserves the existing journal's transport and broker uncertainty.
type TradeProposalPreparedStatusResult struct {
	Preparation *TradeProposalPreparation `json:"preparation,omitempty"`
	Order       *OrderStatusResult        `json:"order,omitempty"`
	Blockers    []TradingBlocker          `json:"blockers,omitempty"`
	AsOf        time.Time                 `json:"as_of"`
}
