package appweb

import (
	"fmt"
	"os"
	"regexp"
	"strings"
	"testing"
)

func TestAppJSDoesNotUseBareNotificationGlobal(t *testing.T) {
	t.Parallel()
	js := embeddedSPASource(t)
	bareNotification := regexp.MustCompile(`(^|[^.$A-Za-z0-9_])Notification([.()]|\b)`)
	for lineNo, line := range strings.Split(js, "\n") {
		if bareNotification.MatchString(line) && !strings.Contains(line, "globalThis.Notification") {
			t.Fatalf("app.js:%d uses unguarded Notification global: %s", lineNo+1, line)
		}
	}
}

func TestAppJSPushControlsUseCapabilityHelpers(t *testing.T) {
	t.Parallel()
	js := embeddedSPASource(t)
	for _, want := range []string{
		"function notificationStateLabel()",
		"function hasNotifications()",
		"function canUseWebPush()",
		`$("pushState").textContent = notificationStateLabel();`,
		"if (!canUseWebPush())",
	} {
		if !strings.Contains(js, want) {
			t.Fatalf("app.js missing push compatibility guard %q", want)
		}
	}
}

func TestAppJSStalePairingURLFallsBackToDeviceLogin(t *testing.T) {
	t.Parallel()
	js := embeddedSPASource(t)
	main := jsFunctionBlock(t, js, "main")
	for _, want := range []string{
		`history.replaceState({}, "", "/");`,
		`localStorage.setItem("ibkrRemoteRoute", remote);`,
		`bootstrapped = await bootstrapWithRetry();`,
		"Pairing link expired; opening paired app.",
	} {
		if !strings.Contains(main, want) {
			t.Fatalf("main missing stale pairing recovery contract %q", want)
		}
	}
	// A transient bootstrap failure must retry, never dead-end; only a
	// definitive device rejection may show the re-pair instruction.
	retry := jsFunctionBlock(t, js, "bootstrapWithRetry")
	for _, want := range []string{
		"state.pairingRequired",
		"retrying automatically",
	} {
		if !strings.Contains(retry, want) {
			t.Fatalf("bootstrapWithRetry missing retry contract %q", want)
		}
	}
	fetchBootstrap := jsFunctionBlock(t, js, "fetchBootstrap")
	for _, want := range []string{
		"state.pairingRequired = true",
		"Scan a fresh QR code",
	} {
		if !strings.Contains(fetchBootstrap, want) {
			t.Fatalf("fetchBootstrap missing definitive-rejection contract %q", want)
		}
	}
}

func TestManifestUsesStableRootLaunchScope(t *testing.T) {
	t.Parallel()
	data, err := Files.ReadFile("manifest.webmanifest")
	if err != nil {
		t.Fatalf("read manifest.webmanifest: %v", err)
	}
	manifest := string(data)
	for _, want := range []string{
		`"id": "/"`,
		`"start_url": "/"`,
		`"scope": "/"`,
	} {
		if !strings.Contains(manifest, want) {
			t.Fatalf("manifest missing stable launch contract %q", want)
		}
	}
}

func TestVisibleProductIdentityIsCanary(t *testing.T) {
	t.Parallel()
	manifestData, err := Files.ReadFile("manifest.webmanifest")
	if err != nil {
		t.Fatalf("read manifest.webmanifest: %v", err)
	}
	htmlData, err := Files.ReadFile("index.html")
	if err != nil {
		t.Fatalf("read index.html: %v", err)
	}
	serviceWorkerData, err := Files.ReadFile("service-worker.js")
	if err != nil {
		t.Fatalf("read service-worker.js: %v", err)
	}
	manifest, html, serviceWorker := string(manifestData), string(htmlData), string(serviceWorkerData)
	for name, check := range map[string]struct {
		source string
		wants  []string
	}{
		"manifest": {manifest, []string{`"name": "Canary"`, `"short_name": "Canary"`}},
		"browser":  {html, []string{`<title>Canary</title>`, `<h1 aria-label="Canary"><span>Canary</span></h1>`}},
		"push":     {serviceWorker, []string{`: "Canary";`, `: "Open Canary for details.";`}},
	} {
		for _, want := range check.wants {
			if !strings.Contains(check.source, want) {
				t.Errorf("%s identity missing %q", name, want)
			}
		}
	}
	for _, stale := range []string{"ibkr canary", "Canary · IBKR", "Canary IBKR"} {
		for name, source := range map[string]string{"manifest": manifest, "browser": html, "push": serviceWorker} {
			if strings.Contains(source, stale) {
				t.Errorf("%s retains stale product identity %q", name, stale)
			}
		}
	}
}

func TestProductRenameKeepsPinnedRuntimeConfigPaths(t *testing.T) {
	t.Parallel()
	alertsData, err := Files.ReadFile("alerts.js")
	if err != nil {
		t.Fatalf("read alerts.js: %v", err)
	}
	alerts := string(alertsData)
	for _, want := range []string{
		"~/.config/ibkr/flex-token",
		"~/.config/ibkr/config.toml",
	} {
		if !strings.Contains(alerts, want) {
			t.Errorf("product rename removed safety-pinned runtime path %q", want)
		}
	}
	if strings.Contains(alerts, "~/.config/canary/") {
		t.Error("product rename invented a nonexistent Canary XDG config namespace")
	}
}

func TestVisibleStressCopyDoesNotUseCanaryAsDomainTerm(t *testing.T) {
	t.Parallel()
	stressData, err := Files.ReadFile("stress.js")
	if err != nil {
		t.Fatalf("read stress.js: %v", err)
	}
	protectionData, err := Files.ReadFile("protection.js")
	if err != nil {
		t.Fatalf("read protection.js: %v", err)
	}
	htmlData, err := Files.ReadFile("index.html")
	if err != nil {
		t.Fatalf("read index.html: %v", err)
	}
	screenshotData, err := os.ReadFile("../../scripts/app-screenshots.mjs")
	if err != nil {
		t.Fatalf("read app-screenshots.mjs: %v", err)
	}
	stress, protection, html, screenshots := string(stressData), string(protectionData), string(htmlData), string(screenshotData)
	for name, check := range map[string]struct {
		source string
		wants  []string
	}{
		"stress renderer": {stress, []string{
			`"Stress driver"`,
			`"No active stress drivers"`,
			`"Waiting for stress snapshot."`,
			`fully confirmed stress signal.`,
			`defensive stress action.`,
			`source: "stress read"`,
			`before treating the stress read as a market signal.`,
		}},
		"cold shell":         {html, []string{"Waiting for stress snapshot."}},
		"protection source":  {protection, []string{"Source: stress snapshot."}},
		"screenshot fixture": {screenshots, []string{"No defensive stress action is indicated."}},
	} {
		for _, want := range check.wants {
			if !strings.Contains(check.source, want) {
				t.Errorf("%s missing renamed stress copy %q", name, want)
			}
		}
	}
	for _, stale := range []string{
		"Canary driver",
		"canary drivers",
		"canary snapshot",
		"canary trigger",
		"defensive canary action",
		"canary market read",
		"canary portfolio snapshot",
		"canary as a market signal",
	} {
		for name, source := range map[string]string{
			"stress renderer":    stress,
			"cold shell":         html,
			"protection source":  protection,
			"screenshot fixture": screenshots,
		} {
			if strings.Contains(strings.ToLower(source), strings.ToLower(stale)) {
				t.Errorf("%s retains stale stress-domain copy %q", name, stale)
			}
		}
	}
}

func TestCleanSlateRenameUsesStressDOMAndRetainsSafetyContracts(t *testing.T) {
	t.Parallel()
	htmlData, err := Files.ReadFile("index.html")
	if err != nil {
		t.Fatalf("read index.html: %v", err)
	}
	cssData, err := Files.ReadFile("styles.css")
	if err != nil {
		t.Fatalf("read styles.css: %v", err)
	}
	serviceWorkerData, err := Files.ReadFile("service-worker.js")
	if err != nil {
		t.Fatalf("read service-worker.js: %v", err)
	}
	html, css, js, serviceWorker := string(htmlData), string(cssData), embeddedSPASource(t), string(serviceWorkerData)

	for _, id := range []string{
		"stressAsOf", "stressHero", "stressDetailToggle", "stressAction", "stressSummary",
		"stressSeverity", "stressRulesCard", "stressRulesCounts", "stressRulesToggle",
		"stressRulesBrief", "stressRulesNotesToggle", "stressRulesNotesDialog",
		"stressRulesNotesLabel", "stressRulesNotesTitle", "stressRulesNotesClose",
		"stressRulesNotesList", "stressRulesDetailPanel", "stressRulesGrid",
		"stressDetailPanel", "stressDetailGrid", "stressDrivers",
	} {
		if !strings.Contains(html, `id="`+id+`"`) {
			t.Errorf("clean-slate rename missing stress DOM id %q", id)
		}
	}
	for _, class := range []string{
		"signal-half--stress", "stress-hero", "stress-hero__copy",
		"stress-rules", "stress-rules__head", "stress-rules__brief",
		"stress-rules__strip", "stress-rules-detail",
	} {
		if !strings.Contains(html, class) || !strings.Contains(css, "."+class) {
			t.Errorf("clean-slate rename missing stress DOM/CSS class %q", class)
		}
	}
	if !strings.Contains(html, "signal-detail-col--stress") {
		t.Error(`clean-slate rename missing DOM class "signal-detail-col--stress"`)
	}
	for _, stale := range []string{
		"canaryAsOf", "canaryHero", "canaryDetailToggle", "canaryAction", "canarySummary",
		"canarySeverity", "canaryRulesCard", "canaryRulesCounts", "canaryRulesToggle",
		"canaryRulesBrief", "canaryRulesNotesToggle", "canaryRulesNotesDialog",
		"canaryRulesNotesLabel", "canaryRulesNotesTitle", "canaryRulesNotesClose",
		"canaryRulesNotesList", "canaryRulesDetailPanel", "canaryRulesGrid",
		"canaryDetailPanel", "canaryDetailGrid", "canaryDrivers",
	} {
		if strings.Contains(html, `id="`+stale+`"`) || strings.Contains(js, `$("`+stale+`")`) {
			t.Errorf("clean-slate rename retains stale sensor DOM id %q", stale)
		}
	}
	for _, stale := range []string{
		"signal-half--canary", "canary-hero", "canary-summary-actions",
		"canary-rules", "canary-rules-detail", "signal-detail-col--canary",
	} {
		if strings.Contains(html, stale) || strings.Contains(css, "."+stale) {
			t.Errorf("clean-slate rename retains stale sensor DOM/CSS class %q", stale)
		}
	}
	for _, identifier := range []string{
		`"ibkrRemoteRoute"`, `"ibkrDeviceID"`,
		`"ibkrDeviceKeyJWK"`, `"ibkrPurgeBook"`, `"ibkrPurgeBooks"`,
		`indexedDB.open("ibkr-app", 1)`,
	} {
		if !strings.Contains(js, identifier) {
			t.Errorf("rename removed safety-critical persisted browser identifier %q", identifier)
		}
	}
	// ibkrDeviceSecret was the plaintext bearer credential the crypto-less
	// pairing path kept in localStorage. It is gone; the HttpOnly device
	// cookie is that path's credential now, and no reader may come back.
	if strings.Contains(js, "ibkrDeviceSecret") {
		t.Error("plaintext device secret reappeared in SPA storage")
	}
	for _, identifier := range []string{
		`"canaryAccountValueVisible"`, `"canarySelectedMarket"`, `"canaryActiveTab"`,
	} {
		if !strings.Contains(js, identifier) {
			t.Errorf("clean-slate rename missing canonical browser preference %q", identifier)
		}
	}
	for _, stale := range []string{
		`"ibkrAccountValueVisible"`, `"ibkrSelectedMarket"`, `"ibkrActiveTab"`,
	} {
		if strings.Contains(js, stale) {
			t.Errorf("clean-slate rename retained non-safety browser preference %q", stale)
		}
	}
	if !strings.Contains(js, `globalThis.__canarySmoke`) {
		t.Error("clean-slate rename missing the Canary-owned nonpersistent smoke hook")
	}
	if !strings.Contains(js, `"canary", "regime", "rulebook"`) {
		t.Error("rename removed the daemon alert-source contract identifier")
	}
	for _, route := range []string{
		`navigator.serviceWorker?.register("/service-worker.js")`,
		`fetch("/api/pairing/complete"`,
		`fetch("/api/auth/challenge"`,
		`fetch("/api/auth/session"`,
	} {
		if !strings.Contains(js, route) {
			t.Errorf("rename removed required browser route %q", route)
		}
	}
	for _, route := range []string{
		`monitor: "/?tab=monitor"`,
		`brief: "/?tab=brief"`,
		`alerts: "/?tab=alerts"`,
	} {
		if !strings.Contains(serviceWorker, route) {
			t.Errorf("rename removed required notification route %q", route)
		}
	}
	if !strings.Contains(html, `https://osauer.dev/canary/feedback/?source=canary-app`) {
		t.Error("feedback must use the canonical Canary public path and product-origin source identifier")
	}
	if strings.Contains(html, `osauer.dev/ibkr/`) {
		t.Error("clean-slate SPA must not link to the removed /ibkr public path")
	}
}

func TestAppJSTradingStateUsesSnapshotCanWrite(t *testing.T) {
	t.Parallel()
	js := embeddedSPASource(t)
	renderSettings := jsFunctionBlock(t, js, "renderSettings")
	if !strings.Contains(renderSettings, "const status = state.snapshot?.trading || {};") {
		t.Fatalf("renderSettings must use live snapshot trading status, not settings.trading.status")
	}
	if strings.Contains(renderSettings, "trading.status") {
		t.Fatalf("renderSettings must not read embedded settings.trading.status")
	}
	for _, name := range []string{
		"tradingStatusSettingsLabel",
		"orderModifyGate",
	} {
		body := jsFunctionBlock(t, js, name)
		if !strings.Contains(body, "can_write") {
			t.Fatalf("%s must use can_write", name)
		}
	}
	canWriteUnderlyings := jsFunctionBlock(t, js, "canWriteUnderlyings")
	if strings.Contains(canWriteUnderlyings, "can_write") || !strings.Contains(canWriteUnderlyings, "return false") {
		t.Fatal("typed-disabled purge/restore must not inherit generic trading.can_write authority")
	}
	underlyingWriteReason := jsFunctionBlock(t, js, "underlyingWriteReason")
	if !strings.Contains(underlyingWriteReason, "Purge/restore submission is unavailable") {
		t.Fatal("underlying purge/restore copy must disclose the unconditional capability blocker")
	}
	cancelGate := jsFunctionBlock(t, js, "orderCancelGate")
	if strings.Contains(cancelGate, "can_write") {
		t.Fatalf("orderCancelGate must not gate directly on can_write; tradingCancelAllowed mirrors cancel policy")
	}
	if !strings.Contains(cancelGate, "tradingCancelAllowed(trading)") {
		t.Fatalf("orderCancelGate must use tradingCancelAllowed for freeze-aware cancel policy")
	}
	for _, want := range []string{"trading.mode", "trading.account"} {
		if !strings.Contains(cancelGate, want) {
			t.Fatalf("orderCancelGate must require %s for broker confirmation", want)
		}
	}
	cancelAllowed := jsFunctionBlock(t, js, "tradingCancelAllowed")
	for _, want := range []string{"trading.can_write", "write_blockers", "trading_frozen"} {
		if !strings.Contains(cancelAllowed, want) {
			t.Fatalf("tradingCancelAllowed must include %s in cancel readiness policy", want)
		}
	}
	for _, old := range []string{"local_gate", "can_transmit", "can_modify", "can_cancel", "preview_required"} {
		if strings.Contains(js, old) {
			t.Fatalf("app.js still references removed trading field %q", old)
		}
	}
}

func TestAppJSQuoteErrorsRespectSnapshotMarketSession(t *testing.T) {
	t.Parallel()
	js := embeddedSPASource(t)
	render := jsFunctionBlock(t, js, "renderMarketContext")
	if !strings.Contains(render, "snap.market_calendar") {
		t.Fatalf("renderMarketContext must thread the snapshot market calendar into quote tiles:\n%s", render)
	}
	closed := jsFunctionBlock(t, js, "marketQuoteSessionClosed")
	for _, want := range []string{"Boolean(sessionState)", "session.is_open === false", `session?.state || ""`, `sessionState !== "unknown"`} {
		if !strings.Contains(closed, want) {
			t.Errorf("marketQuoteSessionClosed missing contract %q", want)
		}
	}
	cell := jsFunctionBlock(t, js, "marketQuoteCell")
	for _, want := range []string{`closed ? "Closed" : marketQuoteInterruptedLine`, `error && !closed`, `"Selected market session is closed"`} {
		if !strings.Contains(cell, want) {
			t.Errorf("marketQuoteCell missing closed-session quote contract %q", want)
		}
	}
	interrupted := jsFunctionBlock(t, js, "marketQuoteInterruptedLine")
	if !strings.Contains(interrupted, `"Feed issue"`) {
		t.Fatal("open-session quote errors must retain the Feed issue fallback")
	}
}

func TestAppJSSnapshotBannerClaimsLastGoodOnlyWhenPresent(t *testing.T) {
	t.Parallel()
	js := embeddedSPASource(t)
	summary := jsFunctionBlock(t, js, "snapshotIssueSummary")
	for _, want := range []string{
		`["account", "positions"]`,
		`!snapshotPayloadPresent(snap, err.source)`,
		`Account and positions unavailable.`,
		`if (missingSources.length > 0)`,
		`feed interrupted; showing last good snapshot.`,
	} {
		if !strings.Contains(summary, want) {
			t.Errorf("snapshotIssueSummary missing last-good honesty contract %q", want)
		}
	}
	present := jsFunctionBlock(t, js, "snapshotPayloadPresent")
	for _, want := range []string{`sourceKey === "calendar" ? "market_calendar" : sourceKey`, "Object.keys(payload).length > 0"} {
		if !strings.Contains(present, want) {
			t.Errorf("snapshotPayloadPresent missing retained-payload contract %q", want)
		}
	}
	portfolioUnavailable := strings.Index(summary, `if (unavailablePortfolio.length > 0)`)
	gateway := strings.Index(summary, `const gateway = gatewayIssueText(snap)`)
	if portfolioUnavailable < 0 || gateway < portfolioUnavailable {
		t.Fatalf("cold account/positions unavailability must take precedence over gateway copy:\n%s", summary)
	}
}

func TestAppJSConfirmInputsUsesTraderSafeCopy(t *testing.T) {
	t.Parallel()
	js := embeddedSPASource(t)
	for _, want := range []string{
		`if (action === "confirm_inputs") return "Check data";`,
		"function stressSummaryText(stress, snap = {})",
		"before treating the stress read as a market signal",
		"no market-stress action",
		"function stressNeedsInputCheck(stress)",
		"function stressInputCheckBlocksAction(stress)",
		"function stressInputIssueSummary(stress, snap = {})",
	} {
		if !strings.Contains(js, want) {
			t.Fatalf("app.js missing confirm-inputs copy contract %q", want)
		}
	}
	if strings.Contains(js, `if (action === "confirm_inputs") return "Confirm";`) {
		t.Fatalf("app.js maps confirm_inputs to bare Confirm")
	}
}

func TestAppJSRegimeCardSeparatesDataGapsFromRegime(t *testing.T) {
	t.Parallel()
	js := embeddedSPASource(t)
	for _, want := range []string{
		`marketRegimeLabel(posture)`,
		"function regimePosture(snap = {}, stress = {}, market = {})",
		"function regimeWeatherClass(tone)",
		"function normalizeRegimePosture(candidate)",
		`snap.regime?.posture`,
		`market.regime_posture`,
		"function marketRegimeStatusLine(snap, stress, market, indicators)",
		"Paper gateway live quotes OK",
		"HYG 50-DMA",
		"USD/JPY baseline",
		"gamma cache",
	} {
		if !strings.Contains(js, want) {
			t.Fatalf("app.js missing regime data-gap contract %q", want)
		}
	}
	for _, forbidden := range []string{
		`if (redClusters > 0) return "red";`,
		`return "Risk-off";`,
	} {
		if strings.Contains(js, forbidden) {
			t.Fatalf("app.js still has UI-owned regime policy %q", forbidden)
		}
	}
}

func TestAppJSStressDetailUsesSourceBackedEvidenceRows(t *testing.T) {
	t.Parallel()
	js := embeddedSPASource(t)
	cssData, err := Files.ReadFile("styles.css")
	if err != nil {
		t.Fatalf("read styles.css: %v", err)
	}
	css := string(cssData)

	renderDetail := jsFunctionBlock(t, js, "renderStressDetail")
	for _, want := range []string{
		"stressDriverRows(stress)",
		"stressExplanationCards(stress, snap)",
	} {
		if !strings.Contains(renderDetail, want) {
			t.Fatalf("renderStressDetail missing stress evidence contract %q", want)
		}
	}
	if strings.Contains(renderDetail, "(stress.rows || []).slice(0, 3)") {
		t.Fatalf("renderStressDetail must not show the first three stress rows as drivers")
	}

	driverRows := jsFunctionBlock(t, js, "stressDriverRows")
	for _, want := range []string{
		`cleanDetail(row.title).toLowerCase() !== "portfolio stress"`,
		"stressRowNeedsAttention",
		"stressDriverPriority",
		".slice(0, 5)",
	} {
		if !strings.Contains(driverRows, want) {
			t.Fatalf("stressDriverRows missing source-backed driver contract %q", want)
		}
	}
	if strings.Contains(driverRows, ".slice(0, 3)") {
		t.Fatalf("stressDriverRows must not cap evidence at the old first-three rows")
	}

	for _, want := range []string{
		"function sourceHealthMentions(source, needle)",
		"Early market warning, not confirmed yet",
		"market-event sources",
	} {
		if !strings.Contains(js, want) {
			t.Fatalf("app.js missing stress clarity contract %q", want)
		}
	}
	// renderStressActions (the "Review blockers"/"Held actions"/"Alerts"
	// quick-action row) and the standalone Readiness explanation card
	// ("Prestage only" etc.) were deliberately removed as noise: the first
	// was self-referential or duplicated top-level navigation already one
	// tap away, the second had no unique signal a risk-conscious trader
	// couldn't already get from the Market/Portfolio cards.
	for _, forbidden := range []string{
		"function renderStressActions(stress)",
		"Prestage only",
	} {
		if strings.Contains(js, forbidden) {
			t.Fatalf("app.js should not reintroduce removed stress clutter %q", forbidden)
		}
	}
	if strings.Contains(css, ".stress-hero p {") {
		t.Fatalf("styles.css must not clamp every paragraph inside the expanded stress panel")
	}
	if !strings.Contains(css, ".stress-hero__copy p") || !strings.Contains(css, ".detail-panel--dark .driver-row.warn") {
		t.Fatalf("styles.css missing scoped stress summary/detail styling")
	}
}

func TestAppMobileDashboardContracts(t *testing.T) {
	t.Parallel()
	js := embeddedSPASource(t)
	htmlData, err := Files.ReadFile("index.html")
	if err != nil {
		t.Fatalf("read index.html: %v", err)
	}
	cssData, err := Files.ReadFile("styles.css")
	if err != nil {
		t.Fatalf("read styles.css: %v", err)
	}
	html := string(htmlData)
	css := string(cssData)

	for _, want := range []string{
		`const symbols = ["SPY", "VIX", "QQQ"];`,
		"function handleExpandablePanelTap(event, which)",
		`$("regimeSummaryCard").addEventListener("click"`,
		`$("stressHero").addEventListener("click"`,
		`"trading", "auto_trade", "proposals", "opportunities", "settings", "regime", "stress"`,
		"function setupLiveRefreshLoop()",
		"function setupBottomTabs()",
		"function renderTabs()",
		"function resetViewportScroll()",
		`window.addEventListener("resize", resetViewportScroll)`,
		"function renderSettings()",
		"function setPurgeRestoreEnabled(enabled)",
		"function purgeRestoreSettingEnabled()",
		"function setStockProtectionEnabled(enabled)",
		"function stockProtectionSettingEnabled()",
		"function protectionMetricText(proposal = {})",
		"function protectionRiskTicket(proposal = {}, metricText = \"\")",
		"function protectionCoverageFromPositions(snap = state.snapshot || {})",
		"function stressProtectionCoverageFor(snap = state.snapshot || {}, stress = snap.stress || {})",
		"function protectionCoverageDetailFact(coverage = null, baseCurrency = \"\")",
		"function protectionCoverageStressLine(stress = {}, snap = state.snapshot || {})",
		"function protectionRiskExcessSummary(counts = {})",
		"compactWholeMoney(counts.risk_reduction_excess_notional, riskExcessCurrency)",
		"counts.risk_reduction_excess_notional_base",
		"counts.theta_per_day_base",
		"compactWholeMoney(proposal.risk_excess_notional, proposal.risk_excess_currency || \"\")",
		"function compactWholeMoney(value, currency)",
		"function protectionQuoteLine(proposal = {})",
		"function protectionQuantityStepper(proposal = {})",
		"function protectionQuantityStepDelta(current = 0, direction = 1)",
		"function nudgeProtectionQuantity(proposal = {}, direction = 1)",
		"function protectionEffectiveQuantity(proposal = {})",
		"function protectionLiveTrailStop(proposal = {}, trail = {})",
		"function protectionSubmitLabel(proposal = {})",
		"function protectionUsesPreviewFlow(proposal = {})",
		"function protectionNeedsSnapshotSync(proposals = {}, autoTrade = {})",
		"function protectionVisibleRows(rows = [], marketEvents = {})",
		"existing_protective_order",
		"No protection proposals requiring action.",
		"function queueProtectionSnapshotSync()",
		"function syncProtectionSnapshot()",
		"function applyProtectionSnapshot(proposals = {})",
		"trading: proposals.trading",
		`fetch("/api/proposals", { credentials: "include", cache: "no-store" })`,
		"function renderOpportunitiesPanel(opportunities = {})",
		"function opportunityMetricRow(opportunity = {})",
		"function opportunityPostExerciseRiskMetrics(opportunity = {})",
		"function opportunityPostExerciseRiskChangeLabel(risk = {})",
		"function opportunityPreviewGate(opportunity = {})",
		"function opportunitySubmitGate(opportunity = {}, previewResult = null)",
		"function previewOpportunityExercise(opportunity)",
		"function submitOpportunityExercise(opportunity)",
		"function refreshOpportunities()",
		"function applyOpportunitySnapshot(opportunities = {})",
		`fetch("/api/opportunities", { credentials: "include", cache: "no-store" })`,
		`fetch("/api/opportunities/preview-exercise"`,
		`fetch("/api/opportunities/ignore"`,
		`fetch("/api/opportunities/refresh"`,
		"Exercise submission unavailable",
		"exact option-to-underlying risk policy and durable one-shot authority are not approved",
		"function protectionPreviewGate(proposal = {})",
		"function protectionPreviewSubmitGate(proposal = {}, previewResult = null)",
		"function protectionWriteUnavailableReason(trading = {})",
		"function protectionPreviewStateKey(proposal = {})",
		"function protectionPreviewText(result = null, proposal = {})",
		"function protectionPreviewOutcomeLabel(",
		"function protectionPreviewSubmitEligible(result = {})",
		"function protectionPreviewSubmitBlockedReason(result = {})",
		"function protectionWhatIfDetails(whatIf = {})",
		"function protectionSubmitStateText(",
		"function protectionSubmitResultText(result = {})",
		"function protectionSubmitButtonTitle(",
		"function protectionWriteConfirmation(proposal = {})",
		"function protectionWriteConfirmationLabel()",
		"function protectionStopDraftSummary(proposal = {})",
		"function shortPreviewMessage(message = \"\")",
		"function protectionPreviewTimeoutMs(proposal = {})",
		"function previewProtectionProposal(proposal)",
		"protection-row__blocker",
		"Order draft ready; broker WhatIf running",
		`fetch("/api/proposals/preview"`,
		`fetch("/api/proposals/submit"`,
		"timeout_ms: protectionPreviewTimeoutMs(proposal)",
		`fast_path: proposal.bucket === "trailing_stop"`,
		"confirm_account: confirmation.account",
		"confirm_mode: confirmation.mode",
		"Broker WhatIf accepted; no order placed",
		"Submit stop",
		"confirm_account: confirmation.account",
		"confirm_mode: confirmation.mode",
		"confirm_account: modifyConfirmation.account",
		"confirm_mode: modifyConfirmation.mode",
		"confirm_account: cancelConfirmation.account",
		"confirm_mode: cancelConfirmation.mode",
		"Submit blocked",
		"write_blockers",
		"Broker preview is not enabled by trading.status",
		"function protectionSideLabel(proposal = {})",
		"Buy to cover stop",
		"function protectionInferredReference(proposal = {}, trail = {}, action = \"\")",
		"function protectionEffectiveBlockers(proposal = {}, events = {})",
		"function protectionMarketEventBlocker(proposal = {}, events = {})",
		"function protectionMarketCalendar(proposal = {})",
		"function proposalMarketKey(proposal = {})",
		"function protectionQuoteStatusLabel(quote = null)",
		"function protectionMarketStateHint(proposal = {})",
		"broker WhatIf remains the submit authority",
		"broker may queue after fresh WhatIf",
		// The broker-managed-stop mechanics moved from per-row reason
		// boilerplate into the action-button title (2026-06-12 noise
		// reduction); the contract is that the explanation still ships.
		"IBKR maintains the stop and raises it as the instrument price rises above the submission reference",
		`body: JSON.stringify({ features: { stock_protection: { enabled } } })`,
		"function refreshBootstrapIfSSEUnavailable()",
		"function renderAccountDailyPnlPct(account = {})",
		"function accountDailyPnlPct(account = {})",
		"function setUnderlyingExpansion(open)",
		"function renderUnderlyingExpansion()",
		"function handleUnderlyingPanelTap(event)",
		"function underlyingHeldDailyPnlTotals(rows, baseCurrency)",
		"function compareUnderlyingRows(a, b)",
		"function heldUnderlyingChange(group, quote, price)",
		"function heldUnderlyingDailyPnl(group, baseCurrency, currency)",
		"function quoteChange(quote)",
		"function signedDisplayMoney(value, currency)",
		"const pnl = heldUnderlyingDailyPnl(group, baseCurrency, currency);",
		`source: "daily P/L"`,
		`group.group_daily_pnl_base`,
		"function marketQuoteChangeClass(symbol, change)",
		"function handlePortfolioPanelTap(event)",
		"function setPortfolioExpansion(open)",
		"function portfolioDeltaPosture(portfolio = {}, account = {})",
		"function regimePostureDetailTone(posture = {})",
		`setupBottomTabs();`,
		`tabs.addEventListener("pointerup", activate);`,
		`tabs.dataset.bound = "true";`,
		`$("underlyingDetailToggle").addEventListener("click"`,
		`$("underlyingPanel").addEventListener("click", handleUnderlyingPanelTap);`,
		`$("portfolioPanel").addEventListener("click", handlePortfolioPanelTap);`,
		"change: heldUnderlyingChange(group, quote, price.value)",
		"function gatewayIssueText(snap = {})",
		"snap.status?.last_error",
		"client id .*already in use",
	} {
		if !strings.Contains(js, want) {
			t.Fatalf("app.js missing mobile dashboard contract %q", want)
		}
	}
	for _, want := range []string{
		`id="bannerStack"`,
		`id="appScroll"`,
		`id="bottomTabs"`,
		`data-tab="monitor"`,
		`data-tab="alerts"`,
		`data-tab="settings"`,
		`id="accountPanel"`,
		`id="dailyPnlPct"`,
		`id="underlyingPanel" data-open="false"`,
		`id="underlyingDetailToggle"`,
		`Winner daily P/L`,
		`id="underlyingWinnerPnl"`,
		`Loser daily P/L`,
		`id="underlyingLoserPnl"`,
		`Purge all`,
		`Restore all`,
		`Rebuild all`,
		`id="underlyingBookListPanel" hidden`,
		`id="portfolioPanel" data-open="false"`,
		`Delta posture`,
		`id="portfolioDeltaMeaning"`,
		`id="alertsTab" data-tab-panel="alerts"`,
		`id="settingsTab" data-tab-panel="settings"`,
		`id="purgeRestoreToggle"`,
		`id="stockProtectionToggle"`,
		`id="settingsTradingLimits"`,
		`id="settingsMarketDataStatus"`,
		`id="opportunitiesPanel" data-open="false"`,
		`id="opportunitiesToggle"`,
		`id="opportunitiesCount"`,
		`id="opportunitiesExpectedGain"`,
		`id="opportunitiesRefreshButton"`,
		`id="opportunitiesDetailPanel" hidden`,
		`id="opportunitiesRows"`,
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("index.html missing mobile dashboard contract %q", want)
		}
	}
	if strings.Index(html, `id="bannerStack"`) > strings.Index(html, `id="accountPanel"`) {
		t.Fatalf("snapshot banner should render above account panel")
	}
	if strings.Contains(html, `<details class="panel underlying-panel"`) {
		t.Fatalf("underlyings panel should not hide summary/actions inside native details")
	}
	if strings.Contains(js, "Disabled while") {
		t.Fatalf("protection submit gate should not hard-block paper broker stops only because the market calendar is closed")
	}
	for _, want := range []string{
		".source-banner",
		"background: var(--red);",
		"color: #fff;",
		`.underlying-panel[data-open="true"] .panel-chevron::after`,
		".underlying-pnl-summary",
		".underlying-pnl-card--winner",
		".underlying-pnl-card--loser",
		".underlying-row__metric--change",
		"touch-action: manipulation;",
		".underlying-book__list-panel",
		".account-pnl-pct",
		".portfolio-delta-posture",
		".portfolio-panel .panel-chevron",
		".portfolio-detail-panel",
		".app-scroll",
		"overflow-y: auto;",
		"overscroll-behavior: contain;",
		".bottom-tabs",
		"--bottom-tabs-space: 92px;",
		"padding-bottom: calc(var(--bottom-tabs-space) + var(--bottom-tab-safe));",
		".bottom-tabs {\n  position: absolute;",
		// Panel Dark seats the bar flush on the bottom edge, so the
		// safe-area inset moved from `bottom:` into the bar's own padding.
		"padding: 6px 4px calc(8px + var(--bottom-tab-safe));",
		"transform: translateX(-50%);",
		"--bottom-tab-safe: 0px;",
		"@media (display-mode: standalone), (display-mode: fullscreen)",
		"--bottom-tab-safe: env(safe-area-inset-bottom);",
		".bottom-tab.active",
		".settings-panel",
		".toggle-switch input:checked + span",
		".protection-row:first-child",
		".protection-row__trail",
		".protection-row__trail--fallback",
		".protection-row__risk-ticket",
		".protection-preview",
		".opportunities-panel",
		".opportunities-summary",
		".opportunity-row",
		".opportunity-row__metric--gain",
		".opportunity-row__metric--risk",
		".opportunity-preview",
	} {
		if !strings.Contains(css, want) {
			t.Fatalf("styles.css missing mobile dashboard contract %q", want)
		}
	}
	if strings.Contains(js, `fetch("/api/opportunities/exercise"`) {
		t.Fatal("Canary must not expose the typed-disabled option-exercise submit transport")
	}
	if strings.Contains(css, ".bottom-tabs {\n  position: fixed;") {
		t.Fatalf("bottom tabs must be pinned by shell layout, not fixed to the browser viewport")
	}
}

func TestAppJSRendersTrailSizingFallback(t *testing.T) {
	t.Parallel()
	js := embeddedSPASource(t)
	for _, want := range []string{
		"protectionTrailSizingLabel(proposal.trail_sizing)",
		"protectionTrailSizingFallback(proposal)",
		"fallback trail used",
		"dynamic stop unavailable",
	} {
		if !strings.Contains(js, want) {
			t.Fatalf("app.js missing trail sizing UX contract %q", want)
		}
	}
}

// TestAppJSLiveWritesCarryNoTypedConfirmation pins the live-gate
// simplification of 2026-06-11: the SPA must not prompt for, hard-code, or
// forward the removed typed "live/<account>" phrase, and the arm/confirm
// double-click is gone. Live writes ride on the preview token, the
// server-validated confirm_account/confirm_mode fields, and the daemon's
// origin policy.
func TestAppJSLiveWritesCarryNoTypedConfirmation(t *testing.T) {
	t.Parallel()
	js := embeddedSPASource(t)
	for _, banned := range []string{
		"live_confirmation",
		"liveWriteConfirmation",
		"protectionConfirmKey",
		"modifyConfirmationText",
		"cancelConfirmationText",
	} {
		if strings.Contains(js, banned) {
			t.Fatalf("app.js must not reference removed live-confirmation surface %q", banned)
		}
	}
	// Purge/restore and exercise submission are typed-disabled, so Canary has
	// no broker-write prompt for either capability. No other prompt may exist.
	if got := strings.Count(js, "window.prompt"); got != 0 {
		t.Fatalf("app.js window.prompt count = %d, want zero", got)
	}
	if strings.Contains(js, "window.confirm") {
		t.Fatalf("app.js must not use window.confirm; broker writes confirm via single-click buttons")
	}
}

func TestAppJSProtectionQuantityStepperAcceleratesAtTenBoundary(t *testing.T) {
	t.Parallel()
	js := embeddedSPASource(t)
	stepper := jsFunctionBlock(t, js, "protectionQuantityStepper")
	for _, want := range []string{
		`nudgeProtectionQuantity(proposal, -1)`,
		`nudgeProtectionQuantity(proposal, 1)`,
		`dec.setAttribute("aria-label", "Decrease sell size")`,
		`inc.setAttribute("aria-label", "Increase sell size")`,
	} {
		if !strings.Contains(stepper, want) {
			t.Fatalf("protectionQuantityStepper missing accelerated-step wiring %q", want)
		}
	}
	delta := jsFunctionBlock(t, js, "protectionQuantityStepDelta")
	for _, want := range []string{
		"const protectionQuantityAcceleratedStep = 10",
		"qty < protectionQuantityAcceleratedStep",
		"qty <= protectionQuantityAcceleratedStep && dir < 0",
		"qty % protectionQuantityAcceleratedStep !== 0",
		"return dir * protectionQuantityAcceleratedStep",
	} {
		if !strings.Contains(js, want) && !strings.Contains(delta, want) {
			t.Fatalf("protectionQuantityStepDelta missing boundary logic %q", want)
		}
	}
	nudge := jsFunctionBlock(t, js, "nudgeProtectionQuantity")
	for _, want := range []string{
		"const current = protectionEffectiveQuantity(proposal)",
		"setProtectionQuantity(proposal, current + protectionQuantityStepDelta(current, direction))",
	} {
		if !strings.Contains(nudge, want) {
			t.Fatalf("nudgeProtectionQuantity must read current quantity at click time; missing %q", want)
		}
	}
}

func TestUnderlyingWinnerLoserTotalsUseDailyPnl(t *testing.T) {
	t.Parallel()
	js := embeddedSPASource(t)
	for _, name := range []string{
		"underlyingHeldDailyPnlTotals",
		"heldUnderlyingDailyPnl",
	} {
		body := jsFunctionBlock(t, js, name)
		lower := strings.ToLower(body)
		if !strings.Contains(lower, "daily") {
			t.Fatalf("%s must aggregate daily P/L", name)
		}
		for _, forbidden := range []string{"group_unrealized_pnl", "unrealized_pnl_base"} {
			if strings.Contains(lower, forbidden) {
				t.Fatalf("%s must aggregate daily P/L, not open/unrealized field %q", name, forbidden)
			}
		}
	}
	if strings.Contains(js, "heldUnderlyingQuoteMarkedPnl") || strings.Contains(js, "heldUnderlyingQuotePnlAdjustment") {
		t.Fatalf("underlying winner/loser totals must use broker daily P/L, not client quote-marked estimates")
	}
}

func TestAppJSAccountPrivacyMasksUnderlyingPnl(t *testing.T) {
	t.Parallel()
	js := embeddedSPASource(t)
	for _, want := range []string{
		"function setAccountValueVisible(visible)",
		"function syncAccountPrivacyState()",
		"function sensitiveDisplayMoney(value, currency)",
		"function sensitiveMoneyHidden(value)",
		"function privacyMask()",
		`window.addEventListener("storage"`,
	} {
		if !strings.Contains(js, want) {
			t.Fatalf("app.js missing account privacy contract %q", want)
		}
	}
	summary := jsFunctionBlock(t, js, "setUnderlyingSummaryPnl")
	for _, want := range []string{
		"if (sensitiveMoneyHidden(value))",
		"el.textContent = privacyMask();",
	} {
		if !strings.Contains(summary, want) {
			t.Fatalf("setUnderlyingSummaryPnl must use privacy helper %q", want)
		}
	}
	row := jsFunctionBlock(t, js, "underlyingBookRow")
	for _, want := range []string{
		`pnlValue.className = sensitiveMoneyHidden(row.pnl) ? "is-private" : signedClass(row.pnl);`,
		`pnlValue.textContent = sensitiveDisplayMoney(row.pnl, row.pnlCurrency || baseCurrency);`,
	} {
		if !strings.Contains(row, want) {
			t.Fatalf("underlyingBookRow must use privacy helper %q", want)
		}
	}
}

func TestAppJSRendersBorrowFeeMarketEvent(t *testing.T) {
	t.Parallel()
	js := embeddedSPASource(t)
	for _, want := range []string{
		`case "borrow_fee_extreme": return "Fee extreme";`,
		"function marketFlagChip(flag = {}, options = {})",
		"function marketEventTone(flag = {})",
		`if (severity === "act" || severity === "watch") return "friction";`,
		"function marketEventTitle(flag = {})",
		"function marketEventFlagsForSymbol(symbol, events = {})",
		"function underlyingHeroMarketFlags(rows, events = {})",
		"function protectionHeroMarketFlags(rows = [], marketEvents = {})",
		"marketFlagRow(row.marketFlags || [])",
		"marketFlagRow(protectionDecisionFlags(proposal, marketEvents))",
		"function protectionDecisionFlags(proposal = {}, events = {})",
		`return tone === "hard" || tone === "friction";`,
		"function protectionActionLabel(proposal = {})",
		`return secType === "OPT" || secType === "OPTION" ? "Buy to close" : "Buy to cover";`,
		"function proposalIsBuyToCover(proposal = {})",
		"function protectionMetricText(proposal = {})",
		"function protectionStopChanged(snapshotStop, liveStop)",
		"function protectionQuoteFor(proposal = {})",
		"function protectionQuoteTickDir(key, price, at = \"\")",
	} {
		if !strings.Contains(js, want) {
			t.Fatalf("app.js missing borrow-fee market-event rendering contract %q", want)
		}
	}
}

func TestAppJSProtectionSummaryUsesDataDrivenRiskTones(t *testing.T) {
	t.Parallel()
	js := embeddedSPASource(t)
	htmlData, err := Files.ReadFile("index.html")
	if err != nil {
		t.Fatalf("read index.html: %v", err)
	}
	cssData, err := Files.ReadFile("styles.css")
	if err != nil {
		t.Fatalf("read styles.css: %v", err)
	}
	render := jsFunctionBlock(t, js, "renderProtectionPanel")
	for _, want := range []string{
		`setMetricTone(thetaEl, hasNumericValue(theta.value) && theta.value > 0 ? "alert" : "neutral")`,
		`setMetricTone(riskExcessEl, riskExcess.risk ? "risk" : "neutral")`,
		`money(theta.value, theta.currency)`,
		`const noStop = protectionNoStopExposureSummary(rows, marketEvents, currentProtectionCoverage());`,
		`$("protectionNoStopExposure")`,
	} {
		if !strings.Contains(render, want) {
			t.Fatalf("renderProtectionPanel missing protection summary contract %q", want)
		}
	}
	noStop := jsFunctionBlock(t, js, "protectionNoStopExposureSummary")
	for _, want := range []string{
		"protectionCoverageNoStopSummary(coverage)",
		"protectionVisibleRows(rows, marketEvents)",
		`proposal.bucket === "trailing_stop"`,
		"protectionProposalNotional(proposal)",
		"sum of visible trailing-stop proposal notionals",
	} {
		if !strings.Contains(noStop, want) {
			t.Fatalf("protectionNoStopExposureSummary missing row-backed contract %q", want)
		}
	}
	html := string(htmlData)
	for _, want := range []string{`id="protectionNoStopExposure"`, "No-stop exposure"} {
		if !strings.Contains(html, want) {
			t.Fatalf("index.html missing no-stop summary element %q", want)
		}
	}
	css := string(cssData)
	for _, want := range []string{".protection-summary b.metric-risk", ".protection-summary b.metric-alert", ".protection-summary b.metric-neutral", ".detail-fact.risk", ".protection-row__risk-ticket", ".protection-coverage-ledger", ".protection-row__ladder"} {
		if !strings.Contains(css, want) {
			t.Fatalf("styles.css missing protection metric tone rule %q", want)
		}
	}
	if strings.Contains(css, "#protectionTheta {\n  color: var(--red);") {
		t.Fatal("protection theta must not be unconditionally red")
	}
}

func TestAppJSRendersProtectionCoverageAndRiskTickets(t *testing.T) {
	t.Parallel()
	js := embeddedSPASource(t)

	currentCoverage := jsFunctionBlock(t, js, "currentProtectionCoverage")
	if !strings.Contains(currentCoverage, "protectionCoverageFromPositions(state.snapshot || {})") || strings.Contains(currentCoverage, "stress") {
		t.Fatalf("currentProtectionCoverage must use positions coverage only, got:\n%s", currentCoverage)
	}
	riskTicket := jsFunctionBlock(t, js, "protectionRiskTicketParts")
	for _, want := range []string{
		"proposal.execution_semantics",
		"proposal.stop_risk",
		"trigger ${trigger}",
		"est. loss ${loss}",
		"protectionStopRiskGapName",
		"protectionExecutionWarningLabel",
	} {
		if !strings.Contains(riskTicket, want) {
			t.Fatalf("protectionRiskTicketParts missing stop-risk contract %q", want)
		}
	}
	protectionRow := jsFunctionBlock(t, js, "protectionRow")
	for _, want := range []string{
		"protectionStopLadder(proposal)",
		"copy.append(ladder)",
	} {
		if !strings.Contains(protectionRow, want) {
			t.Fatalf("protectionRow missing inline stop ladder contract %q", want)
		}
	}
	ladder := jsFunctionBlock(t, js, "protectionStopLadder")
	for _, want := range []string{
		`proposal.bucket !== "trailing_stop"`,
		"proposal.stop_ladder",
		`aria-label", "Stop ladder comparison"`,
		"protectionStopLadderDisplaySteps",
	} {
		if !strings.Contains(ladder, want) {
			t.Fatalf("protectionStopLadder missing inline ladder contract %q", want)
		}
	}
	warnings := jsFunctionBlock(t, js, "protectionExecutionWarningLabel")
	for _, want := range []string{
		`stop_limit_can_leave_position_unfilled`,
		`stop_price_is_not_execution_price`,
		`limit may not fill`,
		`trigger becomes market`,
	} {
		if !strings.Contains(warnings, want) {
			t.Fatalf("protectionExecutionWarningLabel missing Schwab-style warning %q", want)
		}
	}
	detailRows := jsFunctionBlock(t, js, "portfolioDetailRows")
	for _, want := range []string{
		"protectionCoverageDetailFact(positions.protection_coverage, baseCurrency)",
		"rows.push(coverageFact)",
	} {
		if !strings.Contains(detailRows, want) {
			t.Fatalf("portfolioDetailRows missing coverage fact contract %q", want)
		}
	}
	coverageBody := jsFunctionBlock(t, js, "protectionCoverageDetailBody")
	for _, want := range []string{
		"Largest unprotected:",
		"Stale protective orders:",
		"Coverage ledger compares stock/ETF positions to observed open stop orders.",
	} {
		if !strings.Contains(coverageBody, want) {
			t.Fatalf("protectionCoverageDetailBody missing coverage wording %q", want)
		}
	}
	coverageLedger := jsFunctionBlock(t, js, "protectionCoverageLedger")
	for _, want := range []string{
		"protectionCoverageDisplayRows(coverage)",
		`aria-label", "Per-underlying protection coverage"`,
		"protectionCoverageQuantityText(row)",
		"protectionCoverageNotionalText(row, baseCurrency",
		"protectionCoverageOrderText(row)",
	} {
		if !strings.Contains(coverageLedger, want) {
			t.Fatalf("protectionCoverageLedger missing per-underlying contract %q", want)
		}
	}
	detailFact := jsFunctionBlock(t, js, "detailFact")
	if !strings.Contains(detailFact, "fact.detail instanceof Node") {
		t.Fatalf("detailFact must append structured protection coverage detail")
	}
	portfolioExplanation := jsFunctionBlock(t, js, "portfolioExplanation")
	if !strings.Contains(portfolioExplanation, "protectionCoverageStressLine(stress, snap)") {
		t.Fatalf("portfolioExplanation must include stress protection coverage context")
	}
	stressLine := jsFunctionBlock(t, js, "protectionCoverageStressLine")
	for _, want := range []string{
		"stressProtectionCoverageFor(snap, stress)",
		"Protection coverage: ${headline}",
		"largest unprotected",
		"protectionCoverageStaleText(coverage)",
	} {
		if !strings.Contains(stressLine, want) {
			t.Fatalf("protectionCoverageStressLine missing stress coverage contract %q", want)
		}
	}
}

func TestAppJSProtectionFastPathKeepsHardMarketEventBlocker(t *testing.T) {
	t.Parallel()
	js := embeddedSPASource(t)
	effectiveBlockers := jsFunctionBlock(t, js, "protectionEffectiveBlockers")
	if !strings.Contains(effectiveBlockers, "protectionMarketEventBlocker(proposal, events)") {
		t.Fatalf("protectionEffectiveBlockers must include current market-event blockers")
	}
	eventBlocker := jsFunctionBlock(t, js, "protectionMarketEventBlocker")
	for _, want := range []string{
		`status !== "active"`,
		`id === "halt_regulatory_or_news"`,
		`id === "luld_pause"`,
		`flag.role === "hard_blocker"`,
		`flag.severity === "block"`,
		"`market_event_${id || \"blocker\"}`",
	} {
		if !strings.Contains(eventBlocker, want) {
			t.Fatalf("protectionMarketEventBlocker missing hard-block contract %q", want)
		}
	}
	for _, name := range []string{"protectionPreviewGate", "protectionSubmitGate"} {
		body := jsFunctionBlock(t, js, name)
		if !strings.Contains(body, "protectionEffectiveBlockers(proposal, state.snapshot?.market_events || {})") {
			t.Fatalf("%s must gate against current market events", name)
		}
	}
}

// Money formatters must never invent a currency: an amount whose currency is
// genuinely unknown renders bare (no symbol), never with a coerced USD label.
// This is the static twin of the 2026-07-02 money-flap fix — a EUR-base
// account once showed "$729.87" next to "€12K" because money(value, "")
// defaulted to USD.
func TestAppJSMoneyFormattersNeverDefaultToUSD(t *testing.T) {
	t.Parallel()
	js := embeddedSPASource(t)
	if strings.Contains(js, `"USD"`) {
		t.Fatalf(`app.js hardcodes a "USD" currency literal; thread the real currency or render the amount bare`)
	}
	moneyFn := jsFunctionBlock(t, js, "money")
	for _, want := range []string{
		"const ccy = normalizeCurrency(currency);",
		"return ccy ? `${amount} ${ccy}` : amount;",
	} {
		if !strings.Contains(moneyFn, want) {
			t.Fatalf("money() missing bare-render contract %q", want)
		}
	}
	for _, want := range []string{
		// Formatter-level guards.
		"function protectionLossCurrency(usedBase, risk = {})",
		// [trading].max_notional is defined in the account currency.
		`money(notional, state.snapshot?.account?.base_currency || "")`,
		// Sweep risk cuts are base-currency dollar-delta, not contract currency.
		`deriskLegRow(leg, Boolean(d.submitted), res.base_currency || "")`,
		"money(leg.risk_contribution_cut, baseCurrency)",
		// Base-converted day-change money must not inherit the contract currency.
		`signedMoneyRead(dayMoney, proposal.position_day_change_currency || "")`,
	} {
		if !strings.Contains(js, want) {
			t.Fatalf("app.js missing currency-threading contract %q", want)
		}
	}
}

func TestRegimeAuthorityHealthControlsVisibleDataQualityPosture(t *testing.T) {
	t.Parallel()
	js := embeddedSPASource(t)
	render := jsFunctionBlock(t, js, "renderRegimePanel")
	for _, want := range []string{
		`regimeAuthorityLabel(posture, authority)`,
		`regimePresentationPosture(posture, authority)`,
		`renderRegimeAuthorityTimestamp(snap, latestRegimeTimestamp(stress, indicators))`,
	} {
		if !strings.Contains(render, want) {
			t.Fatalf("renderRegimePanel missing authority-health presentation contract %q", want)
		}
	}
	view := jsFunctionBlock(t, js, "regimeAuthorityView")
	for _, want := range []string{
		`snap.regime?.authority_health`,
		`snap.sources?.regime`,
		`authorityStatus === "unavailable"`,
		`authorityStatus === "stale"`,
		`sourceState === "stale"`,
	} {
		if !strings.Contains(view, want) {
			t.Fatalf("regimeAuthorityView missing typed health contract %q", want)
		}
	}
	label := jsFunctionBlock(t, js, "regimeAuthorityLabel")
	if !strings.Contains(label, "Last known · ${canonical}") {
		t.Fatal("degraded Regime label must qualify the retained canonical verdict")
	}
	presentation := jsFunctionBlock(t, js, "regimePresentationPosture")
	if !strings.Contains(presentation, `tone: "data_quality"`) {
		t.Fatal("degraded Regime authority must render with data-quality tone")
	}
	timestamp := jsFunctionBlock(t, js, "renderRegimeAuthorityTimestamp")
	for _, want := range []string{`if (!authority.degraded) return;`, `el.hidden = false`, `el.classList.add("stale")`} {
		if !strings.Contains(timestamp, want) {
			t.Fatalf("Regime authority timestamp must surface typed degradation immediately: missing %q", want)
		}
	}
}

func TestActiveAlertInboxIsTheSoleRenderedAuthority(t *testing.T) {
	t.Parallel()
	modules := embeddedSPAModuleSources(t)
	alertJS := modules["alert-inbox.js"]
	lifecycle := modules["lifecycle.js"]
	app := modules["app.js"]
	serviceWorkerData, err := Files.ReadFile("service-worker.js")
	if err != nil {
		t.Fatalf("read service-worker.js: %v", err)
	}
	serviceWorker := string(serviceWorkerData)
	htmlData, err := Files.ReadFile("index.html")
	if err != nil {
		t.Fatalf("read index.html: %v", err)
	}
	cssData, err := Files.ReadFile("styles.css")
	if err != nil {
		t.Fatalf("read styles.css: %v", err)
	}
	html, css := string(htmlData), string(cssData)
	for _, want := range []string{
		`id="alertAuthorityState"`,
		`id="alertCoverageSummary"`,
		`id="alertSourceList"`,
		`id="alertDeliveryHealth"`,
		`id="alertDeliveryAcceptance"`,
		`id="currentSignalList"`,
		`id="alertHistoryList"`,
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("active alert inbox missing %q", want)
		}
	}
	for _, want := range []string{".alert-authority", ".alert-source-row", ".alert-authority__delivery"} {
		if !strings.Contains(css, want) {
			t.Fatalf("styles.css missing active alert style %q", want)
		}
	}
	for _, want := range []string{
		`const ALERT_SCHEMA = "alerts-v1"`,
		`const ALERT_VERSION = "alert-delivery-v4"`,
		`"title", "body"`,
		`source.status`,
		`source.reason`,
		`last_push_service_acceptance_at`,
		`does not prove the phone displayed it or that it was read`,
		`value.coverage.state !== "complete"`,
		`value.coverage.freshness !== "current"`,
		`now > Date.parse(source.fresh_until)`,
		`state.renderedAlertAttention`,
		`fetch("/api/alerts/attention"`,
		`fetch("/api/alerts/attention/read"`,
	} {
		if !strings.Contains(alertJS, want) {
			t.Fatalf("active alert module missing %q", want)
		}
	}
	for _, want := range []string{
		`import { handleAttentionContextChange, ingestAlerts, ingestAlertsEvent, renderAlerts, renderSelectedAlert, scheduleAlertsRefresh } from "./alert-inbox.js";`,
		`ingestAlerts(data.alerts);`,
		`type === "alerts"`,
		`ingestAlertsEvent(event.data);`,
		`scheduleAlertsRefresh({ delayMs: 500, ensureTrailing: true })`,
	} {
		if !strings.Contains(lifecycle, want) {
			t.Fatalf("lifecycle missing active alert wiring %q", want)
		}
	}
	if !strings.Contains(app, `from "./alert-inbox.js"`) || !strings.Contains(app, "renderAlerts();") {
		t.Fatal("renderAll must use the active alert inbox")
	}
	for _, want := range []string{`/api/alerts/attention`, `payload.display_id`, `notificationRoutes`} {
		if !strings.Contains(serviceWorker, want) {
			t.Fatalf("service worker missing active alert contract %q", want)
		}
	}
	for _, forbidden := range []string{
		"alert-inbox-v2", "alert_inbox_v2", "/api/attention", "clearAlertsButton",
		"dismissCurrentButton", "previousContextList", "shadow-advisory",
	} {
		for name, source := range map[string]string{
			"html": html, "css": css, "active alert module": alertJS,
			"lifecycle": lifecycle, "app": app, "service worker": serviceWorker,
		} {
			if strings.Contains(source, forbidden) {
				t.Fatalf("%s retains removed alert contract %q", name, forbidden)
			}
		}
	}
	if strings.Contains(serviceWorker, "payload.alert_id") {
		t.Fatal("service worker must not use a legacy alert id as notification identity")
	}
}

func embeddedSPASource(t *testing.T) string {
	t.Helper()
	modules := embeddedSPAModuleSources(t)
	var source strings.Builder
	for _, name := range EmbeddedJavaScriptFileNames() {
		module, ok := modules[name]
		if !ok {
			continue
		}
		fmt.Fprintf(&source, "\n// module: %s\n%s", name, module)
	}
	return source.String()
}

func embeddedSPAModuleSources(t *testing.T) map[string]string {
	t.Helper()
	modules := make(map[string]string)
	for _, name := range EmbeddedJavaScriptFileNames() {
		if name == "service-worker.js" {
			continue
		}
		data, err := Files.ReadFile(name)
		if err != nil {
			t.Fatalf("read embedded SPA module %s: %v", name, err)
		}
		modules[name] = string(data)
	}
	if len(modules) == 0 {
		t.Fatal("embedded SPA contains no modules")
	}
	return modules
}

func jsFunctionBlock(t *testing.T, _ string, name string) string {
	t.Helper()
	marker := "function " + name + "("
	var filename, js string
	for candidate, source := range embeddedSPAModuleSources(t) {
		if !strings.Contains(source, marker) {
			continue
		}
		if filename != "" {
			t.Fatalf("SPA function %s appears in both %s and %s", name, filename, candidate)
		}
		filename, js = candidate, source
	}
	if filename == "" {
		t.Fatalf("SPA modules missing function %s", name)
	}
	start := strings.Index(js, marker)
	end := len(js)
	for _, nextMarker := range []string{"\nfunction ", "\nasync function ", "\nconst ", "\nlet ", "\nvar ", "\nexport {"} {
		if next := strings.Index(js[start+1:], nextMarker); next >= 0 {
			end = min(end, start+1+next)
		}
	}
	if end == len(js) {
		return js[start:]
	}
	return js[start:end]
}
