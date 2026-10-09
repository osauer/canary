package rpc

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"time"

	"github.com/osauer/canary/v2/internal/risk"
)

// MethodPortfolioPlan reads one next-action plan; it cannot reserve or trade.
const MethodPortfolioPlan = "portfolio.plan"

// ValidatePortfolioPlanParams permits no caller-supplied policy, book or orders.
func ValidatePortfolioPlanParams(raw json.RawMessage) error {
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	var empty struct{}
	if err := d.Decode(&empty); err != nil {
		return err
	}
	if d.Decode(new(any)) != io.EOF {
		return fmt.Errorf("one empty portfolio planning object is required")
	}
	return nil
}

// PortfolioPlanResult preserves desired targets separately from the one checked
// next action. All later actions require a fresh plan after the broker outcome.
type PortfolioPlanResult struct {
	Kind              string                 `json:"kind"`
	AsOf              time.Time              `json:"as_of"`
	ValidUntil        time.Time              `json:"valid_until,omitzero"`
	State             string                 `json:"state"`
	Reason            string                 `json:"reason"`
	BaseCurrency      string                 `json:"base_currency,omitempty"`
	Authority         *AccountDataAuthority  `json:"authority,omitempty"`
	PolicyFingerprint string                 `json:"policy_fingerprint,omitempty"`
	WatchlistRevision int64                  `json:"watchlist_revision"`
	ProposalRevision  string                 `json:"proposal_revision,omitempty"`
	ProposalAsOf      time.Time              `json:"proposal_as_of,omitzero"`
	Regime            string                 `json:"regime,omitempty"`
	RegimeAsOf        time.Time              `json:"regime_as_of,omitzero"`
	Unassigned        []ContractParams       `json:"unassigned"`
	Targets           []risk.PortfolioIntent `json:"targets"`
	Reductions        []TradeProposal        `json:"reductions"`
	Next              *PortfolioPlanAction   `json:"next,omitempty"`
	Blockers          []TradingBlocker       `json:"blockers"`
}

// PortfolioPlanAction is advisory, with either exact proposal provenance or Add
// sizing evidence. It contains no preview token or authorization material.
type PortfolioPlanAction struct {
	Kind             string         `json:"kind"`
	ProposalKey      string         `json:"proposal_key,omitempty"`
	ProposalRevision string         `json:"proposal_revision,omitempty"`
	Add              *AddPlanResult `json:"add,omitempty"`
}
