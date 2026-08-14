import { enablePush, renderAlertMode, renderReconciliationCard, sendReconciliationCheck, sendSafeNotificationTest, setAlertMode } from "./alerts.js";
import { renderAlerts, setupAttentionVisibility } from "./alert-inbox.js";
import { completePairing } from "./auth.js";
import { renderBriefCard } from "./brief.js";
import { renderStressDetail, renderStressStatus, renderStressTimestamp, renderMarketContext, renderRegimePanel, renderRulesCard } from "./stress.js";
import { ensureRegimeStressExpansion, handleAccountPanelTap, handleOpportunitiesPanelTap, handlePortfolioPanelTap, handleProtectionPanelTap, handleUnderlyingPanelTap, renderTabs, resetViewportScroll, setAccountOverviewExpansion, setAccountValueVisible, setActiveTab, setOpportunitiesExpansion, setProtectionExpansion, setProtectionSheetOpen, setRegimeStressExpansion, setRulesSheetOpen, setUnderlyingsSheetOpen, setupBottomTabs, syncAccountPrivacyState } from "./chrome.js";
import { bootstrap, bootstrapWithRetry, refreshBootstrapIfSSEUnavailable, showPairing } from "./lifecycle.js";
import { refreshOpportunities, renderOpportunitiesPanel } from "./opportunities.js";
import { ACTIVE_ORDERS_REFRESH_MS, refreshOpenOrders, renderOpenOrders } from "./orders.js";
import { renderPortfolioRisk, setPortfolioExpansion } from "./portfolio.js";
import { cancelProtectionDerisk, previewProtectionDerisk, renderProtectionPanel } from "./protection.js";
import { installRenderAll } from "./render-runtime.js";
import { renderSettings, setStockProtectionEnabled } from "./settings.js";
import { $, accountBaseCurrency, accountFieldValue, pct, renderSensitiveText } from "./shared.js";
import { renderSourceBanners, renderSyncStrip, renderTopbar, setupMarketSelect } from "./shell.js";
import { state } from "./state.js";
import { renderStrategies } from "./strategies.js";
import { positionsAuthorityView, renderAccountPanel, renderPositionsFreshness, renderUnderlyings, setUnderlyingExpansion } from "./underlyings.js";
import { renderUpdateStatus, requestUpdate } from "./update.js";

installRenderAll(renderAll);
installSmokeHooks();

function installSmokeHooks() {
  const smoke = globalThis.__canarySmoke;
  if (!smoke || smoke.applySnapshotPatch) return;
  smoke.applySnapshotPatch = (patch = {}, ui = {}) => {
    const current = state.snapshot || {};
    const snapshotPatch = patch;
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
  setupAttentionVisibility();
  await navigator.serviceWorker?.register("/service-worker.js");
  const params = new URLSearchParams(location.search);
  const launchTab = ["monitor", "brief", "alerts"].includes(params.get("tab")) ? params.get("tab") : "";
  const pair = params.get("pair");
  const nonce = params.get("nonce");
  const remote = params.get("remote");
  if (remote) {
    // The relay addresses this phone's route by an HttpOnly cookie; mirror
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
    // The brief re-renders on every SSE event; re-rendering it on the 1s
    // tick as well replaced its topic buttons mid-tap for no fresher data.
    if (state.snapshot) {
      renderAccountPanel(snap.account || {}, snap.positions || {}, snap.stress || {});
      renderUnderlyings(snap.positions || {}, snap.account || {}, snap.market_events || {});
      renderPortfolioRisk(snap.positions || {}, snap.account || {});
      renderProtectionPanel(snap.proposals || {}, snap.auto_trade || {}, snap.market_events || {});
      renderOpportunitiesPanel(snap.opportunities || {});
    }
    if (state.authenticated && state.activeTab === "orders") {
      void refreshOpenOrders({ minIntervalMs: ACTIVE_ORDERS_REFRESH_MS });
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
  const cushion = accountFieldValue(account, "cushion");
  renderSensitiveText("cushion", typeof cushion === "number" ? pct(cushion * 100) : "--", typeof cushion === "number");
  renderPositionsFreshness($("positionsAsOf"), positions, snap.sources?.positions || {});
  const positionsAvailable = positionsAuthorityView(positions, snap.sources?.positions || {}).available;
  $("stockCount").textContent = positionsAvailable ? (positions.stocks || []).length : "--";
  $("optionCount").textContent = positionsAvailable ? (positions.options || []).length : "--";
  $("baseCurrency").textContent = accountBaseCurrency(account) || "--";
  renderStressStatus(stress, snap);
  renderRulesCard(snap.rules);
  renderStressTimestamp(stress);
  renderProtectionPanel(snap.proposals || {}, snap.auto_trade || {}, snap.market_events || {});
  renderOpportunitiesPanel(snap.opportunities || {});
  renderOpenOrders();
  renderStrategies(positions);
  renderMarketContext(snap);
  renderRegimePanel(snap);
  renderStressDetail(stress, snap);
  renderPortfolioRisk(positions, account);
  renderSourceBanners(snap);
  renderAlertMode();
  renderAlerts();
  renderReconciliationCard();
  renderSettings();
  renderTabs();
  renderSyncStrip(snap);
  renderUpdateStatus();
}

document.querySelectorAll("#alertSegments button").forEach((button) => {
  button.addEventListener("click", () => setAlertMode(button.dataset.mode));
});

$("enablePushButton").addEventListener("click", enablePush);
$("safeNotificationTestButton").addEventListener("click", sendSafeNotificationTest);
$("reconciliationCheckButton").addEventListener("click", sendReconciliationCheck);
$("retryAuthButton").addEventListener("click", bootstrap);
$("updateAction").addEventListener("click", requestUpdate);
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
$("stressDetailToggle").addEventListener("click", () => {
  setRegimeStressExpansion("stress", !state.stressDetailOpen);
});
$("stressRulesToggle").addEventListener("click", () => {
  state.rulesDetailOpen = !state.rulesDetailOpen;
  renderRulesCard(state.snapshot?.rules);
});
// Tap-through: the instrument that reports a subject is the way into the
for (const [openerID, setSheet] of [
  ["protectionTile", setProtectionSheetOpen],
  ["stressRulesCard", setRulesSheetOpen],
  ["moversPlacard", setUnderlyingsSheetOpen],
  ["moversRow", setUnderlyingsSheetOpen],
]) {
  $(openerID).addEventListener("click", () => setSheet(true));
  $(openerID).addEventListener("keydown", (event) => {
    if (event.key !== "Enter" && event.key !== " ") return;
    event.preventDefault();
    setSheet(true);
  });
}
// Escape and backdrop dismissal never run the Close handler, so the dialog's
for (const [sheetID, closeID, setSheet] of [
  ["protectionSheet", "protectionSheetClose", setProtectionSheetOpen],
  ["rulesSheet", "rulesSheetClose", setRulesSheetOpen],
  ["underlyingsSheet", "underlyingsSheetClose", setUnderlyingsSheetOpen],
]) {
  $(closeID).addEventListener("click", () => setSheet(false));
  $(sheetID).addEventListener("close", () => setSheet(false));
  $(sheetID).addEventListener("click", (event) => {
    if (event.target === event.currentTarget) event.currentTarget.close();
  });
}
// The lamp-test stamp opens the panel's own self-report: which alert sources
// broker report stands. Detail behind the stamp that reports it, not a
$("lampTestButton").addEventListener("click", () => {
  $("lampTestDialog").showModal();
});
$("lampTestDialogClose").addEventListener("click", () => {
  $("lampTestDialog").close();
});
$("lampTestDialog").addEventListener("click", (event) => {
  if (event.target === event.currentTarget) event.currentTarget.close();
});
$("stressRulesNotesToggle").addEventListener("click", () => {
  $("stressRulesNotesDialog").showModal();
});
$("stressRulesNotesClose").addEventListener("click", () => {
  $("stressRulesNotesDialog").close();
});
$("stressRulesNotesDialog").addEventListener("click", (event) => {
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
// The labeled Show/Hide detail buttons are the only expansion controls for
// Market Regime and Desk. A tap anywhere else on those grids used to toggle a
// panel that rendered far below — that read as erratic, so it is gone.
$("regimeDetailToggle").addEventListener("click", () => {
  setRegimeStressExpansion("regime", !state.regimeDetailOpen);
});
$("underlyingDetailToggle").addEventListener("click", () => {
  setUnderlyingExpansion(!state.underlyingDetailOpen);
});
$("underlyingPanel").addEventListener("click", handleUnderlyingPanelTap);
$("portfolioDetailToggle").addEventListener("click", () => {
  setPortfolioExpansion(!state.portfolioDetailOpen);
});
$("portfolioPanel").addEventListener("click", handlePortfolioPanelTap);
$("stockProtectionToggle").addEventListener("change", (event) => {
  setStockProtectionEnabled(event.currentTarget.checked);
});
$("protectionDeriskPercent").addEventListener("change", (event) => {
  state.protectionDerisk.percent = Number(event.currentTarget.value) || 25;
  // A different percentage is a different sweep: abandon any in-flight
  cancelProtectionDerisk();
});
$("protectionDeriskPreview").addEventListener("click", previewProtectionDerisk);
$("protectionDeriskCancel").addEventListener("click", cancelProtectionDerisk);

window.addEventListener("storage", (event) => {
  if (event.key !== "canaryAccountValueVisible") return;
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
