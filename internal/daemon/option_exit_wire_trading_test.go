//go:build trading

package daemon

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/osauer/canary/v3/internal/config"
	ibkr "github.com/osauer/canary/v3/pkg/ibkr"
)

func TestOptionExitWireGuardRejectsExpiredEconomicEvidence(t *testing.T) {
	s := newOrderPreviewTestServer(t, config.Trading{Mode: config.TradingModePaper})
	calls := 0
	s.orderPlaceBroker = func(context.Context, *ibkr.Contract, *ibkr.RawOrder) error { calls++; return nil }
	auth, binding, err := s.authorizeBrokerWriteTransaction("", false)
	if err != nil {
		t.Fatal(err)
	}
	binding.riskBound = true
	binding.optionExitExpiresAt = s.orderNow().Add(-time.Second)
	guard, release := s.brokerWireGuard(binding, auth.Status, false)
	defer release()
	if err := guard(); err == nil || !strings.Contains(err.Error(), "economic-role evidence expired before broker send") {
		t.Fatalf("expired evidence reached past first-byte guard: %v", err)
	}
	if calls != 0 {
		t.Fatal("expired evidence made a broker call")
	}
}
