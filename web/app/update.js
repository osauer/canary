import { $ } from "./shared.js";
import { state } from "./state.js";

const UPDATE_TARGET_KEY = "canaryUpdateTarget";
const UPDATE_POLL_MS = 1500;
const UPDATE_COMPLETE_MS = 8000;

function updateVersionLabel(version) {
  const clean = String(version || "").trim().replace(/^v/, "");
  return clean ? `v${clean}` : "Canary";
}

function renderUpdateStatus() {
  const button = $("updateAction");
  const reveal = () => {
    button.hidden = false;
    $("syncStrip").hidden = false;
  };
  const status = state.updateStatus || {};
  button.hidden = true;
  button.disabled = false;
  button.classList.remove("sync-strip__update--complete", "sync-strip__update--failed");
  button.title = "";
  if (state.readOnlyPreview) return;

  if (status.state === "available" && status.available && status.latest_version) {
    reveal();
    button.textContent = `${updateVersionLabel(status.latest_version)} available · Update`;
    button.title = `Install ${updateVersionLabel(status.latest_version)} and restart the running Canary app and daemon`;
    return;
  }
  if (status.state === "updating") {
    reveal();
    button.disabled = true;
    button.textContent = `Updating to ${updateVersionLabel(status.target_version || status.latest_version)}…`;
    button.title = status.message || "Canary will reconnect after the app and daemon restart.";
    return;
  }
  if (status.state === "failed" && status.latest_version) {
    reveal();
    button.classList.add("sync-strip__update--failed");
    button.textContent = `${updateVersionLabel(status.latest_version)} update failed · Retry`;
    button.title = status.message || "The update did not complete.";
    return;
  }
  if (status.state === "completed") {
    reveal();
    button.disabled = true;
    button.classList.add("sync-strip__update--complete");
    button.textContent = `Updated to ${updateVersionLabel(status.target_version)}`;
  }
}

function applyUpdateStatus(status = {}) {
  state.updateStatus = status && typeof status === "object" ? status : null;
  renderUpdateStatus();
  if (status.checking || status.state === "updating") scheduleUpdatePoll();
}

async function refreshUpdateStatus() {
  if (state.updateStatus?.state === "completed") return;
  try {
    const res = await fetch("/api/update", { credentials: "include" });
    if (!res.ok) return;
    applyUpdateStatus(await res.json());
  } catch {
    if (localStorage.getItem(UPDATE_TARGET_KEY)) scheduleUpdatePoll();
  }
}

async function requestUpdate() {
  const target = String(state.updateStatus?.latest_version || "").trim();
  if (!target || state.readOnlyPreview || state.updateStatus?.state === "updating") return;
  localStorage.setItem(UPDATE_TARGET_KEY, target);
  applyUpdateStatus({ ...state.updateStatus, state: "updating", available: false, target_version: target, message: "Starting the signed Canary update." });
  try {
    const res = await fetch("/api/update", {
      method: "POST",
      credentials: "include",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ target_version: target }),
    });
    if (!res.ok) throw new Error(await res.text());
    applyUpdateStatus(await res.json());
  } catch {
    localStorage.removeItem(UPDATE_TARGET_KEY);
    applyUpdateStatus({ ...state.updateStatus, state: "failed", available: true, latest_version: target, message: "Could not start the update; retry here or run `canary update --restart` on the Mac." });
  }
}

function observeAppVersion(version) {
  const target = String(localStorage.getItem(UPDATE_TARGET_KEY) || "").trim();
  if (!target || updateVersionLabel(version) !== updateVersionLabel(target)) return false;
  localStorage.removeItem(UPDATE_TARGET_KEY);
  if (state.updatePollTimer) clearTimeout(state.updatePollTimer);
  state.updatePollTimer = null;
  applyUpdateStatus({ state: "completed", current_version: version, target_version: target });
  if (state.updateCompleteTimer) clearTimeout(state.updateCompleteTimer);
  state.updateCompleteTimer = setTimeout(() => {
    state.updateCompleteTimer = null;
    if (state.updateStatus?.state !== "completed") return;
    state.updateStatus = null;
    renderUpdateStatus();
    refreshUpdateStatus();
  }, UPDATE_COMPLETE_MS);
  return true;
}

function scheduleUpdatePoll() {
  if (state.updatePollTimer) return;
  state.updatePollTimer = setTimeout(() => {
    state.updatePollTimer = null;
    refreshUpdateStatus();
  }, UPDATE_POLL_MS);
}

export { applyUpdateStatus, observeAppVersion, refreshUpdateStatus, renderUpdateStatus, requestUpdate, updateVersionLabel };
