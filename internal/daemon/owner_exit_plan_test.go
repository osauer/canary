package daemon

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/osauer/canary/v3/internal/marketcal"
	"github.com/osauer/canary/v3/internal/rpc"
	ibkrlib "github.com/osauer/canary/v3/pkg/ibkr"
)

var exitPlanTestOpen = time.Date(2026, 11, 24, 14, 50, 0, 0, time.UTC)

// exitPlanTestTerms are a valid synthetic plan: a 20-call SYNA line, a first
// sale of 10 at 4.20 or better, and a 3% trail from a 49.50 stop.
func exitPlanTestTerms() (first, trail rpc.QueuedAuthTerms) {
	option := rpc.ContractParams{ConID: 910001, Symbol: "SYNA", SecType: "OPT", Currency: "USD", Expiry: "20261218", Strike: 55, Right: "C", Multiplier: 100}
	base := rpc.QueuedAuthTerms{Version: queuedPlanTermsVersion, QueueID: "q", AccountID: "U1234567", AccountMode: "paper", Key: "owner:910001:SELL", Bucket: "owner_order",
		RevisionAtQueue: "owner", Contract: option, Action: "SELL", PositionQuantity: 20, MaxSpreadPctOfMid: 25, Currency: "USD", Market: "us_options",
		SessionDate: "2026-11-24", NotBefore: exitPlanTestOpen, TIF: "DAY", ArmDeadline: exitPlanTestOpen.Add(-time.Hour)}
	first, trail = base, base
	first.PositionEffect, first.MaxQuantity, first.Style, first.WorstPrice, first.ReferenceMark, first.NotAfter = "reduce", 10, "midpoint_bounded", 4.20, 4.20, exitPlanTestOpen.Add(time.Hour)
	first.OwnerOrder = &rpc.QueuedOwnerOrder{OrderType: "LMT", Plan: &rpc.QueuedPlan{PlanID: "plan1", Role: rpc.QueuedPlanRoleFirstSale, WaitMinutes: 20}}
	trail.PositionEffect, trail.MaxQuantity, trail.Style, trail.NotAfter = "close", 20, rpc.QueuedAuthStyleStockTrail, exitPlanTestOpen.Add(6*time.Hour)
	trail.OwnerOrder = &rpc.QueuedOwnerOrder{OrderType: "LMT", Plan: &rpc.QueuedPlan{PlanID: "plan1", Role: rpc.QueuedPlanRoleTrail, WaitMinutes: 20},
		StockTrail: &rpc.QueuedStockTrail{Underlying: rpc.ContractParams{ConID: 900001, Symbol: "SYNA", SecType: "STK", Currency: "USD", Exchange: "SMART"},
			TrailPercent: 3, InitialStopPrice: 49.50, ReferencePrice: 51.24, ReferenceAt: exitPlanTestOpen.Add(-10 * time.Hour)}}
	return first, trail
}

// The terms encode as they always did for v1 and v2, a zero worst price
// included (a full-position stop stores it), so no stored digest moves; a v3
// trail omits its absent worst price.
func TestQueuedTermsEncodingKeepsStoredBytes(t *testing.T) {
	type plain rpc.QueuedAuthTerms
	stop := rpc.QueuedAuthTerms{Version: 2, QueueID: "q", Style: "broker_trail", OwnerOrder: &rpc.QueuedOwnerOrder{OrderType: "TRAIL", FullPosition: true}}
	mid := rpc.QueuedAuthTerms{Version: 2, QueueID: "q", Style: "midpoint_bounded", WorstPrice: 24.99, OwnerOrder: &rpc.QueuedOwnerOrder{OrderType: "LMT"}}
	v1 := rpc.QueuedAuthTerms{Version: 1, QueueID: "q", Style: "bounded_limit", Concession: 0.5}
	for _, terms := range []rpc.QueuedAuthTerms{stop, mid, v1} {
		got, _ := json.Marshal(terms)
		want, _ := json.Marshal(plain(terms))
		if string(got) != string(want) || !strings.Contains(string(got), `"worst_price":`) {
			t.Fatalf("v%d terms encode differently:\n got %s\nwant %s", terms.Version, got, want)
		}
	}
	first, trail := exitPlanTestTerms()
	raw, _ := json.Marshal(trail)
	if strings.Contains(string(raw), "worst_price") || !strings.Contains(string(raw), `"stock_trail":`) {
		t.Fatalf("trail terms: %s", raw)
	}
	raw, _ = json.Marshal(first)
	if !strings.Contains(string(raw), `"worst_price":4.2`) || strings.Contains(string(raw), "stock_trail") {
		t.Fatalf("first-sale terms: %s", raw)
	}
	var back rpc.QueuedAuthTerms
	if err := json.Unmarshal(raw, &back); err != nil || back.WorstPrice != 4.20 || back.OwnerOrder.Plan.Role != rpc.QueuedPlanRoleFirstSale {
		t.Fatalf("round trip: %+v %v", back, err)
	}
}

func TestExitPlanTermsValidation(t *testing.T) {
	first, trail := exitPlanTestTerms()
	if p := ownerQueueProblem(first); p != "" {
		t.Fatalf("valid first sale refused: %s", p)
	}
	if p := ownerQueueProblem(trail); p != "" {
		t.Fatalf("valid trail refused: %s", p)
	}
	clone := func(t rpc.QueuedAuthTerms) rpc.QueuedAuthTerms {
		o := *t.OwnerOrder
		plan := *o.Plan
		o.Plan = &plan
		if o.StockTrail != nil {
			st := *o.StockTrail
			o.StockTrail = &st
		}
		t.OwnerOrder = &o
		return t
	}
	for name, edit := range map[string]func(f, tr *rpc.QueuedAuthTerms){
		"no plan":                  func(f, tr *rpc.QueuedAuthTerms) { tr.OwnerOrder.Plan = nil },
		"unknown role":             func(f, tr *rpc.QueuedAuthTerms) { tr.OwnerOrder.Plan.Role = "second_sale" },
		"wait above 120":           func(f, tr *rpc.QueuedAuthTerms) { tr.OwnerOrder.Plan.WaitMinutes = 121 },
		"wait zero":                func(f, tr *rpc.QueuedAuthTerms) { f.OwnerOrder.Plan.WaitMinutes = 0 },
		"empty plan id":            func(f, tr *rpc.QueuedAuthTerms) { f.OwnerOrder.Plan.PlanID = "" },
		"stock":                    func(f, tr *rpc.QueuedAuthTerms) { tr.Contract.SecType = "STK" },
		"buy":                      func(f, tr *rpc.QueuedAuthTerms) { f.Action = "BUY" },
		"first sale whole line":    func(f, tr *rpc.QueuedAuthTerms) { f.MaxQuantity = 20 },
		"first sale negative min":  func(f, tr *rpc.QueuedAuthTerms) { f.WorstPrice = -0.05 },
		"first sale long window":   func(f, tr *rpc.QueuedAuthTerms) { f.NotAfter = f.NotBefore.Add(2 * time.Hour) },
		"first sale stock line":    func(f, tr *rpc.QueuedAuthTerms) { f.OwnerOrder.StockTrail = tr.OwnerOrder.StockTrail },
		"first sale adaptive":      func(f, tr *rpc.QueuedAuthTerms) { f.OwnerOrder.AdaptivePriority = "Normal" },
		"first sale outside hours": func(f, tr *rpc.QueuedAuthTerms) { f.OwnerOrder.OutsideRTH = true },
		"trail with minimum":       func(f, tr *rpc.QueuedAuthTerms) { tr.WorstPrice = 1 },
		"trail part of line":       func(f, tr *rpc.QueuedAuthTerms) { tr.MaxQuantity = 19 },
		"trail above line":         func(f, tr *rpc.QueuedAuthTerms) { tr.MaxQuantity, tr.PositionQuantity = 21, 20 },
		"trail no stock line":      func(f, tr *rpc.QueuedAuthTerms) { tr.OwnerOrder.StockTrail = nil },
		"trail percent zero":       func(f, tr *rpc.QueuedAuthTerms) { tr.OwnerOrder.StockTrail.TrailPercent = 0 },
		"trail percent above 50":   func(f, tr *rpc.QueuedAuthTerms) { tr.OwnerOrder.StockTrail.TrailPercent = 50.5 },
		"trail no starting stop":   func(f, tr *rpc.QueuedAuthTerms) { tr.OwnerOrder.StockTrail.InitialStopPrice = 0 },
		"trail on an option":       func(f, tr *rpc.QueuedAuthTerms) { tr.OwnerOrder.StockTrail.Underlying.SecType = "OPT" },
		"trail other symbol":       func(f, tr *rpc.QueuedAuthTerms) { tr.OwnerOrder.StockTrail.Underlying.Symbol = "SYNB" },
		"trail no conid":           func(f, tr *rpc.QueuedAuthTerms) { tr.OwnerOrder.StockTrail.Underlying.ConID = 0 },
		"trail broker trail":       func(f, tr *rpc.QueuedAuthTerms) { tr.OwnerOrder.Trail = &rpc.OrderTrailSpec{InitialStopPrice: 1} },
		"trail full position":      func(f, tr *rpc.QueuedAuthTerms) { tr.OwnerOrder.FullPosition = true },
		"trail empty window":       func(f, tr *rpc.QueuedAuthTerms) { tr.NotAfter = tr.NotBefore },
		"trail two days":           func(f, tr *rpc.QueuedAuthTerms) { tr.NotAfter = tr.NotBefore.Add(48 * time.Hour) },
		"trail midpoint style":     func(f, tr *rpc.QueuedAuthTerms) { tr.Style = "midpoint_bounded" },
		"trail concession":         func(f, tr *rpc.QueuedAuthTerms) { tr.Concession = 0.5 },
		"fractional line":          func(f, tr *rpc.QueuedAuthTerms) { tr.PositionQuantity = 20.5 },
		"put":                      func(f, tr *rpc.QueuedAuthTerms) { tr.Contract.Right = "P" },
		"wait and start both":      func(f, tr *rpc.QueuedAuthTerms) { tr.OwnerOrder.Plan.StartInMinutes = new(5) },
		"start above 120": func(f, tr *rpc.QueuedAuthTerms) {
			tr.OwnerOrder.Plan.WaitMinutes, tr.OwnerOrder.Plan.StartInMinutes = 0, new(121)
		},
		"start negative": func(f, tr *rpc.QueuedAuthTerms) {
			tr.OwnerOrder.Plan.WaitMinutes, tr.OwnerOrder.Plan.StartInMinutes = 0, new(-1)
		},
	} {
		f, tr := clone(first), clone(trail)
		edit(&f, &tr)
		if ownerQueueProblem(f) == "" && ownerQueueProblem(tr) == "" {
			t.Errorf("%s: accepted", name)
		}
	}
	// A version-2 record never carries a plan's terms.
	v2 := clone(first)
	v2.Version = 2
	v2.OwnerOrder.AdaptivePriority = ""
	if ownerQueueProblem(v2) == "" {
		t.Fatal("a v2 record with a plan was accepted")
	}
}

func TestExitPlanRecordReadability(t *testing.T) {
	_, trail := exitPlanTestTerms()
	digest, _ := queuedTermsDigest(trail)
	rec := queuedAuthRecord{Version: queuedDocumentVersion, Terms: trail, TermsDigest: digest, State: rpc.QueuedAuthArmed}
	if p := queuedRecordProblem(rec); p != "" {
		t.Fatalf("valid trail record: %s", p)
	}
	rec.Watch = queuedTrailState{HighWater: 52, StopPrice: 50.44}
	raw, _ := json.Marshal(rec)
	back, err := decodeQueuedRecord(raw)
	if err != nil || back.problem != "" || back.Watch != rec.Watch {
		t.Fatalf("stored trail did not read back: %+v %v %q", back.Watch, err, back.problem)
	}
	first, _ := exitPlanTestTerms()
	digest, _ = queuedTermsDigest(first)
	other := queuedAuthRecord{Version: queuedDocumentVersion, Terms: first, TermsDigest: digest, State: rpc.QueuedAuthArmed, Watch: queuedTrailState{StopPrice: 1}}
	if queuedRecordProblem(other) == "" {
		t.Fatal("a watch on a first sale was accepted")
	}
}

// stop = max(initial, high * (1 - pct/100)); the first price seeds the high,
// and the stop never falls.
func TestTrailStopArithmetic(t *testing.T) {
	st := rpc.QueuedStockTrail{TrailPercent: 3, InitialStopPrice: 49.50}
	at := exitPlanTestOpen
	w, raised := trailAdvance(queuedTrailState{}, st, 50.00, at)
	if !raised || w.HighWater != 50 || w.StopPrice != 49.50 {
		t.Fatalf("seed: %+v", w)
	}
	w, raised = trailAdvance(w, st, 52.40, at.Add(time.Minute))
	if want := 50.828; !raised || w.HighWater != 52.40 || w.StopPrice != want {
		t.Fatalf("rally: %+v want stop %g", w, want)
	}
	high := w
	w, raised = trailAdvance(w, st, 51.00, at.Add(2*time.Minute))
	if raised || w != high {
		t.Fatalf("a lower price moved the watch: %+v", w)
	}
	// A restart reads back a stop above what the stored high would set (an
	// older rounding, say): it is kept, never lowered.
	kept := queuedTrailState{HighWater: 50, StopPrice: 49.90}
	w, _ = trailAdvance(kept, st, 50.10, at)
	if w.StopPrice != 49.90 {
		t.Fatalf("stop lowered to %g", w.StopPrice)
	}
	if got := trailStop(49.50, 0, 3); got != 49.50 {
		t.Fatalf("no high: %g", got)
	}
	if _, raised := trailAdvance(kept, st, 0, at); raised {
		t.Fatal("a missing price moved the watch")
	}
	if q, ok := trailSendQuantity(10, 20); !ok || q != 10 {
		t.Fatalf("sells what is held: %d %v", q, ok)
	}
	if q, ok := trailSendQuantity(25, 20); !ok || q != 20 {
		t.Fatalf("never above the signed maximum: %d %v", q, ok)
	}
	if _, ok := trailSendQuantity(10.5, 20); ok {
		t.Fatal("fractional line accepted")
	}
}

func TestExitPlanDigest(t *testing.T) {
	sum := sha256.Sum256([]byte("aa11\nbb22"))
	if got := exitPlanDigest([]string{"aa11", "bb22"}); got != hex.EncodeToString(sum[:]) {
		t.Fatalf("plan digest %s", got)
	}
	sum = sha256.Sum256([]byte("bb22"))
	if got := exitPlanDigest([]string{"bb22"}); got != hex.EncodeToString(sum[:]) {
		t.Fatalf("trail-only plan digest %s", got)
	}
}

// A bounded limit with no worst price says so; a zero worst price alone is
// still refused, never read as no bound.
func TestBoundedLimitNoWorstPriceIsExplicit(t *testing.T) {
	bid, ask := 2.00, 2.10
	q := rpc.OrderQuoteSnapshot{Bid: &bid, Ask: &ask, DataType: rpc.MarketDataLive}
	c := rpc.ContractParams{SecType: "OPT", MinTick: 0.01}
	if _, err := boundedLimitPrice("SELL", &rpc.OrderBoundedLimit{MaxSpreadPctOfMid: 25}, nil, c, q); err == nil {
		t.Fatal("zero worst price accepted as no bound")
	}
	limit, err := boundedLimitPrice("SELL", &rpc.OrderBoundedLimit{MaxSpreadPctOfMid: 25, NoWorstPrice: true}, nil, c, q)
	if err != nil || limit != 2.05 {
		t.Fatalf("no-minimum sale at the mid: %g %v", limit, err)
	}
	if _, err := boundedLimitPrice("SELL", &rpc.OrderBoundedLimit{MaxSpreadPctOfMid: 25, NoWorstPrice: true, WorstPrice: 1}, nil, c, q); err == nil {
		t.Fatal("no-worst bound with a worst price accepted")
	}
	wide := 3.00
	q.Ask = &wide
	if _, err := boundedLimitPrice("SELL", &rpc.OrderBoundedLimit{MaxSpreadPctOfMid: 25, NoWorstPrice: true}, nil, c, q); err == nil {
		t.Fatal("a spread above the limit priced")
	}
}

// The loop checks a trail at least every ten seconds while it is inside its
// window, sooner while a read at or below the line awaits confirmation, and
// not at all otherwise: no trail, before not_before, after not_after, or an
// ended record.
func TestQueuedTrailTickOnlyInsideTheWindow(t *testing.T) {
	rig := newQueueTestRig(t)
	e := rig.engine
	if _, ok := e.queuedTrailTick(exitPlanTestOpen); ok {
		t.Fatal("ticks with no trail")
	}
	_, trail := exitPlanTestTerms()
	trail.QueueID = "trail1"
	digest, _ := queuedTermsDigest(trail)
	if err := e.queued.update(t.Context(), exitPlanTestOpen, func(records map[string]*queuedAuthRecord) []queuedAuthEvent {
		r := &queuedAuthRecord{Version: queuedDocumentVersion, Terms: trail, TermsDigest: digest, CreatedAt: exitPlanTestOpen}
		records[trail.QueueID] = r
		return []queuedAuthEvent{r.transition(queuedEventArmed, rpc.QueuedAuthArmed, exitPlanTestOpen)}
	}); err != nil {
		t.Fatal(err)
	}
	for name, at := range map[string]time.Time{"before not_before": trail.NotBefore.Add(-time.Second), "at not_after": trail.NotAfter} {
		if _, ok := e.queuedTrailTick(at); ok {
			t.Fatalf("ticks %s", name)
		}
	}
	in := trail.NotBefore.Add(time.Minute)
	if d, ok := e.queuedTrailTick(in); !ok || d != queuedTrailCheckEvery {
		t.Fatalf("inside the window: %s %v", d, ok)
	}
	e.setTrailLive(trail.QueueID, queuedTrailLive{breachAt: in.Add(-2 * time.Second)})
	if d, ok := e.queuedTrailTick(in); !ok || d != 3*time.Second {
		t.Fatalf("awaiting confirmation: %s %v", d, ok)
	}
	if err := e.queued.update(t.Context(), in, func(records map[string]*queuedAuthRecord) []queuedAuthEvent {
		return []queuedAuthEvent{records[trail.QueueID].resolve(queuedEventCancelled, rpc.QueuedAuthCancelled, queuedReasonOwnerCancelled, "", in)}
	}); err != nil {
		t.Fatal(err)
	}
	if _, ok := e.queuedTrailTick(in); ok {
		t.Fatal("ticks for an ended trail")
	}
}

// A halt or a limit-up-limit-down pause on the stock makes its quote no
// price to fire on; a recent, ended one does not.
func TestStockHaltReason(t *testing.T) {
	events := rpc.MarketEventsResult{BySymbol: map[string][]rpc.MarketEventFlag{
		"SYNA": {{ID: rpc.MarketEventHaltRegulatoryOrNews, Status: rpc.MarketEventStatusActive, Label: "Halt"}},
		"SYNB": {{ID: rpc.MarketEventLULDRecent, Status: rpc.MarketEventStatusActive, Label: "LULD active"}},
		"SYNC": {{ID: rpc.MarketEventHaltRegulatoryOrNews, Status: rpc.MarketEventStatusRecent}},
	}}
	for symbol, halted := range map[string]bool{"SYNA": true, "syna": true, "SYNB": true, "SYNC": false, "SYNX": false} {
		if got := stockHaltReason(events, symbol) != ""; got != halted {
			t.Errorf("%s: halted %v, want %v", symbol, got, halted)
		}
	}
}

// The plan's sales close or reduce a held long line, so neither the
// drawdown brake nor a sell-only (risk-adding) gate ever holds them.
func TestExitPlanSalesNeverAddRisk(t *testing.T) {
	s := &Server{}
	first, trail := exitPlanTestTerms()
	for _, tc := range []struct {
		terms  rpc.QueuedAuthTerms
		after  float64
		effect string
	}{{first, 10, rpc.OrderPositionEffectReduce}, {trail, 0, rpc.OrderPositionEffectClose}} {
		draft := rpc.OrderDraft{Action: "SELL", Contract: tc.terms.Contract, Quantity: tc.terms.MaxQuantity}
		impact := rpc.OrderPositionImpact{Before: 20, After: tc.after, Effect: tc.effect}
		if s.riskAddingOrder(draft, impact) {
			t.Fatalf("%s reads as adding risk", tc.terms.OwnerOrder.Plan.Role)
		}
		if b, _ := s.riskPolicyBrake(draft, impact); b != nil {
			t.Fatalf("%s braked: %+v", tc.terms.OwnerOrder.Plan.Role, b)
		}
	}
}

// A fired trail's sale is followed attempt by attempt: an ended order adds
// what it sold and returns the trail to send the rest, the line sold ends
// it, the window's end ends it, and an order gone without a recorded end is
// held and never resent.
func TestTrailSaleProgress(t *testing.T) {
	_, trail := exitPlanTestTerms()
	at := trail.NotBefore.Add(time.Hour)
	sent := func() *queuedAuthRecord {
		return &queuedAuthRecord{Terms: trail, State: rpc.QueuedAuthSent, OrderRef: "ref1", PreviewTokenID: "tok1", QuantitySent: 20, LimitPrice: 1.60,
			Watch: queuedTrailState{TriggeredAt: trail.NotBefore, SaleAttempts: 1}}
	}
	r := sent()
	planEnd := func(queuedAuthRecord) (string, string) {
		return queuedReasonPlanValidityEnded, "the plan's validity ended"
	}
	if _, ok := trailSaleProgress(r, rpc.OrderView{LifecycleStatus: rpc.OrderLifecycleCancelled, Filled: 8}, at, planEnd); !ok ||
		r.State != rpc.QueuedAuthHeld || r.HoldCode != queuedHoldTrailSaleEnded || r.Watch.SaleFilled != 8 || r.OrderRef != "" || r.PreviewTokenID != "" ||
		r.Watch.SaleStep != 1 || r.Watch.LastLimit != 1.60 {
		t.Fatalf("a cancelled attempt: %+v", r)
	}
	r = sent()
	r.Watch.SaleFilled = 8
	r.QuantitySent = 12
	if _, ok := trailSaleProgress(r, rpc.OrderView{LifecycleStatus: rpc.OrderLifecycleFilled, Filled: 12}, at, planEnd); !ok || r.State != rpc.QueuedAuthFilled || r.Watch.SaleFilled != 20 {
		t.Fatalf("the line sold: %+v", r)
	}
	r = sent()
	if _, ok := trailSaleProgress(r, rpc.OrderView{LifecycleStatus: rpc.OrderLifecycleRejected}, at, planEnd); !ok || r.State != rpc.QueuedAuthHeld || r.HoldCode != queuedHoldTrailSaleEnded {
		t.Fatalf("a rejected attempt: %+v", r)
	}
	r = sent()
	if _, ok := trailSaleProgress(r, rpc.OrderView{LifecycleStatus: rpc.OrderLifecycleCancelled, Filled: 5}, trail.NotAfter, planEnd); !ok || r.State != rpc.QueuedAuthPartiallyFilled || r.ReasonCode != queuedReasonPlanValidityEnded {
		t.Fatalf("at the window's end: %+v", r)
	}
	r = sent()
	if _, ok := trailSaleProgress(r, rpc.OrderView{LifecycleStatus: rpc.OrderLifecycleClosedReconciled}, at, planEnd); !ok || r.State != rpc.QueuedAuthHeld || r.HoldCode != queuedHoldTrailSaleUnconfirmed || r.OrderRef != "ref1" {
		t.Fatalf("an end not recorded: %+v", r)
	}
	if _, ok := trailSaleProgress(r, rpc.OrderView{LifecycleStatus: rpc.OrderLifecycleClosedReconciled}, at, planEnd); ok {
		t.Fatal("an unconfirmed attempt was handled twice")
	}
	r = sent()
	r.State, r.HoldCode = rpc.QueuedAuthHeld, queuedReasonSendUnclear
	if _, ok := trailSaleProgress(r, rpc.OrderView{LifecycleStatus: rpc.OrderLifecycleSubmitted}, at, planEnd); !ok || r.State != rpc.QueuedAuthSent || r.HoldCode != "" {
		t.Fatalf("an unclear attempt the broker shows: %+v", r)
	}
	r = sent()
	r.CancelRequested = true
	if _, ok := trailSaleProgress(r, rpc.OrderView{LifecycleStatus: rpc.OrderLifecycleCancelled, Filled: 3}, at, planEnd); !ok || r.State != rpc.QueuedAuthCancelled || r.Watch.SaleFilled != 3 {
		t.Fatalf("an owner cancel: %+v", r)
	}
}

// The price steps: the mid, halfway to the bid, the bid, and the bid after
// that; each works one tick on the way down and the re-price interval at the
// bid.
func TestTrailSaleSteps(t *testing.T) {
	for step, want := range []float64{0, 0.5, 1, 1, 1} {
		if got := trailSaleSteps[trailStep(step)].concession; got != want {
			t.Fatalf("step %d concession %g, want %g", step, got, want)
		}
	}
	if trailStepWorks(0) != queuedTrailCheckEvery || trailStepWorks(1) != queuedTrailCheckEvery || trailStepWorks(2) != queuedTrailRepriceAfter || trailStepWorks(7) != queuedTrailRepriceAfter {
		t.Fatal("step durations")
	}
	bid, ask := 1.60, 1.61
	_, trail := exitPlanTestTerms()
	rec := queuedAuthRecord{Terms: trail, Watch: queuedTrailState{SaleStep: 1, LastLimit: 1.61, LastBid: 1.60, LastAsk: 1.61}}
	same := &rpc.OrderPreviewResult{Draft: rpc.OrderDraft{LimitPrice: 1.61}, Quote: rpc.OrderQuoteSnapshot{Bid: &bid, Ask: &ask}}
	if !trailStepRepeats(rec, same) {
		t.Fatal("a repeated limit on the same quote was not skipped")
	}
	moved := 1.59
	if trailStepRepeats(rec, &rpc.OrderPreviewResult{Draft: rpc.OrderDraft{LimitPrice: 1.61}, Quote: rpc.OrderQuoteSnapshot{Bid: &moved, Ask: &ask}}) {
		t.Fatal("skipped on a moved quote")
	}
	rec.Watch.SaleStep = 2
	if trailStepRepeats(rec, same) {
		t.Fatal("skipped past the bid")
	}
}

// A first sale has a positive minimum or none. With one, its terms encode
// exactly as before (worst_price present, the digest of the same bytes);
// without one, worst_price is omitted as on the trail, and a negative or
// unusable minimum is refused.
func TestExitPlanFirstSaleMinimumIsOptional(t *testing.T) {
	type plain rpc.QueuedAuthTerms
	first, _ := exitPlanTestTerms()
	raw, _ := json.Marshal(first)
	before, _ := json.Marshal(plain(first))
	sum := sha256.Sum256(before)
	if digest, _ := queuedTermsDigest(first); string(raw) != string(before) || digest != hex.EncodeToString(sum[:]) {
		t.Fatalf("a first sale with a minimum encodes differently:\n%s\n%s", raw, before)
	}
	open := first
	open.WorstPrice, open.ReferenceMark = 0, 0
	if p := ownerQueueProblem(open); p != "" {
		t.Fatalf("a first sale without a minimum refused: %s", p)
	}
	raw, _ = json.Marshal(open)
	if strings.Contains(string(raw), "worst_price") {
		t.Fatalf("no minimum, yet worst_price encoded: %s", raw)
	}
	var back rpc.QueuedAuthTerms
	if err := json.Unmarshal(raw, &back); err != nil || back.WorstPrice != 0 {
		t.Fatalf("round trip: %v %g", err, back.WorstPrice)
	}
	if again, _ := json.Marshal(back); string(again) != string(raw) {
		t.Fatal("a stored first sale without a minimum does not re-encode to its bytes")
	}
	for _, bad := range []float64{-1, math.NaN(), math.Inf(1)} {
		b := first
		b.WorstPrice = bad
		if ownerQueueProblem(b) == "" {
			t.Fatalf("worst price %g accepted", bad)
		}
	}
}

// A whole-line trail sells the line held when it fired, contracts added
// after signing included; a capped one never more than its signed maximum.
func TestTrailLineCap(t *testing.T) {
	_, trail := exitPlanTestTerms()
	capped := queuedAuthRecord{Terms: trail, Watch: queuedTrailState{LineAtTrigger: 30}}
	if got := trailLineCap(capped); got != 20 {
		t.Fatalf("capped trail sells up to %d", got)
	}
	whole := trail
	st := *trail.OwnerOrder.StockTrail
	st.WholeLine = true
	o := *trail.OwnerOrder
	o.StockTrail = &st
	whole.OwnerOrder = &o
	if p := ownerQueueProblem(whole); p != "" {
		t.Fatalf("whole-line terms refused: %s", p)
	}
	if got := trailLineCap(queuedAuthRecord{Terms: whole, Watch: queuedTrailState{LineAtTrigger: 30}}); got != 30 {
		t.Fatalf("whole-line trail sells up to %d", got)
	}
	if got := trailLineCap(queuedAuthRecord{Terms: whole}); got != 20 {
		t.Fatalf("a whole-line trail with no line read sells up to %d", got)
	}
	raw, _ := json.Marshal(whole)
	if !strings.Contains(string(raw), `"whole_line":true`) {
		t.Fatalf("terms: %s", raw)
	}
	raw, _ = json.Marshal(trail)
	if strings.Contains(string(raw), "whole_line") {
		t.Fatal("a capped trail's terms changed bytes")
	}
}

// A plan signed in the session carries start_in_minutes (0 is at once) and
// no wait; exactly one of the two.
func TestExitPlanStartInTerms(t *testing.T) {
	first, trail := exitPlanTestTerms()
	for _, terms := range []rpc.QueuedAuthTerms{first, trail} {
		o := *terms.OwnerOrder
		plan := *o.Plan
		plan.WaitMinutes, plan.StartInMinutes = 0, new(0)
		o.Plan = &plan
		terms.OwnerOrder = &o
		if p := ownerQueueProblem(terms); p != "" {
			t.Fatalf("%s starting at once refused: %s", plan.Role, p)
		}
		raw, _ := json.Marshal(terms)
		if !strings.Contains(string(raw), `"wait_minutes":0,"start_in_minutes":0`) {
			t.Fatalf("terms: %s", raw)
		}
	}
	raw, _ := json.Marshal(first)
	if strings.Contains(string(raw), "start_in_minutes") {
		t.Fatal("a plan signed before the open changed bytes")
	}
}

// A fired trail that has not sold, or another armed record inside its
// window, waits on the open orders; a trail that has not fired does not.
func TestSaleWaitsOnOpenOrders(t *testing.T) {
	rig := newQueueTestRig(t)
	rig.server.tradeProposals = rig.engine
	_, trail := exitPlanTestTerms()
	trail.QueueID = "trail1"
	digest, _ := queuedTermsDigest(trail)
	in := trail.NotBefore.Add(time.Minute)
	if err := rig.engine.queued.update(t.Context(), in, func(records map[string]*queuedAuthRecord) []queuedAuthEvent {
		r := &queuedAuthRecord{Version: queuedDocumentVersion, Terms: trail, TermsDigest: digest, CreatedAt: in}
		records[trail.QueueID] = r
		return []queuedAuthEvent{r.transition(queuedEventArmed, rpc.QueuedAuthArmed, in)}
	}); err != nil {
		t.Fatal(err)
	}
	if rig.server.saleWaitsOnOpenOrders(in) {
		t.Fatal("a trail that has not fired waits on the open orders")
	}
	if err := rig.engine.queued.update(t.Context(), in, func(records map[string]*queuedAuthRecord) []queuedAuthEvent {
		r := records[trail.QueueID]
		r.Watch.TriggeredAt = in
		return []queuedAuthEvent{r.note(queuedEventTriggered, r.State, in)}
	}); err != nil {
		t.Fatal(err)
	}
	if !rig.server.saleWaitsOnOpenOrders(in) {
		t.Fatal("a fired trail does not wait on the open orders")
	}
	if rig.server.saleWaitsOnOpenOrders(trail.NotAfter) {
		t.Fatal("a trail past its window waits on the open orders")
	}
}

// exitPlanCarryTerms are exitPlanTestTerms carried over three sessions from
// Tuesday 2026-11-24: Thanksgiving closes Thursday, so the third is Friday
// 2026-11-27, whose stock market closes early at 13:00 New York.
func exitPlanCarryTerms() (first, trail rpc.QueuedAuthTerms) {
	first, trail = exitPlanTestTerms()
	for _, terms := range []*rpc.QueuedAuthTerms{&first, &trail} {
		o := *terms.OwnerOrder
		plan := *o.Plan
		plan.Validity, plan.ValidUntil, plan.ResumeWaitMinutes = "3d", "2026-11-27", 20
		o.Plan = &plan
		terms.OwnerOrder, terms.Version = &o, queuedPlanSessionsTermsVersion
	}
	trail.NotAfter = time.Date(2026, 11, 27, 18, 0, 0, 0, time.UTC)
	return first, trail
}

func TestExitPlanValidity(t *testing.T) {
	for in, want := range map[string]struct {
		norm     string
		sessions int
		carry    bool
	}{"": {"", 1, false}, "day": {"", 1, false}, "1d": {"", 1, false}, "3d": {"3d", 3, true}, " GTC ": {"gtc", 0, true}, "10D": {"10d", 10, true}} {
		norm, sessions, carry, ok := exitPlanValidity(in)
		if !ok || norm != want.norm || sessions != want.sessions || carry != want.carry {
			t.Errorf("%q: %q %d %v %v", in, norm, sessions, carry, ok)
		}
	}
	for _, in := range []string{"0d", "-2d", "3", "d", "03d", "3 sessions", "week"} {
		if _, _, _, ok := exitPlanValidity(in); ok {
			t.Errorf("%q accepted", in)
		}
	}
}

// A carry-over plan is version 4 and signs its validity; a one-session plan
// keeps version 3 and the bytes it had.
func TestExitPlanCarryOverTerms(t *testing.T) {
	first, trail := exitPlanCarryTerms()
	for _, terms := range []rpc.QueuedAuthTerms{first, trail} {
		if p := ownerQueueProblem(terms); p != "" {
			t.Fatalf("valid carry-over %s refused: %s", terms.OwnerOrder.Plan.Role, p)
		}
		if !queuedOwnerOrder(terms) || queuedPlanOf(terms) == nil {
			t.Fatal("a version-4 record is not an exit plan's")
		}
	}
	raw, _ := json.Marshal(trail)
	if !strings.Contains(string(raw), `"plan":{"plan_id":"plan1","role":"trail","wait_minutes":20,"validity":"3d","valid_until":"2026-11-27","resume_wait_minutes":20}`) {
		t.Fatalf("carry-over terms: %s", raw)
	}
	_, one := exitPlanTestTerms()
	raw, _ = json.Marshal(one)
	if !strings.Contains(string(raw), `"plan":{"plan_id":"plan1","role":"trail","wait_minutes":20}`) || strings.Contains(string(raw), "valid") {
		t.Fatalf("a one-session plan changed bytes: %s", raw)
	}
	edit := func(terms rpc.QueuedAuthTerms, change func(*rpc.QueuedAuthTerms, *rpc.QueuedPlan)) rpc.QueuedAuthTerms {
		o := *terms.OwnerOrder
		plan := *o.Plan
		o.Plan = &plan
		terms.OwnerOrder = &o
		change(&terms, &plan)
		return terms
	}
	for name, change := range map[string]func(*rpc.QueuedAuthTerms, *rpc.QueuedPlan){
		"no validity":             func(t *rpc.QueuedAuthTerms, p *rpc.QueuedPlan) { p.Validity = "" },
		"unknown validity":        func(t *rpc.QueuedAuthTerms, p *rpc.QueuedPlan) { p.Validity = "week" },
		"one day":                 func(t *rpc.QueuedAuthTerms, p *rpc.QueuedPlan) { p.Validity = "1d" },
		"past the expiry":         func(t *rpc.QueuedAuthTerms, p *rpc.QueuedPlan) { p.ValidUntil = "2026-12-19" },
		"before the first":        func(t *rpc.QueuedAuthTerms, p *rpc.QueuedPlan) { p.ValidUntil = "2026-11-23" },
		"no date":                 func(t *rpc.QueuedAuthTerms, p *rpc.QueuedPlan) { p.ValidUntil = "" },
		"no resume wait":          func(t *rpc.QueuedAuthTerms, p *rpc.QueuedPlan) { p.ResumeWaitMinutes = 0 },
		"resume wait above 120":   func(t *rpc.QueuedAuthTerms, p *rpc.QueuedPlan) { p.ResumeWaitMinutes = 121 },
		"no expiry":               func(t *rpc.QueuedAuthTerms, p *rpc.QueuedPlan) { t.Contract.Expiry = "" },
		"version 3 with validity": func(t *rpc.QueuedAuthTerms, p *rpc.QueuedPlan) { t.Version = queuedPlanTermsVersion },
		"beyond the horizon": func(t *rpc.QueuedAuthTerms, p *rpc.QueuedPlan) {
			t.NotAfter = t.NotBefore.Add(queuedPlanValidityMax + time.Hour)
		},
	} {
		if ownerQueueProblem(edit(trail, change)) == "" {
			t.Errorf("%s: accepted", name)
		}
	}
	// A one-session plan never runs past its day.
	if ownerQueueProblem(edit(one, func(t *rpc.QueuedAuthTerms, _ *rpc.QueuedPlan) { t.NotAfter = t.NotBefore.Add(72 * time.Hour) })) == "" {
		t.Fatal("a version-3 trail across three days was accepted")
	}
}

// The validity walks the official calendar: holidays do not count, a half
// day ends at its early close, and nothing runs past the option's last
// trading day.
func TestExitPlanLastSessionWalksTheCalendar(t *testing.T) {
	s := &Server{}
	useQueueCalendar(s)
	at := time.Date(2026, 11, 24, 15, 0, 0, 0, time.UTC)
	day, _ := s.previewSession(marketcal.MarketUSOptions, at)
	stockDay, _ := s.previewSession(marketcal.MarketUSEquity, at)
	for name, c := range map[string]struct {
		sessions int
		last     string
		date     string
		end      time.Time
	}{
		"today":                     {1, "2026-12-18", "2026-11-24", time.Date(2026, 11, 24, 21, 0, 0, 0, time.UTC)},
		"over Thanksgiving":         {3, "2026-12-18", "2026-11-27", time.Date(2026, 11, 27, 18, 0, 0, 0, time.UTC)},
		"until cancelled":           {0, "2026-12-04", "2026-12-04", time.Date(2026, 12, 4, 21, 0, 0, 0, time.UTC)},
		"more sessions than listed": {30, "2026-12-04", "2026-12-04", time.Date(2026, 12, 4, 21, 0, 0, 0, time.UTC)},
		"expiry on a holiday":       {0, "2026-11-26", "2026-11-25", time.Date(2026, 11, 25, 21, 0, 0, 0, time.UTC)},
	} {
		date, end := s.exitPlanLastSession(marketcal.MarketUSOptions, marketcal.MarketUSEquity, day, stockDay, c.sessions, c.last)
		if date != c.date || !end.Equal(c.end) {
			t.Errorf("%s: %s %s, want %s %s", name, date, end, c.date, c.end)
		}
	}
}

// storeTrail arms terms on the rig as a waiting trail.
func storeTrail(t *testing.T, e *proposalEngine, terms rpc.QueuedAuthTerms, at time.Time) {
	t.Helper()
	digest, _ := queuedTermsDigest(terms)
	if err := e.queued.update(t.Context(), at, func(records map[string]*queuedAuthRecord) []queuedAuthEvent {
		r := &queuedAuthRecord{Version: queuedDocumentVersion, Terms: terms, TermsDigest: digest, CreatedAt: at}
		records[terms.QueueID] = r
		return []queuedAuthEvent{r.transition(queuedEventArmed, rpc.QueuedAuthArmed, at)}
	}); err != nil {
		t.Fatal(err)
	}
}

// A carry-over trail watches only inside the regular session of the option
// and the stock: not overnight, not on a holiday, and on a later session
// only from the signed wait after the open. Off session it does not tick,
// it wakes for the next watch, and its view says when that is.
func TestTrailCarriesOverTheNight(t *testing.T) {
	rig := newQueueTestRig(t)
	e := rig.engine
	_, trail := exitPlanCarryTerms()
	trail.QueueID = "trail1"
	storeTrail(t, e, trail, exitPlanTestOpen)
	rec := func() queuedAuthRecord { r, _ := e.queued.get(trail.QueueID); return r }
	day2Open := time.Date(2026, 11, 25, 14, 30, 0, 0, time.UTC)
	for name, c := range map[string]struct {
		at   time.Time
		in   bool
		code string
		next time.Time
	}{
		"day one":              {time.Date(2026, 11, 24, 16, 0, 0, 0, time.UTC), true, "", time.Time{}},
		"day one's evening":    {time.Date(2026, 11, 24, 21, 30, 0, 0, time.UTC), false, rpc.ReadinessMarketClosed, day2Open.Add(20 * time.Minute)},
		"day two's pre-market": {time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC), false, rpc.ReadinessMarketClosed, day2Open.Add(20 * time.Minute)},
		"day two's opening":    {day2Open.Add(5 * time.Minute), false, rpc.ReadinessOpeningWindow, day2Open.Add(20 * time.Minute)},
		"day two":              {day2Open.Add(21 * time.Minute), true, "", time.Time{}},
		"Thanksgiving":         {time.Date(2026, 11, 26, 16, 0, 0, 0, time.UTC), false, rpc.ReadinessMarketClosed, time.Date(2026, 11, 27, 14, 50, 0, 0, time.UTC)},
		"after the last close": {time.Date(2026, 11, 27, 18, 5, 0, 0, time.UTC), false, rpc.ReadinessMarketClosed, time.Time{}},
	} {
		got := e.trailSessionAt(rec(), c.at)
		if got.in != c.in || got.code != c.code || !got.next.Equal(c.next) {
			t.Errorf("%s: %+v", name, got)
		}
		if _, ticks := e.queuedTrailTick(c.at); ticks != c.in && c.at.Before(trail.NotAfter) {
			t.Errorf("%s: ticks %v", name, ticks)
		}
	}
	evening := time.Date(2026, 11, 24, 21, 30, 0, 0, time.UTC)
	if d, ok := e.queuedWake(evening); !ok || d != day2Open.Add(20*time.Minute).Sub(evening) {
		t.Fatalf("wakes in %s %v", d, ok)
	}
	rig.now = evening
	if w := e.queuedView(rec()).Watch; w.State != rpc.QueuedWatchWaiting || w.ReasonCode != rpc.ReadinessMarketClosed || !w.NextWatchAt.Equal(day2Open.Add(20*time.Minute)) {
		t.Fatalf("evening view: %+v", w)
	}
	// A trail that fired yesterday and did not sell starts its steps again at
	// the mid in the new session, once.
	if err := e.queued.update(t.Context(), evening, func(records map[string]*queuedAuthRecord) []queuedAuthEvent {
		r := records[trail.QueueID]
		r.Watch.TriggeredAt, r.Watch.SaleStep, r.Watch.LastLimit, r.Watch.LastBid, r.Watch.LastAsk = exitPlanTestOpen, 2, 1.50, 1.45, 1.70
		return []queuedAuthEvent{r.note(queuedEventTriggered, r.State, evening)}
	}); err != nil {
		t.Fatal(err)
	}
	rig.now = day2Open.Add(21 * time.Minute)
	got := e.enterTrailSession(t.Context(), rec(), day2Open)
	if got.Watch.SaleStep != 0 || got.Watch.LastLimit != 0 || !got.Watch.SessionOpen.Equal(day2Open) || got.Watch.TriggeredAt.IsZero() {
		t.Fatalf("a new session: %+v", got.Watch)
	}
	events := 0
	for range 2 {
		before, _ := e.queued.get(trail.QueueID)
		after := e.enterTrailSession(t.Context(), before, day2Open)
		if after.Watch != before.Watch {
			events++
		}
	}
	if events != 0 {
		t.Fatal("the same session was entered twice")
	}
	// A one-session plan is unchanged: the calendar's answer only confirms
	// its window.
	_, one := exitPlanTestTerms()
	one.QueueID = "trail2"
	storeTrail(t, e, one, exitPlanTestOpen)
	r2, _ := e.queued.get(one.QueueID)
	if got := e.trailSessionAt(r2, exitPlanTestOpen.Add(time.Hour)); !got.in {
		t.Fatalf("a one-session trail in its window: %+v", got)
	}
}

// A trail whose validity ends says how the plan ended: not crossed, or
// crossed with what it sold; a plan that ends with its contract says so.
// A first sale keeps its send-window end.
func TestExitPlanEndReasons(t *testing.T) {
	rig := newQueueTestRig(t)
	e := rig.engine
	first, trail := exitPlanCarryTerms()
	first.QueueID, trail.QueueID = "first1", "trail1"
	storeTrail(t, e, first, exitPlanTestOpen)
	storeTrail(t, e, trail, exitPlanTestOpen)
	rig.now = trail.NotAfter.Add(time.Second)
	e.expireQueued(t.Context())
	got, _ := e.queued.get(trail.QueueID)
	if got.State != rpc.QueuedAuthExpired || got.ReasonCode != queuedReasonPlanValidityEnded ||
		got.Reason != "the plan's validity ended and the exit line was not crossed; nothing was sold" {
		t.Fatalf("not crossed: %s %s %q", got.State, got.ReasonCode, got.Reason)
	}
	if f, _ := e.queued.get(first.QueueID); f.ReasonCode != queuedReasonWindowEnded {
		t.Fatalf("first sale: %s %q", f.ReasonCode, f.Reason)
	}
	// Valid until the option's last trading day, Friday 2026-12-18.
	_, last := exitPlanCarryTerms()
	last.QueueID, last.NotAfter = "trail2", time.Date(2026, 12, 18, 21, 0, 0, 0, time.UTC)
	last.OwnerOrder.Plan.Validity, last.OwnerOrder.Plan.ValidUntil = queuedPlanValidityGTC, "2026-12-18"
	r := queuedAuthRecord{Terms: last, Watch: queuedTrailState{TriggeredAt: exitPlanTestOpen, SaleFilled: 5}}
	if code, reason := e.planEndReason(r); code != queuedReasonPlanContractExpired ||
		reason != "the option's last trading day closed, so the plan ends with its contract after the exit line was crossed, with 5 of 20 sold" {
		t.Fatalf("with its contract: %s %q", code, reason)
	}
}

// An exit plan's sale is held only by a hand order on its own option, or one
// without a contract id that could be on it; a resting order elsewhere (a
// GTC stop on a stock) holds every other queued send but not the plan's.
func TestPlanSaleSettlingHoldIsOnItsOption(t *testing.T) {
	_, trail := exitPlanTestTerms()
	stock := automaticHandOrder{Mark: "perm:7#a", Order: ibkrlib.OrderLifecycleEvent{PermID: 7, ConID: 900002, Symbol: "SYNB", SecType: "STK", Action: "SELL"}}
	same := automaticHandOrder{Mark: "perm:8#b", Order: ibkrlib.OrderLifecycleEvent{PermID: 8, ConID: trail.Contract.ConID, Symbol: "SYNA", SecType: "OPT", Action: "SELL"}}
	unknown := automaticHandOrder{Mark: "perm:9#c", Order: ibkrlib.OrderLifecycleEvent{PermID: 9, Symbol: "SYNA", SecType: "OPT", Action: "SELL"}}
	since := exitPlanTestOpen
	book := automaticSettlingBook{ok: true, hand: []automaticHandOrder{stock}}
	if book.holdSince(since, nil, true) != automaticHoldHandOrder {
		t.Fatal("the account-wide rule no longer holds")
	}
	if got := book.holdSinceOn(trail.Contract, since, nil, true); got != "" {
		t.Fatalf("a stock order held the plan's sale: %s", got)
	}
	for name, h := range map[string]automaticHandOrder{"same option": same, "no contract id": unknown} {
		b := automaticSettlingBook{ok: true, hand: []automaticHandOrder{stock, h}}
		if got := b.holdSinceOn(trail.Contract, since, nil, true); got != automaticHoldHandOrder {
			t.Fatalf("%s: %q", name, got)
		}
	}
	if got := (automaticSettlingBook{}).holdSinceOn(trail.Contract, since, nil, true); got != automaticHoldUnavailable {
		t.Fatalf("an unread book: %q", got)
	}
}

// A standing plan (watching, not fired) never holds back another sale of its
// option: rule rows are neither gated nor marked queued. A fired trail, or
// one whose sale is at the broker, holds the contract as before.
func TestStandingPlanYieldsTheContract(t *testing.T) {
	rig := newQueueTestRig(t)
	e := rig.engine
	_, trail := exitPlanCarryTerms()
	trail.QueueID = "trail1"
	scope := e.currentScope()
	trail.AccountID, trail.AccountMode = scope.Account, scope.Mode
	row := rpc.TradeProposal{Key: "option_loss_exit:910001", Contract: trail.Contract, Action: rpc.OrderActionSell}
	digest, _ := queuedTermsDigest(trail)
	if err := e.queued.update(t.Context(), exitPlanTestOpen, func(records map[string]*queuedAuthRecord) []queuedAuthEvent {
		r := &queuedAuthRecord{Version: queuedDocumentVersion, Terms: trail, TermsDigest: digest, ContractSide: sameContractSide(row), CreatedAt: exitPlanTestOpen}
		records[trail.QueueID] = r
		return []queuedAuthEvent{r.transition(queuedEventArmed, rpc.QueuedAuthArmed, exitPlanTestOpen)}
	}); err != nil {
		t.Fatal(err)
	}
	if b := e.queuedIntentGateBlockers(row, nil); len(b) != 0 {
		t.Fatalf("a standing plan gated a reduction: %+v", b)
	}
	if e.queuedLiveKeys([]rpc.TradeProposal{row})[row.Key] {
		t.Fatal("a standing plan marked the row queued")
	}
	if err := e.queued.update(t.Context(), exitPlanTestOpen, func(records map[string]*queuedAuthRecord) []queuedAuthEvent {
		r := records[trail.QueueID]
		r.Watch.TriggeredAt = exitPlanTestOpen
		return []queuedAuthEvent{r.note(queuedEventTriggered, r.State, exitPlanTestOpen)}
	}); err != nil {
		t.Fatal(err)
	}
	if b := e.queuedIntentGateBlockers(row, nil); len(b) == 0 || b[0].Code != queuedIntentExistsCode {
		t.Fatalf("a fired trail let a second sale through: %+v", b)
	}
}

// A trail held because the protection policy changed after signing says so
// at once, in a typed reason Desk can raise as a notice.
func TestPolicyHoldIsVisibleOnTheWatch(t *testing.T) {
	rig := newQueueTestRig(t)
	e := rig.engine
	_, trail := exitPlanTestTerms()
	trail.QueueID = "trail1"
	storeTrail(t, e, trail, exitPlanTestOpen)
	at := exitPlanTestOpen.Add(time.Minute)
	rig.now = at
	e.setTrailLive(trail.QueueID, queuedTrailLive{price: 50, usableAt: at})
	r, _ := e.queued.get(trail.QueueID)
	e.holdQueued(t.Context(), r, queuedHoldPolicyChanged, "the protection policy changed after the owner signed")
	r, _ = e.queued.get(trail.QueueID)
	if w := e.queuedView(r).Watch; w.State != rpc.QueuedWatchUnwatched || w.ReasonCode != queuedHoldPolicyChanged || r.HoldCode != queuedHoldPolicyChanged {
		t.Fatalf("policy hold view: %+v", w)
	}
}

// A fired trail always prices an order. Inside the spread limit it steps
// from the mid to halfway to the bid to the bid. Wider, it starts at the mid
// and steps down no further than the bid of a quote at the limit:
// floor = mid × (1 − limit/200), 12.5% under the mid at 25%.
func TestTrailSalePricesAWideQuote(t *testing.T) {
	option := rpc.ContractParams{Symbol: "SYNA", SecType: "OPT", Right: "C", Strike: 55, Expiry: "20261218", Multiplier: 100}
	quote := func(bid, ask float64) rpc.OrderQuoteSnapshot {
		return rpc.OrderQuoteSnapshot{Bid: new(bid), Ask: new(ask), DataType: rpc.MarketDataLive}
	}
	if got := trailWideFloor(1.40, 25); math.Abs(got-1.225) > 1e-9 {
		t.Fatalf("floor %g", got)
	}
	for name, c := range map[string]struct {
		q     rpc.OrderQuoteSnapshot
		steps [3]float64
	}{
		"57% wide": {quote(1.00, 1.80), [3]float64{1.40, 1.23, 1.23}},
		"7% wide":  {quote(1.35, 1.45), [3]float64{1.40, 1.38, 1.35}},
		"60% wide": {quote(0.70, 1.30), [3]float64{1.00, 0.88, 0.88}},
	} {
		for step, want := range c.steps {
			got, err := boundedLimitPrice(rpc.OrderActionSell, trailSaleBound(step, 25), nil, option, c.q)
			if err != nil || math.Abs(got-want) > 1e-9 {
				t.Errorf("%s step %d: %g %v, want %g", name, step, got, err, want)
			}
		}
	}
	// One-sided: there is no mid, and nothing is priced.
	if _, err := boundedLimitPrice(rpc.OrderActionSell, trailSaleBound(0, 25), nil, option, rpc.OrderQuoteSnapshot{Bid: new(1.0), DataType: rpc.MarketDataLive}); !previewRefusedWith(err, previewQuoteNotTwoSidedCode) {
		t.Fatalf("one-sided: %v", err)
	}
	// Every other bounded limit still refuses a wide quote.
	if _, err := boundedLimitPrice(rpc.OrderActionSell, &rpc.OrderBoundedLimit{Concession: 0, NoWorstPrice: true, MaxSpreadPctOfMid: 25}, nil, option, quote(1.00, 1.80)); !previewRefusedWith(err, previewBoundedSpreadCode) {
		t.Fatalf("a first sale's bound priced a wide quote: %v", err)
	}
}

// A fired trail whose sale works on a quote wider than its limit says so in
// a typed way, with the spread and the floor.
func TestTrailWideSaleView(t *testing.T) {
	rig := newQueueTestRig(t)
	e := rig.engine
	_, trail := exitPlanTestTerms()
	trail.QueueID = "trail1"
	storeTrail(t, e, trail, exitPlanTestOpen)
	at := exitPlanTestOpen.Add(time.Minute)
	rig.now = at
	if err := e.queued.update(t.Context(), at, func(records map[string]*queuedAuthRecord) []queuedAuthEvent {
		r := records[trail.QueueID]
		r.Watch.TriggeredAt, r.Watch.SaleAttempts = at, 1
		r.State, r.OrderRef, r.SentAt, r.LimitPrice = rpc.QueuedAuthSent, "ref1", at, 1.40
		r.SendQuote = queuedSendQuote(rpc.OrderQuoteSnapshot{Bid: new(1.00), Ask: new(1.80), DataType: rpc.MarketDataLive})
		return []queuedAuthEvent{r.note(queuedEventTriggered, r.State, at)}
	}); err != nil {
		t.Fatal(err)
	}
	r, _ := e.queued.get(trail.QueueID)
	w := e.queuedView(r).Watch
	if w.State != rpc.QueuedWatchTriggered || w.ReasonCode != queuedTrailSaleWideSpread || math.Abs(w.SaleSpreadPct-57.142857) > 1e-3 || math.Abs(w.SaleFloor-1.225) > 1e-9 {
		t.Fatalf("wide sale view: %+v", w)
	}
}
