package canary_test

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"

	"github.com/osauer/canary/v2"
	"github.com/osauer/canary/v2/canarytest"
)

func TestCashSweepPreferenceClientIsNarrowAndPreservesReceipts(t *testing.T) {
	s := canarytest.Serve(t)
	current := "eur_first"
	want := canary.CashSweepPreferences{Revision: 4, CurrencyPriority: &current, EffectivePriority: current, Source: "runtime", Writable: true, SavedRevision: 2, Replay: true, RequestID: "immutable-first"}
	s.Handle("settings.cash_sweep.get", canarytest.Result(want))
	s.Handle("settings.cash_sweep.set_priority", func(_ context.Context, raw json.RawMessage) (json.RawMessage, error) {
		var wire map[string]json.RawMessage
		if err := json.Unmarshal(raw, &wire); err != nil {
			t.Fatal(err)
		}
		if len(wire) != 3 || len(wire["currency_priority"]) == 0 || len(wire["expected_revision"]) == 0 || len(wire["request_id"]) == 0 {
			t.Fatal("generic fields reached priority setter", string(raw))
		}
		return json.Marshal(want)
	})
	c := canary.New(canary.Options{SocketPath: s.SocketPath()})
	got, err := c.CashSweepPreferences(t.Context())
	if err != nil || !reflect.DeepEqual(got, &want) {
		t.Fatalf("get %+v %v", got, err)
	}
	old := "balanced"
	got, err = c.SetCashSweepPriority(t.Context(), canary.SetCashSweepPriorityRequest{CurrencyPriority: &old, ExpectedRevision: 1, RequestID: "immutable-first"})
	if err != nil || !reflect.DeepEqual(got, &want) {
		t.Fatalf("set receipt %+v %v", got, err)
	}
	s.Handle("settings.cash_sweep.set_priority", func(context.Context, json.RawMessage) (json.RawMessage, error) {
		return nil, canarytest.Fail(canary.CodeSettingsConflict, "refresh")
	})
	_, err = c.SetCashSweepPriority(t.Context(), canary.SetCashSweepPriorityRequest{CurrencyPriority: &old, ExpectedRevision: 1, RequestID: "stale"})
	if failure, ok := errors.AsType[*canary.Error](err); !ok || failure.Code != canary.CodeSettingsConflict {
		t.Fatal("typed conflict lost", err)
	}
	for _, tool := range canary.Tools() {
		for _, method := range tool.Methods {
			if method == "settings.cash_sweep.set_priority" {
				t.Fatal("MCP gained settings writer")
			}
		}
	}
}
