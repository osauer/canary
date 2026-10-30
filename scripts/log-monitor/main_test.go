package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// testNow is the clock finalizeForTest reports against.
var testNow = time.Date(2026, 10, 13, 6, 0, 0, 0, time.UTC)

// finalizeForTest classifies and finalizes one log with no recurrence
// history, no previous run and no declarations.
func finalizeForTest(scanned scannedLog, maxSignals int, daemon bool) logReport {
	f := &finalizer{cfg: defaultEngineConfig(), now: testNow, maxSignals: maxSignals,
		prior: openItemsFile{Items: map[string]openItem{}}, next: openItemsFile{Items: map[string]openItem{}}}
	if daemon {
		return f.finalize("daemon", classifyDaemon(scanned, f.cfg, nil, ""), history{})
	}
	return f.finalize("app", classifyApp(scanned), history{})
}

func daemonReport(maxSignals int, lines ...string) logReport {
	return finalizeForTest(scannedLog{state: "scanned", lines: lines}, maxSignals, true)
}

func appReport(maxSignals int, lines ...string) logReport {
	return finalizeForTest(scannedLog{state: "scanned", lines: lines}, maxSignals, false)
}

func TestRunClassifiesAndCommitsBoundedReports(t *testing.T) {
	dir := t.TempDir()
	daemonLog := filepath.Join(dir, "daemon.log")
	appLog := filepath.Join(dir, "app.log")
	dOffset := filepath.Join(dir, "daemon.offset")
	aOffset := filepath.Join(dir, "app.offset")
	writeTestFile(t, daemonLog, strings.Join([]string{
		`time=2026-08-08T06:00:00Z level=INFO msg="Connected to IB Gateway"`,
		`time=2026-08-08T06:00:10Z level=WARN msg="RateLimiter draining"`,
		`time=2026-08-08T06:02:00Z level=WARN msg="unexpected feed state for DU1234567; protocol misalignment for IWM via reqMktData symbol=IWM reqid=12"`,
		`time=2026-08-08T06:03:00Z level=ERROR msg="No security definition has been found" code=200`,
	}, "\n")+"\n")
	writeTestFile(t, appLog, strings.Join([]string{
		`time=2026-08-08T06:00:00Z level=INFO msg="Request completed" status=200`,
		`time=2026-08-08T06:00:01Z level=INFO msg="Request completed" status=503`,
		`2026/08/08 06:00:02 WARN legacy warning`,
		`2026/08/08 06:00:03 raw production line`,
	}, "\n")+"\n")

	got, err := run(options{
		daemonLog: daemonLog, appLog: appLog,
		daemonOffset: dOffset, appOffset: aOffset,
		maxSignals: 2, commit: true,
	}, time.Date(2026, 8, 8, 6, 5, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if !got.NeedsAttention || got.Version != 4 || len(got.Attention) == 0 {
		t.Fatal("report should require attention")
	}
	if got.Daemon.NewLines != 4 || got.Daemon.KnownBenign != 1 || len(got.Daemon.Signals) != 2 {
		t.Fatalf("daemon report = %+v", got.Daemon)
	}
	raw, _ := json.Marshal(got)
	if strings.Contains(string(raw), "DU1234567") || strings.Contains(string(raw), "IWM") {
		t.Fatalf("report leaked private identity: %s", raw)
	}
	if got.App.NewLines != 4 || got.App.KnownBenign != 1 || len(got.App.Signals) != 2 || got.App.SuppressedSignals != 1 {
		t.Fatalf("app report = %+v", got.App)
	}
	for _, path := range []string{dOffset, aOffset} {
		c, err := readCursor(path)
		if err != nil || c.Version != 2 || c.Lines != 4 {
			t.Fatalf("cursor: %+v %v", c, err)
		}
	}

	again, err := run(options{
		daemonLog: daemonLog, appLog: appLog,
		daemonOffset: dOffset, appOffset: aOffset,
		maxSignals: 2, commit: true,
	}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if again.NeedsAttention || again.Daemon.State != "unchanged" || again.App.State != "unchanged" {
		t.Fatalf("unchanged report = %+v", again)
	}
}

func TestRunResetsOffsetAfterRotation(t *testing.T) {
	dir := t.TempDir()
	daemonLog := filepath.Join(dir, "daemon.log")
	appLog := filepath.Join(dir, "app.log")
	dOffset := filepath.Join(dir, "daemon.offset")
	aOffset := filepath.Join(dir, "app.offset")
	writeTestFile(t, daemonLog, "time=2026-08-08T06:00:00Z level=INFO msg=ready\n")
	writeTestFile(t, appLog, "time=2026-08-08T06:00:00Z level=INFO msg=ready\n")
	writeTestFile(t, dOffset, "99\n")
	writeTestFile(t, aOffset, "99\n")

	got, err := run(options{
		daemonLog: daemonLog, appLog: appLog,
		daemonOffset: dOffset, appOffset: aOffset,
		maxSignals: defaultMaxSignals, commit: false,
	}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if !got.Daemon.OffsetReset || !got.App.OffsetReset || got.Daemon.NewLines != 1 || got.App.NewLines != 1 {
		t.Fatalf("rotated report = %+v", got)
	}
}

func TestMissingLogsReportLostCoverageAndDoNotCreateOffsets(t *testing.T) {
	dir := t.TempDir()
	dOffset := filepath.Join(dir, "daemon.offset")
	aOffset := filepath.Join(dir, "app.offset")
	got, err := run(options{
		daemonLog:    filepath.Join(dir, "missing-daemon"),
		appLog:       filepath.Join(dir, "missing-app"),
		daemonOffset: dOffset, appOffset: aOffset,
		maxSignals: defaultMaxSignals, commit: true,
	}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if !got.NeedsAttention || got.Daemon.State != "missing" || got.App.State != "missing" {
		t.Fatalf("missing report = %+v", got)
	}
	for _, path := range []string{dOffset, aOffset} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("offset %s should not exist, stat err=%v", path, err)
		}
	}
}

func TestPanicStackIsOneSignal(t *testing.T) {
	got := appReport(defaultMaxSignals,
		"panic: broken",
		"goroutine 1 [running]:",
		"\tmain.main()",
		"\t/tmp/main.go:12 +0x1",
	)
	if len(got.Signals) != 1 || got.Signals[0].Kind != "panic" || got.Signals[0].Attention != "error" {
		t.Fatalf("panic report = %+v", got)
	}
}

func TestKnownNoiseExplosionBecomesOneSignal(t *testing.T) {
	lines := make([]string, 151)
	for i := range lines {
		lines[i] = `time=2026-08-08T06:00:00Z level=ERROR msg="No security definition has been found" code=200`
	}
	got := daemonReport(defaultMaxSignals, lines...)
	if len(got.Signals) != 1 || got.Signals[0].Kind != "broker_read_notice" || got.Signals[0].Count != 151 {
		t.Fatalf("noise report = %+v", got)
	}
}

// Reproduces the 2026-08-13 night shape: a 3,000-line escalating retry loop,
// dozens of paired connectivity warnings, and a spread of one-off warnings.
// The flap is one incident, the loop one family, the one-offs one family:
// three rows where the line-shape counter kept ten signals and cut 22.
func TestEscalatingLoopIsOneFamilyAndFlapIsOneIncident(t *testing.T) {
	var lines []string
	for i := range 30 {
		lines = append(lines, fmt.Sprintf(`time=2026-08-13T04:%02d:00Z level=WARN msg="one-off warning variant %d"`, i, i))
	}
	for i := range 85 {
		ts := time.Date(2026, 8, 13, 5, 0, 0, 0, time.UTC).Add(time.Duration(i/2) * 10 * time.Second)
		lines = append(lines, `time=`+ts.Format(time.RFC3339)+` level=WARN msg="IBKRConnector: TWS lost connectivity to the IBKR backend (code 1100); waiting for a 1101/1102 restore notice"`)
	}
	for range 3000 {
		lines = append(lines, `time=2026-08-13T06:00:00Z level=WARN msg="System notice code=200: No security definition has been found for the request"`)
	}
	got := daemonReport(defaultMaxSignals, lines...)

	if len(got.Incidents) != 2 {
		t.Fatalf("incidents = %+v, want the flap and the one-off run", got.Incidents)
	}
	flap := got.Incidents[1]
	if flap.Kind != "backend_link" || flap.Anchors["backend_loss"] != 43 || !flap.Open || flap.Attention != "open_incident" {
		t.Fatalf("flap = %+v, want 43 collapsed losses still open", flap)
	}
	if run := got.Incidents[0]; run.Kind != "failure_run" || run.Subject != "one-off" || run.DependentLines != 30 {
		t.Fatalf("run = %+v", run)
	}
	// The open flap owns every later line: the loop is its dependent.
	if len(got.Signals) != 0 || flap.DependentLines != 3000 || flap.Dependents[0].Family != "broker_code_200_no_definition" {
		t.Fatalf("signals = %+v, flap dependents = %+v", got.Signals, flap.Dependents)
	}
}

// Suppressed rows are keyed by family: a code-354 notice cut by the cap
// never sits under a code-200 sample (2026-10-12).
func TestSuppressedRowsAreKeyedByFamily(t *testing.T) {
	var lines []string
	for range 5 {
		lines = append(lines, `time=2026-10-12T05:00:00Z level=WARN msg="[IBKR cid=15] System notice reqID=1 (SYNTHX STK) code=200: No security definition has been found for the request"`)
	}
	for range 3 {
		lines = append(lines, `time=2026-10-12T05:01:00Z level=WARN msg="[IBKR cid=15] System notice reqID=2 (SYNTHB BILL) code=354: Requested market data is not subscribed"`)
	}
	lines = append(lines, `time=2026-10-12T05:02:00Z level=WARN msg="regime: SYNTHY spot subscribe delivered no tick"`)
	got := daemonReport(1, lines...)

	raw, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	var decoded struct {
		Suppressed []struct {
			Family string `json:"family"`
			Count  int    `json:"count"`
			Sample string `json:"sample"`
		} `json:"suppressed"`
		SuppressedSignals int `json:"suppressed_signals"`
	}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.SuppressedSignals != 4 || len(decoded.Suppressed) != 2 {
		t.Fatalf("suppressed = %s", raw)
	}
	notice := decoded.Suppressed[0]
	if notice.Family != "broker_code_354" || notice.Count != 3 || !strings.Contains(notice.Sample, "broker_code_354") {
		t.Fatalf("354 row = %+v", notice)
	}
	if regime := decoded.Suppressed[1]; regime.Family != "regime" || regime.Count != 1 {
		t.Fatalf("regime row = %+v", regime)
	}
}

// The 2026-10-07 WhatIf timeout names the broker order it waited for in free
// text, which the key=value redaction never saw; the report passed it on.
// Broker order references are redacted wherever a message is emitted, and
// ordinary words and positions stay readable.
func TestOrderReferencesAreRedactedInFreeText(t *testing.T) {
	whatIf := `time=2026-10-07T14:03:00+02:00 level=WARN msg="WhatIf BOND SYNTHB: timeout waiting for broker WhatIf response (IBKR sent nothing naming order 48213)" component=IBKR`
	const want = "WhatIf BOND SYNTHB: timeout waiting for broker WhatIf response (IBKR sent nothing naming order [ref])"

	got := daemonReport(defaultMaxSignals, whatIf)
	if len(got.Signals) != 1 || got.Signals[0].Message != want {
		t.Fatalf("signal = %+v, want %q", got.Signals, want)
	}
	cut := daemonReport(1, `time=2026-10-07T14:00:00Z level=ERROR msg="kept error"`, whatIf)
	if len(cut.Suppressed) != 1 || cut.Suppressed[0].Sample != want {
		t.Fatalf("suppressed = %+v, want sample %q", cut.Suppressed, want)
	}

	for in, out := range map[string]string{
		"IBKR sent for order 48213: notice 2109: outside regular hours":                 "IBKR sent for order [ref]: notice 2109: outside regular hours",
		"OrderId 48213 that needs to be cancelled cannot be cancelled":                  "OrderId [ref] that needs to be cancelled cannot be cancelled",
		"open order orderId=48213 permId=91827364 status=Submitted":                     "open order orderId=[ref] permId=[ref] status=Submitted",
		"Order permId =91827364 is not cancellable":                                     "Order permId =[ref] is not cancellable",
		"dropping callback for broker order 48213 (perm 91827364)":                      "dropping callback for broker order [ref] (perm [ref])",
		"order #48213 rejected":                                                         "order #[ref] rejected",
		"order history unavailable; 3 orders cancelled":                                 "order history unavailable; 3 orders cancelled",
		"order 2 of 3 in the plan follows after this fill":                              "order 2 of 3 in the plan follows after this fill",
		"order 48213 of 100 shares rejected":                                            "order [ref] of 100 shares rejected",
		"modify preview token targets broker order ID 48213, current order is 48214":    "modify preview token targets broker order ID [ref], current order is [ref]",
		"modify preview token targets permanent ID 91827364, current order is 91827365": "modify preview token targets permanent ID [ref], current order is [ref]",
		"child order parentId 48213 not found":                                          "child order parentId [ref] not found",
		"openOrder 48213 arrived for an unknown contract":                               "openOrder [ref] arrived for an unknown contract",
		"reorder 5 legs by expiry":                                                      "reorder 5 legs by expiry",
	} {
		if got := redactMessage(in); got != out {
			t.Errorf("redactMessage(%q) = %q, want %q", in, got, out)
		}
	}
}

func TestSpacedRestartsAreNotALoop(t *testing.T) {
	var lines []string
	for i := range 10 {
		stop := time.Date(2026, 8, 11, 7+i, 6, 0, 0, time.UTC)
		start := stop.Add(2 * time.Second)
		lines = append(lines,
			`time=`+stop.Format(time.RFC3339)+` level=INFO msg="Shutting down server." reason=terminated`,
			`time=`+start.Format(time.RFC3339)+` level=INFO msg="canary app serving" listen=0.0.0.0:8765`)
	}
	got := appReport(defaultMaxSignals, lines...)
	if len(got.Signals) != 0 {
		t.Fatalf("hourly restarts should not signal: %+v", got.Signals)
	}
	if got.Families["lifecycle"] != 20 {
		t.Fatalf("lifecycle count = %d, want 20", got.Families["lifecycle"])
	}
}

func TestAppStoppedWithErrorPages(t *testing.T) {
	got := appReport(defaultMaxSignals,
		`time=2026-08-11T07:06:00Z level=ERROR msg="canary app stopped" error="listen tcp 0.0.0.0:8765: address already in use"`,
	)
	if len(got.Signals) != 1 || got.Signals[0].Severity != "ERROR" {
		t.Fatalf("app stopped with error should signal: %+v", got.Signals)
	}
	if got.Families["lifecycle"] != 0 {
		t.Fatalf("error exit must not count as routine lifecycle: %+v", got.Families)
	}
}

func TestClusteredRestartsAreALoop(t *testing.T) {
	var lines []string
	for i := range 5 {
		ts := time.Date(2026, 8, 11, 7, i, 0, 0, time.UTC)
		lines = append(lines, `time=`+ts.Format(time.RFC3339)+` level=INFO msg="canary app serving" listen=0.0.0.0:8765`)
	}
	got := appReport(defaultMaxSignals, lines...)
	if len(got.Signals) != 1 || got.Signals[0].Kind != "restart_loop" || got.Signals[0].Count != 5 {
		t.Fatalf("clustered restarts report = %+v", got.Signals)
	}
}

func writeTestFile(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
}

// Reproduces 2026-09-25: 172 gamma prewarm failures, each carrying its own
// expiry, elapsed time and progress counters, were 172 singletons. Values
// that vary per attempt collapse into one sample per family; codes that tell
// broker errors apart do not.
func TestPerAttemptValuesCollapseButCodesStayDistinct(t *testing.T) {
	var lines []string
	for i := range 40 {
		lines = append(lines, fmt.Sprintf(`time=2026-09-25T16:00:00Z level=WARN msg="gamma.prewarm class=SPY expiry=202609%02d cached=%d dropped=%d elapsed=%dm%d.%03ds err=prewarm SPY 202609%02d class=SPY route attempts SMART+ARCA,ARCA,CBOE: prewarm timeout after %ds (cached %d so far)"`, 26+i%3, i*7, i%4, 1+i%3, i, i*13, 26+i%3, 60+i, i*7))
		lines = append(lines, fmt.Sprintf(`time=2026-09-25T16:00:00Z level=WARN msg="market history refresh SPX CBOE 1D: contract details request failed (IBKR 200); next attempt after 16:%02d:19"`, i+1))
		lines = append(lines, fmt.Sprintf(`time=2026-09-25T16:00:00Z level=WARN msg="fx rate EUR/USD: live resolution failed; serving last-known-good 0.85%02d (age %dm0s)"`, i, i))
	}
	lines = append(lines,
		`time=2026-09-25T17:00:00Z level=WARN msg="[IBKR cid=15] System notice code=200: The destination or exchange selected is Invalid"`,
		`time=2026-09-25T17:00:01Z level=WARN msg="[IBKR cid=15] System notice code=162: Historical Market Data Service error message"`,
	)
	got := daemonReport(defaultMaxSignals, lines...)
	if len(got.Signals) != 5 || got.SuppressedSignals != 0 {
		t.Fatalf("signals = %+v, suppressed = %d; want 5 signals", got.Signals, got.SuppressedSignals)
	}
	for i, want := range []struct{ family, text string }{
		{"gamma", "gamma.prewarm class=SPY expiry=[expiry] cached=N dropped=N elapsed=[duration]"},
		{"market_history", "next attempt after [clock]"},
		{"fx_rate", "last-known-good [rate] (age [duration])"},
	} {
		if s := got.Signals[i]; s.Count != 40 || s.Distinct != 1 || s.Family != want.family || !strings.Contains(s.Message, want.text) {
			t.Fatalf("signal %d = %+v, want 40x %s %q in one sample", i, s, want.family, want.text)
		}
	}
	if !strings.Contains(got.Signals[0].Message, "(cached N so far)") {
		t.Fatalf("progress counter kept: %q", got.Signals[0].Message)
	}
	if got.Signals[3].Family != "broker_code_200" || got.Signals[4].Family != "broker_code_162" ||
		!strings.Contains(got.Signals[3].Message, "code=200") || !strings.Contains(got.Signals[4].Message, "code=162") {
		t.Fatalf("broker codes must stay distinct: %+v", got.Signals[3:])
	}
}

// TestDefinitionNoticeNamesItsContractButNeverAHolding witnesses a report
// that counted thousands of code-200 notices without saying which contract
// they were about. The family is one signal; its samples name the contracts,
// and a current holding is masked there and in free text alike.
func TestDefinitionNoticeNamesItsContractButNeverAHolding(t *testing.T) {
	saved := held
	t.Cleanup(func() { held = saved })
	held = newHoldings(holdingsFromCache, []string{"SYNTHQ", "SYNTHA B", "SPY"})
	notice := func(tag string) string {
		return `time=2026-09-30T05:00:00Z level=WARN msg="[IBKR cid=16] System notice ` + tag + ` code=200: No security definition has been found for the request"`
	}
	got := daemonReport(defaultMaxSignals,
		notice("reqID=1 (OKE STK)"),
		notice("reqID=1 (OKE STK)"),
		notice("reqID=25696 (SYNTHQ STK)"),
		notice("reqID=41 (SYNTHA B STK)"),
		notice("reqID=7 (SPY STK)"),
		notice("reqID=9"),
		`time=2026-09-30T05:00:01Z level=WARN msg="market history refresh SYNTHQ: no security definition for contract; paused for the rest of this broker session"`,
	)
	if len(got.Signals) != 2 || got.Families["broker_code_200_no_definition"] != 6 {
		t.Fatalf("signals = %+v families = %v", got.Signals, got.Families)
	}
	def := got.Signals[0]
	if def.Family != "broker_code_200_no_definition" || def.Count != 6 || def.Distinct != 4 {
		t.Fatalf("definition signal = %+v", def)
	}
	samples := append([]string{def.Message}, def.OtherSamples...)
	for _, label := range []string{"(OKE STK)", "([holding] STK)", "(SPY STK)", "(untagged)"} {
		if !slices.ContainsFunc(samples, func(s string) bool { return strings.HasSuffix(s, label) }) {
			t.Fatalf("samples %q lack %s", samples, label)
		}
	}
	if s := got.Signals[1]; s.Message != "market history refresh [holding]: no security definition for contract; paused for the rest of this broker session" {
		t.Fatalf("free text = %q", s.Message)
	}
	raw, _ := json.Marshal(got)
	if strings.Contains(string(raw), "SYNTHQ") || strings.Contains(string(raw), "SYNTHA") {
		t.Fatalf("holding named: %s", raw)
	}

	held = holdings{source: holdingsUnavailable}
	if got := noticeContractLabel(notice("reqID=1 (OKE STK)")); got != "[contract] STK" {
		t.Fatalf("label without a holdings list = %q, want the name withheld", got)
	}
}

// TestHoldingsComeFromTheDaemonsFile reads the holdings file the daemon
// writes for the account-data gate, and reports unavailable without it.
func TestHoldingsComeFromTheDaemonsFile(t *testing.T) {
	dir := t.TempDir()
	cache := filepath.Join(dir, "holdings-denylist")
	writeTestFile(t, cache, "# as of 2026-09-29\nSYNTHQ\nsyntha\n")
	got := loadHoldings(cache)
	if got.source != holdingsFromCache || !got.tickers["SYNTHQ"] || !got.tickers["SYNTHA"] || got.mask("refresh SYNTHQ now") != "refresh [holding] now" {
		t.Fatalf("cache holdings = %+v", got)
	}
	if got := loadHoldings(filepath.Join(dir, "missing")); got.available() || got.mask("SYNTHQ") != "SYNTHQ" {
		t.Fatalf("no source = %+v, want unavailable", got)
	}
}

// The daemon's start marker names its build, so a check can tell lines from a
// replaced build; a notice the daemon logged at INFO is its own verdict that
// the line is routine and never pages.
func TestDaemonBuildsAndInfoNoticesStayQuiet(t *testing.T) {
	got := daemonReport(defaultMaxSignals,
		`time=2026-10-07T09:16:09+02:00 level=INFO msg="canary daemon serving v3.17.0 (build d59b6bf3) on /tmp/canary.sock (gateway=127.0.0.1:7496, clientID=15)"`,
		`time=2026-10-07T09:16:10+02:00 level=INFO msg="Connected to IB Gateway 127.0.0.1:7496 (clientID=15, tls=false)"`,
		`time=2026-10-07T09:20:00+02:00 level=INFO msg="[IBKR cid=15] System notice reqID=7 (AAA.OLD STK) code=200 @ 2026-10-07T07:20:00Z: No security definition has been found for the request (the requester records this verdict)" component=IBKR`,
		`time=2026-10-07T09:21:00+02:00 level=INFO msg="[IBKR cid=15] System notice reqID=8 (NDX IND) code=354: Requested market data is not subscribed (known entitlement gap; repeat probe)" component=IBKR`,
		`time=2026-10-07T10:00:00+02:00 level=INFO msg="canary daemon serving v3.17.0 (build 0a1b2c3d+modified) on /tmp/canary.sock (gateway=127.0.0.1:7496, clientID=15)"`,
	)
	if len(got.Signals) != 0 {
		t.Fatalf("routine lines signalled: %+v", got.Signals)
	}
	if want := []string{"d59b6bf3", "0a1b2c3d+modified"}; !slices.Equal(got.Builds, want) {
		t.Fatalf("builds = %v, want %v", got.Builds, want)
	}
	if got.Families["lifecycle"] != 3 || got.Families["broker_code_200_no_definition"] != 1 || got.Families["broker_code_354"] != 1 {
		t.Fatalf("families = %v", got.Families)
	}
	if len(got.Incidents) != 2 || got.Incidents[1].Kind != "daemon_restart" || !got.Incidents[1].BuildChanged || got.Incidents[1].Attention != "" {
		t.Fatalf("restarts = %+v, want two quiet restarts, the second a build change", got.Incidents)
	}
}

// A report counted eleven code-354 refusals without saying which line was
// refused. Each attributed contract is now its own signal with its count, as
// code 200's are; a current holding stays masked.
func TestEntitlementNoticeNamesItsContract(t *testing.T) {
	saved := held
	t.Cleanup(func() { held = saved })
	held = newHoldings(holdingsFromCache, []string{"SYNTHQ"})
	notice := func(tag string) string {
		return `time=2026-10-11T20:00:29.146+02:00 level=WARN msg="[IBKR cid=15] System notice ` + tag + ` code=354 @ 2026-10-11T17:59:35Z: Requested market data is not subscribed. Check API status by selecting the Account menu then under Management choose Market Data Subscription Manager and/or Market Data Subscriptions." component=IBKR`
	}
	lines := []string{
		notice("reqID=49175 (CRWV STK)"),
		notice("reqID=49180 (CRWV STK)"),
		notice("reqID=49176 (SYNTHQ STK)"),
		notice("reqID=103007 (SPX OPT)"),
		notice("reqID=49190"),
	}
	got := daemonReport(defaultMaxSignals, lines...)
	if len(got.Signals) != 1 || got.Families["broker_code_354"] != 5 {
		t.Fatalf("signals = %+v families = %v", got.Signals, got.Families)
	}
	sig := got.Signals[0]
	if sig.Family != "broker_code_354" || sig.Count != 5 || sig.Distinct != 4 {
		t.Fatalf("entitlement signal = %+v", sig)
	}
	samples := append([]string{sig.Message}, sig.OtherSamples...)
	for _, label := range []string{"(CRWV STK)", "([holding] STK)", "(SPX OPT)", "(untagged)"} {
		if !slices.ContainsFunc(samples, func(s string) bool { return strings.HasSuffix(s, label) }) {
			t.Fatalf("samples %q lack %s", samples, label)
		}
	}
	if raw, _ := json.Marshal(got); strings.Contains(string(raw), "SYNTHQ") {
		t.Fatalf("holding named: %s", raw)
	}

	held = holdings{source: holdingsUnavailable}
	if got := noticeContractLabel(notice("reqID=49175 (CRWV STK)")); got != "[contract] STK" {
		t.Fatalf("label without a holdings list = %q, want the name withheld", got)
	}
}
