package daemon

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/osauer/canary/v3/internal/rpc"
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
		"first sale no minimum":    func(f, tr *rpc.QueuedAuthTerms) { f.WorstPrice = 0 },
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
	if _, ok := trailSaleProgress(r, rpc.OrderView{LifecycleStatus: rpc.OrderLifecycleCancelled, Filled: 8}, at); !ok ||
		r.State != rpc.QueuedAuthHeld || r.HoldCode != queuedHoldTrailSaleEnded || r.Watch.SaleFilled != 8 || r.OrderRef != "" || r.PreviewTokenID != "" ||
		r.Watch.SaleStep != 1 || r.Watch.LastLimit != 1.60 {
		t.Fatalf("a cancelled attempt: %+v", r)
	}
	r = sent()
	r.Watch.SaleFilled = 8
	r.QuantitySent = 12
	if _, ok := trailSaleProgress(r, rpc.OrderView{LifecycleStatus: rpc.OrderLifecycleFilled, Filled: 12}, at); !ok || r.State != rpc.QueuedAuthFilled || r.Watch.SaleFilled != 20 {
		t.Fatalf("the line sold: %+v", r)
	}
	r = sent()
	if _, ok := trailSaleProgress(r, rpc.OrderView{LifecycleStatus: rpc.OrderLifecycleRejected}, at); !ok || r.State != rpc.QueuedAuthHeld || r.HoldCode != queuedHoldTrailSaleEnded {
		t.Fatalf("a rejected attempt: %+v", r)
	}
	r = sent()
	if _, ok := trailSaleProgress(r, rpc.OrderView{LifecycleStatus: rpc.OrderLifecycleCancelled, Filled: 5}, trail.NotAfter); !ok || r.State != rpc.QueuedAuthPartiallyFilled || r.ReasonCode != queuedReasonWindowEnded {
		t.Fatalf("at the window's end: %+v", r)
	}
	r = sent()
	if _, ok := trailSaleProgress(r, rpc.OrderView{LifecycleStatus: rpc.OrderLifecycleClosedReconciled}, at); !ok || r.State != rpc.QueuedAuthHeld || r.HoldCode != queuedHoldTrailSaleUnconfirmed || r.OrderRef != "ref1" {
		t.Fatalf("an end not recorded: %+v", r)
	}
	if _, ok := trailSaleProgress(r, rpc.OrderView{LifecycleStatus: rpc.OrderLifecycleClosedReconciled}, at); ok {
		t.Fatal("an unconfirmed attempt was handled twice")
	}
	r = sent()
	r.State, r.HoldCode = rpc.QueuedAuthHeld, queuedReasonSendUnclear
	if _, ok := trailSaleProgress(r, rpc.OrderView{LifecycleStatus: rpc.OrderLifecycleSubmitted}, at); !ok || r.State != rpc.QueuedAuthSent || r.HoldCode != "" {
		t.Fatalf("an unclear attempt the broker shows: %+v", r)
	}
	r = sent()
	r.CancelRequested = true
	if _, ok := trailSaleProgress(r, rpc.OrderView{LifecycleStatus: rpc.OrderLifecycleCancelled, Filled: 3}, at); !ok || r.State != rpc.QueuedAuthCancelled || r.Watch.SaleFilled != 3 {
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
