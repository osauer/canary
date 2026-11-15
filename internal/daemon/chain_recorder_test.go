package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/osauer/canary/v3/internal/chainrec"
	"github.com/osauer/canary/v3/internal/config"
	"github.com/osauer/canary/v3/internal/rpc"
	ibkrlib "github.com/osauer/canary/v3/pkg/ibkr"
)

func newChainRecorderTestServer(t *testing.T) *Server {
	t.Helper()
	srv := newPlatformSettingsTestServer(t, config.Trading{})
	srv.logger = NewLogger(io.Discard, "warn")
	srv.coreStorePath = filepath.Join(t.TempDir(), "state", "daemon.db")
	return srv
}

func newYork(t *testing.T, value string) time.Time {
	t.Helper()
	at, err := time.ParseInLocation("2006-01-02 15:04", value, newYorkLocation())
	if err != nil {
		t.Fatal(err)
	}
	return at
}

func setChainRecorder(t *testing.T, srv *Server, recorder string) error {
	t.Helper()
	return srv.applyPlatformSettingsPatch(context.Background(), map[string]json.RawMessage{"chains": json.RawMessage(`{"recorder":` + recorder + `}`)}, rpc.OrderOriginHumanTTY)
}

// Collection needs no switch (strategy-register.md, section 4): unset, the
// recorder is on and records what holdings and registered strategies
// declare. With nothing declared and no held option underlying a due
// instant runs nothing and opens no file; false, the owner's off switch,
// keeps it off.
func TestChainRecorderRunsOnDeclarationsWithoutASwitch(t *testing.T) {
	srv := newChainRecorderTestServer(t)
	settings := chainRecorderSettingsFrom(srv.platformSettings.snapshot())
	if !settings.Enabled || len(settings.Symbols) != 0 || len(chainrec.DefaultSymbols) != 0 {
		t.Fatalf("default settings = %+v, want on with no research names", settings)
	}
	if srv.maybeRunChainRecorder(context.Background(), newYork(t, "2027-03-17 15:40")) {
		t.Fatal("the recorder ran with nothing declared")
	}
	res, err := srv.handleChainsRecorded(context.Background(), &rpc.Request{})
	if err != nil || !res.Recorder.Enabled || res.Recorder.NextRun == nil || res.Store.Present || len(res.Runs) != 0 || !res.Recorder.SymbolsDefault || len(res.Recorder.Declared) != 0 {
		t.Fatalf("chains.recorded = %+v, %v", res, err)
	}
	rows, err := srv.handleChainsRecordedRows(context.Background(), &rpc.Request{Params: json.RawMessage(`{"date":"2027-03-17"}`)})
	if err != nil || len(rows.Rows) != 0 {
		t.Fatalf("rows = %+v, %v", rows, err)
	}
	if _, err := os.Stat(filepath.Dir(srv.chainRecorderStorePath())); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a recorder with nothing to record touched the state directory: %v", err)
	}
	view := chainRecorderPlatformSettings(srv.platformSettings.snapshot())
	if !view.Recorder.Enabled.Value || view.Recorder.Enabled.Access != rpc.SettingsAccessWrite || len(view.Recorder.Symbols.Value) != 0 {
		t.Fatalf("settings view = %+v", view)
	}
	if err := setChainRecorder(t, srv, `{"enabled":false,"symbols":["SYNA"]}`); err != nil {
		t.Fatal(err)
	}
	if srv.maybeRunChainRecorder(context.Background(), newYork(t, "2027-03-17 15:40")) {
		t.Fatal("the recorder ran while the owner had switched it off")
	}
	if res, err := srv.handleChainsRecorded(context.Background(), &rpc.Request{}); err != nil || res.Recorder.Enabled || res.Recorder.NextRun != nil {
		t.Fatalf("off: chains.recorded = %+v, %v", res.Recorder, err)
	}
}

// A registered strategy's declared chains join the plan, after held option
// underlyings and before the research list, with the strategy as source
// (strategy-register.md, section 4). The core declares none.
func TestChainRecorderPlanCarriesTheDeclarations(t *testing.T) {
	srv, _ := mandateTestServer(t, testCoreConstitution())
	if got := srv.registerChainDeclarations(); len(got) != 0 {
		t.Fatalf("the core declares no chain, got %+v", got)
	}
	plan := srv.chainRecorderPlan(srv.platformSettings.snapshot())
	if !plan.Enabled || len(plan.Declared) != 0 {
		t.Fatalf("plan %+v", plan)
	}
}

func TestChainRecorderSettingsPatch(t *testing.T) {
	srv := newChainRecorderTestServer(t)
	if err := setChainRecorder(t, srv, `{"enabled":true,"symbols":["spx","SYNA","spx"]}`); err != nil {
		t.Fatal(err)
	}
	got := chainRecorderSettingsFrom(srv.platformSettings.snapshot())
	if !got.Enabled || !slices.Equal(got.Symbols, []string{"SPX", "SYNA"}) {
		t.Fatalf("settings = %+v", got)
	}
	if err := setChainRecorder(t, srv, `{"symbols":["SP X"]}`); err == nil || !strings.Contains(err.Error(), "chains.recorder.symbols") {
		t.Fatalf("invalid symbol err = %v", err)
	}
	if err := setChainRecorder(t, srv, `{"symbols":[]}`); err != nil {
		t.Fatal(err)
	}
	if got := chainRecorderSettingsFrom(srv.platformSettings.snapshot()); got.Symbols == nil || len(got.Symbols) != 0 {
		t.Fatalf("an empty list must record held underlyings only, got %+v", got.Symbols)
	}
	reloaded, err := newPlatformSettingsStore(srv.platformSettings.path)
	if err != nil {
		t.Fatal(err)
	}
	if data := reloaded.snapshot(); data.Version != platformSettingsDocVersion || data.Chains.Recorder.Enabled == nil || !*data.Chains.Recorder.Enabled ||
		data.Chains.Recorder.Symbols == nil || len(*data.Chains.Recorder.Symbols) != 0 {
		t.Fatalf("reloaded = %+v", data.Chains)
	}
	if err := setChainRecorder(t, srv, `{"enabled":null,"symbols":null}`); err != nil {
		t.Fatal(err)
	}
	if got := chainRecorderSettingsFrom(srv.platformSettings.snapshot()); !got.Enabled || !slices.Equal(got.Symbols, chainrec.DefaultSymbols) {
		t.Fatalf("null must restore the defaults, got %+v", got)
	}
}

// A version-5 document upgrades with the recorder off; one that already
// carries the recorder block is refused, so a rollback never misreads it.
func TestChainRecorderSettingsDocumentUpgradesStrictly(t *testing.T) {
	v5 := `{"version":5,"trading_control_generation":3,"display":{},"cash_sweep":{},"features":{},"trading":{"freeze":true,"tws_order_control":{"enabled":true}},"regime":{},"stress":{},"history":{}}`
	data, err := decodePlatformSettings([]byte(v5))
	if err != nil {
		t.Fatalf("decode v5: %v", err)
	}
	if data.Version != platformSettingsDocVersion || data.TradingControlGeneration != 3 || data.Trading.Freeze == nil || !*data.Trading.Freeze ||
		data.Trading.TWSOrderControl.Enabled == nil || data.Chains.Recorder.Enabled != nil || data.Chains.Recorder.Symbols != nil {
		t.Fatalf("upgraded v5 = %+v", data)
	}
	leaked := strings.Replace(v5, `"history":{}`, `"history":{},"chains":{"recorder":{"enabled":true}}`, 1)
	if _, err := decodePlatformSettings([]byte(leaked)); err == nil {
		t.Fatal("a v5 document carrying the recorder decoded")
	}
	data.Chains.Recorder.Enabled = new(true)
	data.Chains.Recorder.Symbols = &[]string{"SPX"}
	raw, _ := json.Marshal(data)
	again, err := decodePlatformSettings(raw)
	if err != nil || again.Chains.Recorder.Enabled == nil || !*again.Chains.Recorder.Enabled || !slices.Equal(*again.Chains.Recorder.Symbols, []string{"SPX"}) {
		t.Fatalf("v6 round trip = %+v, %v", again.Chains, err)
	}
}

// Enabled with no gateway: the run waits five minutes for one, then records
// the outage as a failed run, once per session.
func TestChainRecorderRecordsAnOutageOncePerSession(t *testing.T) {
	srv := newChainRecorderTestServer(t)
	if err := setChainRecorder(t, srv, `{"enabled":true,"symbols":["SPX","SYNA"]}`); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if srv.maybeRunChainRecorder(ctx, newYork(t, "2027-03-17 15:29")) {
		t.Fatal("ran before the slot")
	}
	if srv.maybeRunChainRecorder(ctx, newYork(t, "2027-03-17 15:32")) {
		t.Fatal("ran without a gateway inside the grace period")
	}
	if _, err := os.Stat(srv.chainRecorderStorePath()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("store created before a run: %v", err)
	}
	if !srv.maybeRunChainRecorder(ctx, newYork(t, "2027-03-17 15:36")) {
		t.Fatal("no run after the gateway grace")
	}
	if srv.maybeRunChainRecorder(ctx, newYork(t, "2027-03-17 15:37")) {
		t.Fatal("a session ran twice")
	}
	info, err := os.Stat(srv.chainRecorderStorePath())
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("store = %v, %v; want a private file", info, err)
	}
	res, err := srv.handleChainsRecorded(ctx, &rpc.Request{Params: json.RawMessage(`{"date":"2027-03-17"}`)})
	if err != nil || len(res.Runs) != 1 {
		t.Fatalf("chains.recorded = %+v, %v", res, err)
	}
	run := res.Runs[0]
	if run.Status != chainrec.RunFailed || run.StopReason != "no_contracts" || !run.HeldUnavailable || len(run.Symbols) != 2 ||
		run.Symbols[0].Status != chainrec.SymbolChainUnavailable || run.Coverage.Planned != 0 || res.Recorder.NextRun == nil || res.Recorder.Running {
		t.Fatalf("run = %+v", run)
	}
	srv.closeChainRecorderStore()
}

func TestHeldOptionUnderlyings(t *testing.T) {
	opt := func(sym, ccy string, qty float64) *ibkrlib.RawPosition {
		return &ibkrlib.RawPosition{Contract: ibkrlib.Contract{Symbol: sym, SecType: "OPT", Currency: ccy}, Position: qty}
	}
	got := heldOptionUnderlyings([]*ibkrlib.RawPosition{
		opt("SYNB", "USD", 2), opt("syna", "USD", -1), opt("SYNA", "", 3), opt("SYND", "EUR", 1), opt("SYNE", "USD", 0), nil,
		{Contract: ibkrlib.Contract{Symbol: "SYNC", SecType: "STK", Currency: "USD"}, Position: 100},
	})
	if !slices.Equal(got, []string{"SYNA", "SYNB"}) {
		t.Fatalf("held underlyings = %v, want SYNA SYNB", got)
	}
}

func TestChainRecorderFailureStatus(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want string
	}{
		{ibkrlib.ErrIBKRUnavailable, chainrec.StatusUnavailable},
		{&SubscriptionRejectedError{Rejection: ibkrlib.SubscriptionRejection{Code: 354}}, chainrec.StatusEntitlement},
		{&SubscriptionRejectedError{Rejection: ibkrlib.SubscriptionRejection{Code: 200}}, chainrec.StatusNoContract},
		{errors.New("rate limiter circuit breaker open until 2026-10-14T15:40:00Z"), chainrec.StatusPacing},
		{errors.New("request timeout after 30s"), chainrec.StatusTimeout},
		{errors.New("broker said no"), chainrec.StatusRejected},
	} {
		if got := chainRecorderFailureStatus(tc.err); got != tc.want {
			t.Errorf("%v = %s, want %s", tc.err, got, tc.want)
		}
	}
}

// The next run skips a session that already ran.
func TestChainRecorderNextRunSkipsARecordedSession(t *testing.T) {
	on := chainrec.Settings{Enabled: true, Symbols: chainrec.DefaultSymbols}
	now := newYork(t, "2027-03-17 15:45")
	st := chainRecorderState(on, true, false, now, func(string) bool { return false })
	if st.NextRun == nil || !st.NextRun.Equal(newYork(t, "2027-03-17 15:30")) {
		t.Fatalf("next run = %v, want today's slot", st.NextRun)
	}
	st = chainRecorderState(on, true, false, now, func(date string) bool { return date == "2027-03-17" })
	if st.NextRun == nil || !st.NextRun.Equal(newYork(t, "2027-03-18 15:30")) {
		t.Fatalf("next run = %v, want tomorrow's slot", st.NextRun)
	}
	if st := chainRecorderState(chainrec.Settings{}, true, false, now, nil); st.NextRun != nil || st.Symbols == nil {
		t.Fatalf("off state = %+v, want no next run", st)
	}
}
