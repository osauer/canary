package daemon

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/osauer/canary/v2/internal/rpc"
)

// A persisted IBKR key, fingerprint or term must keep its bytes for as long as a
// daemon.db written before the second broker exists: there is no migration. This
// table pins every scoped family to the bytes the tree produced before the broker
// kind entered the scope (recorded at 2928cd46, 2026-11-04), under a synthetic paper
// account, and holds the second broker's bytes apart from them.
func scopeFamilies(t *testing.T, scope brokerStateScope) map[string]string {
	t.Helper()
	must := func(s string, err error) string {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	asJSON := func(v any) string {
		t.Helper()
		raw, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		return string(raw)
	}
	alert, err := newAlertShadowBrokerScope(scope)
	if err != nil {
		t.Fatal(err)
	}
	mark, err := liveMarkBindingFor("synth", scope, 42)
	if err != nil {
		t.Fatal(err)
	}
	return map[string]string{
		"broker permissions":   brokerPermissionKey(scope, "stock"),
		"desk executions":      deskExecutionKey(scope, "kind", "id", "suffix"),
		"tws order control":    twsOrderControlKey(scope, "kind", "id"),
		"core frozen scope":    coreScope(scope, 42),
		"core episode":         coreEpisode(scope, "2026-11-04"),
		"strategy mark":        mark.scopeKey(),
		"opportunity ignored":  opportunityIgnoreKey(scope, "key"),
		"proposal ignored":     scopedIgnoreKey(scope, "key"),
		"opportunity revision": opportunityRevision(rpc.Fingerprint{}, rpc.OpportunitySourceFingerprints{}, scope, nil),
		"proposal revision":    proposalRevisionProjection{Policy: rpc.Fingerprint{}, Account: proposalRevisionAccount(scope), Mode: "paper"}.hash(),
		"fee rate":             marketEventFeeRateScopeFingerprint(scope),
		"alert authority":      alert.authority,
		"risk capital":         must(riskCapitalScopeKey(scope)),
		"reconciliation":       reconScopeIdentity(scope),
		"cash sweep trace":     cashSweepTraceScopeKey(scope),
		"daily pnl":            dailyPnLScopeSource(scope),
		"edge":                 edgeScopeFingerprint(scope),
		"account data scope":   asJSON(accountDataScope(scope)),
	}
}

var ibkrScopeBytes = map[string]string{
	"account data scope":   "{\"account_id\":\"DU1234567\",\"account_mode\":\"paper\"}",
	"alert authority":      "alert-authority-scope-v1:0b9919c92bdd22c18eef3f9db5bb10830448635232a3b80628c50ce54a14b54c",
	"broker permissions":   "DU1234567|paper|stock",
	"cash sweep trace":     "cash_sweep:5d9eff56a920e665e1ca92cd63008452e15cc083eadc3c4a48cba44e993a29d1",
	"core episode":         "core:DU1234567:paper:2026-11",
	"core frozen scope":    "DU1234567|paper|42",
	"daily pnl":            "paper|DU1234567",
	"desk executions":      "desk-exec:DU1234567:paper:kind:id:suffix",
	"edge":                 "scope_eb65f3440a357ac3332ae605966a2180",
	"fee rate":             "sha256:063cf352e6dea41dcef10ea9df8d140fd280cb194124dcb33a09eb2909845413",
	"opportunity ignored":  "DU1234567|paper|key",
	"opportunity revision": "sha256:307cbf257d9480b5e0140d9eef4cbe5ec1290abb5a7bbac4d40bd19f49e98e72",
	"proposal ignored":     "DU1234567|paper|key",
	"proposal revision":    "sha256:0d1c002b9496968cf39530f2db88ff9fadd2322353af2fc47183cbac98490e3f",
	"reconciliation":       "sha256:7e3db37d9c598a81a0943b65a18b2ad69f4eff6a746acbd65ab3bdb5f6a8c2dc",
	"risk capital":         "risk-capital-scope-v1:486eb28acf9a49dcc271767057bf464ed32e40176d61664c1d559c9569840bfb",
	"strategy mark":        "strategy/synth/DU1234567|paper|42",
	"tws order control":    "tws-control:DU1234567:paper:kind:id",
}

func TestIBKRScopeBytesAreUnchanged(t *testing.T) {
	scope := brokerStateScope{Account: "DU1234567", Mode: rpc.AccountModePaper}
	got := scopeFamilies(t, scope)
	for family, want := range ibkrScopeBytes {
		if got[family] != want {
			t.Errorf("%s changed:\n got %s\nwant %s", family, got[family], want)
		}
	}
	if len(got) != len(ibkrScopeBytes) {
		t.Errorf("%d families pinned, %d built", len(ibkrScopeBytes), len(got))
	}
	if strings.Contains(strings.Join([]string{got["core frozen scope"]}, ""), ":") {
		t.Errorf("an IBKR account must never contain the kind separator: %s", got["core frozen scope"])
	}
}
