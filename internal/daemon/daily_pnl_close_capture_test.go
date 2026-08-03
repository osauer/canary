package daemon

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/config"
	"github.com/osauer/canary/v2/internal/daemon/corestore"
	"github.com/osauer/canary/v2/internal/marketcal"
	ibkrlib "github.com/osauer/canary/v2/pkg/ibkr"
)

// usEquitySessionAt resolves the official session so eligibility tests run
// against the real embedded calendar rather than a fixture the calendar could
// drift away from.
func usEquitySessionAt(t *testing.T, at time.Time) marketcal.Session {
	t.Helper()
	sess, err := marketcal.New().SessionAt(marketcal.MarketUSEquity, at)
	if err != nil {
		t.Fatalf("SessionAt(%s): %v", at, err)
	}
	return sess
}

func TestDailyPnLCloseCaptureEligibilityWindow(t *testing.T) {
	// Friday 2026-07-31 is a regular session: close 16:00 ET = 20:00 UTC (EDT).
	regularClose := time.Date(2026, 7, 31, 20, 0, 0, 0, time.UTC)
	// Friday 2026-11-27 (day after Thanksgiving) is an early close:
	// 13:00 ET = 18:00 UTC (EST).
	earlyClose := time.Date(2026, 11, 27, 18, 0, 0, 0, time.UTC)
	if sess := usEquitySessionAt(t, earlyClose); sess.State != marketcal.StateEarlyClose {
		t.Fatalf("2026-11-27 state = %s, want early_close (calendar fixture drifted)", sess.State)
	}

	tests := []struct {
		name string
		at   time.Time
		want bool
	}{
		{name: "one second before the close", at: regularClose.Add(-time.Second), want: false},
		{name: "at the close", at: regularClose, want: true},
		{name: "just inside the window", at: regularClose.Add(dailyPnLCloseCaptureWindow - time.Second), want: true},
		{name: "window expired", at: regularClose.Add(dailyPnLCloseCaptureWindow), want: false},
		{name: "early close inside window", at: earlyClose.Add(5 * time.Minute), want: true},
		{name: "early close at the regular hour", at: time.Date(2026, 11, 27, 21, 2, 0, 0, time.UTC), want: false},
		{name: "holiday", at: time.Date(2026, 9, 7, 20, 2, 0, 0, time.UTC), want: false},
		{name: "weekend", at: time.Date(2026, 8, 1, 20, 2, 0, 0, time.UTC), want: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			sess := usEquitySessionAt(t, tc.at)
			if got := dailyPnLCloseCaptureEligible(sess, tc.at); got != tc.want {
				t.Fatalf("eligible(%s: state=%s close=%s) = %v, want %v", tc.at, sess.State, sess.Close, got, tc.want)
			}
		})
	}
}

func TestDailyPnLCloseFrameUsable(t *testing.T) {
	sessionClose := time.Date(2026, 7, 31, 20, 0, 0, 0, time.UTC)
	now := sessionClose.Add(20 * time.Second)
	value := 123.45
	nan := ibkrlib.AccountDailyPnL{DailyPnL: &value, DailyPnLStatus: ibkrlib.DailyPnLFrameMalformed, AsOf: now}

	tests := []struct {
		name     string
		snap     ibkrlib.AccountDailyPnL
		hasFrame bool
		want     bool
	}{
		{name: "post-close frame", snap: ibkrlib.AccountDailyPnL{DailyPnL: &value, DailyPnLStatus: ibkrlib.DailyPnLFrameAvailable, AsOf: sessionClose.Add(5 * time.Second)}, hasFrame: true, want: true},
		{name: "at-close frame inside the stale grace", snap: ibkrlib.AccountDailyPnL{DailyPnL: &value, AsOf: sessionClose.Add(-time.Minute)}, hasFrame: true, want: true},
		{name: "frame predating the close beyond the grace", snap: ibkrlib.AccountDailyPnL{DailyPnL: &value, AsOf: sessionClose.Add(-2 * time.Minute)}, hasFrame: true, want: false},
		{name: "no frame", snap: ibkrlib.AccountDailyPnL{}, hasFrame: false, want: false},
		{name: "nil value", snap: ibkrlib.AccountDailyPnL{DailyPnLStatus: ibkrlib.DailyPnLFrameUnavailable, AsOf: now}, hasFrame: true, want: false},
		{name: "malformed status", snap: nan, hasFrame: true, want: false},
		{name: "zero as-of", snap: ibkrlib.AccountDailyPnL{DailyPnL: &value}, hasFrame: true, want: false},
		{name: "future frame", snap: ibkrlib.AccountDailyPnL{DailyPnL: &value, AsOf: now.Add(time.Minute)}, hasFrame: true, want: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := dailyPnLCloseFrameUsable(tc.snap, tc.hasFrame, sessionClose, now); got != tc.want {
				t.Fatalf("usable = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestDailyPnLCloseCaptureAuthorityPersistsAcrossRestart(t *testing.T) {
	databasePath := filepath.Join(privateTestDir(t), "daemon.db")
	store, err := corestore.Open(t.Context(), corestore.Options{Path: databasePath})
	if err != nil {
		t.Fatal(err)
	}
	first := dailyPnLCloseCaptureAuthority{}
	if err := first.bindCore(t.Context(), store); err != nil {
		t.Fatal(err)
	}
	source := "paper|DU123"
	capture := persistedDailyPnLCloseCapture{
		SessionKey: "2026-07-31", DailyPnL: -433.7, BaseCurrency: "EUR",
		SessionClose: time.Date(2026, 7, 31, 20, 0, 0, 0, time.UTC),
		CapturedAt:   time.Date(2026, 7, 31, 20, 0, 9, 0, time.UTC),
	}
	if err := first.capture(t.Context(), source, capture); err != nil {
		t.Fatal(err)
	}
	doc, ok, err := store.GetStateDocument(t.Context(), daemonStateScope, dailyPnLCloseCaptureStateKind)
	if err != nil || !ok {
		t.Fatalf("persisted capture document missing: ok=%v err=%v", ok, err)
	}
	if bytes.Contains(doc.JSON, []byte("DU123")) {
		t.Fatalf("persisted capture exposed account identity: %s", doc.JSON)
	}

	// Re-capturing the same session keeps the first frame; a later frame is a
	// more drifted one.
	drifted := capture
	drifted.DailyPnL = -500
	if err := first.capture(t.Context(), source, drifted); err != nil {
		t.Fatal(err)
	}
	if got, _ := first.captureFor(source); got.DailyPnL != capture.DailyPnL {
		t.Fatalf("same-session recapture overwrote the close print: %+v", got)
	}

	// A second scope holds its own capture without touching the first.
	liveCapture := capture
	liveCapture.DailyPnL = 12
	if err := first.capture(t.Context(), "live|U999", liveCapture); err != nil {
		t.Fatal(err)
	}

	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	restartedStore, err := corestore.Open(t.Context(), corestore.Options{Path: databasePath})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = restartedStore.Close() })
	restarted := dailyPnLCloseCaptureAuthority{}
	if err := restarted.bindCore(t.Context(), restartedStore); err != nil {
		t.Fatal(err)
	}
	got, ok := restarted.captureFor(source)
	if !ok || got != capture {
		t.Fatalf("restarted capture = %+v ok=%v, want %+v", got, ok, capture)
	}
	if live, ok := restarted.captureFor("live|U999"); !ok || live.DailyPnL != 12 {
		t.Fatalf("second scope capture = %+v ok=%v", live, ok)
	}

	// A newer session replaces the retained one for its scope.
	next := capture
	next.SessionKey = "2026-08-03"
	next.DailyPnL = 88.25
	if err := restarted.capture(t.Context(), source, next); err != nil {
		t.Fatal(err)
	}
	if got, _ := restarted.captureFor(source); got.SessionKey != "2026-08-03" || got.DailyPnL != 88.25 {
		t.Fatalf("newer session did not replace retained capture: %+v", got)
	}
}

func TestDailyPnLCloseCaptureDocumentValidation(t *testing.T) {
	goodKey := dailyPnLObservationSourceKey("paper|DU123")
	good := persistedDailyPnLCloseCapture{
		SessionKey: "2026-07-31", DailyPnL: 1, BaseCurrency: "EUR",
		SessionClose: time.Date(2026, 7, 31, 20, 0, 0, 0, time.UTC),
		CapturedAt:   time.Date(2026, 7, 31, 20, 0, 9, 0, time.UTC),
	}
	broken := func(mutate func(*persistedDailyPnLCloseCapture) string) (dailyPnLCloseCaptureDocument, string) {
		capture := good
		key := mutate(&capture)
		if key == "" {
			key = goodKey
		}
		return dailyPnLCloseCaptureDocument{Version: 1, Captures: map[string]persistedDailyPnLCloseCapture{key: capture}}, key
	}

	if err := validateDailyPnLCloseCaptureDocument(dailyPnLCloseCaptureDocument{Version: 1}); err != nil {
		t.Fatalf("empty document must validate: %v", err)
	}
	if doc, _ := broken(func(*persistedDailyPnLCloseCapture) string { return "" }); validateDailyPnLCloseCaptureDocument(doc) != nil {
		t.Fatalf("good capture must validate")
	}
	if err := validateDailyPnLCloseCaptureDocument(dailyPnLCloseCaptureDocument{Version: 2}); err == nil {
		t.Fatal("unsupported version must refuse")
	}
	cases := []struct {
		name   string
		mutate func(*persistedDailyPnLCloseCapture) string
	}{
		{name: "invalid scope key", mutate: func(*persistedDailyPnLCloseCapture) string { return "not-hex" }},
		{name: "invalid session key", mutate: func(c *persistedDailyPnLCloseCapture) string { c.SessionKey = "July 31"; return "" }},
		{name: "missing currency", mutate: func(c *persistedDailyPnLCloseCapture) string { c.BaseCurrency = " "; return "" }},
		{name: "zero session close", mutate: func(c *persistedDailyPnLCloseCapture) string { c.SessionClose = time.Time{}; return "" }},
		{name: "zero capture time", mutate: func(c *persistedDailyPnLCloseCapture) string { c.CapturedAt = time.Time{}; return "" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			doc, _ := broken(tc.mutate)
			if err := validateDailyPnLCloseCaptureDocument(doc); err == nil {
				t.Fatal("invalid document must refuse to load")
			}
		})
	}
}

// fakeDailyPnLCloseSource is the connector slice the capture reads.
type fakeDailyPnLCloseSource struct {
	snap     ibkrlib.AccountDailyPnL
	hasFrame bool
	summary  *ibkrlib.RawAccountSummary
}

func (f *fakeDailyPnLCloseSource) AccountDailyPnL() (ibkrlib.AccountDailyPnL, bool) {
	return f.snap, f.hasFrame
}
func (f *fakeDailyPnLCloseSource) CachedAccountSummary() *ibkrlib.RawAccountSummary {
	return f.summary
}

func newCloseCaptureTestServer(account string) *Server {
	port := 4002
	return &Server{
		cfg:    &config.Resolved{Gateway: config.Gateway{Host: "127.0.0.1", Port: &port, Account: account}},
		logger: NewLogger(&bytes.Buffer{}, "error"),
	}
}

// provenBaseSummary is the shape parseAccountSummary produces for an account
// whose base currency an eligible broker field established: the typed base and
// its provenance, beside the legacy numeric-row fallback that agrees with them.
func provenBaseSummary(ccy string) *ibkrlib.RawAccountSummary {
	return &ibkrlib.RawAccountSummary{
		Currency:               ccy,
		BaseCurrency:           ccy,
		BaseCurrencyProvenance: ibkrlib.AccountBaseCurrencyValueSuffix,
	}
}

func TestMaybeCaptureDailyPnLCloseCapturesOncePerScopeAndSession(t *testing.T) {
	closeUTC := time.Date(2026, 7, 31, 20, 0, 0, 0, time.UTC)
	now := closeUTC.Add(12 * time.Second)
	sess := usEquitySessionAt(t, now)
	value := -433.7
	source := &fakeDailyPnLCloseSource{
		snap:     ibkrlib.AccountDailyPnL{DailyPnL: &value, DailyPnLStatus: ibkrlib.DailyPnLFrameAvailable, AsOf: closeUTC.Add(3 * time.Second)},
		hasFrame: true,
		summary:  provenBaseSummary("EUR"),
	}

	s := newCloseCaptureTestServer("DU123")
	s.maybeCaptureDailyPnLClose(t.Context(), source, sess, now)
	capture, ok := s.dailyPnLCloseCaptures.captureFor("paper|DU123")
	if !ok || capture.SessionKey != "2026-07-31" || capture.DailyPnL != value || capture.BaseCurrency != "EUR" {
		t.Fatalf("capture = %+v ok=%v", capture, ok)
	}
	if !capture.CapturedAt.Equal(closeUTC.Add(3*time.Second)) || !capture.SessionClose.Equal(closeUTC) {
		t.Fatalf("capture instants = %+v", capture)
	}

	// The same session never recaptures, even from a fresher frame.
	drifted := -900.0
	source.snap = ibkrlib.AccountDailyPnL{DailyPnL: &drifted, DailyPnLStatus: ibkrlib.DailyPnLFrameAvailable, AsOf: now}
	s.maybeCaptureDailyPnLClose(t.Context(), source, sess, now.Add(15*time.Second))
	if capture, _ = s.dailyPnLCloseCaptures.captureFor("paper|DU123"); capture.DailyPnL != value {
		t.Fatalf("recapture overwrote the close print: %+v", capture)
	}

	// A scope switch inside the window captures separately for the new scope
	// (the paper port keeps the mode; the account changes the fingerprint).
	s.cfg.Gateway.Account = "DU777"
	s.maybeCaptureDailyPnLClose(t.Context(), source, sess, now.Add(20*time.Second))
	if switched, ok := s.dailyPnLCloseCaptures.captureFor("paper|DU777"); !ok || switched.DailyPnL != drifted {
		t.Fatalf("scope-switch capture = %+v ok=%v", switched, ok)
	}
	if capture, _ = s.dailyPnLCloseCaptures.captureFor("paper|DU123"); capture.DailyPnL != value {
		t.Fatalf("scope switch disturbed the first scope's capture: %+v", capture)
	}
}

func TestMaybeCaptureDailyPnLCloseFailsTowardAbsence(t *testing.T) {
	closeUTC := time.Date(2026, 7, 31, 20, 0, 0, 0, time.UTC)
	now := closeUTC.Add(12 * time.Second)
	sess := usEquitySessionAt(t, now)
	value := 55.5
	goodFrame := ibkrlib.AccountDailyPnL{DailyPnL: &value, DailyPnLStatus: ibkrlib.DailyPnLFrameAvailable, AsOf: closeUTC.Add(2 * time.Second)}

	tests := []struct {
		name    string
		account string
		at      time.Time
		session marketcal.Session
		source  *fakeDailyPnLCloseSource
	}{
		{name: "open session", account: "DU123", at: closeUTC.Add(-2 * time.Hour),
			session: usEquitySessionAt(t, closeUTC.Add(-2*time.Hour)),
			source:  &fakeDailyPnLCloseSource{snap: goodFrame, hasFrame: true, summary: provenBaseSummary("EUR")}},
		{name: "window expired", account: "DU123", at: closeUTC.Add(dailyPnLCloseCaptureWindow),
			session: sess,
			source:  &fakeDailyPnLCloseSource{snap: goodFrame, hasFrame: true, summary: provenBaseSummary("EUR")}},
		{name: "non-concrete scope", account: "All", at: now, session: sess,
			source: &fakeDailyPnLCloseSource{snap: goodFrame, hasFrame: true, summary: provenBaseSummary("EUR")}},
		{name: "stale frame", account: "DU123", at: now, session: sess,
			source: &fakeDailyPnLCloseSource{snap: ibkrlib.AccountDailyPnL{DailyPnL: &value, AsOf: closeUTC.Add(-3 * time.Minute)}, hasFrame: true, summary: provenBaseSummary("EUR")}},
		{name: "no summary", account: "DU123", at: now, session: sess,
			source: &fakeDailyPnLCloseSource{snap: goodFrame, hasFrame: true}},
		// The legacy numeric-row currency is a deterministic fallback, not proof
		// of the account's base unit, and a unit exchange rate never was proof
		// either. Neither may be stored as the capture's denomination.
		{name: "legacy currency without provenance", account: "DU123", at: now, session: sess,
			source: &fakeDailyPnLCloseSource{snap: goodFrame, hasFrame: true,
				summary: &ibkrlib.RawAccountSummary{Currency: "CHF"}}},
		{name: "unit exchange rate provenance", account: "DU123", at: now, session: sess,
			source: &fakeDailyPnLCloseSource{snap: goodFrame, hasFrame: true, summary: &ibkrlib.RawAccountSummary{
				Currency: "CHF", BaseCurrency: "CHF", BaseCurrencyProvenance: ibkrlib.AccountBaseCurrencyUnitExchangeRate}}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := newCloseCaptureTestServer(tc.account)
			s.maybeCaptureDailyPnLClose(t.Context(), tc.source, tc.session, tc.at)
			s.dailyPnLCloseCaptures.mu.Lock()
			defer s.dailyPnLCloseCaptures.mu.Unlock()
			if len(s.dailyPnLCloseCaptures.captures) != 0 {
				t.Fatalf("capture recorded: %+v", s.dailyPnLCloseCaptures.captures)
			}
		})
	}
}

// A conflicting legacy fallback must never displace the proven base currency:
// the two fields diverge whenever one numeric row's suffix disagrees with the
// evidence that actually established the account's base unit.
func TestMaybeCaptureDailyPnLCloseStoresProvenBaseOverLegacyCurrency(t *testing.T) {
	closeUTC := time.Date(2026, 7, 31, 20, 0, 0, 0, time.UTC)
	now := closeUTC.Add(12 * time.Second)
	value := -12.5
	source := &fakeDailyPnLCloseSource{
		snap:     ibkrlib.AccountDailyPnL{DailyPnL: &value, DailyPnLStatus: ibkrlib.DailyPnLFrameAvailable, AsOf: closeUTC.Add(2 * time.Second)},
		hasFrame: true,
		summary: &ibkrlib.RawAccountSummary{
			Currency:               "CHF",
			BaseCurrency:           "EUR",
			BaseCurrencyProvenance: ibkrlib.AccountBaseCurrencyExplicitTag,
		},
	}

	s := newCloseCaptureTestServer("DU123")
	s.maybeCaptureDailyPnLClose(t.Context(), source, usEquitySessionAt(t, now), now)
	capture, ok := s.dailyPnLCloseCaptures.captureFor("paper|DU123")
	if !ok || capture.BaseCurrency != "EUR" {
		t.Fatalf("capture base currency = %q ok=%v, want EUR", capture.BaseCurrency, ok)
	}
}

func TestLastCompletedUSEquitySessionDate(t *testing.T) {
	tests := []struct {
		name string
		at   time.Time
		want string
	}{
		{name: "after the close it is today", at: time.Date(2026, 7, 31, 20, 30, 0, 0, time.UTC), want: "2026-07-31"},
		{name: "during the session it is the prior date", at: time.Date(2026, 7, 31, 18, 0, 0, 0, time.UTC), want: "2026-07-30"},
		{name: "pre-open it is the prior date", at: time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC), want: "2026-07-30"},
		{name: "weekend walks back to Friday", at: time.Date(2026, 8, 2, 15, 0, 0, 0, time.UTC), want: "2026-07-31"},
		{name: "holiday walks back across the weekend", at: time.Date(2026, 9, 7, 18, 0, 0, 0, time.UTC), want: "2026-09-04"},
		{name: "after an early close it is today", at: time.Date(2026, 11, 27, 18, 30, 0, 0, time.UTC), want: "2026-11-27"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := lastCompletedUSEquitySessionDate(tc.at)
			if !ok || got != tc.want {
				t.Fatalf("lastCompleted(%s) = %q ok=%v, want %q", tc.at, got, ok, tc.want)
			}
		})
	}
	if got, ok := lastCompletedUSEquitySessionDate(time.Date(2031, 1, 10, 18, 0, 0, 0, time.UTC)); ok {
		t.Fatalf("outside coverage must fail closed, got %q", got)
	}
}

func TestComposeBriefLastSessionServesOnlyMatchingCapture(t *testing.T) {
	saturday := time.Date(2026, 8, 1, 15, 0, 0, 0, time.UTC)
	capture := persistedDailyPnLCloseCapture{
		SessionKey: "2026-07-31", DailyPnL: -433.7, BaseCurrency: "EUR",
		SessionClose: time.Date(2026, 7, 31, 20, 0, 0, 0, time.UTC),
		CapturedAt:   time.Date(2026, 7, 31, 20, 0, 9, 0, time.UTC),
	}

	s := newCloseCaptureTestServer("DU123")
	if err := s.dailyPnLCloseCaptures.capture(t.Context(), "paper|DU123", capture); err != nil {
		t.Fatal(err)
	}
	row := s.composeBriefLastSession(saturday)
	if row.Status != "ok" || row.SessionDate != "2026-07-31" || row.DailyPnLBase == nil || *row.DailyPnLBase != capture.DailyPnL {
		t.Fatalf("served row = %+v", row)
	}
	if row.BaseCurrency != "EUR" || !row.CapturedAt.Equal(capture.CapturedAt) || !row.SessionClose.Equal(capture.SessionClose) {
		t.Fatalf("served row provenance = %+v", row)
	}

	// A capture for any other session must read as not captured.
	stale := newCloseCaptureTestServer("DU123")
	older := capture
	older.SessionKey = "2026-07-30"
	if err := stale.dailyPnLCloseCaptures.capture(t.Context(), "paper|DU123", older); err != nil {
		t.Fatal(err)
	}
	row = stale.composeBriefLastSession(saturday)
	if row.Status != "unavailable" || row.DailyPnLBase != nil || !strings.Contains(row.Detail, "not captured for 2026-07-31") {
		t.Fatalf("stale capture served: %+v", row)
	}

	// No capture at all reads the same way.
	row = newCloseCaptureTestServer("DU123").composeBriefLastSession(saturday)
	if row.Status != "unavailable" || row.SessionDate != "2026-07-31" || !strings.Contains(row.Detail, "not captured") {
		t.Fatalf("missing capture row = %+v", row)
	}

	// A non-concrete broker scope cannot bind a capture.
	row = newCloseCaptureTestServer("All").composeBriefLastSession(saturday)
	if row.Status != "unavailable" || !strings.Contains(row.Detail, "concrete account") {
		t.Fatalf("non-concrete scope row = %+v", row)
	}

	// A capture from the other scope never crosses.
	crossed := newCloseCaptureTestServer("DU123")
	if err := crossed.dailyPnLCloseCaptures.capture(t.Context(), "live|U999", capture); err != nil {
		t.Fatal(err)
	}
	row = crossed.composeBriefLastSession(saturday)
	if row.Status != "unavailable" || row.DailyPnLBase != nil {
		t.Fatalf("cross-scope capture served: %+v", row)
	}
}
