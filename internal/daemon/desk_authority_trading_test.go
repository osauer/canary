//go:build trading

package daemon

import (
	"context"
	"github.com/osauer/canary/v2/internal/config"
	"github.com/osauer/canary/v2/internal/rpc"
	ibkr "github.com/osauer/canary/v2/pkg/ibkr"
	"strings"
	"testing"
	"time"
)

func TestDeskAuthorityExpiryDuringFinalWireChecksRefusesSend(t *testing.T) {
	s := newOrderPreviewTestServer(t, config.Trading{Mode: config.TradingModePaper})
	s.orderPlaceBroker = func(context.Context, *ibkr.Contract, *ibkr.RawOrder) error {
		t.Fatal("test must never place an order")
		return nil
	}
	now := s.orderNow()
	s.now = func() time.Time { return now }
	s.startedAt = now
	auth, binding, err := s.authorizeBrokerWriteTransaction("", false)
	if err != nil || !auth.Allowed {
		t.Fatalf("test admission: %+v %v", auth, err)
	}
	head, err := s.coreStore.AuthorityHead(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	terms := rpc.DeskAuthorityTerms{AuthorityEpoch: head.AuthorityEpoch, Version: 1, Kind: "desk_automatic_authority", ID: "synthetic-wire-deadline", AccountID: auth.Status.Account, AccountMode: auth.Status.Mode, MaximumScope: "full", ControllerHash: strings.Repeat("a", 64), ConfirmBefore: now.Add(time.Minute), Pricing: "fixed-midpoint", Algorithm: "Adaptive", TIF: "DAY", Protection: "existing-policy", Capacity: "existing-policy-reuse-confirmed-funds", Selection: "desk-deterministic-controller"}
	row := deskAuthorityRecord{DeskAuthorityStatus: rpc.DeskAuthorityStatus{Terms: &terms, Scope: "full", Running: true, PreferencesHash: strings.Repeat("b", 64), AdaptivePriority: "Normal"}, ControlProcess: now}
	if _, err := s.saveDeskAuthority(t.Context(), 0, row, "test-confirmed"); err != nil {
		t.Fatal(err)
	}
	binding.deskAuthority = &deskAuthorityFence{ID: terms.ID, Generation: 1, PreferencesHash: row.PreferencesHash, AdaptivePriority: "Normal", Action: "entry", ValidUntil: now.Add(time.Second)}
	// The origin check runs after the mandate lease. Advance the source clock
	// there to model time spent waiting for remaining first-byte checks.
	s.orderWriteOriginBlockersForTest = func(rpc.TradingStatus, string) []rpc.TradingBlocker { now = now.Add(2 * time.Second); return nil }
	guard, release := s.brokerWireGuard(binding, auth.Status, false)
	defer release()
	if err := guard(); err == nil || !strings.Contains(err.Error(), "automatic signal expired") {
		t.Fatalf("expired signal reached wire: %v", err)
	}
}
