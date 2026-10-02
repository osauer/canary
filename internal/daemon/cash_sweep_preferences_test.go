package daemon

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/osauer/canary/v2/internal/daemon/corestore"
	"github.com/osauer/canary/v2/internal/rpc"
)

const oldCashPreferenceSettings = `{"version":3,"trading_control_generation":9,"display":{"date_format":"eu"},"features":{"rulebook":{"enabled":false,"earnings_overrides":{"SYNTH":"2026-10-02"}}},"trading":{"freeze":true,"max_notional":300},"regime":{},"stress":{},"history":{}}`

func cashPreferenceServer(t *testing.T) (*Server, *corestore.Store) {
	t.Helper()
	core, err := corestore.Open(t.Context(), corestore.Options{Path: filepath.Join(privateTestDir(t), "daemon.db")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = core.Close() })
	_, err = core.CompareAndSwapStateDocument(t.Context(), corestore.StateDocumentCAS{ScopeKey: daemonStateScope, Kind: stateKindPlatformSettings, JSON: []byte(oldCashPreferenceSettings)})
	if err != nil {
		t.Fatal(err)
	}
	settings := &platformSettingsStore{}
	if err = settings.bindCore(t.Context(), core); err != nil {
		t.Fatal(err)
	}
	return &Server{platformSettings: settings}, core
}

func saveCashPreference(t *testing.T, s *Server, priority *string, revision int64, id string) (*rpc.CashSweepPreferences, error) {
	t.Helper()
	raw, _ := json.Marshal(rpc.SetCashSweepPriorityRequest{CurrencyPriority: priority, ExpectedRevision: revision, RequestID: id})
	return s.handleCashSweepPrioritySet(t.Context(), &rpc.Request{Params: raw})
}

func TestCashSweepPreferencesStrictV3UpgradeAndNoEagerWrite(t *testing.T) {
	s, core := cashPreferenceServer(t)
	doc, _, err := core.GetStateDocument(t.Context(), daemonStateScope, stateKindPlatformSettings)
	if err != nil {
		t.Fatal(err)
	}
	if doc.Revision != 1 || !bytes.Equal(doc.JSON, []byte(oldCashPreferenceSettings)) {
		t.Fatal("binding eagerly rewrote old settings")
	}
	if got := s.platformSettings.snapshot(); got.Version != 4 || got.TradingControlGeneration != 9 || got.Trading.Freeze == nil || !*got.Trading.Freeze || *got.Display.DateFormat != "eu" {
		t.Fatalf("upgrade lost controls: %+v", got)
	}
	for _, raw := range []string{
		strings.Replace(oldCashPreferenceSettings, `"display":`, `"cash_sweep":{"currency_priority":"balanced"},"display":`, 1),
		strings.Replace(oldCashPreferenceSettings, `"version":3`, `"version":4,"cash_sweep":{"currency_priority":"all"}`, 1),
		strings.Replace(oldCashPreferenceSettings, `"version":3`, `"version":4,"cash_sweep":{"currency_priority":"balanced","execution_enabled":true}`, 1),
	} {
		if _, err := decodePlatformSettings([]byte(raw)); err == nil {
			t.Fatal("accepted invalid old/new document", raw)
		}
	}
	out, err := s.handleCashSweepPreferences()
	if err != nil || out.CurrencyPriority != nil || out.EffectivePriority != "usd_first" || out.Source != "default" || !out.Writable {
		t.Fatalf("default %+v %v", out, err)
	}
}

func TestCashSweepPriorityCASImmutableReceiptNoOpAndReplayCurrent(t *testing.T) {
	s, core := cashPreferenceServer(t)
	original := s.platformSettings.snapshot()
	balanced, eur := "balanced", "eur_first"
	first, err := saveCashPreference(t, s, &balanced, 1, "first")
	if err != nil || first.Revision != 2 || first.SavedRevision != 2 || first.RequestID != "first" {
		t.Fatalf("first %+v %v", first, err)
	}
	*first.CurrencyPriority = "eur_first"
	if got, _ := s.handleCashSweepPreferences(); *got.CurrencyPriority != "balanced" {
		t.Fatal("response aliased runtime settings")
	}
	next := s.platformSettings.snapshot()
	next.Version = original.Version
	next.CashSweep = original.CashSweep
	if !reflect.DeepEqual(next, original) {
		t.Fatal("priority changed other controls")
	}
	if _, err = saveCashPreference(t, s, &eur, 1, "first"); err == nil {
		t.Fatal("same request changed terms")
	}
	if _, err = saveCashPreference(t, s, &eur, 1, "stale"); err == nil || !strings.Contains(err.Error(), rpc.CodeSettingsConflict) {
		t.Fatalf("stale accepted: %v", err)
	}
	second, err := saveCashPreference(t, s, &eur, 2, "second")
	if err != nil || second.Revision != 3 {
		t.Fatalf("second %+v %v", second, err)
	}
	head, _ := core.AuthorityHead(t.Context())
	replay, err := saveCashPreference(t, s, &balanced, 1, "first")
	if err != nil || !replay.Replay || replay.Revision != 3 || replay.SavedRevision != 2 || *replay.CurrencyPriority != eur {
		t.Fatalf("old retry reapplied: %+v %v", replay, err)
	}
	after, _ := core.AuthorityHead(t.Context())
	if head != after {
		t.Fatal("replay appended audit")
	}
	noOp, err := saveCashPreference(t, s, &eur, 3, "noop")
	if err != nil || noOp.Revision != 3 || noOp.SavedRevision != 3 {
		t.Fatalf("noop %+v %v", noOp, err)
	}
	events, err := core.LoadEvents(t.Context(), corestore.EventQuery{ScopeKey: daemonStateScope, Type: cashPriorityReceiptType})
	if err != nil || len(events) != 3 {
		t.Fatalf("receipt audit count %d %v", len(events), err)
	}
	for _, event := range events {
		if event.Origin != rpc.OrderOriginAgent {
			t.Fatal("invented human origin", event.Origin)
		}
	}
	cleared, err := saveCashPreference(t, s, nil, 3, "clear")
	if err != nil || cleared.Revision != 4 || cleared.CurrencyPriority != nil || cleared.Source != "default" {
		t.Fatalf("clear %+v %v", cleared, err)
	}
}

func TestCashSweepPriorityConcurrentCASAndNarrowValidation(t *testing.T) {
	s, _ := cashPreferenceServer(t)
	for _, raw := range []string{`{}`, `{"currency_priority":"balanced","expected_revision":1}`, `{"currency_priority":"all","expected_revision":1,"request_id":"bad"}`, `{"currency_priority":"balanced","expected_revision":null,"request_id":"bad"}`, `{"currency_priority":"balanced","expected_revision":1,"request_id":"bad","origin":"human_tty"}`, `{"currency_priority":"balanced","expected_revision":1,"request_id":"bad","trading":{"freeze":false}}`} {
		if _, err := s.handleCashSweepPrioritySet(t.Context(), &rpc.Request{Params: []byte(raw)}); err == nil {
			t.Fatal("accepted non-narrow request", raw)
		}
	}
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for i, value := range []string{"balanced", "eur_first"} {
		wg.Go(func() { _, err := saveCashPreference(t, s, &value, 1, []string{"race-a", "race-b"}[i]); results <- err })
	}
	wg.Wait()
	close(results)
	success := 0
	for err := range results {
		if err == nil {
			success++
		} else if !strings.Contains(err.Error(), rpc.CodeSettingsConflict) {
			t.Fatal(err)
		}
	}
	if success != 1 {
		t.Fatal("CAS winners", success)
	}
}

func TestRuntimeCashPriorityOnlyReordersWithoutReserveOptIn(t *testing.T) {
	policy := cashSweepTestPolicy(rpc.CashSweepModeShadow, 1e9)
	if policy.Buckets.CashSweep.reserveDesignEnabled() {
		t.Fatal("fixture already opted in")
	}
	in := cashSweepTestInput(map[string]float64{"USD": 60000, "EUR": 40000})
	baseline := cashSweepPlanFor(policy, in, cashSweepTestNow())
	for _, priority := range []string{"usd_first", "balanced", "eur_first"} {
		in.OrderingPriority = &priority
		plan := cashSweepPlanFor(policy, in, cashSweepTestNow())
		if policy.Buckets.CashSweep.reserveDesignEnabled() || policy.Buckets.CashSweep.CurrencyPriority != "" || plan.status.ReserveState != "" || plan.status.ReserveCushionEUR != nil {
			t.Fatal("ordering opted into reserve design", priority)
		}
		if plan.status.CurrencyPriority != priority || plan.status.CurrencyPrioritySource != "runtime" {
			t.Fatal("source missing")
		}
		for _, original := range baseline.currencies {
			current := cashSweepCurrencyOf(t, plan, original.status.Currency)
			current.status.PriorityRank = original.status.PriorityRank
			if !reflect.DeepEqual(current, original) {
				t.Fatalf("ordering changed financial plan for %s", priority)
			}
		}
	}
	priority := "eur_first"
	in.OrderingPriority = &priority
	if got := cashSweepPlanFor(policy, in, cashSweepTestNow()).currencies[0].status.Currency; got != "EUR" {
		t.Fatal("ordering override unused", got)
	}
	policy.Buckets.CashSweep.CurrencyPriority = "usd_first"
	plan := cashSweepPlanFor(policy, in, cashSweepTestNow())
	if plan.status.ReserveState != "unavailable" || !strings.HasPrefix(plan.status.ReserveReason, "reserve_calibration_required:") {
		t.Fatal("override bypassed opted-in reserve hold")
	}
}

func TestCashSweepPriorityDispatchPreservesConflict(t *testing.T) {
	s, core := cashPreferenceServer(t)
	dispatch := func(method string, params string) rpc.Response {
		t.Helper()
		var out bytes.Buffer
		s.dispatch(t.Context(), &rpc.Request{ID: "wire-priority", Method: method, Params: json.RawMessage(params)}, json.NewEncoder(&out), bufio.NewReader(strings.NewReader("")))
		var response rpc.Response
		if err := json.Unmarshal(out.Bytes(), &response); err != nil {
			t.Fatalf("dispatch response: %s: %v", out.Bytes(), err)
		}
		return response
	}
	if got := dispatch(rpc.MethodCashSweepPreferencesGet, `{}`); !got.Ok {
		t.Fatalf("getter not dispatched: %+v", got)
	}
	first := dispatch(rpc.MethodCashSweepPrioritySet, `{"currency_priority":"balanced","expected_revision":1,"request_id":"wire-first"}`)
	if !first.Ok {
		t.Fatalf("setter not dispatched: %+v", first)
	}
	head, _ := core.AuthorityHead(t.Context())
	stale := dispatch(rpc.MethodCashSweepPrioritySet, `{"currency_priority":"eur_first","expected_revision":1,"request_id":"wire-stale"}`)
	if stale.Ok || stale.Error == nil || stale.Error.Code != rpc.CodeSettingsConflict {
		t.Fatalf("CAS conflict lost at daemon wire: %+v", stale)
	}
	after, _ := core.AuthorityHead(t.Context())
	if after != head {
		t.Fatal("failed dispatch appended audit")
	}
	invalid := dispatch(rpc.MethodCashSweepPrioritySet, `{"currency_priority":"balanced","expected_revision":2,"request_id":"wire-invalid","origin":"human_tty"}`)
	if invalid.Ok || invalid.Error == nil || invalid.Error.Code != rpc.CodeBadRequest {
		t.Fatalf("narrow payload boundary lost at daemon wire: %+v", invalid)
	}
}

func TestCashSweepPreferencesAbsentStateReceiptAndCancelledRead(t *testing.T) {
	core, err := corestore.Open(t.Context(), corestore.Options{Path: filepath.Join(privateTestDir(t), "daemon.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer core.Close()
	// Synthetic pre-document shape for the revision-zero receipt contract.
	// Production bindCore separately requires completed cutover bootstrap.
	settings := &platformSettingsStore{core: core, data: platformSettingsData{Version: platformSettingsDocVersion}}
	s := &Server{platformSettings: settings}
	initial, err := s.handleCashSweepPreferences()
	if err != nil || initial.Revision != 0 || !initial.Writable || initial.EffectivePriority != "usd_first" {
		t.Fatalf("fresh accepted default: %+v %v", initial, err)
	}
	cleared, err := saveCashPreference(t, s, nil, 0, "initial-clear")
	if err != nil || cleared.Revision != 0 || cleared.SavedRevision != 0 || cleared.CurrencyPriority != nil {
		t.Fatalf("initial no-op clear: %+v %v", cleared, err)
	}
	if _, found, err := core.GetStateDocument(t.Context(), daemonStateScope, stateKindPlatformSettings); err != nil || found {
		t.Fatal("initial no-op clear eagerly wrote settings", err)
	}
	head, _ := core.AuthorityHead(t.Context())
	replayed, err := saveCashPreference(t, s, nil, 0, "initial-clear")
	if err != nil || !replayed.Replay || replayed.Revision != 0 {
		t.Fatalf("initial clear replay: %+v %v", replayed, err)
	}
	after, _ := core.AuthorityHead(t.Context())
	if after != head {
		t.Fatal("initial clear replay advanced audit")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	unconfirmed, err := s.handleCashSweepPreferencesContext(ctx)
	if err != nil || unconfirmed.Writable || unconfirmed.Revision != 0 || !strings.Contains(unconfirmed.Reason, "previous accepted") {
		t.Fatalf("cancelled read advertised writable: %+v %v", unconfirmed, err)
	}
}
