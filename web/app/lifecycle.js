import { applyGovernanceCutoverOverlay, refreshGovernance, refreshPushState, scheduleGovernanceRefresh, validateGovernanceResponse } from "./alerts.js";
import { handleAttentionContextChange, ingestAlerts, ingestAlertsEvent, refreshAlerts, renderAlerts, renderSelectedAlert } from "./alert-inbox.js";
import { renderAll } from "./app.js";
import { tryDeviceLogin } from "./auth.js";
import { refreshOpenOrders } from "./orders.js";
import { $ } from "./shared.js";
import { refreshSelectedMarketCalendar, renderTopbar } from "./shell.js";
import { state } from "./state.js";
import { refreshPurgeStatus } from "./underlyings.js";

async function bootstrap(options = {}) {
  try {
    const data = await fetchBootstrap();
    if (!data) return false;
    applyBootstrap(data);
    return true;
  } catch (err) {
    if (!options.quiet) {
      showPairing("Connecting to the Mac failed: " + err.message + " — retrying.");
    }
    return false;
  }
}

// bootstrapWithRetry keeps trying until the app answers or definitively
// rejects the device. The Mac is often mid-restart (make app-refresh,
// launchd respawn) exactly when the phone opens; a one-shot bootstrap that
// dead-ends on "scan a fresh QR code" trains the user to re-pair devices
// whose credentials are perfectly valid.
async function bootstrapWithRetry() {
  let delay = 2000;
  for (;;) {
    if (await bootstrap({ quiet: true })) return true;
    if (state.pairingRequired) return false;
    showPairing("Connecting to the Mac… retrying automatically.");
    await new Promise((resolve) => setTimeout(resolve, delay));
    delay = Math.min(delay * 2, 15000);
  }
}

async function fetchBootstrap() {
  let res = await fetch("/api/bootstrap", { credentials: "include" });
  if (res.status === 401) {
    const login = await tryDeviceLogin();
    if (login === "repair") {
      // Definitive rejection: the device grant or local credential is
      // gone. Only here is a fresh QR pairing the right advice.
      state.pairingRequired = true;
      showPairing("Scan a fresh QR code from `canary app pair` on the Mac.");
      return null;
    }
    if (login !== "ok") {
      throw new Error("device login is temporarily unavailable");
    }
    res = await fetch("/api/bootstrap", { credentials: "include" });
    if (res.status === 401) {
      throw new Error("fresh session was not accepted yet");
    }
  }
  if (!res.ok) {
    throw new Error(await res.text());
  }
  state.pairingRequired = false;
  return res.json();
}

function applyBootstrap(data) {
  state.snapshot = applyGovernanceCutoverOverlay(data.snapshot);
  state.governance = validateGovernanceResponse(data.governance);
  state.governanceRefreshSucceeded = null;
  state.authenticated = Boolean(data.auth?.authenticated);
  state.settings = data.settings || data.snapshot?.settings || state.settings;
  if (state.snapshot && state.settings) state.snapshot.settings = state.settings;
  state.alertSettings = data.alert_settings || state.alertSettings;
  state.attentionEpoch = (state.attentionEpoch || 0) + 1;
  ingestAlerts(data.alerts);
  state.vapidPublicKey = data.vapid_public_key || "";
  $("pairingPanel").hidden = true;
  $("accountPanel").hidden = false;
  $("underlyingPanel").hidden = false;
  $("tabPanels").hidden = false;
  $("bottomTabs").hidden = false;
  $("dashboard").hidden = false;
  $("alertsPanel").hidden = false;
  setConnection("Connected", true);
  renderAll();
  refreshPushState();
  handleAttentionContextChange();
  connectEvents();
  refreshOpenOrders();
  refreshPurgeStatus();
  if (state.selectedMarket !== "us") {
    refreshSelectedMarketCalendar();
  }
}

function connectEvents() {
  if (state.reconnectTimer) {
    clearTimeout(state.reconnectTimer);
    state.reconnectTimer = null;
  }
  if (typeof EventSource === "undefined") {
    setConnection("Polling", false);
    return;
  }
  state.eventSource?.close();
  const es = new EventSource("/api/events", { withCredentials: true });
  state.eventSource = es;
  for (const type of ["snapshot", "status", "market_calendar", "account", "positions", "market_events", "market_quotes", "trading", "auto_trade", "proposals", "opportunities", "settings", "regime", "stress", "rules", "brief", "nudges", "alerts"]) {
    es.addEventListener(type, (event) => {
      if (type === "alerts") {
        ingestAlertsEvent(event.data);
        renderAlerts();
        renderSelectedAlert();
        state.lastEventAt = Date.now();
        setConnection("Connected", true);
        return;
      }
      const data = JSON.parse(event.data);
      if (type === "snapshot") state.snapshot = applyGovernanceCutoverOverlay(data);
      if (type !== "snapshot") state.snapshot = { ...(state.snapshot || {}), [type]: data };
      if (type === "nudges") {
        const observedAt = data?.as_of || new Date().toISOString();
        state.snapshot = {
          ...(state.snapshot || {}),
          nudges: data,
          sources: {
            ...(state.snapshot?.sources || {}),
            nudges: { state: "current", updated_at: observedAt, last_success_at: observedAt },
          },
        };
        state.snapshot = applyGovernanceCutoverOverlay(state.snapshot);
      }
      if (type === "snapshot" || type === "settings") state.settings = type === "settings" ? data : data.settings || state.settings;
      state.lastEventAt = Date.now();
      setConnection("Connected", true);
      renderAll();
      if (["snapshot", "stress", "nudges"].includes(type)) handleAttentionContextChange();
      if (type === "stress") {
        setTimeout(refreshAlerts, 500);
      }
      if (type === "nudges" && state.authenticated) {
        refreshGovernance();
        scheduleGovernanceRefresh({ delayMs: 1500, minIntervalMs: 0, ensureTrailing: true });
      }
      if (type === "snapshot" && state.authenticated) {
        scheduleGovernanceRefresh();
      }
    });
  }
  es.onerror = () => scheduleEventRecovery();
}

async function refreshBootstrapIfSSEUnavailable() {
  if (!state.snapshot || state.fallbackRefreshBusy || !sseUnavailable()) return;
  state.fallbackRefreshBusy = true;
  try {
    await bootstrap({ quiet: true });
  } finally {
    state.fallbackRefreshBusy = false;
  }
}

function sseUnavailable() {
  if (!state.eventSource || !state.connectionOK) return true;
  if (typeof EventSource !== "undefined" && state.eventSource.readyState !== EventSource.OPEN) return true;
  return false;
}

function scheduleEventRecovery() {
  setConnection("Reconnecting", false);
  state.eventSource?.close();
  if (state.reconnectTimer) return;
  state.reconnectTimer = setTimeout(async () => {
    state.reconnectTimer = null;
    const recovered = await bootstrap({ quiet: true });
    if (!recovered && !state.pairingRequired) {
      scheduleEventRecovery();
    }
  }, 1000);
}

function setConnection(text, ok) {
  state.connectionText = text;
  state.connectionOK = ok;
  $("statusDot").className = "status-dot " + (ok ? "ok" : "risk");
  const statusLabel = ok ? "App data stream connected" : "App data stream reconnecting";
  $("statusDot").setAttribute("aria-label", statusLabel);
  $("statusDot").title = statusLabel;
  renderTopbar(state.snapshot || {});
}

function showPairing(text) {
  state.authenticated = false;
  $("pairingPanel").hidden = false;
  $("tabPanels").hidden = true;
  $("bottomTabs").hidden = true;
  $("accountPanel").hidden = true;
  $("bannerStack").hidden = true;
  $("syncStrip").hidden = true;
  $("pairingText").textContent = text;
  setConnection("Locked", false);
}

export { applyBootstrap, bootstrap, bootstrapWithRetry, connectEvents, fetchBootstrap, refreshBootstrapIfSSEUnavailable, scheduleEventRecovery, setConnection, showPairing, sseUnavailable };
