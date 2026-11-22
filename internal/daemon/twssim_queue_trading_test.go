//go:build trading

package daemon

import (
	"slices"
	"testing"
	"time"

	"github.com/osauer/canary/v3/internal/marketcal"
	"github.com/osauer/canary/v3/internal/risk"
	"github.com/osauer/canary/v3/internal/rpc"
	"github.com/osauer/canary/v3/pkg/ibkr/twssim"
)

// queueOpenIn installs a US equity session whose send time (the open plus
// the stock opening offset) is in from now: the session opened just under
// five minutes ago, so a queue is still offered and its window starts soon.
// There is no clock seam in the simulated daemon; this is the wall-clock
// workaround.
func queueOpenIn(t *testing.T, in time.Duration) {
	t.Helper()
	t.Cleanup(marketcal.ClearSimulatedSessions)
	now := time.Now()
	marketcal.SimulateSession(marketcal.MarketUSEquity, marketcal.Window{Open: now.Add(in - readinessStockOpeningOffset), Close: now.Add(6 * time.Hour)})
}

// queueBuy prepares and arms an owner-queued stock buy as Desk does, with the
// synthetic device envelope the queue tests use.
func (d *simDesk) queueBuy(contract rpc.ContractParams, qty int, limit float64) rpc.TradeProposalQueuePrepareResult {
	d.t.Helper()
	e := d.srv.tradeProposals
	scope := e.currentScope()
	q, err := e.QueuePrepare(d.t.Context(), rpc.TradeProposalQueuePrepareParams{OwnerOrder: true, AccountID: scope.Account, AccountMode: scope.Mode,
		Order: &rpc.OrderPreviewParams{Contract: contract, Action: "BUY", Quantity: qty, OrderType: "LMT", TIF: "DAY", LimitPrice: &limit}})
	if err != nil || !q.Accepted {
		d.t.Fatalf("queue prepare: %+v %v", q.Blockers, err)
	}
	res, err := e.QueueArm(d.t.Context(), rpc.TradeProposalQueueArmParams{QueuedRef: q.QueuedRef, TermsDigest: q.Queue.TermsDigest,
		DeskActionID: "desk-action-1", Credential: "companion", Envelope: `{"signature":"synthetic"}`, Origin: rpc.OrderOriginAgent})
	if err != nil || !res.Accepted {
		d.t.Fatalf("queue arm: %+v %v", res.Blockers, err)
	}
	return q
}

func (d *simDesk) queuedRecord(id string) queuedAuthRecord {
	d.t.Helper()
	rec, ok := d.srv.tradeProposals.queued.get(id)
	if !ok {
		d.t.Fatalf("no queued record %s", id)
	}
	return rec
}

// Q1. A stock buy queued for the open on a book with a standing Add warning
// (the simulated book carries no index protection, so rule 4 warns on any
// stock buy). The owner signs the warnings with the terms; at the open the
// warnings send it once with the reviewed terms, also when only their
// figures moved; a warning whose code the owner did not sign, or a record
// written before warnings were signed, stops it under queue_new_add_warning
// and nothing is sent.
func TestSimulatedTWSQueuedBuyWithAStandingWarning(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func([]risk.StockAddBlocker) []risk.StockAddBlocker // the signed set the fixture leaves
		sent bool
	}{
		{"same-warnings-sent", nil, true},
		{"figure-drift-sent", func(w []risk.StockAddBlocker) []risk.StockAddBlocker {
			for i := range w {
				w[i].Message += " (figure as of the evening)"
			}
			return w
		}, true},
		{"new-code-held", func(w []risk.StockAddBlocker) []risk.StockAddBlocker {
			for i := range w {
				w[i].Code += "_other"
			}
			return w
		}, false},
		{"legacy-record-held", func([]risk.StockAddBlocker) []risk.StockAddBlocker { return nil }, false},
	} {
		changed := !tc.sent
		t.Run(tc.name, func(t *testing.T) {
			queueOpenIn(t, 20*time.Second)
			d := startSimDesk(t, twssim.Options{}, true)
			plan := d.plan(synaAdd)
			if len(plan.Warnings) == 0 || len(plan.Blockers) > 0 {
				t.Fatalf("the simulated book should warn on a stated quantity: warnings %+v blockers %+v", plan.Warnings, plan.Blockers)
			}
			q := d.queueBuy(plan.Contract, 10, 50.10)
			if signed := q.Queue.Terms.OwnerOrder.AddWarnings; !slices.Equal(signed, plan.Warnings) {
				t.Fatalf("signed terms carry warnings %+v, the plan shows %+v", signed, plan.Warnings)
			}
			if tc.edit != nil {
				if err := d.srv.tradeProposals.queued.update(t.Context(), time.Now(), func(records map[string]*queuedAuthRecord) []queuedAuthEvent {
					r := records[q.Queue.Terms.QueueID]
					r.Terms.OwnerOrder.AddWarnings = tc.edit(slices.Clone(r.Terms.OwnerOrder.AddWarnings))
					r.TermsDigest, _ = queuedTermsDigest(r.Terms)
					return []queuedAuthEvent{r.note("fixture", r.State, time.Now())}
				}); err != nil {
					t.Fatal(err)
				}
			}
			id := q.Queue.Terms.QueueID
			var rec queuedAuthRecord
			d.waitFor("the queued buy to leave armed", 60*time.Second, func() bool {
				rec = d.queuedRecord(id)
				return rec.State != rpc.QueuedAuthArmed && rec.State != rpc.QueuedAuthSending && rec.State != rpc.QueuedAuthHeld ||
					rec.State == rpc.QueuedAuthHeld && rec.HoldCode != rpc.ReadinessMarketClosed && rec.HoldCode != rpc.ReadinessOpeningWindow
			})
			sent := d.realPlaces()
			if changed {
				if len(sent) != 0 || rec.HoldCode != "queue_new_add_warning" && rec.ReasonCode != "queue_new_add_warning" {
					t.Fatalf("a new warning after the signature: state %s hold %q reason %q, %d frames", rec.State, rec.HoldCode, rec.ReasonCode, len(sent))
				}
				t.Logf("new warning: state %s hold %q reason %q %q", rec.State, rec.HoldCode, rec.ReasonCode, rec.Reason)
				return
			}
			if len(sent) != 1 {
				t.Fatalf("queued buy: state %s hold %q %q reason %q %q, %d frames", rec.State, rec.HoldCode, rec.HoldReason, rec.ReasonCode, rec.Reason, len(sent))
			}
			o := sent[0]
			if o.Action != "BUY" || o.Quantity != 10 || o.OrderType != "LMT" || o.LimitPrice > 50.10 || o.TIF != "DAY" || o.ConID != plan.Contract.ConID {
				t.Fatalf("sent %+v", o)
			}
			time.Sleep(2 * time.Second)
			if n := len(d.realPlaces()); n != 1 {
				t.Fatalf("queued buy sent %d times", n)
			}
		})
	}
}
