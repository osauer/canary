package daemon

import (
	"context"
	"encoding/json"
	"math"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/osauer/canary/v3/internal/broker"
	"github.com/osauer/canary/v3/internal/rpc"
)

// All prices here are invented.

func indicationCodes(r *rpc.MarketReadResult) map[string]rpc.MarketReadIndication {
	out := map[string]rpc.MarketReadIndication{}
	for _, i := range r.Indications {
		out[i.Code] = i
	}
	return out
}

func future(contract string, last, close float64) *marketReadFutureSource {
	return &marketReadFutureSource{Contract: contract, Quote: broker.BriefQuote{Last: last, Close: close}}
}

// Before the open the index futures indicate the open: their change, the
// level it implies for the index against its prior range, and the Nasdaq
// future beside the S&P 500's.
func TestMarketReadIndicatesTheOpenFromTheFutures(t *testing.T) {
	for _, tc := range []struct {
		name          string
		es, nq        float64 // last against a prior close of 1000
		position      string
		want, notWant []string
	}{
		{"a small gap up inside the range", 1003, 1003, "inside", []string{"indicated_gap_up"}, []string{"indicated_above_prior_high", "nasdaq_indicated_weaker", "nasdaq_indicated_stronger"}},
		{"above the prior high", 1060, 1060, "above_prior_high", []string{"indicated_gap_up", "indicated_above_prior_high"}, nil},
		{"below the prior low, Nasdaq weaker", 940, 934, "below_prior_low", []string{"indicated_gap_down", "indicated_below_prior_low", "nasdaq_indicated_weaker"}, []string{"indicated_gap_up"}},
		{"flat, Nasdaq stronger", 1001, 1006, "inside", []string{"nasdaq_indicated_stronger"}, []string{"indicated_gap_up", "indicated_gap_down"}},
	} {
		m := newReadMarket(t) // SPX prior range 90 to 100, close 95
		m.src.ES, m.src.NQ = future("ESZ6", tc.es, 1000), future("NQZ6", tc.nq, 1000)
		r := m.read(m.session.Open.Add(-time.Hour))
		ind := r.Indicated
		if ind == nil || ind.Index == nil || ind.Nasdaq == nil {
			t.Fatalf("%s: indicated %+v", tc.name, ind)
		}
		if ind.Index.Source != "ES front future ESZ6" || ind.Nasdaq.Source != "NQ front future NQZ6" || ind.Index.PriorClose != 1000 {
			t.Errorf("%s: sources %q %q", tc.name, ind.Index.Source, ind.Nasdaq.Source)
		}
		wantLevel := 95 * tc.es / 1000
		if ind.Index.ImpliedLevel == nil || math.Abs(*ind.Index.ImpliedLevel-wantLevel) > 1e-9 || ind.Index.ImpliedPosition != tc.position {
			t.Errorf("%s: implied %v %q, want %v %q", tc.name, ind.Index.ImpliedLevel, ind.Index.ImpliedPosition, wantLevel, tc.position)
		}
		codes := indicationCodes(r)
		for _, c := range tc.want {
			got, ok := codes[c]
			if !ok {
				t.Errorf("%s: no %s in %+v", tc.name, c, r.Indications)
				continue
			}
			if got.Tone == "" || len(got.Evidence) == 0 {
				t.Errorf("%s: %s without tone or evidence: %+v", tc.name, c, got)
			}
		}
		for _, c := range tc.notWant {
			if _, ok := codes[c]; ok {
				t.Errorf("%s: unexpected %s", tc.name, c)
			}
		}
		if len(r.Tags) != 0 {
			t.Errorf("%s: an indication became a tag: %+v", tc.name, r.Tags)
		}
	}
}

// The leaders indicate by their pre-market prices against the prior close
// (100) and low (98); a price that is still the prior close indicates nothing.
func TestMarketReadLeadersIndicateTheOpen(t *testing.T) {
	m := newReadMarket(t)
	pre := m.session.Open.Add(-time.Hour)
	m.leader("AAA", nil, new(98.5))  // down, near the low
	m.leader("BBB", nil, new(98.9))  // down, near the low
	m.leader("CCC", nil, new(97.0))  // down, below the low
	m.leader("DDD", nil, new(99.5))  // down
	m.leader("EEE", nil, new(100.1)) // flat
	m.leader("FFF", nil, new(100.0)) // still the prior close: not quoted
	r := m.read(pre)
	l := r.Indicated.Leaders
	if l == nil || l.Quoted != 5 || l.Down != 4 || l.Flat != 1 || l.Up != 0 || l.NearPriorLow != 2 || l.BelowPriorLow != 1 {
		t.Fatalf("leader counts: %+v", l)
	}
	if row := r.Leaders.Rows[0]; row.IndicatedGapPct == nil || math.Abs(*row.IndicatedGapPct+1.5) > 1e-9 {
		t.Errorf("indicated gap: %+v", row)
	}
	if row := r.Leaders.Rows[5]; row.IndicatedGapPct != nil || row.IndicatedGapUnavailable == "" {
		t.Errorf("a prior-close price counted as an indication: %+v", row)
	}
	codes := indicationCodes(r)
	if c, ok := codes["leaders_indicated_down"]; !ok || c.Tone != "negative" || c.Evidence["down"] != 4 || c.Evidence["quoted"] != 5 {
		t.Errorf("leaders_indicated_down: %+v", codes)
	}
	if c, ok := codes["leaders_near_prior_low"]; !ok || c.Evidence["near_prior_low"] != 2 || c.Evidence["below_prior_low"] != 1 {
		t.Errorf("three of five near or below their lows: %+v", codes)
	}
}

// Half or more of the quoted leaders near or below their prior low is named;
// fewer is not.
func TestMarketReadLeadersNearPriorLow(t *testing.T) {
	m := newReadMarket(t)
	pre := m.session.Open.Add(-time.Hour)
	m.leader("AAA", nil, new(98.5))
	m.leader("BBB", nil, new(97.0))
	m.leader("CCC", nil, new(101.0))
	m.leader("DDD", nil, new(101.0))
	codes := indicationCodes(m.read(pre))
	if c, ok := codes["leaders_near_prior_low"]; !ok || c.Tone != "caution" || c.Evidence["near_prior_low"] != 1 || c.Evidence["below_prior_low"] != 1 {
		t.Errorf("two of four near or below: %+v", codes)
	}
	if _, ok := codes["leaders_indicated_up"]; ok {
		t.Error("two of four up is under the share")
	}
	m.leader("EEE", nil, new(101.0))
	if _, ok := indicationCodes(m.read(pre))["leaders_near_prior_low"]; ok {
		t.Error("two of five near or below was named")
	}
	// Up the same way.
	m = newReadMarket(t)
	for _, s := range []string{"AAA", "BBB", "CCC"} {
		m.leader(s, nil, new(101.0))
	}
	if c, ok := indicationCodes(m.read(pre))["leaders_indicated_up"]; !ok || c.Tone != "positive" {
		t.Errorf("three of three up: %+v", c)
	}
}

// Without quotes, or with too few, the indications are silent and each
// missing part says why; the rest of the read still answers.
func TestMarketReadIndicationsAreSilentWithoutQuotes(t *testing.T) {
	m := newReadMarket(t)
	pre := m.session.Open.Add(-time.Hour)
	m.leader("AAA", nil, new(97.0))
	m.leader("BBB", nil, new(97.0))
	r := m.read(pre)
	if len(r.Indications) != 0 {
		t.Fatalf("indications without futures and with two leaders: %+v", r.Indications)
	}
	ind := r.Indicated
	if ind == nil || ind.Index != nil || ind.IndexUnavailable == "" || ind.Nasdaq != nil || ind.NasdaqUnavailable == "" || ind.Leaders == nil || ind.Leaders.Quoted != 2 {
		t.Fatalf("indicated: %+v", ind)
	}
	raw, _ := json.Marshal(r)
	if !strings.Contains(string(raw), `"indications":[]`) {
		t.Errorf("indications must be an empty list: %s", raw)
	}
	// A future without a previous close is unavailable, not zero.
	m.src.ES = future("ESZ6", 1000, 0)
	if r := m.read(pre); r.Indicated.Index != nil || !strings.Contains(r.Indicated.IndexUnavailable, "previous close") {
		t.Errorf("ES without a previous close: %+v", r.Indicated)
	}
	// No leader with a pre-market price leaves the leader part absent.
	m = newReadMarket(t)
	m.leader("AAA", nil, new(100.0))
	if r := m.read(pre); r.Indicated.Leaders != nil || r.Indicated.LeadersUnavailable == "" {
		t.Errorf("leaders without a pre-market price: %+v", r.Indicated)
	}
	// Once the session is open there is no indicated block.
	m.src.ES = future("ESZ6", 1010, 1000)
	if r := m.read(m.at(3)); r.Indicated != nil || len(r.Indications) != 0 {
		t.Errorf("indicated during the session: %+v", r.Indicated)
	}
}

// Before the open the index has no position from yesterday's last print.
func TestMarketReadPreMarketPositionIsNotThePriorClose(t *testing.T) {
	m := newReadMarket(t)
	pre := m.session.Open.Add(-time.Hour)
	m.src.SPXQuote = &broker.BriefQuote{Last: 95, Close: 95}
	r := m.read(pre)
	if r.Index.Position != "" || r.Index.PositionUnavailable != "the session has not opened" {
		t.Errorf("pre-market position from the prior close: %q (%s)", r.Index.Position, r.Index.PositionUnavailable)
	}
	m.src.SPXQuote = &broker.BriefQuote{Last: 96, Close: 95}
	if r := m.read(m.at(3)); r.Index.Position != "inside" {
		t.Errorf("session position: %q", r.Index.Position)
	}
}

// Leaders that gap down and hold it, or reclaim the prior close, are named;
// a breakdown under the prior lows comes first, then the side with more
// gapped names.
func TestMarketReadLeadersGapDownTags(t *testing.T) {
	open, holds, reclaims := new(99.0), new(98.8), new(100.2)
	now := newReadMarket(t).at(3)
	m := newReadMarket(t)
	m.leader("AAA", open, holds)
	m.leader("BBB", open, holds)
	m.leader("CCC", open, holds)
	r := m.read(now)
	if tag := tagOf(r, "leaders"); tag == nil || tag.Code != "leaders_gap_down_hold" || tag.Tone != "negative" || tag.SinceUnavailable == "" {
		t.Fatalf("three of three hold the gap down: %+v", tag)
	}
	if r.Leaders.GappedDown != 3 || r.Leaders.HoldingGapDown != 3 || r.Leaders.GappedUp != 0 {
		t.Errorf("counts: %+v", r.Leaders)
	}

	m = newReadMarket(t)
	m.leader("AAA", open, reclaims)
	m.leader("BBB", open, reclaims)
	m.leader("CCC", open, reclaims)
	if tag := tagOf(m.read(now), "leaders"); tag == nil || tag.Code != "leaders_reclaiming" || tag.Tone != "positive" {
		t.Errorf("three of three reclaim: %+v", tag)
	}

	// Two gapped down is too few.
	m = newReadMarket(t)
	m.leader("AAA", open, holds)
	m.leader("BBB", open, holds)
	if got := codeOf(m.read(now), "leaders"); got != "" {
		t.Errorf("two gapped down: %q", got)
	}

	// Mixed, neither share: no tag.
	m = newReadMarket(t)
	m.leader("AAA", open, holds)
	m.leader("BBB", open, reclaims)
	m.leader("CCC", open, new(99.7)) // held: between half the gap and the close
	m.leader("DDD", open, reclaims)
	if got := codeOf(m.read(now), "leaders"); got != "" {
		t.Errorf("two hold, two reclaim: %q", got)
	}

	// More names gapped up than down: the gap-up side is read.
	m = newReadMarket(t)
	for _, s := range []string{"U1", "U2", "U3", "U4"} {
		m.leader(s, new(102.0), new(101.8))
	}
	for _, s := range []string{"D1", "D2", "D3"} {
		m.leader(s, open, holds)
	}
	if got := codeOf(m.read(now), "leaders"); got != "leaders_hold" {
		t.Errorf("four up, three down: %q, want leaders_hold", got)
	}
	// More down than up: the gap-down side.
	m = newReadMarket(t)
	for _, s := range []string{"U1", "U2", "U3"} {
		m.leader(s, new(102.0), new(101.8))
	}
	for _, s := range []string{"D1", "D2", "D3", "D4"} {
		m.leader(s, open, holds)
	}
	if got := codeOf(m.read(now), "leaders"); got != "leaders_gap_down_hold" {
		t.Errorf("three up, four down: %q, want leaders_gap_down_hold", got)
	}
	// Most under their prior lows is a breakdown first.
	m = newReadMarket(t)
	for _, s := range []string{"D1", "D2", "D3"} {
		m.leader(s, open, new(97.0))
	}
	if got := codeOf(m.read(now), "leaders"); got != "leaders_breaking_down" {
		t.Errorf("three under their lows: %q, want leaders_breaking_down", got)
	}
}

// While the session is open, the Nasdaq beside the S&P 500 by day change:
// the index when it quotes, else QQQ, named in source.
func TestMarketReadNasdaqBesideTheSP500(t *testing.T) {
	for _, tc := range []struct {
		name      string
		ndx, qqq  *broker.BriefQuote
		symbol    string
		code, ton string
	}{
		{"lagging", &broker.BriefQuote{Last: 990, Close: 1000}, nil, "NDX", "nasdaq_lagging", "caution"},
		{"leading", &broker.BriefQuote{Last: 1020, Close: 1000}, nil, "NDX", "nasdaq_leading", "neutral"},
		{"in step", &broker.BriefQuote{Last: 1010, Close: 1000}, nil, "NDX", "", ""},
		{"QQQ stands in", nil, &broker.BriefQuote{Last: 495, Close: 500}, "QQQ", "nasdaq_lagging", "caution"},
	} {
		m := newReadMarket(t)                                       // SPX prior close 95
		m.src.SPXQuote = &broker.BriefQuote{Last: 95.95, Close: 95} // +1%
		m.src.NDXQuote, m.src.QQQQuote = tc.ndx, tc.qqq
		r := m.read(m.at(3))
		rel := r.Relative
		if rel == nil || rel.Nasdaq == nil || rel.Nasdaq.Symbol != tc.symbol || rel.SP500ChangePct == nil || math.Abs(*rel.SP500ChangePct-1) > 1e-9 || rel.SpreadPct == nil {
			t.Fatalf("%s: relative %+v", tc.name, rel)
		}
		tag := tagOf(r, "relative")
		switch {
		case tc.code == "" && tag != nil:
			t.Errorf("%s: unexpected %+v", tc.name, tag)
		case tc.code != "" && (tag == nil || tag.Code != tc.code || tag.Tone != tc.ton || tag.SinceUnavailable == "" || !tag.Since.IsZero()):
			t.Errorf("%s: %+v, want %s", tc.name, tag, tc.code)
		}
	}
	// Neither quotes: unavailable, no tag; before the open it is not read.
	m := newReadMarket(t)
	m.src.SPXQuote = &broker.BriefQuote{Last: 95.95, Close: 95}
	r := m.read(m.at(3))
	if r.Relative == nil || r.Relative.Nasdaq != nil || r.Relative.NasdaqUnavailable == "" || r.Relative.SpreadPct != nil || tagOf(r, "relative") != nil {
		t.Errorf("without a Nasdaq quote: %+v", r.Relative)
	}
	if r := m.read(m.session.Open.Add(-time.Hour)); r.Relative != nil || r.RelativeUnavailable == "" {
		t.Errorf("relative before the open: %+v", r.Relative)
	}
}

// The futures looks and the Nasdaq looks stay inside the read's budget when
// a quote never answers.
func TestMarketReadQuoteLooksStayInsideTheBudget(t *testing.T) {
	for _, tc := range []struct {
		name string
		now  time.Time
		want []string
	}{
		{"before the open", time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC), []string{"ES", "NQ"}},
		{"in the session", time.Date(2026, 9, 23, 15, 0, 0, 0, time.UTC), []string{"NDX", "QQQ"}},
	} {
		release := make(chan struct{})
		asked := make(chan string, 64)
		now := tc.now
		s := &Server{now: func() time.Time { return now },
			marketReadLookForTest: func(ctx context.Context, c rpc.ContractParams) (broker.BriefQuote, string, error) {
				if !slices.Contains(tc.want, c.Symbol) {
					return broker.BriefQuote{}, "", context.DeadlineExceeded
				}
				asked <- c.Symbol
				<-release // a quote that never answers within the read
				return broker.BriefQuote{}, "", context.DeadlineExceeded
			}}
		start := time.Now()
		r, err := s.handleMarketRead(t.Context(), &rpc.Request{})
		took := time.Since(start)
		close(release)
		if err != nil {
			t.Fatal(err)
		}
		if took > marketReadBudget+time.Second {
			t.Fatalf("%s: the read took %s, over its %s budget", tc.name, took, marketReadBudget)
		}
		seen := map[string]bool{}
		for len(asked) > 0 {
			seen[<-asked] = true
		}
		for _, sym := range tc.want {
			if !seen[sym] {
				t.Errorf("%s: %s was not looked at (%v)", tc.name, sym, seen)
			}
		}
		switch {
		case r.Session.State == rpc.MarketReadPre && (r.Indicated == nil || r.Indicated.IndexUnavailable == "" || r.Indicated.NasdaqUnavailable == ""):
			t.Errorf("%s: unanswered futures not marked unavailable: %+v", tc.name, r.Indicated)
		case r.Session.State == rpc.MarketReadOpen && (r.Relative == nil || r.Relative.NasdaqUnavailable == ""):
			t.Errorf("%s: unanswered Nasdaq not marked unavailable: %+v", tc.name, r.Relative)
		}
	}
}
