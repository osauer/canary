package daemon

import (
	"slices"

	"github.com/osauer/canary/v3/internal/rpc"
)

type ownerQueueAdmissionKey struct{}
type ownerQueueAdmission struct {
	QueueID         string
	ConID, Quantity int
	Cap             float64
}

// Only the current serialized executor can size its exact signed buy. The
// context identity travels with the preview's evidence into the first-byte
// binding; a simultaneous manual preview never inherits a global grant.
func (s *Server) ownerQueueAdmissionCurrent(scope brokerStateScope, a *ownerQueueAdmission) bool {
	if a == nil || s.tradeProposals == nil {
		return false
	}
	g := s.queuedGrant.Load()
	if g == nil || g.QueueID != a.QueueID {
		return false
	}
	r, ok := s.tradeProposals.queued.get(a.QueueID)
	return ok && r.liveIntent() && !r.CancelRequested && sameBrokerScope(r.scope(), scope) && queuedRecordProblem(r) == "" &&
		r.Terms.Version == 2 && !ownerQueueProtection(r.Terms) && r.Terms.Action == "BUY" && r.Terms.Contract.SecType == "STK" &&
		r.Terms.Contract.ConID == a.ConID && r.Terms.MaxQuantity == a.Quantity && r.Terms.WorstPrice == a.Cap &&
		g.Key == r.Terms.Key && g.Bucket == r.Terms.Bucket
}

func (s *Server) stockAddQueueRecords(scope brokerStateScope, a *ownerQueueAdmission) []queuedAuthRecord {
	if s.tradeProposals == nil {
		return nil
	}
	// An exit plan only sells a held option line: it commits no cash and
	// never holds a stock Add, though its trail lives all session.
	records := slices.DeleteFunc(s.tradeProposals.queued.list(), func(r queuedAuthRecord) bool { return queuedPlanOf(r.Terms) != nil })
	if !s.ownerQueueAdmissionCurrent(scope, a) {
		return records
	}
	var kept []queuedAuthRecord
	for _, r := range records {
		// Future intents have spent nothing. Each gets a fresh admission after
		// earlier sends, whose journal and complete broker inventory remain in
		// the cash/risk calculation. Any other attempted/uncertain queue stays.
		if sameBrokerScope(r.scope(), scope) && (r.Terms.QueueID == a.QueueID ||
			(r.State == rpc.QueuedAuthArmed || r.State == rpc.QueuedAuthHeld) && r.OrderRef == "" && r.PreviewTokenID == "") {
			continue
		}
		kept = append(kept, r)
	}
	return kept
}
