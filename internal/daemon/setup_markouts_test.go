package daemon

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/daemon/corestore"
	"github.com/osauer/canary/v2/internal/marketcal"
	"github.com/osauer/canary/v2/internal/rpc"
	ibkrlib "github.com/osauer/canary/v2/pkg/ibkr"
)

func markoutET(t *testing.T, value string) time.Time {
	t.Helper()
	at, err := time.ParseInLocation("2006-01-02 15:04", value, mustLocation(t, "America/New_York"))
	if err != nil {
		t.Fatal(err)
	}
	return at
}

func markoutFill(at time.Time, orderRef, execID string) orderJournalEvent {
	return orderJournalEvent{
		Version: orderJournalFileVersion, At: at, Type: orderJournalEventStatusUpdated, Status: "Execution",
		OrderRef: orderRef, PreviewTokenID: "tok-" + orderRef, ReservedOrderID: 41, ClientID: 15, PermID: 7001,
		Account: "DU1234567", Endpoint: "127.0.0.1:4002", Mode: "paper",
		Symbol: "SYNX", SecType: "STK", ConID: 9001, Exchange: "SMART", Currency: "USD",
		Action: "BUY", Quantity: 100, Filled: 100, ExecID: execID, ExecShares: 100, ExecSide: "BOT", LastFillPrice: 10,
		ExecTime: at.UTC().Format("20060102-15:04:05"),
	}
}

type markoutTestClock struct{ at time.Time }

func (c *markoutTestClock) now() time.Time { return c.at }

func newMarkoutTestServer(t *testing.T, dbPath string, clock *markoutTestClock) (*Server, *corestore.Store) {
	t.Helper()
	core, err := corestore.Open(t.Context(), corestore.Options{Path: dbPath})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = core.Close() })
	s := &Server{now: clock.now, logger: NewLogger(nil, "error")}
	store, err := bindSetupMarkoutStore(t.Context(), core, s.setupMarkoutNow())
	if err != nil {
		t.Fatal(err)
	}
	s.setupMarkouts = newSetupMarkoutRuntime(store)
	return s, core
}

func markoutsByKey(t *testing.T, s *Server) map[string]rpc.SetupMarkoutTarget {
	t.Helper()
	rows, _, err := s.setupMarkouts.store.list(t.Context(), setupMarkoutFilter{})
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]rpc.SetupMarkoutTarget{}
	for _, row := range rows {
		out[row.ExecID+"/"+row.Horizon] = row
	}
	return out
}

func TestSetupMarkoutHorizonsFollowTheExchangeCalendar(t *testing.T) {
	t.Parallel()
	cal := marketcal.New()
	cases := []struct {
		name, fill, t30, nextClose string
	}{
		{"inside session", "2026-10-01 10:00", "2026-10-01 10:30", "2026-10-02 16:00"},
		{"carries remainder across the next open", "2026-10-01 15:45", "2026-10-02 09:45", "2026-10-02 16:00"},
		{"carries across a weekend", "2026-10-02 15:50", "2026-10-05 09:50", "2026-10-05 16:00"},
		{"pre-market fill counts from the open", "2026-10-01 08:00", "2026-10-01 10:00", "2026-10-01 16:00"},
		{"after-hours fill counts from the next open", "2026-10-01 17:30", "2026-10-02 10:00", "2026-10-02 16:00"},
		{"skips the holiday and uses the early close", "2026-11-25 15:50", "2026-11-27 09:50", "2026-11-27 13:00"},
		{"early close carries into the next week", "2026-11-27 12:45", "2026-11-30 09:45", "2026-11-30 16:00"},
		{"ends exactly at the close", "2026-10-01 15:30", "2026-10-01 16:00", "2026-10-02 16:00"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fill := markoutET(t, tc.fill)
			t30, ok := setupMarkoutRegularMinutesAfter(cal, marketcal.MarketUSEquity, fill, setupMarkoutRegularMinutes)
			if !ok || !t30.Equal(markoutET(t, tc.t30)) {
				t.Fatalf("t30 = %s ok=%t, want %s", t30, ok, tc.t30)
			}
			closeAt, ok := setupMarkoutNextClose(cal, marketcal.MarketUSEquity, fill)
			if !ok || !closeAt.Equal(markoutET(t, tc.nextClose)) {
				t.Fatalf("next close = %s ok=%t, want %s", closeAt, ok, tc.nextClose)
			}
		})
	}
	if _, ok := setupMarkoutRegularMinutesAfter(cal, marketcal.MarketUSEquity, markoutET(t, "2028-12-29 15:50"), setupMarkoutRegularMinutes); ok {
		t.Fatal("a target beyond embedded calendar coverage was invented")
	}
}

func TestSetupMarkoutOptionsUseTheSingleNameSession(t *testing.T) {
	t.Parallel()
	fill := markoutFill(markoutET(t, "2026-10-01 15:50"), "ref-opt", "exec-opt")
	fill.SecType, fill.ConID, fill.Expiry, fill.Strike, fill.Right, fill.Multiplier = "OPT", 9101, "20261120", 25, "C", 100
	targets := setupMarkoutTargetsForFill(fill, marketcal.New(), fill.At)
	if targets[0].Status != rpc.SetupMarkoutPending || !targets[0].TargetAt.Equal(markoutET(t, "2026-10-02 09:50")) {
		t.Fatalf("option t30 = %+v", targets[0])
	}
	if !targets[1].TargetAt.Equal(markoutET(t, "2026-10-02 16:00")) || targets[1].Contract.Right != "C" || targets[1].Contract.Multiplier != 100 {
		t.Fatalf("option next close used another session or lost identity: %+v", targets[1])
	}
	fill.Multiplier = 0
	for _, target := range setupMarkoutTargetsForFill(fill, marketcal.New(), fill.At) {
		if target.Status != rpc.SetupMarkoutMissing || target.Reason != "option_identity_incomplete" {
			t.Fatalf("incomplete option identity was scheduled: %+v", target)
		}
	}
}

func TestSetupMarkoutAcceptsOnlyCanaryPreviewPathFills(t *testing.T) {
	t.Parallel()
	base := markoutFill(markoutET(t, "2026-10-01 10:00"), "ref-1", "exec-1")
	if !setupMarkoutEligibleFill(base) {
		t.Fatal("preview-path stock execution was not eligible")
	}
	for name, mutate := range map[string]func(*orderJournalEvent){
		"manual order":     func(ev *orderJournalEvent) { ev.PreviewTokenID = "" },
		"bypass preview":   func(ev *orderJournalEvent) { ev.BypassPreview = true },
		"order status":     func(ev *orderJournalEvent) { ev.ExecID = "" },
		"no order ref":     func(ev *orderJournalEvent) { ev.OrderRef = "" },
		"bond":             func(ev *orderJournalEvent) { ev.SecType = "BOND" },
		"broker ack event": func(ev *orderJournalEvent) { ev.Type = orderJournalEventBrokerAcknowledged },
	} {
		ev := base
		mutate(&ev)
		if setupMarkoutEligibleFill(ev) {
			t.Fatalf("%s was eligible", name)
		}
	}
	for name, tc := range map[string]struct {
		mutate func(*orderJournalEvent)
		reason string
	}{
		"quantity": {func(ev *orderJournalEvent) { ev.ExecShares = 0 }, "fill_quantity_unknown"},
		"price":    {func(ev *orderJournalEvent) { ev.LastFillPrice = 0 }, "fill_price_unknown"},
		"side":     {func(ev *orderJournalEvent) { ev.ExecSide, ev.Action = "", "" }, "fill_side_unknown"},
		"identity": {func(ev *orderJournalEvent) { ev.ConID = 0 }, "contract_identity_unknown"},
	} {
		ev := base
		tc.mutate(&ev)
		for _, target := range setupMarkoutTargetsForFill(ev, marketcal.New(), ev.At) {
			if target.Status != rpc.SetupMarkoutMissing || target.Reason != tc.reason || target.Bid != nil {
				t.Fatalf("%s: %+v", name, target)
			}
		}
	}
}

func TestSetupMarkoutFillTimePrefersZonedBrokerTime(t *testing.T) {
	t.Parallel()
	receipt := markoutET(t, "2026-10-01 10:00").Add(2 * time.Second)
	ev := markoutFill(receipt, "ref-1", "exec-1")
	for raw, want := range map[string]string{
		"20261001 10:00:00 US/Eastern":       "broker_exec_time",
		"20261001 10:00:00 America/New_York": "broker_exec_time",
		"20261001-14:00:00":                  "broker_exec_time",
		"20261001  10:00:00":                 "journal_receipt",
		"20261001 11:00:00 America/New_York": "journal_receipt",
		"untrusted broker time":              "journal_receipt",
	} {
		ev.ExecTime = raw
		at, source := setupMarkoutFillTime(ev)
		if source != want {
			t.Fatalf("%q source = %s, want %s", raw, source, want)
		}
		if want == "broker_exec_time" && !at.Equal(markoutET(t, "2026-10-01 10:00")) {
			t.Fatalf("%q parsed to %s", raw, at)
		}
		if want == "journal_receipt" && !at.Equal(receipt) {
			t.Fatalf("%q fell back to %s", raw, at)
		}
	}
}

func TestOrderLifecycleExecutionKeepsItsOwnQuantityAndSide(t *testing.T) {
	t.Parallel()
	ev, ok := orderJournalEventFromLifecycle(ibkrlib.OrderLifecycleEvent{
		Type: ibkrlib.OrderLifecycleEventExecDetails, OrderID: 41, OrderRef: "ref-1", ExecID: "exec-1",
		Shares: 40, CumQty: 100, Price: 10.5, ExecutionSide: "BOT", ExecTime: "20261001-14:00:00",
	}, markoutET(t, "2026-10-01 10:00"))
	if !ok || ev.ExecShares != 40 || ev.Filled != 100 || ev.ExecSide != "BOT" || ev.LastFillPrice != 10.5 {
		t.Fatalf("execution journal event = %+v ok=%t", ev, ok)
	}
}

func TestSetupMarkoutScheduleSurvivesRestartAndRecordsMissedTargets(t *testing.T) {
	t.Parallel()
	dbPath := filepath.Join(privateTestDir(t), "daemon.db")
	fillAt := markoutET(t, "2026-10-01 10:00")
	clock := &markoutTestClock{at: fillAt.Add(time.Second)}
	s, core := newMarkoutTestServer(t, dbPath, clock)
	ev := markoutFill(fillAt, "ref-1", "exec-1")
	ev.At = clock.at // durable receipt one second after the broker fill
	s.scheduleSetupMarkoutFill(t.Context(), ev)
	s.scheduleSetupMarkoutFill(t.Context(), ev)
	if pending := s.setupMarkouts.store.pending(); len(pending) != 2 {
		t.Fatalf("pending = %d, want two horizons once", len(pending))
	}
	head, err := core.AuthorityHead(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if head.LastEventSeq != 0 {
		t.Fatalf("markout schedule advanced the order-event frontier to %d", head.LastEventSeq)
	}
	_ = core.Close()

	// The daemon was down across the t30 target.
	clock.at = markoutET(t, "2026-10-01 11:00")
	restarted, _ := newMarkoutTestServer(t, dbPath, clock)
	if pending := restarted.setupMarkouts.store.pending(); len(pending) != 2 {
		t.Fatalf("restart lost the schedule: %d pending", len(pending))
	}
	restarted.sweepSetupMarkouts(t.Context(), clock.at)
	rows := markoutsByKey(t, restarted)
	t30, closeTarget := rows["exec-1/"+rpc.SetupMarkoutHorizonT30], rows["exec-1/"+rpc.SetupMarkoutHorizonNextClose]
	if t30.Status != rpc.SetupMarkoutMissing || t30.Reason != "capture_window_missed" || t30.Bid != nil || t30.Markout != nil {
		t.Fatalf("missed target = %+v", t30)
	}
	if !t30.TargetAt.Equal(markoutET(t, "2026-10-01 10:30")) || !t30.ResolvedAt.Equal(clock.at.UTC()) {
		t.Fatalf("missed target lost its clocks: %+v", t30)
	}
	if closeTarget.Status != rpc.SetupMarkoutPending {
		t.Fatalf("future target resolved early: %+v", closeTarget)
	}
	// Replaying the same execution never reopens a resolved target.
	restarted.scheduleSetupMarkoutFill(t.Context(), ev)
	if pending := restarted.setupMarkouts.store.pending(); len(pending) != 1 || pending[0].Horizon != rpc.SetupMarkoutHorizonNextClose {
		t.Fatalf("replay reopened a resolved target: %+v", pending)
	}
}

func TestSetupMarkoutLateScheduleIsMissingNotBackdated(t *testing.T) {
	t.Parallel()
	fillAt := markoutET(t, "2026-10-01 10:00")
	clock := &markoutTestClock{at: markoutET(t, "2026-10-01 10:45")}
	s, _ := newMarkoutTestServer(t, filepath.Join(privateTestDir(t), "daemon.db"), clock)
	ev := markoutFill(clock.at, "ref-1", "exec-1")
	ev.ExecTime = fillAt.UTC().Format("20060102-15:04:05")
	s.scheduleSetupMarkoutFill(t.Context(), ev)
	rows := markoutsByKey(t, s)
	if got := rows["exec-1/t30"]; got.Status != rpc.SetupMarkoutMissing || got.Reason != "scheduled_after_target" || !got.FillAt.Equal(fillAt.UTC()) {
		t.Fatalf("late-seen fill = %+v", got)
	}
	if got := rows["exec-1/next_close"]; got.Status != rpc.SetupMarkoutPending {
		t.Fatalf("future horizon = %+v", got)
	}
}

func TestSetupMarkoutBacklogIsBoundedOldestFirst(t *testing.T) {
	t.Parallel()
	fillAt := markoutET(t, "2026-10-01 10:00")
	clock := &markoutTestClock{at: fillAt}
	s, _ := newMarkoutTestServer(t, filepath.Join(privateTestDir(t), "daemon.db"), clock)
	fills := setupMarkoutMaxPending/2 + 2
	for i := range fills {
		clock.at = fillAt.Add(time.Duration(i) * time.Second)
		s.scheduleSetupMarkoutFill(t.Context(), markoutFill(clock.at, fmt.Sprintf("ref-%03d", i), fmt.Sprintf("exec-%03d", i)))
	}
	if pending := s.setupMarkouts.store.pending(); len(pending) != setupMarkoutMaxPending {
		t.Fatalf("pending = %d, want bound %d", len(pending), setupMarkoutMaxPending)
	}
	rows := markoutsByKey(t, s)
	for _, key := range []string{"exec-000/t30", "exec-000/next_close", "exec-001/t30", "exec-001/next_close"} {
		if rows[key].Status != rpc.SetupMarkoutMissing || rows[key].Reason != "backlog_full" {
			t.Fatalf("%s = %+v, want oldest resolved as backlog_full", key, rows[key])
		}
	}
	if rows["exec-002/t30"].Status != rpc.SetupMarkoutPending {
		t.Fatal("a newer target was dropped before an older one")
	}
}

func TestSetupMarkoutOfferNeverBlocksAndRescanRecoversFromTheJournal(t *testing.T) {
	t.Parallel()
	dir := privateTestDir(t)
	fillAt := markoutET(t, "2026-10-01 10:00")
	clock := &markoutTestClock{at: fillAt}
	s, _ := newMarkoutTestServer(t, filepath.Join(dir, "daemon.db"), clock)
	s.orderJournal = newTestOrderJournalStore(t, filepath.Join(dir, "order-journal.jsonl"))
	for i := range setupMarkoutFillQueue + 3 {
		ev := markoutFill(fillAt.Add(time.Duration(i)*time.Millisecond), fmt.Sprintf("ref-%03d", i), fmt.Sprintf("exec-%03d", i))
		if i < 3 {
			if err := s.orderJournal.Append(ev); err != nil {
				t.Fatal(err)
			}
		}
		done := make(chan struct{})
		go func() { s.offerSetupMarkoutFill(ev); close(done) }()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("fill handling blocked on the markout queue")
		}
	}
	if !s.setupMarkouts.rescan.Load() {
		t.Fatal("queue overflow did not latch a journal rescan")
	}
	manual := markoutFill(fillAt, "ref-manual", "exec-manual")
	manual.PreviewTokenID = ""
	if err := s.orderJournal.Append(manual); err != nil {
		t.Fatal(err)
	}
	s.rescanSetupMarkoutFills(t.Context())
	rows := markoutsByKey(t, s)
	if len(rows) != 6 || rows["exec-000/t30"].Status != rpc.SetupMarkoutPending {
		t.Fatalf("rescan scheduled %d targets: %+v", len(rows), rows)
	}
	if _, ok := rows["exec-manual/t30"]; ok {
		t.Fatal("rescan scheduled a manual order")
	}
}

func TestSetupMarkoutsReadFiltersAndLabelsTheDiagnostic(t *testing.T) {
	t.Parallel()
	fillAt := markoutET(t, "2026-10-01 10:00")
	clock := &markoutTestClock{at: fillAt.Add(time.Second)}
	s, _ := newMarkoutTestServer(t, filepath.Join(privateTestDir(t), "daemon.db"), clock)
	first, second := markoutFill(fillAt, "ref-1", "exec-1"), markoutFill(fillAt.Add(24*time.Hour), "ref-2", "exec-2")
	second.Symbol = "AAA"
	first.At = clock.at
	s.scheduleSetupMarkoutFill(t.Context(), first)
	clock.at = fillAt.Add(24*time.Hour + time.Second)
	second.At = clock.at
	s.scheduleSetupMarkoutFill(t.Context(), second)
	s.sweepSetupMarkouts(t.Context(), clock.at)

	read := func(p rpc.SetupMarkoutsParams) *rpc.SetupMarkoutsResult {
		t.Helper()
		raw, _ := json.Marshal(p)
		out, err := s.handleSetupMarkouts(t.Context(), &rpc.Request{Params: raw})
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	all := read(rpc.SetupMarkoutsParams{})
	if all.Kind != rpc.SetupMarkoutsKind || len(all.Targets) != 4 || all.Clock.MaxPending != setupMarkoutMaxPending || all.Clock.Pending != 3 {
		t.Fatalf("ledger = %+v", all)
	}
	if all.Clock.NextTargetAt == nil || !all.Clock.NextTargetAt.Equal(markoutET(t, "2026-10-02 10:30")) || all.Clock.TrackingSince.IsZero() {
		t.Fatalf("schedule clock = %+v", all.Clock)
	}
	statuses := map[string]string{}
	for _, row := range all.Targets {
		statuses[row.ExecID+"/"+row.Horizon] = row.Status + ":" + row.Reason
	}
	if statuses["exec-1/t30"] != "missing:capture_window_missed" || statuses["exec-1/next_close"] != "pending:" || statuses["exec-2/t30"] != "pending:" {
		t.Fatalf("statuses = %v", statuses)
	}
	if got := read(rpc.SetupMarkoutsParams{OrderRef: "ref-2"}); len(got.Targets) != 2 || got.Targets[0].OrderRef != "ref-2" {
		t.Fatalf("order filter = %+v", got.Targets)
	}
	if got := read(rpc.SetupMarkoutsParams{Since: "2026-10-02"}); len(got.Targets) != 2 || got.Targets[0].ExecID != "exec-2" {
		t.Fatalf("since filter = %+v", got.Targets)
	}
	if got := read(rpc.SetupMarkoutsParams{Symbol: "synx"}); len(got.Targets) != 2 || got.Targets[0].Contract.Symbol != "SYNX" {
		t.Fatalf("symbol filter = %+v", got.Targets)
	}
	if got := read(rpc.SetupMarkoutsParams{Symbol: "BBB"}); got.Targets == nil || len(got.Targets) != 0 {
		t.Fatal("empty ledger is not an empty array")
	}
	raw, _ := json.Marshal(rpc.SetupMarkoutsParams{Since: "10/01/2026"})
	if _, err := s.handleSetupMarkouts(t.Context(), &rpc.Request{Params: raw}); err == nil {
		t.Fatal("malformed since was accepted")
	}
}
