package daemon

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/broker"
	"github.com/osauer/canary/v2/internal/daemon/corestore"
	"github.com/osauer/canary/v2/internal/marketcal"
	"github.com/osauer/canary/v2/internal/rpc"
)

func accountIntradayTestSession(t *testing.T, date string) marketcal.Session {
	t.Helper()
	day, err := time.Parse(time.DateOnly, date)
	if err != nil {
		t.Fatal(err)
	}
	sess, err := marketcal.New().SessionAt(marketcal.MarketUSEquity, day.Add(17*time.Hour))
	if err != nil || sess.State != marketcal.StateRegular || sess.Date != date {
		t.Fatalf("session %s = %+v, %v", date, sess, err)
	}
	return sess
}

func accountIntradayTestStore(t *testing.T, path string) *corestore.Store {
	t.Helper()
	store, err := corestore.Open(t.Context(), corestore.Options{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	return store
}

func TestAccountIntradaySampleAdmitsOnlyAProvenRequestReadInsideTheSession(t *testing.T) {
	sess := accountIntradayTestSession(t, "2026-10-23")
	scope := brokerStateScope{Account: "U1234567", Mode: rpc.AccountModeLive}
	value := 200000.0
	good := accountSnapshot{
		raw:        &broker.AccountSummary{NetLiquidation: &value, BaseCurrency: "eur", BaseCurrencyProvenance: broker.BaseCurrencyExplicitTag},
		provenance: broker.ProvenanceRequest,
		observedAt: sess.Open.Add(2 * time.Minute),
		source:     accountSnapshotSource{scope: scope},
	}
	point, currency, ok := accountIntradaySample(sess, scope, good)
	if !ok || currency != "EUR" || point.Value != value || !point.At.Equal(good.observedAt) {
		t.Fatalf("admissible read refused: %+v %q %v", point, currency, ok)
	}
	zero, nan := 0.0, 0.0
	nan = nan / nan
	for name, mutate := range map[string]func(*accountSnapshot){
		"cached fallback":   func(s *accountSnapshot) { s.provenance = broker.ProvenanceCachedFallback },
		"no value":          func(s *accountSnapshot) { s.raw.NetLiquidation = nil },
		"zero value":        func(s *accountSnapshot) { s.raw.NetLiquidation = &zero },
		"not a number":      func(s *accountSnapshot) { s.raw.NetLiquidation = &nan },
		"unproven currency": func(s *accountSnapshot) { s.raw.BaseCurrencyProvenance = broker.BaseCurrencyUnknown },
		"other scope": func(s *accountSnapshot) {
			s.source.scope = brokerStateScope{Account: "DU7654321", Mode: rpc.AccountModePaper}
		},
		"before the open":      func(s *accountSnapshot) { s.observedAt = sess.Open.Add(-time.Second) },
		"at the close":         func(s *accountSnapshot) { s.observedAt = sess.Close },
		"no observation clock": func(s *accountSnapshot) { s.observedAt = time.Time{} },
	} {
		snap := good
		raw := *good.raw
		snap.raw = &raw
		mutate(&snap)
		if _, _, ok := accountIntradaySample(sess, scope, snap); ok {
			t.Errorf("%s: read admitted as a sample", name)
		}
	}
	weekend := sess
	weekend.State = marketcal.StateClosed
	if _, _, ok := accountIntradaySample(weekend, scope, good); ok {
		t.Error("a closed day admitted a sample")
	}
}

func TestAccountIntradayAuthorityKeepsOnePointPerBucketTwoSessionsAndSurvivesRestart(t *testing.T) {
	path := filepath.Join(privateTestDir(t), "daemon.db")
	store := accountIntradayTestStore(t, path)
	first := accountIntradayAuthority{}
	if err := first.bindCore(t.Context(), store); err != nil {
		t.Fatal(err)
	}
	key := dailyPnLObservationSourceKey(dailyPnLScopeSource(brokerStateScope{Account: "U1234567", Mode: rpc.AccountModeLive}))
	thursday, friday := accountIntradayTestSession(t, "2026-10-22"), accountIntradayTestSession(t, "2026-10-23")
	add := func(sess marketcal.Session, offset time.Duration, value float64, currency string) bool {
		t.Helper()
		ok, err := first.record(t.Context(), key, sess, currency, rpc.AccountIntradayPoint{At: sess.Open.Add(offset), Value: value}, sess.Open.Add(offset))
		if err != nil {
			t.Fatal(err)
		}
		return ok
	}
	if !add(thursday, 10*time.Second, 100, "EUR") || !add(thursday, 6*time.Minute, 101, "EUR") {
		t.Fatal("first Thursday points refused")
	}
	if add(thursday, 7*time.Minute, 102, "EUR") {
		t.Fatal("a second point in a held bucket was admitted")
	}
	if !first.hasBucket(key, thursday, 1) || first.hasBucket(key, thursday, 2) {
		t.Fatal("bucket bookkeeping is wrong")
	}
	if add(thursday, 20*time.Minute, 103, "USD") {
		t.Fatal("a point in another currency joined the session")
	}
	if !add(friday, 40*time.Minute, 110, "EUR") {
		t.Fatal("the next session's first point was refused")
	}
	if add(thursday, 30*time.Minute, 104, "EUR") {
		t.Fatal("an older session reopened behind a newer one")
	}
	monday := accountIntradayTestSession(t, "2026-10-26")
	if !add(monday, 0, 120, "EUR") {
		t.Fatal("Monday's opening point was refused")
	}
	doc, ok, err := store.GetStateDocument(t.Context(), daemonStateScope, accountIntradayStateKind)
	if err != nil || !ok || bytes.Contains(doc.JSON, []byte("U1234567")) {
		t.Fatalf("persisted document missing or carries the account identity: ok=%v err=%v", ok, err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	reopened := accountIntradayTestStore(t, path)
	t.Cleanup(func() { _ = reopened.Close() })
	restarted := accountIntradayAuthority{}
	if err := restarted.bindCore(t.Context(), reopened); err != nil {
		t.Fatal(err)
	}
	got := accountIntradayProjection(restarted.sessionsFor(key))
	if len(got) != 2 || got[0].Date != "2026-10-26" || got[1].Date != "2026-10-23" {
		t.Fatalf("retained sessions = %+v, want Monday then Friday", got)
	}
	if got[0].OpeningValue == nil || *got[0].OpeningValue != 120 || got[0].IntervalSeconds != 300 {
		t.Fatalf("Monday opening = %+v", got[0])
	}
	if got[1].OpeningValue != nil || len(got[1].Points) != 1 {
		t.Fatalf("a late first point stood in for Friday's opening: %+v", got[1])
	}
}

func TestAccountIntradayAuthorityRejectsMalformedState(t *testing.T) {
	for name, doc := range map[string]accountIntradayDocument{
		"version":  {Version: 2},
		"scope":    {Version: 1, Scopes: map[string]accountIntradayScope{"U1234567": {}}},
		"currency": {Version: 1, Scopes: map[string]accountIntradayScope{dailyPnLObservationSourceKey("x"): {Sessions: []accountIntradaySession{{Date: "2026-10-23", Open: time.Unix(1, 0), Close: time.Unix(2, 0)}}}}},
		"value": {Version: 1, Scopes: map[string]accountIntradayScope{dailyPnLObservationSourceKey("x"): {Sessions: []accountIntradaySession{{Date: "2026-10-23", Open: time.Unix(1, 0), Close: time.Unix(2, 0), BaseCurrency: "EUR",
			Points: []rpc.AccountIntradayPoint{{At: time.Unix(1, 0), Value: 0}}}}}}},
	} {
		if err := validateAccountIntradayDocument(doc); err == nil {
			t.Errorf("%s: malformed state accepted", name)
		}
	}
}

func TestAccountIntradayHandlerStates(t *testing.T) {
	s := newTestServer(t)
	friday := accountIntradayTestSession(t, "2026-10-23")
	now := friday.Open.Add(time.Hour)
	s.now = func() time.Time { return now }
	req := &rpc.Request{Method: rpc.MethodAccountIntraday}

	res, err := s.handleAccountIntraday(t.Context(), req)
	if err != nil || res.Status != rpc.AccountIntradayUnavailable || res.Reason == "" || len(res.Sessions) != 0 {
		t.Fatalf("unresolved scope = %+v, %v", res, err)
	}
	s.cfg.Gateway.Account = "U1234567"
	if res, _ = s.handleAccountIntraday(t.Context(), req); res.Status != rpc.AccountIntradayUnavailable {
		t.Fatalf("unbound store read as available: %+v", res)
	}
	store := accountIntradayTestStore(t, filepath.Join(privateTestDir(t), "daemon.db"))
	t.Cleanup(func() { _ = store.Close() })
	if err := s.accountIntraday.bindCore(t.Context(), store); err != nil {
		t.Fatal(err)
	}
	res, err = s.handleAccountIntraday(t.Context(), req)
	if err != nil || res.Status != rpc.AccountIntradayAvailable || res.Sessions == nil || len(res.Sessions) != 0 {
		t.Fatalf("nothing recorded yet = %+v, %v", res, err)
	}
	if res.MarketSession == nil || !res.MarketSession.IsOpen || res.MarketSession.Date != "2026-10-23" {
		t.Fatalf("market session = %+v", res.MarketSession)
	}
	key := dailyPnLObservationSourceKey(dailyPnLScopeSource(s.currentBrokerStateScope()))
	if _, err := s.accountIntraday.record(t.Context(), key, friday, "EUR", rpc.AccountIntradayPoint{At: friday.Open.Add(time.Minute), Value: 100}, now); err != nil {
		t.Fatal(err)
	}
	res, _ = s.handleAccountIntraday(t.Context(), req)
	raw, _ := json.Marshal(res)
	if len(res.Sessions) != 1 || len(res.Sessions[0].Points) != 1 || bytes.Contains(raw, []byte("U1234567")) {
		t.Fatalf("recorded session = %s", raw)
	}
	if _, err := s.handleAccountIntraday(t.Context(), &rpc.Request{Method: rpc.MethodAccountIntraday, Params: json.RawMessage(`{"account":"U1"}`)}); err == nil {
		t.Fatal("caller-supplied scope accepted")
	}
}
