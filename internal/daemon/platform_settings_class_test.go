package daemon

import (
	"context"
	"reflect"
	"slices"
	"testing"

	"github.com/osauer/canary/v3/internal/chainrec"
	"github.com/osauer/canary/v3/internal/config"
	"github.com/osauer/canary/v3/internal/rpc"
)

// Every setting has a class the daemon judges by the write's origin, deny by
// default (strategy-register.md, "Settings: classes the daemon enforces"):
// an agent, or a write that names no origin, may change only the collection
// settings; the owner's settings (the protection control, the collection off
// switches, the preferences) take a human terminal or the paired console;
// the safety controls a human terminal only. Until 2026-10-18 an agent could
// write every key but trading.*, the hook's refusal stepped round with a
// flag before the verb.
func TestPlatformSettingsClassesByOrigin(t *testing.T) {
	t.Parallel()
	patches := map[string]string{
		rpc.SettingsClassSafety:     `"trading":{"freeze":true}`,
		rpc.SettingsClassOwner:      `"features":{"stock_protection":{"enabled":false}}`,
		rpc.SettingsClassCollection: `"chains":{"recorder":{"symbols":["SYNA"]}}`,
	}
	allowed := map[string][]string{
		rpc.SettingsClassSafety:     {rpc.OrderOriginHumanTTY},
		rpc.SettingsClassOwner:      {rpc.OrderOriginHumanTTY, rpc.OrderOriginPairedDevice},
		rpc.SettingsClassCollection: {"", rpc.OrderOriginAgent, rpc.OrderOriginHumanTTY, rpc.OrderOriginPairedDevice, "daemon-preauthorised"},
	}
	for class, body := range patches {
		for _, origin := range []string{"", rpc.OrderOriginAgent, rpc.OrderOriginHumanTTY, rpc.OrderOriginPairedDevice, "daemon-preauthorised"} {
			t.Run(class+"/"+nonEmptyString(origin, "missing"), func(t *testing.T) {
				srv := newPlatformSettingsTestServer(t, config.Trading{Mode: config.TradingModeLive})
				before := srv.platformSettings.snapshot()
				field := ""
				if origin != "" {
					field = `,"origin":"` + origin + `"`
				}
				_, err := srv.handleSettingsUpdate(context.Background(), &rpc.Request{Params: []byte(`{` + body + field + `}`)})
				if want := slices.Contains(allowed[class], origin); want != (err == nil) {
					t.Fatalf("%s write from %q: err = %v, want allowed %v", class, origin, err, want)
				}
				if err != nil && !reflect.DeepEqual(srv.platformSettings.snapshot(), before) {
					t.Fatal("a refused write changed the settings")
				}
			})
		}
	}
	// A patch mixing a collection key with an owner's key is refused whole.
	srv := newPlatformSettingsTestServer(t, config.Trading{Mode: config.TradingModeLive})
	if _, err := srv.handleSettingsUpdate(context.Background(), &rpc.Request{Params: []byte(`{"chains":{"recorder":{"symbols":["SYNA"],"enabled":false}},"origin":"agent"}`)}); err == nil {
		t.Fatal("an agent's mixed patch was accepted")
	}
	if got := chainRecorderSettingsFrom(srv.platformSettings.snapshot()); !slices.Equal(got.Symbols, chainrec.DefaultSymbols) {
		t.Fatalf("the refused mixed patch wrote %+v", got)
	}
	// The registry gives every key a class the gate knows.
	for _, spec := range rpc.SettingsKeys() {
		switch spec.Class {
		case rpc.SettingsClassSafety, rpc.SettingsClassOwner, rpc.SettingsClassCollection:
		default:
			t.Fatalf("%s has class %q", spec.Key, spec.Class)
		}
	}
}
