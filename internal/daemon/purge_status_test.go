package daemon

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/discover"
	"github.com/osauer/canary/v2/internal/rpc"
)

func TestPurgeStatusFiltersJournalBackedPurgeOrders(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 6, 4, 18, 45, 0, 0, time.UTC)
	srv := &Server{
		orderJournal: newTestOrderJournalStore(t, filepath.Join(t.TempDir(), "order-journal.jsonl")),
		endpoint:     discover.Endpoint{Host: "127.0.0.1", Port: 4002, ClientID: 31, Account: "DU123"},
		now:          func() time.Time { return now },
	}
	events := []orderJournalEvent{
		{
			At:              now.Add(-2 * time.Minute),
			Type:            orderJournalEventSendAttempted,
			Source:          purgeExecuteSource,
			PurgeID:         "purge_a",
			LegID:           "leg_sap",
			OrderRef:        "purge-order-1",
			ReservedOrderID: 1001,
			ClientID:        31,
			Account:         "DU123",
			Endpoint:        "127.0.0.1:4002",
			Mode:            rpc.AccountModePaper,
			Symbol:          "SAP",
			SecType:         "STK",
			Action:          rpc.OrderActionSell,
			OrderType:       rpc.OrderTypeLMT,
			TIF:             rpc.OrderTIFDay,
			Quantity:        1,
			LimitPrice:      242,
			SendState:       orderSendStateSendAttempted,
		},
		{
			At:              now.Add(-time.Minute),
			Type:            orderJournalEventStatusUpdated,
			Source:          purgeExecuteSource,
			PurgeID:         "purge_a",
			OrderRef:        "purge-order-1",
			ReservedOrderID: 1001,
			ClientID:        31,
			Account:         "DU123",
			Endpoint:        "127.0.0.1:4002",
			Mode:            rpc.AccountModePaper,
			Status:          "Filled",
			Filled:          1,
			Remaining:       0,
			SendState:       orderSendStateTerminal,
		},
		{
			At:              now,
			Type:            orderJournalEventStatusUpdated,
			OrderRef:        "normal-order",
			ReservedOrderID: 1002,
			ClientID:        31,
			Account:         "DU123",
			Endpoint:        "127.0.0.1:4002",
			Mode:            rpc.AccountModePaper,
			Symbol:          "MSFT",
			SecType:         "STK",
			Action:          rpc.OrderActionBuy,
			OrderType:       rpc.OrderTypeLMT,
			TIF:             rpc.OrderTIFDay,
			Quantity:        1,
			Status:          "Submitted",
			SendState:       orderSendStateBrokerAcknowledged,
		},
	}
	for _, ev := range events {
		if err := srv.orderJournal.Append(ev); err != nil {
			t.Fatalf("append journal: %v", err)
		}
	}

	raw, err := json.Marshal(rpc.PurgeStatusParams{PurgeID: "purge_a"})
	if err != nil {
		t.Fatalf("marshal params: %v", err)
	}
	res, err := srv.handlePurgeStatus(context.Background(), &rpc.Request{Params: raw})
	if err != nil {
		t.Fatalf("handlePurgeStatus: %v", err)
	}
	if res.Status != purgeStatusFilled || res.TotalOrders != 1 || res.FilledOrders != 1 || res.OpenOrders != 0 || len(res.Orders) != 1 {
		t.Fatalf("purge status = %+v, want one filled purge order", res)
	}
	if res.Orders[0].OrderRef != "purge-order-1" || res.Orders[0].Symbol != "SAP" || res.Orders[0].Source != purgeExecuteSource {
		t.Fatalf("purge order = %+v", res.Orders[0])
	}
}

func TestPurgeStatusIncludesRestoreOrders(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 6, 4, 18, 50, 0, 0, time.UTC)
	srv := &Server{
		orderJournal: newTestOrderJournalStore(t, filepath.Join(t.TempDir(), "order-journal.jsonl")),
		endpoint:     discover.Endpoint{Host: "127.0.0.1", Port: 4002, ClientID: 31, Account: "DU123"},
		now:          func() time.Time { return now },
	}
	if err := srv.orderJournal.Append(orderJournalEvent{
		At:              now,
		Type:            orderJournalEventSendAttempted,
		Source:          purgeRestoreSource,
		PurgeID:         "purge_a",
		LegID:           "leg_sap",
		OrderRef:        "restore-order-1",
		ReservedOrderID: 1002,
		ClientID:        31,
		Account:         "DU123",
		Endpoint:        "127.0.0.1:4002",
		Mode:            rpc.AccountModePaper,
		Symbol:          "SAP",
		SecType:         "STK",
		Action:          rpc.OrderActionBuy,
		OrderType:       rpc.OrderTypeLMT,
		TIF:             rpc.OrderTIFDay,
		Quantity:        1,
		LimitPrice:      240,
		SendState:       orderSendStateSendAttempted,
	}); err != nil {
		t.Fatalf("append journal: %v", err)
	}

	raw, err := json.Marshal(rpc.PurgeStatusParams{PurgeID: "purge_a"})
	if err != nil {
		t.Fatalf("marshal params: %v", err)
	}
	res, err := srv.handlePurgeStatus(context.Background(), &rpc.Request{Params: raw})
	if err != nil {
		t.Fatalf("handlePurgeStatus: %v", err)
	}
	if res.Status != purgeStatusOpen || res.TotalOrders != 1 || res.OpenOrders != 1 || len(res.Orders) != 1 {
		t.Fatalf("restore status = %+v, want one open restore order", res)
	}
	if res.Orders[0].OrderRef != "restore-order-1" || res.Orders[0].Source != purgeRestoreSource {
		t.Fatalf("restore order = %+v", res.Orders[0])
	}
}
