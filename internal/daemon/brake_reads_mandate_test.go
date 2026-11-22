package daemon

import (
	"bufio"
	"encoding/json"
	"math"
	"os"
	"testing"
	"time"

	"github.com/osauer/canary/v3/internal/mandate"
	"github.com/osauer/canary/v3/internal/risk"
	"github.com/osauer/canary/v3/internal/rpc"
)

// brakeMandateStore is a capital store that booked a 10,000 deposit before
// its first reading (so the mandate's series and the brake's differ by the
// flows at the start), then read 300,000, a single higher print of 301,000
// and, five minutes after that, 300,400.
func brakeMandateStore(t *testing.T, c *risk.Constitution) (*riskCapitalStore, time.Time) {
	t.Helper()
	st := newTestRiskCapitalStore(t)
	st.now = func() time.Time { return mandateTestNow }
	reconcileNow(t, st)
	if _, err := st.ApplyCapitalEventForPolicy(rpc.CapitalEventParams{Type: "deposit", AmountBase: 10000}, rpc.OrderOriginHumanTTY, c); err != nil {
		t.Fatal(err)
	}
	at := mandateTestNow.Add(-time.Hour)
	observeWithPnL(st, c, 300000, 0, at)
	observeWithPnL(st, c, 301000, 1000, at.Add(time.Minute))
	observeWithPnL(st, c, 300400, 400, at.Add(6*time.Minute))
	return st, at
}

func brakeMandateDrawdowns(t *testing.T, st *riskCapitalStore, c *risk.Constitution) (brake float64, capital mandate.Capital) {
	t.Helper()
	rep := st.Report(c, nil, testLiveObserveScope)
	if rep.DrawdownBase == nil {
		t.Fatalf("the capital report has no drawdown: %+v", rep)
	}
	capital, _ = st.MandateCapitalForScope(c, brokerStateScope{}, mandateTestNow)
	if !capital.Known {
		t.Fatalf("the mandate's capital is unknown: %v", capital.Reasons)
	}
	return *rep.DrawdownBase, capital
}

// While a [mandate] is active the drawdown brake measures the fall from the
// mandate's confirmed high, not from its own single-print peak: the two read
// the same drawdown, so the brake blocks where the mandate's maximum loss is
// reached and not a print earlier. The stored peak keeps its own ratchet and
// sits above the mark; no state edit brings the two together.
func TestBrakeMeasuresTheFallFromTheMandatesConfirmedHigh(t *testing.T) {
	c := testMandateConstitution()
	st, at := brakeMandateStore(t, c)
	if st.state.Mandate == nil || st.state.Mandate.FlowsAtStart == 0 {
		t.Fatalf("fixture: the mandate series must start with flows on the books: %+v", st.state.Mandate)
	}
	if stored, mark := st.state.AdjustedPeakBase, st.state.Mandate.HWMAdjustedEquity-st.state.Mandate.FlowsAtStart; stored <= mark {
		t.Fatalf("fixture: the stored peak %.0f must sit above the mandate's mark %.0f", stored, mark)
	}
	brake, capital := brakeMandateDrawdowns(t, st, c)
	if brake != capital.DrawdownBase {
		t.Fatalf("at the confirmed high the brake reads a drawdown of %.0f, the mandate %.0f", brake, capital.DrawdownBase)
	}
	observeWithPnL(st, c, 299000, -1000, at.Add(10*time.Minute))
	brake, capital = brakeMandateDrawdowns(t, st, c)
	if brake != capital.DrawdownBase || brake != 1400 {
		t.Fatalf("after a fall of 1,400 from the confirmed high the brake reads %.0f, the mandate %.0f", brake, capital.DrawdownBase)
	}
	rep := st.Report(c, nil, testLiveObserveScope)
	if rep.AdjustedPeakBase == nil || rep.EquityBase == nil || rep.CumExternalFlowsBase == nil ||
		math.Abs(*rep.AdjustedPeakBase-(*rep.EquityBase-*rep.CumExternalFlowsBase)-brake) > 1e-6 {
		t.Fatalf("the reported peak, equity and flows do not add up to the reported drawdown %.0f: %+v", brake, rep)
	}
	if !rep.PeakAsOf.Equal(at.Add(6 * time.Minute)) {
		t.Fatalf("the reported peak is dated %s, want the confirming reading", rep.PeakAsOf)
	}
}

// Without a [mandate] the brake keeps its own peak: the single higher print
// counts, as it always has.
func TestBrakeKeepsItsOwnPeakWithoutAMandate(t *testing.T) {
	c := testConstitutionV3()
	st, _ := brakeMandateStore(t, c)
	rep := st.Report(c, nil, testLiveObserveScope)
	if rep.DrawdownBase == nil || *rep.DrawdownBase != 600 {
		t.Fatalf("drawdown = %v, want 600 from the single print", rep.DrawdownBase)
	}
	if kinds := brakeJournalKinds(t, "brake_peak_source_changed"); len(kinds) != 0 {
		t.Fatalf("a store without a mandate journaled a peak source: %v", kinds)
	}
}

// A journaled human reset or peak correction lowers the brake's own peak
// under the mandate's mark. The brake then measures from that lower peak: the
// mandate never raises the high a human set, so the block never moves
// earlier than it stood, and the commands keep their effect.
func TestBrakeNeverMeasuresFromAboveItsOwnPeak(t *testing.T) {
	c := testMandateConstitution()
	st, at := brakeMandateStore(t, c)
	observeWithPnL(st, c, 296000, -4000, at.Add(10*time.Minute))
	if err := st.ResetDrawdown("test: accept the loss", c); err != nil {
		t.Fatal(err)
	}
	brake, capital := brakeMandateDrawdowns(t, st, c)
	if brake != 0 || capital.DrawdownBase != 4400 {
		t.Fatalf("after the reset the brake reads %.0f (want 0), the mandate %.0f (want 4,400)", brake, capital.DrawdownBase)
	}
}

// Which peak the brake measures from is journaled whenever it changes, with
// both figures: the block point never moves without a line saying so. A
// daemon upgraded onto a persisted state whose stored peak already sits above
// the mandate's mark journals the move at its first reading.
func TestBrakePeakSourceChangeIsJournaled(t *testing.T) {
	c := testMandateConstitution()
	st, at := brakeMandateStore(t, c)
	const kind = "brake_peak_source_changed"
	if entries := brakeJournalKinds(t, kind); len(entries) != 1 || entries[0]["source"] != "mandate" {
		t.Fatalf("the first reading under a mandate journals its source once: %v", entries)
	}
	// The state an older daemon persisted carries no source.
	st.state.BrakePeakFromMandate = false
	observeWithPnL(st, c, 300400, 400, at.Add(7*time.Minute))
	entries := brakeJournalKinds(t, kind)
	if last := entries[len(entries)-1]; len(entries) != 2 || last["source"] != "mandate" || last["stored_peak_base"] != 291000.0 || last["mandate_peak_base"] != 290400.0 || last["brake_peak_base"] != 290400.0 {
		t.Fatalf("the move from the stored peak to the mandate's mark is not journaled with both peaks: %v", entries)
	}
	observeWithPnL(st, c, 296000, -4000, at.Add(10*time.Minute))
	if err := st.ResetDrawdown("test: accept the loss", c); err != nil {
		t.Fatal(err)
	}
	observeWithPnL(st, c, 296000, -4000, at.Add(11*time.Minute))
	entries = brakeJournalKinds(t, kind)
	if last := entries[len(entries)-1]; len(entries) != 3 || last["source"] != "stored" || last["brake_peak_base"] != 286000.0 || last["mandate_peak_base"] != 290400.0 {
		t.Fatalf("the return to the brake's own peak after a reset is not journaled: %v", entries)
	}
}

// brakeJournalKinds reads the risk policy journal's entries of one kind.
func brakeJournalKinds(t *testing.T, kind string) []map[string]any {
	t.Helper()
	path, err := defaultTradingStatePath(riskPolicyJournalFile)
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var out []map[string]any
	for sc := bufio.NewScanner(f); sc.Scan(); {
		var entry map[string]any
		if err := json.Unmarshal(sc.Bytes(), &entry); err != nil {
			t.Fatal(err)
		}
		if entry["kind"] == kind {
			out = append(out, entry)
		}
	}
	return out
}
