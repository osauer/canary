package appweb

import (
	"strings"
	"testing"
)

func TestGovernanceSurfaceStaticContract(t *testing.T) {
	t.Parallel()
	htmlData, err := Files.ReadFile("index.html")
	if err != nil {
		t.Fatal(err)
	}
	html := string(htmlData)
	for _, id := range []string{
		"governanceCurrentState", "governanceCurrentCount", "governanceCurrentList",
		"governanceSourceHealth", "governanceContext", "governanceCoverage",
		"governanceCoverageDetail", "governanceEvidenceDetails",
		"governanceCutoverReviewButton", "governanceCutoverReviewStatus",
		"governanceHistoryCount", "governanceHistoryList", "governanceDeliveryHealth",
		"governanceDeliveryDetail", "safeNotificationTestButton", "safeNotificationTestStatus",
		"reconciliationCard", "reconciliationHeading", "reconciliationState", "reconciliationSummary",
		"reconciliationMeta", "reconciliationCheckButton", "reconciliationCheckStatus",
	} {
		if !strings.Contains(html, `id="`+id+`"`) {
			t.Errorf("index.html missing governance id %q", id)
		}
	}
	if !strings.Contains(html, ">Ended alerts<") || strings.Contains(html, ">Alert History<") {
		t.Fatal("Ended alerts must be visibly distinct from Risk & process history")
	}
	detailsAt := strings.Index(html, `id="governanceEvidenceDetails"`)
	if detailsAt < 0 || strings.Contains(html[detailsAt:strings.Index(html[detailsAt:], `>`)+detailsAt], " open") {
		t.Fatal("governance evidence disclosure must use native details and start closed")
	}
	settingsAt := strings.Index(html, `class="settings-notification-card"`)
	safeTestAt := strings.Index(html, `id="safeNotificationTestButton"`)
	if settingsAt < 0 || safeTestAt < settingsAt {
		t.Fatal("safe notification test must live in the visible Settings notification card")
	}
}

func TestGovernanceRendererConsumesTypedAuthorities(t *testing.T) {
	t.Parallel()
	alertsData, err := Files.ReadFile("alerts.js")
	if err != nil {
		t.Fatal(err)
	}
	alerts := string(alertsData)
	for _, want := range []string{
		`const nudges = snapshot.nudges || null`,
		`const pollSource = snapshot.sources?.nudges || {}`,
		`const governance = state.governance`,
		`const pollState = safeGovernancePollState(pollSource.state)`,
		`const current = pollState === "current"`,
		`"current", "stale", "not_observed", "unavailable"`,
		`No current risk and process reminders.`,
		`Current risk and process reminders are unavailable.`,
		`last push-service acceptance`,
		`refresh unavailable · last known`,
		`updated not observed`,
		`state.governanceRefreshSucceeded = false`,
		`state.governanceRefreshSucceeded = true`,
		`context.drawdown.consumed_pct === null`,
		`coverage?.pre_cutover_flows_unreviewed === true`,
		`body: JSON.stringify({})`,
		`fetch("/api/push/test"`,
		`fetch("/api/governance/cutover-review"`,
		`fetch("/api/recon/check"`,
		`fetch("/api/recon/status"`,
		`const reconciliation = validateReconciliation(governance?.reconciliation)`,
		`Open IBKR Client Portal → Reporting → Flex Queries on this Mac, renew the Flex Web Service token`,
		`process_reminders_not_enabled: "reminders are not enabled"`,
		`renderGovernanceHistory(governance?.occurrences)`,
		`pre_cutover_flows_unreviewed: false`,
		`Older payments marked reviewed.`,
		`Older payments were already marked reviewed.`,
	} {
		if !strings.Contains(alerts, want) {
			t.Errorf("alerts.js missing governance contract %q", want)
		}
	}
	for _, forbidden := range []string{
		`candidate.fingerprint`, `attempt.raw_error`,
		`all clear`, `normal outside market hours`, `return "paused"`,
	} {
		if strings.Contains(alerts, forbidden) {
			t.Errorf("alerts.js contains forbidden governance rendering contract %q", forbidden)
		}
	}
	if strings.Contains(jsFunctionBlock(t, alerts, "governanceOccurrenceElement"), "display_id") {
		t.Error("governance occurrence rendering must not expose display ids")
	}
}

func TestAttentionAndAlertDeliveryStaticContract(t *testing.T) {
	t.Parallel()
	htmlData, _ := Files.ReadFile("index.html")
	settingsData, _ := Files.ReadFile("alerts.js")
	inboxData, _ := Files.ReadFile("alert-inbox.js")
	lifecycleData, _ := Files.ReadFile("lifecycle.js")
	html, settings, inbox, lifecycle := string(htmlData), string(settingsData), string(inboxData), string(lifecycleData)
	for _, id := range []string{"alertUnreadBadge", "attentionStatus", "alertSegments", "alertSettingsStatus", "pushState", "enablePushButton", "alertAuthorityState", "alertSourceList"} {
		if !strings.Contains(html, `id="`+id+`"`) {
			t.Errorf("index.html missing attention/settings id %q", id)
		}
	}
	for _, want := range []string{
		`fetch("/api/alerts/attention"`,
		`fetch("/api/alerts/attention/read"`,
		`body: JSON.stringify({ through_seq: attention.high_water_seq })`,
		`document.visibilityState === "visible"`,
		`state.renderedAlertAttention`,
	} {
		if !strings.Contains(inbox, want) {
			t.Errorf("alert-inbox.js missing attention contract %q", want)
		}
	}
	for _, want := range []string{
		`fetch("/api/alerts/settings"`,
		`registration.pushManager.getSubscription()`,
		`permission granted but not subscribed`,
		`browser subscribed`,
	} {
		if !strings.Contains(settings, want) {
			t.Errorf("alerts.js missing notification settings contract %q", want)
		}
	}
	for _, forbidden := range []string{`localStorage.setItem("ibkrAttention`, `sessionStorage`, `indexedDB`, `fetch("/api/settings"`, `fetch("/api/attention"`} {
		if strings.Contains(inbox+settings, forbidden) {
			t.Errorf("SPA contains forbidden attention/settings contract %q", forbidden)
		}
	}
	for _, want := range []string{`ingestAlerts(data.alerts)`, `handleAttentionContextChange()`, `applyGovernanceCutoverOverlay(data.snapshot)`} {
		if !strings.Contains(lifecycle, want) {
			t.Errorf("lifecycle.js missing alert/cutover wiring %q", want)
		}
	}
}

func TestGovernanceBootstrapSSEAndSmokeHookContract(t *testing.T) {
	t.Parallel()
	stateData, _ := Files.ReadFile("state.js")
	lifecycleData, _ := Files.ReadFile("lifecycle.js")
	appData, _ := Files.ReadFile("app.js")
	stateJS, lifecycle, app := string(stateData), string(lifecycleData), string(appData)
	if !strings.Contains(stateJS, `governance: null`) {
		t.Fatal("governance startup posture must fail closed to null/not observed")
	}
	if !strings.Contains(stateJS, `governanceRefreshSucceeded: null`) {
		t.Fatal("governance refresh evidence must start not observed")
	}
	for _, want := range []string{
		`state.governance = validateGovernanceResponse(data.governance)`,
		`"brief", "nudges"`,
		`nudges: { state: "current"`,
		`refreshGovernance()`,
		`scheduleGovernanceRefresh({ delayMs: 1500, minIntervalMs: 0, ensureTrailing: true })`,
		`type === "snapshot" && state.authenticated`,
		`scheduleGovernanceRefresh()`,
	} {
		if !strings.Contains(lifecycle, want) {
			t.Errorf("lifecycle.js missing governance SSE contract %q", want)
		}
	}
	for _, want := range []string{
		`const { governance, governanceRefreshSucceeded, ...snapshotPatch } = patch`,
		`state.governance = governance`,
		`state.governanceRefreshSucceeded = governanceRefreshSucceeded`,
		`snapshotPatch.sources.nudges`,
		`snapshotPatch.nudges.context`,
		`monthly_pulse`,
		`setActiveTab(launchTab, { persist: false })`,
		`history.replaceState({}, "", location.pathname || "/")`,
	} {
		if !strings.Contains(app, want) {
			t.Errorf("app.js missing governance fixture/navigation contract %q", want)
		}
	}
}
