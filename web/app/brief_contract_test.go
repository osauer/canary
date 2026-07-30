package appweb

import (
	"slices"
	"strings"
	"testing"
)

func TestBriefCardStaticContract(t *testing.T) {
	t.Parallel()

	htmlData, err := Files.ReadFile("index.html")
	if err != nil {
		t.Fatal(err)
	}
	html := string(htmlData)
	// The brief lives on its own tab now: the brief panel must sit inside the
	// brief tab panel, ahead of the monitor dashboard, not among the monitor
	// cards. The signal panel stays in the monitor dashboard.
	briefTabAt := strings.Index(html, `data-tab-panel="brief"`)
	briefAt := strings.Index(html, `id="briefPanel"`)
	monitorAt := strings.Index(html, `data-tab-panel="monitor"`)
	signalAt := strings.Index(html, `id="signalPanel"`)
	if briefTabAt < 0 || briefAt < 0 || monitorAt < 0 || signalAt < 0 || briefAt < briefTabAt || briefAt > monitorAt || signalAt < monitorAt {
		t.Fatalf("brief panel must live inside the brief tab before the monitor dashboard: briefTab=%d brief=%d monitor=%d signal=%d", briefTabAt, briefAt, monitorAt, signalAt)
	}
	if !strings.Contains(html, `id="tabBrief"`) || !strings.Contains(html, `data-tab="brief"`) {
		t.Error("index.html missing the Brief bottom-tab button (#tabBrief / data-tab=\"brief\")")
	}
	for _, id := range []string{"briefTab", "briefPanel", "briefAsOf", "briefSourceBanner", "briefSections", "briefAckStatus"} {
		if !strings.Contains(html, `id="`+id+`"`) {
			t.Errorf("index.html missing brief id %q", id)
		}
	}

	briefData, err := Files.ReadFile("brief.js")
	if err != nil {
		t.Fatal("brief.js is not embedded:", err)
	}
	brief := string(briefData)
	for _, want := range []string{
		// Narrative path: the daemon-composed prose renders when it is served,
		// and the row sections stay the fallback when an older daemon serves
		// none. Both paths keep the same stamp, sign-off, and pulse handling.
		`const narrative = servedNarrative(brief)`,
		`sections.classList.toggle("brief-sections--narrative", Boolean(narrative))`,
		`sections.replaceChildren(...renderNarrative(narrative, brief))`,
		`const RUN_ROLE_CLASSES = { figure: "pd-fig", watch: "pd-wtint", act: "pd-atint" }`,
		`briefPlacard("Review \u00b7 last session")`,
		`briefPlacard("Ready \u00b7 next open")`,
		`runsElement("div", "pd-brf-lead", narrative.lead)`,
		`runsElement("p", "pd-brf-para", runs)`,
		`runsElement("p", "pd-brf-coda", narrative.coda)`,
		`joinValues("Briefing", dateValue(brief.as_of), timeValue(brief.as_of))`,
		`chip.textContent = severity`,
		`signoffControls(brief.review?.one_tap || {}, brief, brief.review?.rules_delta || {})`,
		`renderReviewSection(brief.review || {}, brief)`,
		`renderReadySection(brief.ready || {}, snap.sources || {})`,
		`proposalsValue(section.proposals || {})`,
		`capitalEventsValue(section.capital_events || {})`,
		`briefRow("Session P/L", account`,
		`briefRow("By underlying", section.attribution`,
		`briefRow("Capital events", section.capital_events`,
		`dateTimeValue(brief.as_of)`,
		`percentValue(section.gamma, "gap_pct", "Gap", true)`,
		`briefRow("Artefacts", section.artefacts, null)`,
		`return "not declared"`,
		`completed ${completedAt}`,
		`"not yet completed this week"`,
		`"not yet completed today"`,
		`heldNameEventsUnavailable(sources)`,
		`Held-name events require an available positions snapshot.`,
		`row.rulebook_fingerprint_changed === true`,
		`row.signable === true`,
		`Sign off this reconcile report — statement clean`,
		"button.title = `Report ${reportID}`",
		`rulesDeltaUnclean(rulesDelta)`,
		`review the Rules delta row before signing`,
		"`peak set ${dateTimeValue(capital.peak_as_of)}`",
		`percentValue(section.latch, "consumed_pct_at_latch", "Engaged at")`,
		`fetch("/api/recon/signoff"`,
		`["ok", "attention", "degraded", "unavailable"]`,
		`stressHeadline(section.stress)`,
		`moversValue(section.attribution, currency)`,
		`other_daily_pnl_base`,
		`fieldValue(capital, "tier", "Tier")`,
		`fieldValue(capital, "enforcement", "Enforcement")`,
		`body: JSON.stringify({ report_id: reportID })`,
		`fetch("/api/brief/seen"`,
		`body: JSON.stringify(briefAckBody(brief, fingerprint))`,
		`body.month = brief.ready?.monthly_pulse?.month || ""`,
		`body.evidence = "render"`,
		`monthlyPulseStatus(monthly)`,
		`return "not due"`,
		`return "completed this month"`,
		`return "blocked by policy evidence"`,
		`foreground render recorded`,
		`state.authenticated === true`,
		`state.activeTab === "brief"`,
		`document.visibilityState === "visible"`,
		`const attemptedStampFingerprints = new Set()`,
		`const pendingStampFingerprints = new Set()`,
		`let briefStampArmed = true`,
		`let briefStampScheduled = false`,
		`let briefStampInFlight = false`,
		`briefStampLook += 1`,
		`const look = briefStampLook`,
		`look !== briefStampLook`,
		`globalThis.requestAnimationFrame`,
	} {
		if !strings.Contains(brief, want) {
			t.Errorf("brief.js missing contract %q", want)
		}
	}
	for _, forbidden := range []string{
		"window.confirm",
		"confirm_account",
		"confirm_mode",
		"toLocaleString(",
		"shortTimeWithZone",
		`declared ${artefact.declared}`,
		`completed ${artefact.completed}`,
		`fingerprint changed ${row.rulebook_fingerprint_changed}`,
		// Runs are typed text. The mockup's inline markers are prototype
		// notation and must never reach the SPA, and no run may be injected as
		// markup.
		"innerHTML",
		"[/f]",
		"[/w]",
		"[/a]",
	} {
		if strings.Contains(brief, forbidden) {
			t.Errorf("brief.js contains forbidden contract %q", forbidden)
		}
	}
	runs := jsFunctionBlock(t, brief, "runsElement")
	for _, want := range []string{
		`document.createTextNode(text)`,
		`span.className = roleClass`,
		`span.textContent = text`,
	} {
		if !strings.Contains(runs, want) {
			t.Errorf("runsElement must render served runs as text %q:\n%s", want, runs)
		}
	}
	narrative := jsFunctionBlock(t, brief, "servedNarrative")
	for _, want := range []string{
		`if (!narrative) return null;`,
		`if (lead.length === 0 && review.length === 0 && ready.length === 0) return null;`,
	} {
		if !strings.Contains(narrative, want) {
			t.Errorf("servedNarrative must fall back to the row render when no prose is served %q:\n%s", want, narrative)
		}
	}
	percent := jsFunctionBlock(t, brief, "percentValue")
	for _, want := range []string{`.toFixed(1)`, `signed && object[key] > 0 ? "+" : ""`} {
		if !strings.Contains(percent, want) {
			t.Errorf("percentValue missing one-decimal/signed contract %q", want)
		}
	}
	date := jsFunctionBlock(t, brief, "dateValue")
	for _, want := range []string{"getFullYear()", "getMonth() + 1", "getDate()"} {
		if !strings.Contains(date, want) {
			t.Errorf("dateValue missing local ISO date component %q", want)
		}
	}
	dateTime := jsFunctionBlock(t, brief, "dateTimeValue")
	for _, want := range []string{"dateValue(at)", "getHours()", "getMinutes()"} {
		if !strings.Contains(dateTime, want) {
			t.Errorf("dateTimeValue missing local short-time component %q", want)
		}
	}
	row := jsFunctionBlock(t, brief, "briefRow")
	if !strings.Contains(row, "if (value !== null) el.append(provided);") {
		t.Fatalf("briefRow must omit the value element for a null group-header value:\n%s", row)
	}
	visibility := jsFunctionBlock(t, brief, "setupBriefVisibility")
	for _, want := range []string{
		`document.visibilityState !== "visible"`,
		`briefStampLook += 1`,
		`briefStampArmed = false`,
		`briefStampArmed = true`,
		`renderBriefCard(state.snapshot || {})`,
	} {
		if !strings.Contains(visibility, want) {
			t.Errorf("setupBriefVisibility missing foreground-look contract %q", want)
		}
	}
	schedule := jsFunctionBlock(t, brief, "scheduleBriefStamp")
	for _, want := range []string{
		`!briefStampArmed`,
		`briefStampScheduled`,
		`briefStampInFlight`,
		`attemptedStampFingerprints.has(fingerprint)`,
		`pendingStampFingerprints.has(fingerprint)`,
		`look !== briefStampLook`,
		`state.snapshot?.brief?.brief_fingerprint !== fingerprint`,
		`briefStampVisible()`,
		`globalThis.requestAnimationFrame`,
	} {
		if !strings.Contains(schedule, want) {
			t.Errorf("scheduleBriefStamp missing one-stamp-per-look gate %q", want)
		}
	}
	acknowledge := jsFunctionBlock(t, brief, "acknowledgeBrief")
	nonOK := strings.Index(acknowledge, `if (!res.ok) throw`)
	disarm := strings.Index(acknowledge, `if (look === briefStampLook) briefStampArmed = false;`)
	if nonOK < 0 || disarm < nonOK {
		t.Fatalf("acknowledgeBrief must disarm the current look only after a successful response:\n%s", acknowledge)
	}
	if strings.Index(acknowledge, `briefStampInFlight = false;`) < disarm {
		t.Fatalf("acknowledgeBrief must keep the global in-flight guard through successful disarm:\n%s", acknowledge)
	}

	appData, err := Files.ReadFile("app.js")
	if err != nil {
		t.Fatal(err)
	}
	app := string(appData)
	if !strings.Contains(app, `from "./brief.js"`) || strings.Count(app, "renderBriefCard(snap);") < 2 || !strings.Contains(app, "setupBriefVisibility();") {
		t.Fatal("app.js does not wire the brief renderer into renderAll, the one-second loop, and visibility setup")
	}
	lifecycleData, err := Files.ReadFile("lifecycle.js")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(lifecycleData), `"rules", "brief"`) {
		t.Fatal("lifecycle.js does not subscribe to incremental brief events")
	}
	if !slices.Contains(EmbeddedJavaScriptFileNames(), "brief.js") {
		t.Fatal("EmbeddedJavaScriptFileNames omits brief.js")
	}
}
