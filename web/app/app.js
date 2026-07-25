import { enablePush, renderAlertMode, renderGovernance, sendGovernanceCutoverReview, sendReconciliationCheck, sendSafeNotificationTest, setAlertMode } from "./alerts.js";
import { renderAlerts, renderSelectedAlert, setupAttentionVisibility } from "./alert-inbox.js";
import { completePairing } from "./auth.js";
import { renderBriefCard, setupBriefVisibility } from "./brief.js";
import { stressStageLabel, stressSummaryText, firstClause, renderStressDetail, renderStressStatus, renderStressTimestamp, renderMarketContext, renderRegimePanel, renderRulesCard } from "./stress.js";
import { ensureRegimeStressExpansion, handleAccountPanelTap, handleExpandablePanelTap, handleOpportunitiesPanelTap, handlePortfolioPanelTap, handleProtectionPanelTap, handleUnderlyingPanelTap, renderTabs, resetViewportScroll, setAccountOverviewExpansion, setAccountValueVisible, setActiveTab, setOpportunitiesExpansion, setProtectionExpansion, setRegimeStressExpansion, setupBottomTabs, syncAccountPrivacyState } from "./chrome.js";
import { bootstrap, bootstrapWithRetry, refreshBootstrapIfSSEUnavailable, showPairing } from "./lifecycle.js";
import { refreshOpportunities, renderOpportunitiesPanel } from "./opportunities.js";
import { renderOpenOrders } from "./orders.js";
import { renderPortfolioRisk, setPortfolioExpansion } from "./portfolio.js";
import { cancelProtectionDerisk, previewProtectionDerisk, renderProtectionPanel } from "./protection.js";
import { renderSettings, setPurgeRestoreEnabled, setStockProtectionEnabled } from "./settings.js";
import { $, labelize, pct, renderFreshnessTimestamp, renderSensitiveText } from "./shared.js";
import { renderSourceBanners, renderSyncStrip, renderTopbar, setupMarketSelect } from "./shell.js";
import { state } from "./state.js";
import { renderAccountPanel, renderUnderlyings, runUnderlyingAction, setUnderlyingExpansion } from "./underlyings.js";

installSmokeHooks();

function installSmokeHooks() {
  const smoke = globalThis.__ibkrSmoke;
  if (!smoke || smoke.applySnapshotPatch) return;
  smoke.applySnapshotPatch = (patch = {}, ui = {}) => {
    const current = state.snapshot || {};
    const { governance, governanceRefreshSucceeded, ...snapshotPatch } = patch;
    state.snapshot = {
      ...current,
      ...snapshotPatch,
      account: snapshotPatch.account ? { ...(current.account || {}), ...snapshotPatch.account } : current.account,
      positions: snapshotPatch.positions ? {
        ...(current.positions || {}),
        ...snapshotPatch.positions,
        portfolio: snapshotPatch.positions.portfolio ? { ...(current.positions?.portfolio || {}), ...snapshotPatch.positions.portfolio } : current.positions?.portfolio,
      } : current.positions,
      stress: snapshotPatch.stress ? {
        ...(current.stress || {}),
        ...snapshotPatch.stress,
        portfolio: snapshotPatch.stress.portfolio ? { ...(current.stress?.portfolio || {}), ...snapshotPatch.stress.portfolio } : current.stress?.portfolio,
      } : current.stress,
      proposals: snapshotPatch.proposals ? { ...(current.proposals || {}), ...snapshotPatch.proposals } : current.proposals,
      opportunities: snapshotPatch.opportunities ? { ...(current.opportunities || {}), ...snapshotPatch.opportunities } : current.opportunities,
      sources: snapshotPatch.sources ? {
        ...(current.sources || {}),
        ...snapshotPatch.sources,
        nudges: snapshotPatch.sources.nudges ? { ...(current.sources?.nudges || {}), ...snapshotPatch.sources.nudges } : current.sources?.nudges,
      } : current.sources,
      nudges: snapshotPatch.nudges ? {
        ...(current.nudges || {}),
        ...snapshotPatch.nudges,
        context: Object.prototype.hasOwnProperty.call(snapshotPatch.nudges, "context")
          ? snapshotPatch.nudges.context ? { ...(current.nudges?.context || {}), ...snapshotPatch.nudges.context } : snapshotPatch.nudges.context
          : current.nudges?.context,
      } : current.nudges,
      brief: snapshotPatch.brief ? {
        ...(current.brief || {}),
        ...snapshotPatch.brief,
        ready: snapshotPatch.brief.ready ? {
          ...(current.brief?.ready || {}),
          ...snapshotPatch.brief.ready,
          monthly_pulse: Object.prototype.hasOwnProperty.call(snapshotPatch.brief.ready, "monthly_pulse")
            ? snapshotPatch.brief.ready.monthly_pulse
            : current.brief?.ready?.monthly_pulse,
        } : current.brief?.ready,
      } : current.brief,
    };
    if (Object.prototype.hasOwnProperty.call(patch, "governance")) state.governance = governance;
    if (Object.prototype.hasOwnProperty.call(patch, "governanceRefreshSucceeded")) state.governanceRefreshSucceeded = governanceRefreshSucceeded;
    for (const key of ["protectionOpen", "portfolioDetailOpen", "stressDetailOpen", "opportunitiesOpen"]) {
      if (Object.prototype.hasOwnProperty.call(ui, key)) state[key] = Boolean(ui[key]);
    }
    renderAll();
    return true;
  };
}

async function main() {
  resetViewportScroll();
  setupBottomTabs();
  setupBriefVisibility();
  setupAttentionVisibility();
  await navigator.serviceWorker?.register("/service-worker.js");
  const params = new URLSearchParams(location.search);
  const launchTab = ["monitor", "brief", "alerts"].includes(params.get("tab")) ? params.get("tab") : "";
  const pair = params.get("pair");
  const nonce = params.get("nonce");
  const remote = params.get("remote");
  if (remote) {
    // The relay addresses this phone's route by an HttpOnly cookie; mirror
    // the route id so the relay's recovery page can rebuild the cookie
    // after eviction instead of forcing a re-pair.
    localStorage.setItem("ibkrRemoteRoute", remote);
  }
  let bootstrapped = false;
  if (pair && nonce) {
    try {
      await completePairing(pair, nonce);
      history.replaceState({}, "", "/");
    } catch (err) {
      history.replaceState({}, "", "/");
      showPairing("Pairing link expired; opening paired app.");
    }
  }
  if (!bootstrapped) {
    bootstrapped = await bootstrapWithRetry();
  }
  if (!bootstrapped) {
    return;
  }
  if (launchTab) setActiveTab(launchTab, { persist: false });
  if (params.has("tab")) history.replaceState({}, "", location.pathname || "/");
  resetViewportScroll();
  setupMarketSelect();
  setupBottomTabs();
  setupLiveRefreshLoop();
}

function setupLiveRefreshLoop() {
  setInterval(() => {
    const snap = state.snapshot || {};
    renderTopbar(snap);
    renderSyncStrip(snap);
    renderBriefCard(snap);
    if (state.snapshot) {
      renderAccountPanel(snap.account || {}, snap.positions || {}, snap.stress || {});
      renderUnderlyings(snap.positions || {}, snap.account || {}, snap.market_events || {});
      renderPortfolioRisk(snap.positions || {}, snap.account || {});
      renderProtectionPanel(snap.proposals || {}, snap.auto_trade || {}, snap.market_events || {});
      renderOpportunitiesPanel(snap.opportunities || {});
    }
    refreshBootstrapIfSSEUnavailable();
  }, 1000);
}

function renderAll() {
  const snap = state.snapshot || {};
  const account = snap.account || {};
  const positions = snap.positions || {};
  const stress = snap.stress || {};
  syncAccountPrivacyState();
  ensureRegimeStressExpansion(stress);
  renderBriefCard(snap);
  renderTopbar(snap);
  renderAccountPanel(account, positions, stress);
  renderUnderlyings(positions, account, snap.market_events || {});
  renderSensitiveText("cushion", typeof account.cushion === "number" ? pct(account.cushion * 100) : "--", typeof account.cushion === "number");
  renderFreshnessTimestamp("positionsAsOf", positions.as_of, { staleMinutes: 15, quietWhenFresh: true });
  $("stockCount").textContent = (positions.stocks || []).length;
  $("optionCount").textContent = (positions.options || []).length;
  $("baseCurrency").textContent = account.base_currency || positions.portfolio?.base_currency || "--";
  $("canarySeverity").textContent = labelize(stress.severity || "--");
  $("canaryAction").textContent = stressStageLabel(stress);
  // The hero clamps to 2 lines; cutting at the first clause reads cleaner
  // than a mid-word ellipsis, and the full text stays one tap away in detail.
  const stressSummaryFull = stressSummaryText(stress, snap);
  const stressSummaryEl = $("canarySummary");
  stressSummaryEl.textContent = firstClause(stressSummaryFull);
  stressSummaryEl.title = stressSummaryFull;
  renderStressStatus(stress);
  renderRulesCard(snap.rules);
  renderStressTimestamp(stress);
  renderSelectedAlert();
  renderProtectionPanel(snap.proposals || {}, snap.auto_trade || {}, snap.market_events || {});
  renderOpportunitiesPanel(snap.opportunities || {});
  renderOpenOrders();
  renderMarketContext(snap);
  renderRegimePanel(snap);
  renderStressDetail(stress, snap);
  renderPortfolioRisk(positions, account);
  renderSourceBanners(snap);
  renderAlertMode();
  renderAlerts();
  renderGovernance();
  renderSettings();
  renderTabs();
  renderSyncStrip(snap);
}

document.querySelectorAll("#alertSegments button").forEach((button) => {
  button.addEventListener("click", () => setAlertMode(button.dataset.mode));
});

$("enablePushButton").addEventListener("click", enablePush);
$("safeNotificationTestButton").addEventListener("click", sendSafeNotificationTest);
$("reconciliationCheckButton").addEventListener("click", sendReconciliationCheck);
$("governanceCutoverReviewButton").addEventListener("click", sendGovernanceCutoverReview);
$("retryAuthButton").addEventListener("click", bootstrap);
$("accountPrivacyToggle").addEventListener("click", () => {
  setAccountValueVisible(!state.accountValueVisible);
});
$("accountLargestExposureToggle").addEventListener("click", () => {
  state.accountExposureOpen = !state.accountExposureOpen;
  renderAccountPanel(state.snapshot?.account || {}, state.snapshot?.positions || {}, state.snapshot?.stress || {});
});
$("accountOverviewToggle").addEventListener("click", () => {
  setAccountOverviewExpansion(!state.accountOverviewOpen);
});
$("accountPanel").addEventListener("click", (event) => handleAccountPanelTap(event));
$("canaryDetailToggle").addEventListener("click", () => {
  setRegimeStressExpansion("stress", !state.stressDetailOpen);
});
$("canaryRulesToggle").addEventListener("click", () => {
  state.rulesDetailOpen = !state.rulesDetailOpen;
  renderRulesCard(state.snapshot?.rules);
});
$("canaryRulesNotesToggle").addEventListener("click", () => {
  $("canaryRulesNotesDialog").showModal();
});
$("canaryRulesNotesClose").addEventListener("click", () => {
  $("canaryRulesNotesDialog").close();
});
$("canaryRulesNotesDialog").addEventListener("click", (event) => {
  // A modal dialog's own box is the backdrop hit target; children stop here.
  if (event.target === event.currentTarget) event.currentTarget.close();
});
$("protectionToggle").addEventListener("click", () => {
  setProtectionExpansion(!state.protectionOpen);
});
$("protectionPanel").addEventListener("click", (event) => handleProtectionPanelTap(event));
$("opportunitiesToggle").addEventListener("click", () => {
  setOpportunitiesExpansion(!state.opportunitiesOpen);
});
$("opportunitiesPanel").addEventListener("click", (event) => handleOpportunitiesPanelTap(event));
$("opportunitiesRefreshButton").addEventListener("click", (event) => {
  event.stopPropagation();
  refreshOpportunities();
});
$("clearSelectedAlertButton").addEventListener("click", () => {
  state.selectedAlertID = null;
  renderAlerts();
  renderSelectedAlert();
});
  $("regimeDetailToggle").addEventListener("click", () => {
  setRegimeStressExpansion("regime", !state.regimeDetailOpen);
});
$("regimeSummaryCard").addEventListener("click", (event) => {
  handleExpandablePanelTap(event, "regime");
});
$("canaryHero").addEventListener("click", (event) => {
  handleExpandablePanelTap(event, "stress");
});
$("underlyingDetailToggle").addEventListener("click", () => {
  setUnderlyingExpansion(!state.underlyingDetailOpen);
});
$("underlyingPanel").addEventListener("click", handleUnderlyingPanelTap);
$("buildAllUnderlyingsButton").addEventListener("click", () => {
  runUnderlyingAction("build", { all: true });
});
$("purgeAllUnderlyingsButton").addEventListener("click", () => {
  runUnderlyingAction("purge", { all: true });
});
$("restoreAllUnderlyingsButton").addEventListener("click", () => {
  runUnderlyingAction("restore", { all: true });
});
$("portfolioDetailToggle").addEventListener("click", () => {
  setPortfolioExpansion(!state.portfolioDetailOpen);
});
$("portfolioPanel").addEventListener("click", handlePortfolioPanelTap);
$("purgeRestoreToggle").addEventListener("change", (event) => {
  setPurgeRestoreEnabled(event.currentTarget.checked);
});
$("stockProtectionToggle").addEventListener("change", (event) => {
  setStockProtectionEnabled(event.currentTarget.checked);
});
$("protectionDeriskPercent").addEventListener("change", (event) => {
  state.protectionDerisk.percent = Number(event.currentTarget.value) || 25;
  // A different percentage is a different sweep: abandon any in-flight
  // preview and rendered basket rather than letting a stale-percent result
  // land later.
  cancelProtectionDerisk();
});
$("protectionDeriskPreview").addEventListener("click", previewProtectionDerisk);
$("protectionDeriskCancel").addEventListener("click", cancelProtectionDerisk);

window.addEventListener("storage", (event) => {
  if (event.key !== "ibkrAccountValueVisible") return;
  state.accountValueVisible = event.newValue === "true";
  renderAll();
});
window.addEventListener("resize", resetViewportScroll);
window.addEventListener("orientationchange", resetViewportScroll);

main().catch((err) => {
  console.error(err);
  showPairing(err.message);
});

export { installSmokeHooks, main, renderAll, setupLiveRefreshLoop };
