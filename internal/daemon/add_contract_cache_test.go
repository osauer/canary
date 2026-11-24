package daemon

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/osauer/canary/v3/internal/rpc"
	ibkr "github.com/osauer/canary/v3/pkg/ibkr"
)

// A contract resolved once today is answered from the cache: the broker is
// not asked again (the zero connector here would refuse any lookup). A
// strike the broker said does not exist is kept only for addListingFor, a
// new connector or a new New York day starts the cache over, and a failure
// that is not the broker's no-definition verdict is never kept.
func TestAddContractCacheAnswersRepeatsWithoutTheBroker(t *testing.T) {
	s := &Server{}
	c := &ibkr.Connector{}
	call := ibkr.Contract{Symbol: "SYNA", SecType: "OPT", Exchange: "SMART", Currency: "USD", Expiry: "20270115", Strike: 50, Right: "C", Multiplier: 100, TradingClass: "SYNA"}
	phantom := call
	phantom.Strike = 52.5
	now := time.Now()
	s.addContracts.mu.Lock()
	s.addContracts.currentLocked(c, now)
	s.addContracts.resolved[addContractKeyOf(call)] = addResolution{exact: ibkr.ResolvedOrderContract{Contract: ibkr.Contract{ConID: 4242, Symbol: "SYNA"}}, at: now.Add(-6 * time.Hour)}
	s.addContracts.resolved[addContractKeyOf(phantom)] = addResolution{absent: true, at: now}
	s.addContracts.mu.Unlock()

	got, err := s.addResolveContract(context.Background(), c, ibkr.ConnectorSessionBinding{}, call, time.Second)
	if err != nil || got.Contract.ConID != 4242 {
		t.Fatalf("a contract resolved today was not answered from the cache: %+v %v", got, err)
	}
	if _, err := s.addResolveContract(context.Background(), c, ibkr.ConnectorSessionBinding{}, phantom, time.Second); !errors.Is(err, ibkr.ErrContractNoDefinition) {
		t.Fatalf("a fresh no-definition verdict was not kept: %v", err)
	}

	s.addContracts.mu.Lock()
	s.addContracts.resolved[addContractKeyOf(phantom)] = addResolution{absent: true, at: now.Add(-addListingFor)}
	s.addContracts.mu.Unlock()
	if _, err := s.addResolveContract(context.Background(), c, ibkr.ConnectorSessionBinding{}, phantom, time.Second); err == nil || errors.Is(err, ibkr.ErrContractNoDefinition) {
		t.Fatalf("an old no-definition verdict was answered without asking the broker: %v", err)
	}

	if _, err := s.addResolveContract(context.Background(), &ibkr.Connector{}, ibkr.ConnectorSessionBinding{}, call, time.Second); err == nil {
		t.Fatal("a replaced connector was answered from the old connector's cache")
	}
	s.addContracts.mu.Lock()
	s.addContracts.currentLocked(c, now)
	s.addContracts.resolved[addContractKeyOf(call)] = addResolution{exact: ibkr.ResolvedOrderContract{Contract: ibkr.Contract{ConID: 4242}}, at: now}
	s.addContracts.currentLocked(c, now.Add(24*time.Hour))
	_, kept := s.addContracts.resolved[addContractKeyOf(call)]
	s.addContracts.mu.Unlock()
	if kept {
		t.Fatal("a resolution outlived its New York trading day")
	}
}

// A round of three quotes ends addRoundGrace after the first usable answer:
// a strike the broker never quotes no longer holds the round for the quote
// budget, and the answered call is kept. An answer outside the delta band
// does not start the grace.
func TestAddRoundEndsAGraceAfterTheFirstAnswer(t *testing.T) {
	calls := []ibkr.ResolvedOrderContract{{Contract: ibkr.Contract{Strike: 100}}, {Contract: ibkr.Contract{Strike: 101}}, {Contract: ibkr.Contract{Strike: 99}}}
	quote := func(ctx context.Context, exact ibkr.ResolvedOrderContract) rpc.AddCallCandidate {
		if exact.Contract.Strike != 100 {
			select { // never quoted
			case <-ctx.Done():
			case <-time.After(setupQuoteBudget):
			}
			return rpc.AddCallCandidate{Quote: &rpc.SetupOptionQuote{Status: rpc.SetupQuoteMissing}}
		}
		contract := rpc.ContractParams{Strike: 100}
		return rpc.AddCallCandidate{Contract: &contract, Quote: &rpc.SetupOptionQuote{Status: rpc.SetupQuoteQuoted}, Planning: &rpc.AddCallPlanning{DeltaStatus: "in_range", Price: new(2.5)}}
	}
	began := time.Now()
	row, _ := selectAddCallNear(context.Background(), testCallSide, calls, 100, quote)
	took := time.Since(began)
	if took > addRoundGrace+time.Second {
		t.Fatalf("the round took %s: an unquoted strike held it past the grace", took)
	}
	if row.Contract == nil || row.Contract.Strike != 100 {
		t.Fatalf("the answered quote was not kept: %+v", row)
	}
	if len(row.UnansweredStrikes) != 2 {
		t.Fatalf("the row does not name the strikes it was chosen without: %v", row.UnansweredStrikes)
	}
}

func TestAddRoundGraceWaitsForAUsableAnswer(t *testing.T) {
	calls := []ibkr.ResolvedOrderContract{{Contract: ibkr.Contract{Strike: 100}}, {Contract: ibkr.Contract{Strike: 101}}}
	quote := func(ctx context.Context, exact ibkr.ResolvedOrderContract) rpc.AddCallCandidate {
		contract := rpc.ContractParams{Strike: exact.Contract.Strike}
		if exact.Contract.Strike == 100 { // answers at once, outside the band
			return rpc.AddCallCandidate{Contract: &contract, Quote: &rpc.SetupOptionQuote{Status: rpc.SetupQuoteQuoted}, Planning: &rpc.AddCallPlanning{DeltaStatus: "out_of_range", Price: new(2.5)}}
		}
		select { // answers in band after the grace would have ended
		case <-ctx.Done():
			return rpc.AddCallCandidate{Quote: &rpc.SetupOptionQuote{Status: rpc.SetupQuoteMissing}}
		case <-time.After(addRoundGrace + 300*time.Millisecond):
		}
		return rpc.AddCallCandidate{Contract: &contract, Quote: &rpc.SetupOptionQuote{Status: rpc.SetupQuoteQuoted}, Planning: &rpc.AddCallPlanning{DeltaStatus: "in_range", Price: new(2.4)}}
	}
	row, _ := selectAddCallNear(context.Background(), testCallSide, calls, 100, quote)
	if row.Contract == nil || row.Contract.Strike != 101 {
		t.Fatalf("an out-of-band answer cut the in-band one short: %+v", row)
	}
	if len(row.UnansweredStrikes) != 0 {
		t.Fatalf("a round that heard every quote names unanswered strikes: %v", row.UnansweredStrikes)
	}
}
