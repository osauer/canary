package daemon

import (
	"errors"
	"testing"

	"github.com/osauer/canary/v2/internal/breadth/spx"
	ibkrlib "github.com/osauer/canary/v2/pkg/ibkr"
)

func TestBreadthRecoveryOnlyPermitsHistoricalFarmWarnings(t *testing.T) {
	for _, kind := range []string{"historical", "connectivity", "market", "security_definition"} {
		err := breadthFarmGateError(ibkrlib.DataFarmStatus{Type: kind, Name: "synthetic", Status: "disconnected"})
		_, recoverable := errors.AsType[*spx.RecoveryGateError](err)
		if recoverable != (kind == "historical") {
			t.Fatalf("%s recovery permission = %v", kind, recoverable)
		}
	}
}
