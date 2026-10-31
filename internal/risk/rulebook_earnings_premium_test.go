package risk

import (
	"fmt"
	"math/rand/v2"
	"slices"
	"strings"
	"testing"
)

// Rule 8 read pass for a held name with a perfect date while premium
// was added two and three sessions before the 2026-10-29 print: it freezes
// only an issuer at rule 1's watch level. Rule 20 acts on any premium at
// risk inside the window, whatever the issuer's size.
func TestEarningsPremiumFreezeActsOnAnyPremiumInsideTheWindow(t *testing.T) {
	pol := DefaultRulebookPolicy()
	in := concInputs()
	// 10 calls at 10: 10,000 of a 100,000 NLV, far under rule 1's 30% watch.
	call := optLeg("AAA", "C", 90, 10, 10, 60, 100)
	call.Delta = new(0.5)
	in.Names = []NameInput{withLegs(stockLine("AAA", 100, 100), call)}
	in.Earnings["AAA"] = EarningsInput{Known: true, Date: etDate(2026, 7, 9), TimeOfDay: "amc", SessionsUntil: new(2), Source: "fetched"}
	ev := EvaluateRulebook(in, pol)
	if row := rowByID(t, ev, RuleEarningsSizeFreeze); row.Status != RuleStatusPass {
		t.Fatalf("rule 8 is a size freeze and this issuer is small: %+v", row)
	}
	row := rowByID(t, ev, RuleEarningsPremiumFreeze)
	if row.Status != RuleStatusAct || row.Mode != RuleModeAlert || len(row.Offenders) != 1 || row.ImpactBase != 10000 ||
		!strings.Contains(row.Offenders[0].Note, "1 long option position with 10.0% of NLV of premium at risk, earnings Jul 9 in 2 session(s)") {
		t.Fatalf("rule 20 = %+v", row)
	}
	// Outside the window the same premium passes; without premium the row
	// is not evaluated and names nothing.
	in.Earnings["AAA"] = EarningsInput{Known: true, Date: etDate(2026, 7, 30), TimeOfDay: "amc", SessionsUntil: new(17), Source: "fetched"}
	if row := rowByID(t, EvaluateRulebook(in, pol), RuleEarningsPremiumFreeze); row.Status != RuleStatusPass {
		t.Fatalf("outside the window = %+v", row)
	}
	in.Names = []NameInput{stockLine("AAA", 100, 100)}
	if row := rowByID(t, EvaluateRulebook(in, pol), RuleEarningsPremiumFreeze); row.Status != RuleStatusNotEvaluated || row.Reason != RuleReasonNoLongOptions {
		t.Fatalf("no long option = %+v", row)
	}
}

// Property: rule 20 never passes while a held name carries premium at risk
// without a usable earnings date (unknown, stale, withdrawn, or reported
// with no next date), acts on every name with premium inside the window,
// and never reads a name without premium.
func TestEarningsPremiumFreezeNeverPassesOnAStaleOrWithdrawnDate(t *testing.T) {
	rng := rand.New(rand.NewPCG(20261031, 1029))
	for iter := range 3000 {
		pol := DefaultRulebookPolicy()
		pol.EarningsFreezeSessions = rng.IntN(6)
		in := concInputs()
		var names []NameInput
		premiumNames, unusable, inWindow := 0, 0, 0
		var wantAct, noPremium []string
		for i := range 1 + rng.IntN(4) {
			sym := fmt.Sprintf("N%d", i)
			var n NameInput
			if rng.IntN(2) == 0 {
				n = stockLine(sym, float64(10+rng.IntN(500)), 50)
			} else {
				n = optionsOnly(sym)
			}
			legs := rng.IntN(3)
			for range legs {
				n.Legs = append(n.Legs, optLeg(sym, "C", 100, float64(1+rng.IntN(20)), float64(1+rng.IntN(20)), 30+rng.IntN(200), 100))
			}
			if rng.IntN(4) == 0 {
				// A short leg is not premium at risk.
				n.Legs = append(n.Legs, optLeg(sym, "P", 80, -float64(1+rng.IntN(5)), 2, 40, 100))
			}
			hasPremium := legs > 0
			names = append(names, n)
			if hasPremium {
				premiumNames++
			} else {
				noPremium = append(noPremium, sym)
			}
			switch rng.IntN(7) {
			case 0: // known, fresh, inside the window
				in.Earnings[sym] = EarningsInput{Known: true, Date: etDate(2026, 7, 10), SessionsUntil: new(rng.IntN(pol.EarningsFreezeSessions + 1)), Source: "fetched"}
				if hasPremium {
					inWindow++
					wantAct = append(wantAct, sym)
				}
			case 1: // known, fresh, outside the window
				in.Earnings[sym] = EarningsInput{Known: true, Date: etDate(2026, 8, 20), SessionsUntil: new(pol.EarningsFreezeSessions + 1 + rng.IntN(30)), Source: "fetched"}
			case 2: // stale
				in.Earnings[sym] = EarningsInput{Known: true, Stale: true, Date: etDate(2026, 7, 8), SessionsUntil: new(1), Source: "fetched", Reason: "retained_last_good"}
				if hasPremium {
					unusable++
				}
			case 3: // withdrawn before the day
				in.Earnings[sym] = EarningsInput{Source: "fetched", Reason: "date_withdrawn"}
				if hasPremium {
					unusable++
				}
			case 4: // reported, next date not yet published
				in.Earnings[sym] = EarningsInput{Source: "fetched", Reason: "reported", ReportedOn: etDate(2026, 7, 1)}
				if hasPremium {
					unusable++
				}
			case 5: // no date published
				in.Earnings[sym] = EarningsInput{Source: "fetched", Reason: "no_date_published"}
				if hasPremium {
					unusable++
				}
			case 6: // not observed at all
				if hasPremium {
					unusable++
				}
			}
		}
		in.Names = names
		row := rowByID(t, EvaluateRulebook(in, pol), RuleEarningsPremiumFreeze)
		var want string
		switch {
		case inWindow > 0:
			want = RuleStatusAct
		case unusable > 0:
			want = RuleStatusUnknown
		case premiumNames == 0:
			want = RuleStatusNotEvaluated
		default:
			want = RuleStatusPass
		}
		if row.Status != want {
			t.Fatalf("iteration %d: status %s, want %s (window %d, in %d, unusable %d, premium names %d): %+v", iter, row.Status, want, pol.EarningsFreezeSessions, inWindow, unusable, premiumNames, row)
		}
		if want == RuleStatusUnknown && row.Reason != "earnings_unknown" {
			t.Fatalf("iteration %d: unknown without its reason: %+v", iter, row)
		}
		var named []string
		for _, o := range row.Offenders {
			named = append(named, o.Symbol)
		}
		for _, sym := range wantAct {
			if !slices.Contains(named, sym) {
				t.Fatalf("iteration %d: %s carries premium inside the window and is not an offender: %+v", iter, sym, row)
			}
		}
		for _, sym := range noPremium {
			if slices.Contains(named, sym) {
				t.Fatalf("iteration %d: %s carries no premium and is named: %+v", iter, sym, row)
			}
		}
	}
}
