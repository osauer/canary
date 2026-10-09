package risk

import (
	"math"
	"strings"
	"testing"
	"time"
)

func portfolioPolicyFixture() (*PortfolioPlanPolicy, time.Time, []PortfolioTargetEvidence) {
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	p := &PortfolioPlanPolicy{Contract: PortfolioPlanContract, ValidUntil: now.Add(time.Hour)}
	for i, symbol := range []string{"SYNA", "SYNB"} {
		p.Targets = append(p.Targets, PortfolioPlanTarget{Symbol: symbol, ConID: 101 + i, Currency: "USD", Priority: 2 - i, LowerPctNLV: new(4.), TargetPctNLV: new(5.), UpperPctNLV: new(6.), LimitPrice: new(100.), EntryRegimes: []string{RegimeBucketCalm}, Reason: "Owner target"})
	}
	return p, now, []PortfolioTargetEvidence{{ConID: 101, Symbol: "SYNA", Currency: "USD", FX: 1, Complete: true, InUniverse: true}, {ConID: 102, Symbol: "SYNB", Currency: "USD", FX: 1, Quantity: 20, Mark: 100, Complete: true, InUniverse: true}}
}

func TestPortfolioTargetsUseExplicitPriorityAndZeroHoldingEvidence(t *testing.T) {
	p, now, e := portfolioPolicyFixture()
	rows, err := EvaluatePortfolioTargets(p, now, new(100000.), RegimeBucketCalm, e)
	if err != nil || len(rows) != 2 || rows[0].Symbol != "SYNB" || rows[0].DesiredQuantity != 30 || rows[1].DesiredQuantity != 50 || rows[1].QuantityBefore == nil || *rows[1].QuantityBefore != 0 {
		t.Fatalf("unexpected target plan: %+v %v", rows, err)
	}
	if p.Targets[0].Symbol != "SYNA" || e[1].Quantity != 20 {
		t.Fatal("planning mutated policy or holdings")
	}
	e[0].Complete = false
	rows, _ = EvaluatePortfolioTargets(p, now, new(100000.), RegimeBucketCalm, e)
	if rows[1].Decision != "cannot_evaluate" || rows[1].QuantityBefore != nil || rows[1].DesiredQuantity != 0 {
		t.Fatal("missing holdings became zero")
	}
}

func TestPortfolioTargetsKeepHoldReductionAndUnknownDistinct(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*PortfolioPlanPolicy, *[]PortfolioTargetEvidence, *time.Time, **float64, *string)
		want   string
	}{
		{"within band", func(_ *PortfolioPlanPolicy, e *[]PortfolioTargetEvidence, _ *time.Time, _ **float64, _ *string) {
			(*e)[0].Quantity, (*e)[0].Mark = 50, 100
		}, "hold"},
		{"above band", func(_ *PortfolioPlanPolicy, e *[]PortfolioTargetEvidence, _ *time.Time, _ **float64, _ *string) {
			(*e)[0].Quantity, (*e)[0].Mark = 70, 100
		}, "review_reduction"},
		{"expired", func(p *PortfolioPlanPolicy, _ *[]PortfolioTargetEvidence, now *time.Time, _ **float64, _ *string) {
			*now = p.ValidUntil
		}, "cannot_evaluate"},
		{"NLV missing", func(_ *PortfolioPlanPolicy, _ *[]PortfolioTargetEvidence, _ *time.Time, n **float64, _ *string) {
			*n = nil
		}, "cannot_evaluate"},
		{"NLV invalid", func(_ *PortfolioPlanPolicy, _ *[]PortfolioTargetEvidence, _ *time.Time, n **float64, _ *string) {
			*n = new(math.NaN())
		}, "cannot_evaluate"},
		{"wrong listing", func(_ *PortfolioPlanPolicy, e *[]PortfolioTargetEvidence, _ *time.Time, _ **float64, _ *string) {
			(*e)[0].ConID = 900
		}, "cannot_evaluate"},
		{"ambiguous listing", func(_ *PortfolioPlanPolicy, e *[]PortfolioTargetEvidence, _ *time.Time, _ **float64, _ *string) {
			*e = append(*e, (*e)[0])
		}, "cannot_evaluate"},
		{"not watched or held", func(_ *PortfolioPlanPolicy, e *[]PortfolioTargetEvidence, _ *time.Time, _ **float64, _ *string) {
			(*e)[0].InUniverse = false
		}, "cannot_evaluate"},
		{"short", func(_ *PortfolioPlanPolicy, e *[]PortfolioTargetEvidence, _ *time.Time, _ **float64, _ *string) {
			(*e)[0].Quantity = -1
		}, "cannot_evaluate"},
		{"FX missing", func(_ *PortfolioPlanPolicy, e *[]PortfolioTargetEvidence, _ *time.Time, _ **float64, _ *string) {
			(*e)[0].FX = 0
		}, "cannot_evaluate"},
		{"regime disallows", func(_ *PortfolioPlanPolicy, _ *[]PortfolioTargetEvidence, _ *time.Time, _ **float64, s *string) {
			*s = RegimeBucketConfirmed
		}, "entry_withheld"},
		{"regime missing", func(_ *PortfolioPlanPolicy, _ *[]PortfolioTargetEvidence, _ *time.Time, _ **float64, s *string) {
			*s = ""
		}, "cannot_evaluate"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, now, e := portfolioPolicyFixture()
			nlv, stage := new(100000.), RegimeBucketCalm
			tc.change(p, &e, &now, &nlv, &stage)
			rows, err := EvaluatePortfolioTargets(p, now, nlv, stage, e)
			if err != nil || rows[1].Decision != tc.want || rows[1].DesiredQuantity != 0 {
				t.Fatalf("%+v %v", rows, err)
			}
		})
	}
}

func TestPortfolioPolicyHasNoImplicitTargetsAndFingerprintsEveryDecision(t *testing.T) {
	p, _, _ := portfolioPolicyFixture()
	base := Constitution{SchemaVersion: 2, PolicyID: "synthetic", PolicyVersion: 1}
	before := base.FingerprintKey()
	base.PortfolioPlan = p
	with := base.FingerprintKey()
	if before == with {
		t.Fatal("planning mandate not bound")
	}
	p.Targets[0].Priority = 3
	if base.FingerprintKey() == with {
		t.Fatal("priority not bound")
	}
	base.PortfolioPlan = nil
	if base.FingerprintKey() != before {
		t.Fatal("nil policy changed existing fingerprint")
	}
	for _, mutate := range []func(*PortfolioPlanPolicy){
		func(p *PortfolioPlanPolicy) { p.Contract = "" },
		func(p *PortfolioPlanPolicy) { p.Targets[0].Priority = p.Targets[1].Priority },
		func(p *PortfolioPlanPolicy) { p.Targets[0].TargetPctNLV = nil },
		func(p *PortfolioPlanPolicy) { p.Targets[0].LowerPctNLV = new(7.) },
		func(p *PortfolioPlanPolicy) { p.Targets[0].EntryRegimes = nil },
	} {
		p, _, _ := portfolioPolicyFixture()
		mutate(p)
		if err := p.validate(); err == nil || !strings.Contains(err.Error(), "portfolio_plan") {
			t.Fatalf("unapproved policy accepted: %+v", p)
		}
	}
}
