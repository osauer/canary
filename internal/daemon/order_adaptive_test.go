package daemon

import (
	"github.com/osauer/canary/v2/internal/rpc"
	"testing"
)

func TestAdaptiveModifyCannotLoseKnownAlgorithm(t *testing.T) {
	view := rpc.OrderView{Symbol: "SYN", SecType: "STK", Action: "BUY", OrderType: "LMT", TIF: "DAY", Quantity: 2, Remaining: 2, LimitPrice: 20, AlgoKnown: true, AlgoStrategy: "Adaptive"}
	draft := rpc.OrderDraft{Contract: rpc.ContractParams{Symbol: "SYN", SecType: "STK"}, Action: "BUY", OrderType: "LMT", TIF: "DAY", Quantity: 1, LimitPrice: 20}
	if err := validateModifyDraft(view, draft); err == nil {
		t.Fatal("incomplete Adaptive observation became plain limit")
	}
}

func TestAdaptiveAcknowledgementRejectsDifferentAlgorithm(t *testing.T) {
	intent := orderJournalEvent{Type: orderJournalEventModifyRequested, Action: "BUY", OrderType: "LMT", TIF: "DAY", Quantity: 2, LimitPrice: 20}
	ack := intent
	ack.Type = orderJournalEventBrokerAcknowledged
	ack.AlgoKnown, ack.AlgoStrategy = true, "ArrivalPx"
	if orderModifyAcknowledgementMatches(intent, ack) {
		t.Fatal("different broker algorithm acknowledged plain intent")
	}
}

func TestAdaptiveUnknownAcknowledgementClearsKnownEvidence(t *testing.T) {
	view := rpc.OrderView{AlgoKnown: true, AlgoStrategy: "Adaptive", AdaptivePriority: "Normal"}
	mergeOrderJournalEventIntoView(&view, orderJournalEvent{Type: orderJournalEventBrokerAcknowledged})
	if view.AlgoKnown || view.AlgoStrategy != "" {
		t.Fatal("new unknown callback retained old algorithm proof")
	}
	// Retain intended priority so the unknown callback cannot make the order
	// appear safely modifiable as a plain order.
	if view.AdaptivePriority != "Normal" {
		t.Fatal("lost the intended Adaptive identity")
	}
}
