package ibkr

import (
	"errors"
	"testing"
)

// IBKR answers a history request for a contract it no longer knows with
// code 162 "Unknown contract", not code 200. Read as a route failure, the
// read tried every route and the quote fallback asked again every five
// minutes, six requests each time (WHLR, from 2026-10-29). It is the
// broker's no-definition verdict: no alternate route, the definition
// backoff and the retired-identity refresh.
func TestHistoryUnknownContractIsTheBrokersNoDefinitionVerdict(t *testing.T) {
	err := &HistoricalRequestError{Code: 162, Message: "Historical Market Data Service error message:Unknown contract"}
	if !errors.Is(err, ErrContractNoDefinition) {
		t.Fatal("unknown contract is not read as no definition")
	}
	if shouldRetryHistorical(err) {
		t.Fatal("unknown contract retries another route")
	}
	if got := classifyHistoricalRequestFailure(err); got != HistoricalFailureContractUnavailable {
		t.Fatalf("classified %q, want %q", got, HistoricalFailureContractUnavailable)
	}
	other := &HistoricalRequestError{Code: 162, Message: "Historical Market Data Service error message:HMDS query returned no data"}
	if errors.Is(other, ErrContractNoDefinition) || !shouldRetryHistorical(other) {
		t.Fatal("another 162 changed meaning")
	}
}
