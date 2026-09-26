package daemon

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/daemon/corestore"
	ibkrlib "github.com/osauer/canary/v2/pkg/ibkr"
)

func TestContractCacheRetainsOnlyNonexpiredOptionsInDaemonAuthority(t *testing.T) {
	core, err := corestore.Open(t.Context(), corestore.Options{Path: filepath.Join(privateTestDir(t), "daemon.db")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = core.Close() })
	authority := coreContractCacheAuthority{store: core}
	live := ibkrlib.ContractDetailsLite{ConID: 123456, Symbol: "SYNTH", SecType: "OPT", TradingClass: "SYNTH", Expiry: "20991016", Strike: 100, Right: "C"}
	expired := live
	expired.Expiry = "20000121"
	liveKey := "SYNTH|SYNTH|20991016|100.000000|C"
	expiredKey := "SYNTH|SYNTH|20000121|100.000000|C"
	raw, err := json.Marshal(map[string]any{"version": 3, "as_of": time.Now().UTC(), "contracts": map[string]ibkrlib.ContractDetailsLite{}, "options": map[string]ibkrlib.ContractDetailsLite{liveKey: live, expiredKey: expired}})
	if err != nil {
		t.Fatal(err)
	}
	if err := authority.SaveContractCache(raw, time.Now()); err != nil {
		t.Fatal(err)
	}
	cache := ibkrlib.NewContractStore(t.TempDir())
	if err := cache.UseAuthority(authority); err != nil {
		t.Fatal(err)
	}
	// A connection transition supplies no options; expiry GC still runs.
	if err := cache.SaveRetainingOptions(map[string]ibkrlib.ContractDetailsLite{}, nil, "synthetic"); err != nil {
		t.Fatal(err)
	}
	restarted := ibkrlib.NewContractStore(t.TempDir())
	if err := restarted.UseAuthority(authority); err != nil {
		t.Fatal(err)
	}
	options, err := restarted.LoadOptions()
	if err != nil || len(options) != 1 || options[liveKey].ConID != live.ConID {
		t.Fatalf("durable options after empty save: count=%d err=%v", len(options), err)
	}
	payload, ok, err := authority.LoadContractCache()
	if err != nil || !ok {
		t.Fatal("missing authority")
	}
	var envelope struct {
		Options map[string]ibkrlib.ContractDetailsLite `json:"options"`
	}
	if err := json.Unmarshal(payload, &envelope); err != nil {
		t.Fatal(err)
	}
	if _, ok := envelope.Options[expiredKey]; ok {
		t.Fatal("expired hint retained in durable state")
	}
}

func TestGammaPrewarmRetainsTypedRejectionClassification(t *testing.T) {
	for _, tc := range []struct {
		code int
		want string
	}{{200, gammaLegFailureContractMissing}, {321, gammaLegFailureContractMissing}, {354, gammaLegFailureEntitlement}} {
		err := fmt.Errorf("route attempts exhausted: %w", &ibkrlib.ContractDetailsRequestError{Code: tc.code})
		if got := classifyGammaLegFailure(err); got != tc.want {
			t.Fatalf("code %d: %s, want %s", tc.code, got, tc.want)
		}
	}
}
