package daemon

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/daemon/corestore"
	"github.com/osauer/canary/v2/internal/risk"
	"github.com/osauer/canary/v2/internal/rpc"
)

func TestCashSweepPartialObservationTraceKeepsOriginalClocksAndTransitions(t *testing.T) {
	core, err := corestore.Open(t.Context(), corestore.Options{Path: filepath.Join(privateTestDir(t), "daemon.db")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = core.Close() })
	e := &proposalEngine{store: &proposalStore{core: core}}
	now := time.Now().UTC()
	scope := brokerStateScope{Account: "UTEST", Mode: "paper"}
	policy := cashSweepTestPolicy("shadow", 100000)
	st := rpc.TradeProposalCashSweepStatus{Mode: "shadow", OperationalFunding: &risk.CashSweepOperationalObservation{AsOf: now, Source: "live_partial", State: "partial", AccountReceiptAt: now.Add(-time.Second),
		Currencies: []risk.CashSweepOperationalCurrency{{Currency: "USD", GrossPrincipal: new(50000.)}}, Obligations: []risk.CashSweepFundingObligation{{ConID: 201, Currency: "USD", GrossPrincipal: new(50000.), QuoteOriginalAt: now.Add(-time.Second)}}}}
	persist := func() {
		t.Helper()
		if err := e.persistCashSweepTrace(t.Context(), policy, rpc.TradeProposalSourceFingerprints{}, scope, now, &st); err != nil {
			t.Fatal(err)
		}
	}
	persist()
	original := st.DecisionTrace[0].OperationalFunding.AccountReceiptAt
	st.OperationalFunding.AsOf = now.Add(time.Second)
	st.OperationalFunding.AccountReceiptAt = now
	st.OperationalFunding.Obligations[0].QuoteOriginalAt = now
	persist()
	if len(st.DecisionTrace) != 1 || !st.DecisionTrace[0].OperationalFunding.AccountReceiptAt.Equal(original) {
		t.Fatal("refresh rewrote original observation receipt", st.DecisionTrace)
	}
	*st.DecisionTrace[0].OperationalFunding.Currencies[0].GrossPrincipal = -1
	persist()
	if *st.DecisionTrace[0].OperationalFunding.Currencies[0].GrossPrincipal != 50000 {
		t.Fatal("response mutation poisoned retained trace")
	}
	st.OperationalFunding.Currencies[0].GrossPrincipal = nil
	st.OperationalFunding.Currencies[0].Gaps = []string{"automatic_exercise_policy_unavailable"}
	persist()
	if len(st.DecisionTrace) != 2 || st.DecisionTrace[0].OperationalFunding.Currencies[0].GrossPrincipal != nil {
		t.Fatal("known-to-unknown obligation transition disappeared", st.DecisionTrace)
	}
}
