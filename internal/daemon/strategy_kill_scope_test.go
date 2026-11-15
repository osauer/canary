package daemon

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/osauer/canary/v3/internal/core"
	"github.com/osauer/canary/v3/internal/daemon/corestore"
	"github.com/osauer/canary/v3/internal/rpc"
)

// A live entry's kill marks belong to one account, its paper or live mode,
// the strategy and its line (the v3.25.4 review's finding 1). Keyed by
// strategy and month alone, account A's September mark set "marked" for
// account B in the same state database and B's drawdown of 0.13 K past a
// 0.10 K bound read 0.00 K, not breached. Each test below failed or would
// have failed on that key.

// otherLineConID is a second synthetic line of the trend fund.
const otherLineConID = trendFundConID + 1

// killSeptember is the September decision every test measures: closed at
// 80 on a 9,200 sleeve budget at scale 25, so K at the stage is 15,333.33
// and the entry's own 100 bought at 100 are a fall of 0.1304 K.
var killSeptember = killMark{session: "2026-09-30", close: 80, sleeveBudget: 9200, scalePct: 25}

func testMarkBinding(t *testing.T, srv *Server, id string, conID int) liveMarkBinding {
	t.Helper()
	b, err := liveMarkBindingFor(id, srv.currentBrokerStateScope(), conID)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func testLiveMarks(t *testing.T, srv *Server, id string, conID int) ([]liveMark, error) {
	t.Helper()
	return srv.liveMarks(t.Context(), testMarkBinding(t, srv, id, conID))
}

// keepMark keeps the trend fund's mark of a month on conID under the
// server's current account, through the refresh's writer.
func keepMark(t *testing.T, srv *Server, conID int, m liveMark) {
	t.Helper()
	res := &rpc.StrategyTargetResult{Decision: &rpc.CoreDecision{Decision: m.Session, Close: m.Close, FrozenAt: coreDecisionNow},
		Sizing: &rpc.CoreSizingView{Current: core.Size{SleeveBudget: m.SleeveBudget}}, Instrument: &rpc.CoreInstrument{ConID: conID}}
	r, _ := srv.resolvedStrategy(srv.riskPolicies.snapshot().policy, trendFundID)
	r.ScalePct = m.ScalePct
	srv.liveMarkFillsForTest = func() strategyFills { return strategyFills{Units: m.Units, Cash: m.Cash, Execs: 1} }
	defer func() { srv.liveMarkFillsForTest = nil }()
	srv.recordLiveMark(t.Context(), res, r, srv.currentBrokerStateScope(), coreDecisionNow)
}

// zeroSeptember is a September mark that records no P&L.
var zeroSeptember = liveMark{Session: "2026-09-30", Close: 80, SleeveBudget: 9200, ScalePct: 25}

func selectAccount(srv *Server, account string) {
	srv.cfg.Gateway.Account = account
	srv.mu.Lock()
	srv.endpoint.Account = account
	srv.mu.Unlock()
}

func killTrendFund(t *testing.T, srv *Server, conID int) *rpc.KillView {
	t.Helper()
	r, _ := srv.resolvedStrategy(srv.riskPolicies.snapshot().policy, trendFundID)
	kill, err := srv.strategyKillLive(t.Context(), r, killSeptember, conID)
	if err != nil {
		t.Fatal(err)
	}
	return kill
}

func killTrendFundServer(t *testing.T) *Server {
	t.Helper()
	c := testLiveTrendConstitution()
	c.Mandate.Strategies[trendFundID].KillDrawdownK = new(0.10)
	srv, _ := liveTrendServer(t, c, mandateTestPositions(100), coreDecisionNow)
	return srv
}

// wantBreach is account B's verdict on its own 100 bought at 100: one
// decision, a fall of 0.1304 K, past the 0.10 K bound.
func wantBreach(t *testing.T, kill *rpc.KillView, why string) {
	t.Helper()
	want := 2000 / stageCapital(9200, 25)
	if kill == nil || !kill.Measured || !kill.Breached || kill.Decisions != 1 || kill.DrawdownK < want-1e-9 || kill.DrawdownK > want+1e-9 {
		t.Fatalf("%s: kill %+v, want one decision at %.4f K past the 0.10 K bound", why, kill, want)
	}
}

// The review's witness, kept: account A's zero-loss September mark does not
// mark account B's September, and B measures past its bound exactly as it
// does on an empty store (the control).
func TestLiveMarksCannotCrossAccountScope(t *testing.T) {
	srv := killTrendFundServer(t)
	scopeA := srv.currentBrokerStateScope()
	keepMark(t, srv, trendFundConID, zeroSeptember)
	selectAccount(srv, "DU7654321")
	if sameBrokerScope(scopeA, srv.currentBrokerStateScope()) {
		t.Fatal("the fixture did not change the account")
	}
	recordEntryFill(t, srv, "synthetic-scope-b", "synthetic-scope-b-fill", rpc.OrderActionBuy, 100, 100, coreDecisionNow)
	if own := srv.strategyOwnFills(t.Context(), trendFundID, trendFundConID); own.Units != 100 || own.Cash != -10000 {
		t.Fatalf("account B's own fills %+v", own)
	}
	wantBreach(t, killTrendFund(t, srv, trendFundConID), "account B beside A's mark")

	empty, err := corestore.Open(t.Context(), corestore.Options{Path: filepath.Join(privateTestDir(t), "empty-marks.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer empty.Close()
	kept := srv.coreStore
	srv.coreStore = empty
	wantBreach(t, killTrendFund(t, srv, trendFundConID), "account B on an empty store")
	srv.coreStore = kept

	// A's mark is still A's: back on A it is read, and marks its month.
	selectAccount(srv, scopeA.Account)
	if marks, err := testLiveMarks(t, srv, trendFundID, trendFundConID); err != nil || len(marks) != 1 || marks[0].Account != scopeA.Account || marks[0].Mode != scopeA.Mode || marks[0].ConID != trendFundConID {
		t.Fatalf("account A's marks %+v %v", marks, err)
	}
}

// A paper account's mark does not mark the live account's month. The mode
// is part of the binding on its own: a mark kept under one mode is not read
// under the other, and a mark that names another mode than the scope it is
// kept under holds the entry rather than counting.
func TestLiveMarksCannotCrossPaperAndLive(t *testing.T) {
	srv := killTrendFundServer(t)
	if srv.currentBrokerStateScope().Mode != rpc.AccountModePaper {
		t.Fatalf("fixture scope %+v, want paper", srv.currentBrokerStateScope())
	}
	keepMark(t, srv, trendFundConID, zeroSeptember)
	selectAccount(srv, "U1234567")
	if srv.currentBrokerStateScope().Mode != rpc.AccountModeLive {
		t.Fatalf("scope %+v, want live", srv.currentBrokerStateScope())
	}
	recordEntryFill(t, srv, "synthetic-live", "synthetic-live-fill", rpc.OrderActionBuy, 100, 100, coreDecisionNow)
	wantBreach(t, killTrendFund(t, srv, trendFundConID), "the live account beside the paper mark")

	paper, err := liveMarkBindingFor(trendFundID, brokerStateScope{Account: "DU1234567", Mode: rpc.AccountModePaper}, trendFundConID)
	if err != nil {
		t.Fatal(err)
	}
	live, err := liveMarkBindingFor(trendFundID, brokerStateScope{Account: "DU1234567", Mode: rpc.AccountModeLive}, trendFundConID)
	if err != nil {
		t.Fatal(err)
	}
	if paper.scopeKey() == live.scopeKey() {
		t.Fatalf("one scope for both modes: %s", paper.scopeKey())
	}
	if marks, err := srv.liveMarks(t.Context(), live); err != nil || len(marks) != 0 {
		t.Fatalf("the paper mark read under live: %+v %v", marks, err)
	}
	// A paper mark copied under the live scope is refused, not counted.
	raw, _ := json.Marshal(liveMark{Version: 2, Strategy: trendFundID, Account: "DU1234567", Mode: rpc.AccountModePaper, ConID: trendFundConID, Session: "2026-08-31", Close: 100, SleeveBudget: 9200})
	if _, err := srv.coreStore.AppendEvents(t.Context(), []corestore.EventInput{{ScopeKey: live.scopeKey(), EventKey: liveMarkKey(trendFundID, "2026-08-31"), Type: liveMarkType, Action: "mark", Origin: coreEventOriginDaemon, OccurredAt: coreDecisionNow, PayloadJSON: raw}}); err != nil {
		t.Fatal(err)
	}
	if _, err := srv.liveMarks(t.Context(), live); err == nil || !strings.Contains(err.Error(), "names another account, mode or line") {
		t.Fatalf("a mismatched mark read without refusal: %v", err)
	}
}

// Another line's mark of the same month does not mark this line's month: a
// zero-loss mark on a second line of the fund leaves the traded line's own
// fall measured.
func TestLiveMarksCannotCrossInstruments(t *testing.T) {
	srv := killTrendFundServer(t)
	keepMark(t, srv, otherLineConID, zeroSeptember)
	recordEntryFill(t, srv, "synthetic-line", "synthetic-line-fill", rpc.OrderActionBuy, 100, 100, coreDecisionNow)
	wantBreach(t, killTrendFund(t, srv, trendFundConID), "the traded line beside another line's mark")
	if marks, err := testLiveMarks(t, srv, trendFundID, otherLineConID); err != nil || len(marks) != 1 {
		t.Fatalf("the other line's mark %+v %v", marks, err)
	}
}

// Within one scope a month is marked once: a second write of the month
// keeps the first, and the reading of a marked month is not counted again.
func TestLiveMarksDeduplicateTheMonthWithinOneScope(t *testing.T) {
	srv := killTrendFundServer(t)
	keepMark(t, srv, trendFundConID, liveMark{Session: "2026-09-30", Close: 80, Units: 100, Cash: -10000, SleeveBudget: 9200, ScalePct: 25})
	keepMark(t, srv, trendFundConID, zeroSeptember)
	marks, err := testLiveMarks(t, srv, trendFundID, trendFundConID)
	if err != nil || len(marks) != 1 || marks[0].Units != 100 || marks[0].Cash != -10000 {
		t.Fatalf("marks %+v %v, want the month's first mark once", marks, err)
	}
	// No fills are journaled: the kept mark, not a second reading of zero,
	// is September's measure.
	wantBreach(t, killTrendFund(t, srv, trendFundConID), "the month's kept mark")
}

// Marks survive a restart of the store under their own scope only: reopened,
// account A reads its mark and account B reads none.
func TestLiveMarksSurviveReopenWithinTheirScope(t *testing.T) {
	srv := killTrendFundServer(t)
	path := filepath.Join(privateTestDir(t), "reopen.db")
	store, err := corestore.Open(t.Context(), corestore.Options{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	srv.coreStore = store
	scopeA := srv.currentBrokerStateScope()
	keepMark(t, srv, trendFundConID, zeroSeptember)
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := corestore.Open(t.Context(), corestore.Options{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	srv.coreStore = reopened
	if marks, err := testLiveMarks(t, srv, trendFundID, trendFundConID); err != nil || len(marks) != 1 || marks[0].Account != scopeA.Account {
		t.Fatalf("account A's marks after reopening %+v %v", marks, err)
	}
	if kill := killTrendFund(t, srv, trendFundConID); kill.Decisions != 1 || kill.Breached {
		t.Fatalf("account A after reopening: kill %+v, want its kept zero mark alone", kill)
	}
	selectAccount(srv, "DU7654321")
	recordEntryFill(t, srv, "synthetic-reopen", "synthetic-reopen-fill", rpc.OrderActionBuy, 100, 100, coreDecisionNow)
	wantBreach(t, killTrendFund(t, srv, trendFundConID), "account B after reopening")
}

// A mark v3.25.4 or earlier kept names no account, mode or line, so it is
// nobody's: it never marks a month, for the account that may have written it
// or any other, and the month is measured afresh on the current account's
// own fills. The row stays in the event log, readable for audit.
func TestLegacyLiveMarksNeverCount(t *testing.T) {
	srv := killTrendFundServer(t)
	legacy := liveMark{Version: 1, Strategy: trendFundID, Session: "2026-09-30", Close: 80, SleeveBudget: 9200, ScalePct: 25, RecordedAt: coreDecisionNow}
	raw, _ := json.Marshal(legacy)
	key := liveMarkKey(trendFundID, legacy.Session)
	if _, err := srv.coreStore.AppendEvents(t.Context(), []corestore.EventInput{{ScopeKey: legacyLiveMarkScope(trendFundID), EventKey: key, Type: legacyLiveMarkType, Action: "mark", Origin: coreEventOriginDaemon, OccurredAt: coreDecisionNow, PayloadJSON: raw}}); err != nil {
		t.Fatal(err)
	}
	for _, account := range []string{"DU1234567", "DU7654321"} {
		selectAccount(srv, account)
		recordEntryFill(t, srv, "synthetic-legacy-"+account, "synthetic-legacy-fill-"+account, rpc.OrderActionBuy, 100, 100, coreDecisionNow)
		wantBreach(t, killTrendFund(t, srv, trendFundConID), "account "+account+" beside a legacy mark")
		if marks, err := testLiveMarks(t, srv, trendFundID, trendFundConID); err != nil || len(marks) != 0 {
			t.Fatalf("the legacy mark read as account %s's: %+v %v", account, marks, err)
		}
	}
	// The month is kept afresh under the current scope beside the legacy row,
	// which is left as it was written.
	keepMark(t, srv, trendFundConID, zeroSeptember)
	if marks, err := testLiveMarks(t, srv, trendFundID, trendFundConID); err != nil || len(marks) != 1 || marks[0].Account != "DU7654321" {
		t.Fatalf("the scoped mark beside the legacy one %+v %v", marks, err)
	}
	ev, ok, err := srv.coreStore.GetEvent(t.Context(), legacyLiveMarkScope(trendFundID), key)
	if err != nil || !ok || ev.Type != legacyLiveMarkType || string(ev.PayloadJSON) != string(raw) {
		t.Fatalf("the legacy row %+v %v %v, want it kept as written", ev, ok, err)
	}
}

// Without a resolved account and mode nothing is measured: no verdict, no
// mark kept, and the live entry holds naming the account, never "measured,
// not breached".
func TestLiveKillHoldsWhileTheAccountIsUnresolved(t *testing.T) {
	srv := killTrendFundServer(t)
	recordEntryFill(t, srv, "synthetic-unresolved", "synthetic-unresolved-fill", rpc.OrderActionBuy, 100, 100, coreDecisionNow)
	for _, account := range []string{"", "All", "DU1234567,DU7654321"} {
		selectAccount(srv, account)
		srv.mu.Lock()
		srv.endpoint.Port = 0
		srv.mu.Unlock()
		srv.cfg.Gateway.Port = nil
		if brokerScopeConcrete(srv.currentBrokerStateScope()) {
			t.Fatalf("fixture scope %+v is resolved", srv.currentBrokerStateScope())
		}
		r, _ := srv.resolvedStrategy(srv.riskPolicies.snapshot().policy, trendFundID)
		kill, err := srv.strategyKillLive(t.Context(), r, killSeptember, trendFundConID)
		if !errors.Is(err, errLiveMarkScopeUnresolved) || kill != nil {
			t.Fatalf("account %q: kill %+v err %v, want no verdict", account, kill, err)
		}
		keepMark(t, srv, trendFundConID, zeroSeptember)
		res := liveTrendTarget(t, srv)
		if res.Status != rpc.CoreStatusHold || res.Kill != nil || !slices.ContainsFunc(res.Reasons, func(r string) bool {
			return strings.Contains(r, "kill rule: the account and its paper or live mode are not resolved")
		}) {
			t.Fatalf("account %q: status %s kill %+v reasons %v", account, res.Status, res.Kill, res.Reasons)
		}
	}
	if events, err := srv.coreStore.LoadEvents(t.Context(), corestore.EventQuery{Type: liveMarkType, Limit: 10}); err != nil || len(events) != 0 {
		t.Fatalf("marks kept without an account: %+v %v", events, err)
	}
}
