package daemon

import (
	"github.com/osauer/canary/v3/internal/rpc"
	"testing"
	"time"
)

func TestCoreLastDecisionRejectsOtherAccountEvent(t *testing.T) {
	at := time.Date(2026, 10, 1, 10, 0, 0, 0, time.UTC)
	contract := rpc.ContractParams{ConID: 900001, Symbol: "SYNF", SecType: "STK", Currency: "EUR", Exchange: "SMART"}
	key := proposalKey(rpc.TradeProposalBucketCoreRebalance, contract, rpc.OrderActionBuy)
	scope := coreScope(brokerStateScope{Account: "DU1111111", Mode: "paper"}, contract.ConID)
	s := &Server{tradeProposals: &proposalEngine{store: &proposalStore{events: []proposalEvent{{At: at.Add(time.Hour), Type: "submitted", Key: key, AccountID: "DU2222222", AccountMode: "paper", OrderRef: "synthetic-other-account"}}}}}
	if e := s.freezeCore(t.Context(), &coreFrozenTarget{Version: 1, Scope: scope, Decision: "2026-09-30", Row: &coreDecisionRow{DecidedAt: at, RowKey: key, Served: true}}); e != nil {
		t.Fatal(e)
	}
	got := s.coreLastDecision(t.Context(), scope)
	t.Logf("requested account DU1111111; only event belongs to DU2222222; outcome=%+v", got.Outcome)
	if got.Outcome.Submitted {
		t.Fatal("other account's submission assigned to current core decision")
	}
}
