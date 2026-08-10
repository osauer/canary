package daemon

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/daemon/corestore"
	"github.com/osauer/canary/v2/internal/rpc"
)

func TestRequestStopKeyMatchesGeneratedTrailingStopKey(t *testing.T) {
	t.Parallel()
	for name, row := range map[string]rpc.PositionView{
		"long stock":  {ConID: 101, Symbol: "SYN", SecType: "STOCK", Quantity: 40, Mark: 25, Bid: new(24.9), Ask: new(25.1)},
		"short stock": {ConID: 102, Symbol: "SHRT", SecType: "STK", Quantity: -15, Mark: 12, Bid: new(11.9), Ask: new(12.1)},
		"etf":         {ConID: 103, Symbol: "ETFX", SecType: "ETF", Quantity: 7, Mark: 80, Bid: new(79.9), Ask: new(80.1)},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			policy := defaultProtectionPolicy()
			prop, ok := trailingStopStockProposal(policy, rpc.ProtectionPolicyStatus{}, row, rpc.TradeProposalSourceFingerprints{}, time.Now().UTC(), true, 0.01)
			if !ok {
				t.Fatalf("expected a trailing-stop proposal for %+v", row)
			}
			key := proposalKey(rpc.TradeProposalBucketTrailingStop, proposalContractFromPosition(row, positionWireSecType(row.SecType)), trailActionForPosition(row.Quantity))
			if key != prop.Key {
				t.Fatalf("request-stop un-ignore key %q does not match generated proposal key %q; an ignored stop could never be re-requested", key, prop.Key)
			}
		})
	}
}

func TestRequestStopPositionBlockers(t *testing.T) {
	t.Parallel()
	option := rpc.PositionView{ConID: 7, Symbol: "SYN", SecType: "OPTION", Quantity: 2}
	if blockers := requestStopPositionBlockers(option); len(blockers) != 1 || blockers[0].Code != "unsupported_security_type" {
		t.Fatalf("option row blockers = %+v, want unsupported_security_type", blockers)
	}
	fractional := rpc.PositionView{ConID: 8, Symbol: "FRAC", SecType: "STK", Quantity: 0.4}
	if blockers := requestStopPositionBlockers(fractional); len(blockers) != 1 || blockers[0].Code != "position_not_protectable" {
		t.Fatalf("fractional row blockers = %+v, want position_not_protectable", blockers)
	}
	stock := rpc.PositionView{ConID: 9, Symbol: "SYN", SecType: "STK", Quantity: 12}
	if blockers := requestStopPositionBlockers(stock); len(blockers) != 0 {
		t.Fatalf("stock row blockers = %+v, want none", blockers)
	}
	shortStock := rpc.PositionView{ConID: 10, Symbol: "SHRT", SecType: "STOCK", Quantity: -3}
	if blockers := requestStopPositionBlockers(shortStock); len(blockers) != 0 {
		t.Fatalf("short stock row blockers = %+v, want none", blockers)
	}
}

func TestRequestStopProposalMatches(t *testing.T) {
	t.Parallel()
	row := rpc.PositionView{ConID: 42, Symbol: "SYN", SecType: "STOCK", Quantity: 5}
	byConID := rpc.TradeProposal{Symbol: "SYN", SecType: "STK", Contract: rpc.ContractParams{ConID: 42}}
	if !requestStopProposalMatches(byConID, row) {
		t.Fatal("ConID match rejected")
	}
	otherConID := rpc.TradeProposal{Symbol: "SYN", SecType: "STK", Contract: rpc.ContractParams{ConID: 43}}
	if requestStopProposalMatches(otherConID, row) {
		t.Fatal("different ConID accepted")
	}
	noConID := rpc.TradeProposal{Symbol: "SYN", SecType: "STK"}
	rowNoConID := rpc.PositionView{Symbol: "syn", SecType: "STOCK", Quantity: 5}
	if !requestStopProposalMatches(noConID, rowNoConID) {
		t.Fatal("symbol+sectype fallback rejected")
	}
	wrongType := rpc.TradeProposal{Symbol: "SYN", SecType: "OPT"}
	if requestStopProposalMatches(wrongType, rowNoConID) {
		t.Fatal("sec-type mismatch accepted")
	}
}

// TestUnignoredEventReplayClearsIgnoreDurably proves the request-stop
// un-ignore survives a daemon restart: replaying the persisted event stream
// must clear the earlier ignore, and a later re-ignore must still win.
func TestUnignoredEventReplayClearsIgnoreDurably(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	core, err := corestore.Open(ctx, corestore.Options{Path: filepath.Join(dir, "daemon.db")})
	if err != nil {
		t.Fatalf("open corestore: %v", err)
	}
	defer core.Close()
	if err := initializeCleanProposalOpportunityAuthority(ctx, core); err != nil {
		t.Fatalf("initialize clean authority: %v", err)
	}
	scope := brokerStateScope{Account: "DU111", Mode: "paper"}
	writer := &proposalEngine{store: &proposalStore{}}
	if err := writer.bindCore(ctx, core); err != nil {
		t.Fatalf("bind writer engine: %v", err)
	}
	at := time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC)
	events := []proposalEvent{
		{At: at, Type: "ignored", Key: "trailing_stop:aaaa", AccountID: scope.Account, AccountMode: scope.Mode},
		{At: at.Add(time.Minute), Type: "unignored", Key: "trailing_stop:aaaa", AccountID: scope.Account, AccountMode: scope.Mode, Reason: "stop_requested"},
		{At: at.Add(2 * time.Minute), Type: "ignored", Key: "trailing_stop:bbbb", AccountID: scope.Account, AccountMode: scope.Mode},
		{At: at.Add(3 * time.Minute), Type: "unignored", Key: "trailing_stop:bbbb", AccountID: scope.Account, AccountMode: scope.Mode, Reason: "stop_requested"},
		{At: at.Add(4 * time.Minute), Type: "ignored", Key: "trailing_stop:bbbb", AccountID: scope.Account, AccountMode: scope.Mode},
	}
	for _, ev := range events {
		if err := writer.appendEvent(ev); err != nil {
			t.Fatalf("append %s %s: %v", ev.Type, ev.Key, err)
		}
	}
	restarted := &proposalEngine{store: &proposalStore{}}
	if err := restarted.bindCore(ctx, core); err != nil {
		t.Fatalf("bind restarted engine: %v", err)
	}
	if restarted.isIgnored(scope, "trailing_stop:aaaa") {
		t.Fatal("unignored key aaaa still ignored after replay")
	}
	if !restarted.isIgnored(scope, "trailing_stop:bbbb") {
		t.Fatal("re-ignored key bbbb lost its ignore after replay")
	}
}
