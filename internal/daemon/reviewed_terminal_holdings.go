package daemon

import (
	"maps"
	"time"

	"github.com/osauer/canary/v2/internal/risk"
	"github.com/osauer/canary/v2/internal/rpc"

	ibkrlib "github.com/osauer/canary/v2/pkg/ibkr"
)

// reviewedTerminalHoldingReason labels the connector's suppression of a held
// stock whose exact contract matches a current reviewed terminal record.
const reviewedTerminalHoldingReason = "reviewed terminal evidence"

// publishReviewedTerminalHoldings records held, the full set of held stocks
// whose exact ConID, symbol and STK type matched a current reviewed terminal
// record (markReviewedTerminalStocks), and hands it to c. The connector then
// stops every broker request for those stocks, including lingering
// market-data re-probes, until their review expires or a later read drops them.
func (s *Server) publishReviewedTerminalHoldings(c *ibkrlib.Connector, held map[string]ibkrlib.ReviewedTerminalStock) {
	if s == nil {
		return
	}
	s.reviewedTerminalMu.Lock()
	s.reviewedTerminalHeld = maps.Clone(held)
	s.reviewedTerminalKnown = true
	s.reviewedTerminalMu.Unlock()
	if c != nil {
		c.SetReviewedTerminal(maps.Clone(held))
	}
}

// seedReviewedTerminalHoldings gives a newly published connector the last
// computed set after checking current exact-contract authority, before its
// handshake, so a reconnect does not re-probe those
// stocks while the first positions read is still pending. Before any
// positions read has completed there is nothing to seed.
func (s *Server) seedReviewedTerminalHoldings(c *ibkrlib.Connector) {
	if s == nil || c == nil {
		return
	}
	s.reviewedTerminalMu.Lock()
	held, known := maps.Clone(s.reviewedTerminalHeld), s.reviewedTerminalKnown
	s.reviewedTerminalMu.Unlock()
	if known {
		now := time.Now()
		for symbol, entry := range held {
			if s.earningsTerminal == nil {
				delete(held, symbol)
				continue
			}
			match, found := s.earningsTerminal.terminalEarningsFor(risk.NameInput{Symbol: symbol, StockConID: entry.ConID, StockSecType: "STK"}, now)
			if !found || match.Status != rpc.EarningsStatusTerminalNonReporting || entry.EvidenceFingerprint == "" || entry.EvidenceFingerprint != match.Info.AuthorityBinding || !entry.ValidUntil.Equal(match.Info.RevalidateAfter) {
				delete(held, symbol)
			}
		}
		c.SetReviewedTerminal(held)
	}
}
