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
		`"ibkrDeviceKeyJWK"`,
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
		`id="underlyingBookListPanel" hidden`,
		`id="portfolioPanel" data-open="false"`,
		`Delta posture`,
		`id="portfolioDeltaMeaning"`,
		`id="alertsTab" data-tab-panel="alerts"`,
		`id="settingsTab" data-tab-panel="settings"`,
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
	// Exercise submission uses typed review/confirmation and no free-text
	// broker-write prompt. No other prompt may exist.
	if got := strings.Count(js, "window.prompt"); got != 0 {
		t.Fatalf("app.js window.prompt count = %d, want zero", got)
	}
	if strings.Contains(js, "window.confirm") {
		t.Fatalf("app.js must not use window.confirm; broker writes confirm via single-click buttons")
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
