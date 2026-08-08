import { b64urlToBytes } from "./auth.js";
import { $ } from "./shared.js";
import { state } from "./state.js";

const alertModes = new Set(["none", "act_only", "watch_and_act"]);
const reportStates = new Set(["waiting", "due", "checking", "current", "retry_scheduled", "action_required", "unavailable"]);
const evaluationStates = new Set(["waiting", "checking", "complete", "attention_required", "failed"]);
const reportReasons = new Set([
  "", "none", "before_daily_window", "coverage_pending", "report_not_ready", "service_busy", "rate_limited",
  "network_unavailable", "flex_disabled", "query_missing", "token_missing", "token_invalid", "token_expired",
  "query_invalid", "ip_restricted", "service_inactive", "response_invalid", "report_invalid", "storage_failed",
  "projection_failed", "authority_unavailable",
]);
const evaluationReasons = new Set([
  "", "none", "report_pending", "account_value_pending", "exceptions_need_review", "account_value_mismatch",
  "evaluation_failed", "policy_unapproved",
]);
const transportStates = new Set(["push_service_accepted", "partial_acceptance", "all_failed", "suppressed"]);

function exactKeys(value, expected) {
  if (!value || typeof value !== "object" || Array.isArray(value)) return false;
  const actual = Object.keys(value).sort();
  const wanted = [...expected].sort();
  return actual.length === wanted.length && actual.every((key, index) => key === wanted[index]);
}

function validateAlertSettings(value) {
  return exactKeys(value, ["mode"]) && alertModes.has(value.mode) ? { mode: value.mode } : null;
}

async function setAlertMode(mode) {
  if (!alertModes.has(mode) || state.alertSettingsUpdate.busy) return false;
  const previous = validateAlertSettings(state.alertSettings) || { mode: "watch_and_act" };
  state.alertSettingsUpdate = { busy: true, state: "Saving notification level…", error: false };
  renderAlertMode();
  try {
    const res = await fetch("/api/alerts/settings", {
      method: "PUT",
      headers: { "Content-Type": "application/json" },
      credentials: "include",
      body: JSON.stringify({ mode }),
    });
    const updated = res.ok ? validateAlertSettings(await res.json()) : null;
    if (!updated || updated.mode !== mode) throw new Error("invalid settings response");
    state.alertSettings = updated;
    state.alertSettingsUpdate.state = "Delivery level saved for this app host.";
    return true;
  } catch {
    state.alertSettings = previous;
    state.alertSettingsUpdate.state = "Delivery level was not changed.";
    state.alertSettingsUpdate.error = true;
    return false;
  } finally {
    state.alertSettingsUpdate.busy = false;
    renderAlertMode();
  }
}

function renderAlertMode() {
  document.querySelectorAll("#alertSegments button").forEach((button) => {
    button.classList.toggle("active", button.dataset.mode === state.alertSettings.mode);
    button.disabled = state.alertSettingsUpdate.busy;
  });
  $("pushState").textContent = notificationStateLabel();
  $("alertSettingsStatus").textContent = state.alertSettingsUpdate.state;
  $("alertSettingsStatus").classList.toggle("governance-action-status--error", state.alertSettingsUpdate.error);
  renderNotificationTest();
}

function validateReconciliation(value) {
  if (!value || typeof value !== "object" || Array.isArray(value)) return null;
  const { report, evaluation } = value;
  if (!report || typeof report !== "object" || Array.isArray(report) || !reportStates.has(report.state)) return null;
  if (!evaluation || typeof evaluation !== "object" || Array.isArray(evaluation) || !evaluationStates.has(evaluation.state)) return null;
  const reportReason = typeof report.reason === "string" ? report.reason : "";
  const evaluationReason = typeof evaluation.reason === "string" ? evaluation.reason : "";
  if (!reportReasons.has(reportReason) || !evaluationReasons.has(evaluationReason)) return null;
  return {
    report: {
      state: report.state,
      reason: reportReason,
      expected_coverage_to: safeDate(report.expected_coverage_to),
      coverage_to: safeDate(report.coverage_to),
      last_attempt_at: safeTime(report.last_attempt_at),
      last_completed_at: safeTime(report.last_completed_at),
      next_attempt_at: safeTime(report.next_attempt_at),
      retry_automatic: report.retry_automatic === true,
      can_check_now: report.can_check_now === true,
    },
    evaluation: { state: evaluation.state, reason: evaluationReason },
  };
}

function currentReconciliation() {
  return validateReconciliation(state.snapshot?.nudges?.reconciliation);
}

function renderReconciliationCard(reconciliation = currentReconciliation()) {
  const view = reconciliationView(reconciliation);
  const check = state.reconciliationCheck || { busy: false, state: "", error: false };
  $("reconciliationCard").dataset.state = view.key;
  $("reconciliationState").textContent = view.label;
  $("reconciliationHeading").textContent = view.title;
  $("reconciliationSummary").textContent = view.summary;
  $("reconciliationMeta").textContent = reconciliationMeta(reconciliation);
  const button = $("reconciliationCheckButton");
  button.textContent = check.busy ? "Checking…" : "Check again";
  button.disabled = check.busy || !state.authenticated || reconciliation?.report?.can_check_now !== true;
  $("reconciliationCheckStatus").textContent = check.state;
  $("reconciliationCheckStatus").classList.toggle("governance-action-status--error", check.error);
}

function reconciliationView(reconciliation) {
  if (!reconciliation) return { key: "unavailable", label: "Unavailable", title: "Latest report status unavailable", summary: "Canary cannot confirm the latest IBKR report right now. It will keep trying automatically." };
  const { report, evaluation } = reconciliation;
  if (state.reconciliationCheck?.busy || report.state === "checking" || evaluation.state === "checking") {
    return { key: "checking", label: "Checking", title: "Checking the latest report", summary: "Canary is asking IBKR for the latest daily report and will compare it automatically." };
  }
  if (report.state === "action_required") return { key: "needs_you", label: "Needs you", title: "Fix the report connection", summary: reportActionCopy(report.reason) };
  if (report.state === "unavailable") return { key: "unavailable", label: "Unavailable", title: "Latest report unavailable", summary: reportUnavailableCopy(report) };
  if (report.state === "retry_scheduled") return { key: "retrying", label: "Retrying", title: "Report check will retry", summary: reportRetryCopy(report) };
  if (["attention_required", "failed"].includes(evaluation.state)) return { key: "needs_you", label: "Needs you", title: "Report comparison needs attention", summary: evaluationCopy(evaluation.reason) };
  if (report.state === "current" && evaluation.state === "complete") return { key: "up_to_date", label: "Up to date", title: "Latest report checked", summary: "The daily broker report is current and Canary finished the automatic comparison." };
  if (report.state === "current" && evaluation.reason === "account_value_pending") return { key: "waiting", label: "Waiting", title: "Report received", summary: "The report is current. Canary is waiting for today's account value before it compares the totals; no action is needed." };
  if (report.state === "due") return { key: "waiting", label: "Waiting", title: "Latest report is due", summary: "Canary is ready to check the latest daily report and will do so automatically." };
  const summary = report.reason === "before_daily_window"
    ? "Canary will check after the daily IBKR report window opens and before your morning report. Nothing needs your attention."
    : "Canary is waiting for the information needed to finish the daily report check.";
  return { key: "waiting", label: "Waiting", title: "Waiting for the daily report", summary };
}

function reportActionCopy(reason) {
  if (["token_missing", "token_invalid", "token_expired"].includes(reason)) return "Renew the Flex Web Service token in IBKR Client Portal, replace ~/.config/ibkr/flex-token, then check again.";
  if (["query_missing", "query_invalid", "flex_disabled"].includes(reason)) return "Configure the Activity Flex Query in ~/.config/ibkr/config.toml, restart Canary, then check again.";
  if (reason === "ip_restricted") return "Allow this Mac's public IP for Flex reports in IBKR Client Portal, then check again.";
  if (reason === "service_inactive") return "Reactivate Flex Web Service in IBKR Client Portal, then check again.";
  if (["response_invalid", "report_invalid"].includes(reason)) return "IBKR returned a report Canary could not safely use. Check again; recreate the query if it repeats.";
  if (["storage_failed", "projection_failed"].includes(reason)) return "Canary could not save and compare the report. Restart Canary, then check again.";
  return "Canary could not complete the report check safely. Check again.";
}

function reportUnavailableCopy(report) {
  const manual = report.can_check_now ? " You can also check again now." : "";
  if (report.reason === "authority_unavailable") return "Canary cannot read its local report record. Restart Canary before relying on the report check.";
  if (report.reason === "network_unavailable") return `This Mac could not reach IBKR. Check its internet connection.${manual}`;
  if (["service_busy", "rate_limited"].includes(report.reason)) return `IBKR is temporarily busy. Canary will keep trying automatically.${manual}`;
  return `Canary cannot confirm the latest IBKR report right now. It will keep trying automatically.${manual}`;
}

function reportRetryCopy(report) {
  const next = report.next_attempt_at ? ` at ${timeLabel(report.next_attempt_at)}` : " soon";
  const manual = report.can_check_now ? "; you can also check now." : ".";
  const subject = report.reason === "report_not_ready" ? "IBKR has not published the usable report yet" : "The report check did not finish";
  return `${subject}. Canary will retry${next}${manual}`;
}

function evaluationCopy(reason) {
  if (reason === "exceptions_need_review") return "Canary found a cash movement it could not match. Review that exception in the morning brief, then check again.";
  if (reason === "account_value_mismatch") return "The broker report and account value do not match. Review the figures in the morning brief, then check again.";
  if (reason === "policy_unapproved") return "The report arrived, but the local reconciliation settings are incomplete. Approve the missing setting on this Mac.";
  return "Canary could not finish comparing the report. Check again.";
}

function reconciliationMeta(reconciliation) {
  if (!reconciliation) return "";
  const report = reconciliation.report;
  const facts = [];
  if (report.coverage_to) facts.push(`Report through ${dateLabel(report.coverage_to)}`);
  if (report.last_completed_at) facts.push(`Last checked ${timeLabel(report.last_completed_at)}`);
  if (report.next_attempt_at && report.retry_automatic) facts.push(`Next automatic try ${timeLabel(report.next_attempt_at)}`);
  return facts.join(" · ");
}

async function sendReconciliationCheck(options = {}) {
  const outcome = state.reconciliationCheck || (state.reconciliationCheck = { busy: false, state: "", error: false });
  if (outcome.busy || !state.authenticated) return false;
  Object.assign(outcome, { busy: true, state: "", error: false });
  renderReconciliationCard();
  const delay = options.pollDelayMs === undefined ? 750 : Math.max(0, Number(options.pollDelayMs) || 0);
  const limit = options.maxPolls === undefined ? 20 : Math.max(1, Math.min(40, Number(options.maxPolls) || 1));
  let complete = false;
  try {
    const first = await fetch("/api/recon/check", { method: "POST", headers: { "Content-Type": "application/json" }, credentials: "include", body: JSON.stringify({}) });
    if (!first.ok || !applyReconciliationResponse(await first.json())) throw new Error("check unavailable");
    complete = reconciliationIsTerminal(currentReconciliation());
    for (let attempt = 0; !complete && attempt < limit; attempt++) {
      await new Promise((resolve) => setTimeout(resolve, delay));
      const status = await fetch("/api/recon/status", { credentials: "include" });
      if (!status.ok || !applyReconciliationResponse(await status.json())) throw new Error("status unavailable");
      complete = reconciliationIsTerminal(currentReconciliation());
    }
    outcome.state = complete ? reconciliationCompletionCopy(currentReconciliation()) : "Canary is still checking. This screen will update when it finishes.";
    return true;
  } catch {
    outcome.state = "The report could not be checked right now. Try again.";
    outcome.error = true;
    return false;
  } finally {
    outcome.busy = false;
    renderReconciliationCard();
  }
}

function applyReconciliationResponse(value) {
  const candidate = value && typeof value === "object" && !Array.isArray(value) && Object.prototype.hasOwnProperty.call(value, "reconciliation") ? value.reconciliation : value;
  const reconciliation = validateReconciliation(candidate);
  if (!reconciliation) return false;
  state.snapshot = { ...(state.snapshot || {}), nudges: { ...(state.snapshot?.nudges || {}), reconciliation } };
  renderReconciliationCard(reconciliation);
  return true;
}

function reconciliationIsTerminal(value) {
  return Boolean(value && !["due", "checking"].includes(value.report.state) && value.evaluation.state !== "checking");
}

function reconciliationCompletionCopy(value) {
  if (!value) return "Latest report status is unavailable.";
  if (value.report.state === "current" && value.evaluation.state === "complete") return "Latest report check completed.";
  if (value.report.state === "retry_scheduled") return "Automatic retry scheduled. You can check again sooner if needed.";
  if (value.report.state === "action_required") return "Follow the steps above, then check again.";
  if (["attention_required", "failed"].includes(value.evaluation.state)) return "Review the steps above, then check again.";
  return "Latest report status updated.";
}

function renderNotificationTest() {
  const outcome = state.safeNotificationTest;
  $("safeNotificationTestButton").disabled = outcome.busy;
  $("safeNotificationTestStatus").textContent = outcome.busy ? "Safe notification test pending." : outcome.state;
  $("safeNotificationTestStatus").classList.toggle("governance-action-status--error", outcome.error);
}

async function sendSafeNotificationTest() {
  const outcome = state.safeNotificationTest;
  if (outcome.busy) return false;
  Object.assign(outcome, { busy: true, state: "", error: false });
  renderNotificationTest();
  try {
    const res = await fetch("/api/push/test", { method: "POST", headers: { "Content-Type": "application/json" }, credentials: "include", body: JSON.stringify({}) });
    const body = res.ok ? await res.json() : {};
    const transport = transportStates.has(body.state) ? body.state : "";
    if (!res.ok || !transport) throw new Error("notification unavailable");
    if (body.push_service_accepted === true && transport === "push_service_accepted") outcome.state = "Push-service accepted.";
    else if (body.push_service_accepted === true && transport === "partial_acceptance") outcome.state = "Partial push-service acceptance.";
    else if (transport === "suppressed") outcome.state = "Safe notification test suppressed.";
    else throw new Error("notification failed");
    return true;
  } catch {
    outcome.state = "Safe notification test unavailable.";
    outcome.error = true;
    return false;
  } finally {
    outcome.busy = false;
    renderNotificationTest();
  }
}

async function enablePush() {
  if (!canUseWebPush()) {
    state.pushInspection.state = "unsupported";
    renderAlertMode();
    return false;
  }
  state.pushInspection.busy = true;
  try {
    if (await globalThis.Notification.requestPermission() !== "granted") return false;
    const registration = await navigator.serviceWorker.ready;
    const subscription = await registration.pushManager.getSubscription() || await registration.pushManager.subscribe({ userVisibleOnly: true, applicationServerKey: b64urlToBytes(state.vapidPublicKey) });
    const res = await fetch("/api/push/subscribe", { method: "POST", headers: { "Content-Type": "application/json" }, credentials: "include", body: JSON.stringify(subscription) });
    if (!res.ok) throw new Error("subscription unavailable");
    return true;
  } catch {
    state.pushInspection.state = "status unavailable";
    return false;
  } finally {
    state.pushInspection.busy = false;
    await refreshPushState();
  }
}

async function refreshPushState() {
  let label = "status unavailable";
  if (!canUseWebPush()) label = "unsupported";
  else if (globalThis.Notification.permission === "denied") label = "permission blocked";
  else if (globalThis.Notification.permission !== "granted") label = "permission not granted";
  else {
    try {
      const subscription = await (await navigator.serviceWorker.ready).pushManager.getSubscription();
      label = subscription ? "browser subscribed" : "permission granted but not subscribed";
    } catch { /* keep unavailable */ }
  }
  state.pushInspection.state = label;
  renderAlertMode();
  return label;
}

function safeDate(value) {
  if (typeof value !== "string" || !/^\d{4}-\d{2}-\d{2}$/.test(value)) return "";
  const parsed = new Date(`${value}T12:00:00Z`);
  return Number.isFinite(parsed.getTime()) && parsed.toISOString().slice(0, 10) === value ? value : "";
}

function safeTime(value) { return typeof value === "string" && Number.isFinite(Date.parse(value)) ? value : ""; }
function dateLabel(value) { return new Date(`${value}T12:00:00Z`).toLocaleDateString(undefined, { month: "short", day: "numeric", year: "numeric", timeZone: "UTC" }); }
function timeLabel(value) { return new Date(value).toLocaleString(undefined, { month: "short", day: "numeric", hour: "2-digit", minute: "2-digit" }); }
function notificationStateLabel() { return state.pushInspection.state; }
function hasNotifications() { return typeof globalThis.Notification === "function"; }
function canUseWebPush() { return hasNotifications() && "PushManager" in globalThis && !!navigator.serviceWorker; }

export {
  applyReconciliationResponse, canUseWebPush, enablePush, hasNotifications, notificationStateLabel,
  reconciliationIsTerminal, reconciliationView, refreshPushState, renderAlertMode, renderReconciliationCard,
  sendReconciliationCheck, sendSafeNotificationTest, setAlertMode, validateAlertSettings, validateReconciliation,
};
