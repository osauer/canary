package daemon

import (
	"math"
	"strings"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/risk"
	"github.com/osauer/canary/v2/internal/rpc"
)

func fundingRefuterStudyInput() risk.CashSweepCalibrationInput {
	now := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	return risk.CashSweepCalibrationInput{AsOf: now, Source: "frozen_synthetic", PolicyFingerprint: "synthetic",
		SessionEnds: []time.Time{now.Add(24 * time.Hour), now.Add(48 * time.Hour), now.Add(72 * time.Hour), now.Add(96 * time.Hour), now.Add(120 * time.Hour)},
		BaseNAV:     new(100000.), ProtectedFloor: new(10000.), EURRates: map[string]float64{"EUR": 1},
		ClusterDropPct: 20, TakeoverGapPct: 20, ExitParticipationPct: 10,
		Lines: []risk.CashSweepCalibrationLine{{ConID: 101, Currency: "EUR", SecType: "STK", Quantity: 100, Multiplier: 1, CurrentMark: 100,
			PriceOriginalAt: now.Add(-time.Minute), ExitOriginalAt: now.Add(-time.Minute), ADV20InPositionUnits: new(10000.), ExitSpreadUpper: new(0.), ExitFeeUpper: new(0.)}},
	}
}

func TestFundingRefuterDuplicateIdentityCannotOffsetStressLoss(t *testing.T) {
	in := fundingRefuterStudyInput()
	control := risk.StudyCashSweepFiniteHorizons(in)
	if control[0].State != "study_complete" || control[0].WorstLossEUR == nil || *control[0].WorstLossEUR != 2000 {
		t.Fatalf("bad control: %+v", control[0])
	}
	duplicate := in.Lines[0]
	duplicate.Quantity = -100
	in.Lines = append(in.Lines, duplicate)
	for _, study := range risk.StudyCashSweepFiniteHorizons(in) {
		if study.State == "study_complete" || study.WorstLossEUR != nil {
			t.Fatalf("duplicate contract certified offset stress: state=%s loss=%v", study.State, study.WorstLossEUR)
		}
	}
}

func TestFundingRefuterFiniteCallInputsCannotEmitInfiniteShares(t *testing.T) {
	now := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	in := risk.CashSweepOperationalInput{AsOf: now, Source: "frozen_synthetic", PolicyFingerprint: "synthetic", Currencies: []string{"EUR"},
		Positions: []risk.CashSweepFundingPosition{{ConID: 201, Currency: "EUR", SecType: "OPT", Right: "C", Expiry: "20261016", Quantity: -math.MaxFloat64, Strike: 100, Multiplier: 100,
			Deliverable: &risk.CashSweepDeliverable{ConID: 201, UnderlyingConID: 101, Currency: "EUR", Right: "C", Expiry: "20261016", Strike: 100, Multiplier: 100, Source: "frozen_synthetic", AsOf: now.Add(-time.Minute), ValidUntil: now.Add(time.Hour), EarliestSettlement: now.Add(time.Hour), SharesPerContract: 100, ExerciseCashPerContract: 10000}}},
	}
	out := risk.ObserveCashSweepOperationalFunding(in)
	for _, o := range out.Obligations {
		if o.CoveredShares != nil && (math.IsNaN(*o.CoveredShares) || math.IsInf(*o.CoveredShares, 0)) {
			t.Fatalf("nonfinite covered shares emitted: %+v", o)
		}
		if o.GrossPrincipal != nil {
			t.Fatalf("overflow contract has certified principal: %+v", o)
		}
	}
}

func fundingRefuterCallInput() risk.CashSweepOperationalInput {
	now := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	in := risk.CashSweepOperationalInput{AsOf: now, Source: "frozen_synthetic", PolicyFingerprint: "synthetic", TakeoverGapPct: 20, Currencies: []string{"EUR"},
		Positions: []risk.CashSweepFundingPosition{{ConID: 101, Currency: "EUR", SecType: "STK", Quantity: 100, SettledAvailableShares: new(100.), Price: new(100.), PriceAt: now.Add(-time.Minute)}}}
	for _, id := range []int{201, 202} {
		in.Positions = append(in.Positions, risk.CashSweepFundingPosition{ConID: id, Currency: "EUR", SecType: "OPT", Right: "C", Expiry: "20261016", Quantity: -1, Strike: 100, Multiplier: 100,
			Deliverable: &risk.CashSweepDeliverable{ConID: id, UnderlyingConID: 101, Currency: "EUR", Right: "C", Expiry: "20261016", Strike: 100, Multiplier: 100, Source: "frozen_synthetic", AsOf: now.Add(-time.Minute), ValidUntil: now.Add(time.Hour), EarliestSettlement: now.Add(time.Hour), SharesPerContract: 100, ExerciseCashPerContract: 10000}})
	}
	return in
}

func TestFundingRefuterSharesCreditOnceAndExactNativeIdentity(t *testing.T) {
	in := fundingRefuterCallInput()
	out := risk.ObserveCashSweepOperationalFunding(in)
	covered := 0.
	for _, o := range out.Obligations {
		if o.CoveredShares != nil {
			covered += *o.CoveredShares
		}
	}
	if covered != 100 || len(out.Currencies) != 1 || out.Currencies[0].GrossPrincipal == nil || *out.Currencies[0].GrossPrincipal != 12000 {
		t.Fatalf("shares double-credited or lost uncovered funding: %+v", out)
	}
	in.Positions[0].Currency = "USD"
	out = risk.ObserveCashSweepOperationalFunding(in)
	for _, o := range out.Obligations {
		if o.CoveredShares != nil && *o.CoveredShares != 0 || o.GrossPrincipal != nil {
			t.Fatalf("wrong-native shares credited: %+v", o)
		}
	}
	in = fundingRefuterCallInput()
	in.Source = "live_partial"
	out = risk.ObserveCashSweepOperationalFunding(in)
	for _, o := range out.Obligations {
		if o.GrossPrincipal != nil {
			t.Fatalf("synthetic deliverable admitted into live observation: %+v", o)
		}
	}
}

func TestFundingRefuterPositiveStrikePutCannotHaveZeroExerciseCash(t *testing.T) {
	in := fundingRefuterCallInput()
	in.Positions = in.Positions[1:2]
	in.Positions[0].Right = "P"
	in.Positions[0].Deliverable.Right = "P"
	in.Positions[0].Deliverable.ExerciseCashPerContract = 0
	out := risk.ObserveCashSweepOperationalFunding(in)
	if len(out.Currencies) == 1 && out.Currencies[0].GrossPrincipal != nil {
		t.Fatalf("positive strike put reported certified zero funding: %+v", out)
	}
}

func TestFundingRefuterLiveSourceGapCannotBecomeEmptyBook(t *testing.T) {
	now := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	scope := brokerStateScope{Account: "DU1234567", Mode: "paper"}
	e := &proposalEngine{scope: func() brokerStateScope { return scope }}
	for _, variant := range []string{"nil", "scope", "stale", "future", "ledger", "account_source", "positions_source", "receipt_mismatch", "old_account", "old_positions"} {
		t.Run(variant, func(t *testing.T) {
			authority := func() *rpc.AccountDataAuthority {
				return &rpc.AccountDataAuthority{Scope: accountDataScope(scope), Availability: rpc.AccountDataAvailable, Freshness: rpc.AccountDataFreshnessCurrent, AsOf: now}
			}
			acct := &rpc.AccountResult{AccountID: scope.Account, Authority: authority()}
			pos := &rpc.PositionsResult{AccountID: scope.Account, Authority: authority()}
			acct.Authority.Source = rpc.AccountDataSourceAccountSummaryRequest
			pos.Authority.Source = rpc.AccountDataSourcePortfolioStream
			cash := cashSweepInput{AccountReceiptAt: now, PositionsReceiptAt: now}
			control, _ := e.observeCashSweepFunding(acct, pos, scope, cash, now)
			if control.State != "partial" {
				t.Fatalf("bad valid source control: %+v", control)
			}
			switch variant {
			case "nil":
				acct = nil
			case "scope":
				pos.AccountID = "DU7654321"
			case "stale":
				pos.Authority.Freshness = rpc.AccountDataFreshnessStale
			case "future":
				cash.PositionsReceiptAt = now.Add(time.Minute)
			case "ledger":
				cash.LedgerReason = "unknown_native_cash"
			case "account_source":
				acct.Authority.Source = "unsupported"
			case "positions_source":
				pos.Authority.Source = rpc.AccountDataSourceAccountUpdatesCache
			case "receipt_mismatch":
				acct.Authority.AsOf = now.Add(-time.Second)
			case "old_account":
				acct.Authority.AsOf = now.Add(-time.Hour)
				cash.AccountReceiptAt = acct.Authority.AsOf
			case "old_positions":
				pos.Authority.AsOf = now.Add(-time.Hour)
				cash.PositionsReceiptAt = pos.Authority.AsOf
			}
			out, studies := e.observeCashSweepFunding(acct, pos, scope, cash, now)
			if out.State != "unavailable" || len(out.Currencies) != 0 || len(out.Obligations) != 0 {
				t.Fatalf("source gap reported clean book: %+v", out)
			}
			for _, s := range studies {
				if s.State == "study_complete" || s.NAVAfterEUR != nil {
					t.Fatalf("source gap became stress authority: %+v", s)
				}
			}
		})
	}
}

func TestFundingRefuterOptionStudyPreservesMissingAndInvalidSources(t *testing.T) {
	fixture := func() risk.CashSweepCalibrationInput {
		in := fundingRefuterStudyInput()
		in.VolShockPoints, in.RateShockBPS = new(0.1), new(100.)
		p := &in.Lines[0]
		p.SecType, p.Right, p.ExerciseStyle = "OPT", "P", "american"
		p.Quantity, p.Multiplier, p.CurrentMark, p.Spot, p.Strike = -1, 100, 10, 100, 100
		p.Expiry, p.ExactVanillaDeliverable = in.AsOf.Add(30*24*time.Hour), true
		p.IV, p.RiskFreeRate, p.DividendYield = new(0.2), new(0.02), new(0.)
		return in
	}
	for _, s := range risk.StudyCashSweepFiniteHorizons(fixture()) {
		if s.State != "study_complete" || s.WorstLossEUR == nil || math.IsNaN(*s.WorstLossEUR) || math.IsInf(*s.WorstLossEUR, 0) {
			t.Fatalf("bad exact option control: %+v", s)
		}
	}
	for _, name := range []string{"iv", "rate", "dividend", "deliverable", "spot", "exit", "quote_clock", "sessions", "currency", "overflow"} {
		t.Run(name, func(t *testing.T) {
			in := fixture()
			p := &in.Lines[0]
			switch name {
			case "iv":
				p.IV = nil
			case "rate":
				p.RiskFreeRate = new(math.NaN())
			case "dividend":
				p.DividendYield = nil
			case "deliverable":
				p.ExactVanillaDeliverable = false
			case "spot":
				p.Spot = math.Inf(1)
			case "exit":
				p.ADV20InPositionUnits = nil
			case "quote_clock":
				p.PriceOriginalAt = in.AsOf.Add(time.Minute)
			case "sessions":
				in.SessionEnds[0] = in.AsOf
			case "currency":
				p.Currency = "USD"
			case "overflow":
				p.Quantity = -math.MaxFloat64
			}
			for _, s := range risk.StudyCashSweepFiniteHorizons(in) {
				if s.State == "study_complete" || s.WorstLossEUR != nil || s.NAVAfterEUR != nil {
					t.Fatalf("invalid metadata certified study: %+v", s)
				}
			}
		})
	}
}

func TestFundingRefuterCompleteStudyDoesNotAuthorizeSweeping(t *testing.T) {
	now := cashSweepTestNow()
	policy := cashSweepTestPolicy(rpc.CashSweepModeActive, 100000)
	policy.Buckets.CashSweep.CurrencyPriority = rpc.CashSweepPriorityEURFirst
	in := observedSweepFundingInput(map[string]float64{"EUR": 50000, "USD": 50000})
	in.FundingEvidence = nil
	observation := risk.ObserveCashSweepOperationalFunding(fundingRefuterCallInput())
	in.OperationalFunding = &observation
	in.CalibrationStudies = risk.StudyCashSweepFiniteHorizons(fundingRefuterStudyInput())
	plan := cashSweepPlanFor(policy, in, now)
	if !strings.Contains(plan.status.ReserveReason, "reserve_calibration_required") {
		t.Fatalf("partial observations promoted to reserve evidence: %+v", plan.status)
	}
	for _, currency := range plan.currencies {
		if currency.side != "" {
			t.Fatalf("study authorized sweep action: %+v", currency)
		}
	}
}
