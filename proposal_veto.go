package canary

import (
	"context"

	"github.com/osauer/canary/v2/internal/rpc"
)

// Proposal veto: the owner stops one pre-authorised submission before its
// window ends. Like the Desk authority methods this is outside the MCP
// catalogue; only Desk's backend calls it, for the owner's act on the console
// or the companion, and it stamps the origin the daemon accepts for that act.
// The daemon refuses every non-human origin, so a model tool can never reach
// it. A vetoed row stays vetoed until its revision changes; a vetoed mandate
// row holds for the rest of its breach episode.

// TradeProposalVetoParams names the row (key and, optionally, revision), the
// reason and the origin of the owner's act.
type TradeProposalVetoParams = rpc.TradeProposalVetoParams

// TradeProposalVetoResult is the daemon's answer: whether it accepted, the
// row's automatic state afterwards and its words.
type TradeProposalVetoResult = rpc.TradeProposalVetoResult

// TradeProposalAutomatic is a row's pre-authorisation record, as the daemon
// serves it on every proposal and on a veto's answer.
type TradeProposalAutomatic = rpc.TradeProposalAutomatic

// OriginPairedDevice is the origin of an act the owner confirms on a device
// paired with the desk; the daemon treats it as human.
const OriginPairedDevice = rpc.OrderOriginPairedDevice

// VetoProposal stops the pending pre-authorised submission of one proposal
// row. Accepted false carries the daemon's reason in Message; a transport or
// method error is returned as an error.
func (c *Client) VetoProposal(ctx context.Context, in TradeProposalVetoParams) (*TradeProposalVetoResult, error) {
	var out TradeProposalVetoResult
	if err := c.callPrivate(ctx, rpc.MethodTradeProposalsVeto, in, &out); err != nil {
		return nil, err
	}
	return &out, nil
}
