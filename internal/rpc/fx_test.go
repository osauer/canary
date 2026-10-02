package rpc

import (
	"testing"
	"time"
)

func TestFXContractRejectsUncertifiedTotals(t *testing.T) {
	zero := 0.
	r := FXResult{SchemaVersion: FXSchemaVersion, AsOf: time.Now(), Through: "2026-01-02", State: "no_exposure", Method: "closing_native_book_v1", Periods: []FXPeriod{},
		Days: []FXDay{{Day: "2026-01-01", PreviousDay: "2025-12-31", Contribution: &zero, ReconciliationResidual: &zero}, {Day: "2026-01-02", PreviousDay: "2026-01-01", Contribution: &zero, ReconciliationResidual: &zero}}}
	for _, key := range []string{"day", "week", "month", "ytd"} {
		r.Periods = append(r.Periods, FXPeriod{Key: key, From: "2026-01-01", Through: r.Through, State: "available", Contribution: &zero, ExpectedDays: 2, ObservedDays: 2, MissingDays: []string{}})
	}
	r.Periods[0].ObservedDays = 1
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

func TestFXContractRejectsUncertifiedMoney(t *testing.T) {
	fixture := func() FXResult {
		zero := 0.
		r := FXResult{SchemaVersion: FXSchemaVersion, AsOf: time.Now(), BaseCurrency: "EUR", Through: "2026-01-02", State: "no_exposure", Method: "closing_native_book_v1", Days: []FXDay{{Day: "2026-01-02", PreviousDay: "2025-12-31", Contribution: &zero, ReconciliationResidual: &zero}}, Periods: []FXPeriod{}}
		for _, key := range []string{"day", "week", "month", "ytd"} {
			r.Periods = append(r.Periods, FXPeriod{Key: key, From: "2026-01-01", Through: r.Through, State: "no_exposure", Contribution: &zero, ObservedDays: 1, ExpectedDays: 1, MissingDays: []string{}})
		}
		return r
	}
	if err := ValidateFXResult(fixture()); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*FXResult){
		"unknown method":              func(r *FXResult) { r.Method = "unknown" },
		"unknown state":               func(r *FXResult) { r.State = "fresh" },
		"unreconciled day":            func(r *FXResult) { r.Days[0].ReconciliationResidual = nil },
		"residual failure":            func(r *FXResult) { v := 1.; r.Days[0].ReconciliationResidual = &v },
		"forward opening":             func(r *FXResult) { r.Days[0].PreviousDay = r.Days[0].Day },
		"negative coverage":           func(r *FXResult) { r.Periods[0].ObservedDays = -1 },
		"hidden gap":                  func(r *FXResult) { r.Periods[0].ExpectedDays++ },
		"duplicate period":            func(r *FXResult) { r.Periods[0].Key = "ytd" },
		"wrong cutoff":                func(r *FXResult) { r.Periods[0].Through = "2026-01-03" },
		"foreign exposure suppressed": func(r *FXResult) { r.Days[0].Foreign = true },
		"nonzero no exposure":         func(r *FXResult) { v := 1.; r.Periods[0].Contribution = &v },
		"progress overflow":           func(r *FXResult) { r.Backfill.SnapshotDays = 1 },
	} {
		t.Run(name, func(t *testing.T) {
			r := fixture()
			mutate(&r)
			if ValidateFXResult(r) == nil {
				t.Fatal("uncertified envelope accepted")
			}
		})
	}
}
