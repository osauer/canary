//go:build trading

package daemon

import (
	"context"
	"encoding/json"
	"github.com/osauer/canary/v3/internal/rpc"
	ibkr "github.com/osauer/canary/v3/pkg/ibkr"
	"testing"
)

func TestRegressionTWSRechecksControlAfterInventory(t *testing.T) {
	for _, tc := range []struct{ name, patch string }{{"freeze", `{"freeze":true}`}, {"disable", `{"tws_order_control":{"enabled":false}}`}} {
		t.Run(tc.name, func(t *testing.T) {
			rig := newTWSControlRig(t)
			original := rig.srv.openOrderInventoryForTest
			rig.srv.openOrderInventoryForTest = func(ctx context.Context, refresh bool) (ibkr.OpenOrderSnapshot, brokerStateScope, error) {
				snapshot, scope, err := original(ctx, refresh)
				if e := rig.srv.applyPlatformSettingsPatch(ctx, map[string]json.RawMessage{"trading": json.RawMessage(tc.patch)}, rpc.OrderOriginHumanTTY); e != nil {
					t.Fatal(e)
				}
				return snapshot, scope, err
			}
			p := twsControlTestParams("synthetic-control-race")
			p.Replacement = &rpc.OrderTWSControlReplacement{Quantity: 60, LimitPrice: 149.5}
			raw, _ := json.Marshal(p)
			result, err := rig.srv.handleOrderTWSModify(context.Background(), &rpc.Request{Params: raw})
			if err == nil && result.Accepted && len(rig.runs) > 0 {
				t.Fatalf("manual modify reached broker run after owner %s during inventory: accepted=%v runs=%d", tc.name, result.Accepted, len(rig.runs))
			}
		})
	}
}

func TestTWSControlCancelRetainsFreezeExemptionButHonorsDisable(t *testing.T) {
	for _, tc := range []struct {
		name, patch string
		want        bool
	}{{"freeze", `{"freeze":true}`, true}, {"disable", `{"tws_order_control":{"enabled":false}}`, false}} {
		t.Run(tc.name, func(t *testing.T) {
			rig := newTWSControlRig(t)
			original := rig.srv.openOrderInventoryForTest
			rig.srv.openOrderInventoryForTest = func(ctx context.Context, refresh bool) (ibkr.OpenOrderSnapshot, brokerStateScope, error) {
				snapshot, scope, err := original(ctx, refresh)
				if e := rig.srv.applyPlatformSettingsPatch(ctx, map[string]json.RawMessage{"trading": json.RawMessage(tc.patch)}, rpc.OrderOriginHumanTTY); e != nil {
					t.Fatal(e)
				}
				return snapshot, scope, err
			}
			res, err := rig.srv.twsOrderControl(t.Context(), ibkr.TWSOrderControlCancel, twsControlTestParams("synthetic-cancel-race"))
			if err != nil || res.Accepted != tc.want {
				t.Fatalf("result=%+v err=%v", res, err)
			}
		})
	}
}
