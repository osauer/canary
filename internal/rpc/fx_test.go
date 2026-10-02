package rpc

import (
	"testing"
	"time"
)

func TestFXContractRejectsUncertifiedTotals(t *testing.T) {
	zero := 0.
	r := FXResult{SchemaVersion: FXSchemaVersion, AsOf: time.Now(), Days: []FXDay{}, Periods: []FXPeriod{{Key: "day", State: "available", Contribution: &zero, ExpectedDays: 2, ObservedDays: 1, MissingDays: []string{}}}}
	if ValidateFXResult(r) == nil {
		t.Fatal("partial counts certified as available")
	}
	r.Periods[0].State = "partial"
	if ValidateFXResult(r) == nil {
		t.Fatal("partial period exposed a total")
	}
	r.Periods[0].State = "no_exposure"
	if ValidateFXResult(r) == nil {
		t.Fatal("unknown exposure certified absent")
	}
	r.Periods[0].ObservedDays = 2
	if err := ValidateFXResult(r); err != nil {
		t.Fatal(err)
	}
}
