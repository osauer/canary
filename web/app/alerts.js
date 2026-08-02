import { b64urlToBytes } from "./auth.js";
import { $, shortTime } from "./shared.js";
import { state } from "./state.js";

const alertModes = new Set(["none", "act_only", "watch_and_act"]);

function exactKeys(value, expected) {
  if (!value || typeof value !== "object" || Array.isArray(value)) return false;
  const actual = Object.keys(value).sort();
  const wanted = [...expected].sort();
  return actual.length === wanted.length && actual.every((key, index) => key === wanted[index]);
}

const reconciliationReportStates = new Set(["waiting", "due", "checking", "current", "retry_scheduled", "action_required", "unavailable"]);
const reconciliationEvaluationStates = new Set(["waiting", "checking", "complete", "attention_required", "failed"]);
const reconciliationReportReasons = new Set([
  "", "none", "before_daily_window", "coverage_pending", "report_not_ready", "service_busy", "rate_limited",
  "network_unavailable", "flex_disabled", "query_missing", "token_missing", "token_invalid", "token_expired",
  "query_invalid", "ip_restricted", "service_inactive", "response_invalid", "report_invalid", "storage_failed",
  "projection_failed", "authority_unavailable",
]);
const reconciliationEvaluationReasons = new Set([
  "", "none", "report_pending", "account_value_pending", "exceptions_need_review", "account_value_mismatch",
  "evaluation_failed", "policy_unapproved",
]);

function validateReconciliation(value) {
  if (!value || typeof value !== "object" || Array.isArray(value)) return null;
  const report = value.report;
  const evaluation = value.evaluation;
  if (!report || typeof report !== "object" || Array.isArray(report) || !reconciliationReportStates.has(report.state)) return null;
  if (!evaluation || typeof evaluation !== "object" || Array.isArray(evaluation) || !reconciliationEvaluationStates.has(evaluation.state)) return null;
  const reportReason = typeof report.reason === "string" ? report.reason : "";
  const evaluationReason = typeof evaluation.reason === "string" ? evaluation.reason : "";
  if (!reconciliationReportReasons.has(reportReason) || !reconciliationEvaluationReasons.has(evaluationReason)) return null;
  return {
    report: {
      state: report.state,
      reason: reportReason,
      expected_coverage_to: safeReconciliationDate(report.expected_coverage_to),
      coverage_to: safeReconciliationDate(report.coverage_to),
      last_attempt_at: safeReconciliationTime(report.last_attempt_at),
      last_completed_at: safeReconciliationTime(report.last_completed_at),
      next_attempt_at: safeReconciliationTime(report.next_attempt_at),
      retry_automatic: report.retry_automatic === true,
      can_check_now: report.can_check_now === true,
    },
    evaluation: {
      state: evaluation.state,
      reason: evaluationReason,
    },
  };
}

function validateGovernanceResponse(value) {
  if (!value || typeof value !== "object" || Array.isArray(value)) return null;
  if (!Array.isArray(value.candidates) || !Array.isArray(value.occurrences)) return null;
  for (const field of ["source_health", "poll_source", "delivery_health", "diagnostic"]) {
    if (!value[field] || typeof value[field] !== "object" || Array.isArray(value[field])) return null;
  }
  const reconciliation = Object.prototype.hasOwnProperty.call(value, "reconciliation")
    ? validateReconciliation(value.reconciliation)
    : null;
  return { ...value, reconciliation };
}

function validateAlertSettings(value) {
  return exactKeys(value, ["mode"]) && alertModes.has(value.mode) ? { mode: value.mode } : null;
}

async function setAlertMode(mode) {
  if (!alertModes.has(mode) || state.alertSettingsUpdate.busy) return false;
  const previous = validateAlertSettings(state.alertSettings) || { mode: "watch_and_act" };
  state.alertSettingsUpdate.busy = true;
  state.alertSettingsUpdate.state = "Saving notification level…";
  state.alertSettingsUpdate.error = false;
  renderAlertMode();
  try {
    const res = await fetch("/api/alerts/settings", {
      method: "PUT",
      headers: { "Content-Type": "application/json" },
      credentials: "include",
      body: JSON.stringify({ mode }),
    });
    if (!res.ok) throw new Error("update unavailable");
    const updated = validateAlertSettings(await res.json());
    if (!updated || updated.mode !== mode) throw new Error("update malformed");
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
}

// The Alerts page is severity-first: the header counts conditions that need
// attention (never all-clear rows), passed checks collapse into one disclosure
// line, and history lives behind a collapsed section. Presence of a row must
// mean something needs the operator.
function governanceStateChip(current, aggregate, pollState, candidateCount, sourceHealth, reconciliation) {
  if (!current) return pollState === "not_observed" ? "Waiting" : "Unavailable";
  if (candidateCount > 0 || aggregate === "degraded") return "Needs you";
  if (aggregate === "suppressed" && sourceHealth?.policy?.reason !== "process_reminders_not_enabled") return "Waiting";
  if (reconciliation) {
    const view = reconciliationView(reconciliation);
    if (sourceHealth?.policy?.reason === "process_reminders_not_enabled" && view.key === "up_to_date") return "Report current";
    return view.label;
  }
  if (aggregate === "ready") return "Up to date";
  if (aggregate === "degraded") return "Needs you";
  return "Waiting";
}

function governanceSummaryCopy(current, aggregate, pollState, candidateCount, sourceHealth, reconciliation) {
  if (!current) {
    if (pollState === "stale") return "Process checks are unavailable because the latest update is late. The last known report status is below.";
    if (pollState === "not_observed") return "Waiting for the first process check. The daily report status is below.";
    return "Process checks are unavailable right now. The last known report status is below.";
  }
  if (candidateCount > 0) {
    return candidateCount === 1 ? "1 process reminder needs review below." : `${candidateCount} process reminders need review below.`;
  }
  if (sourceHealth?.policy?.reason === "process_reminders_not_enabled") {
    return "No process reminders. Reminders are not enabled yet; the daily report status is below.";
  }
  if (reconciliation) {
    const view = reconciliationView(reconciliation);
    if (view.key === "up_to_date") return "No process reminders. The daily report check is up to date.";
    return "No process reminders right now. The daily report status is below.";
  }
  if (aggregate === "ready") return "No process reminders; process data sources are healthy.";
  if (aggregate === "degraded") return "Process data sources are degraded; reminders may be incomplete.";
  return "Process reminders are waiting for the information they need.";
}

const governanceInputNames = ["policy", "reconciliation", "capital", "pins", "cadence", "confirmed_flow"];
const governanceSnapshotRefreshMinInterval = 15000;
const governanceTransportClasses = new Set([
  "push_service_accepted", "partial_acceptance", "all_failed", "no_subscription", "missing_keys",
  "sender_unavailable", "attempt_reserved", "interrupted_uncertain", "target_retired", "deadline_retry",
  "canceled_retry", "transport_retry", "http_retry", "http_rejected", "timeout_retry", "rejected",
  "dead_subscription", "state_write_failure", "recovery", "suppressed", "overflow",
  "producer_observation_rejected",
]);

function renderGovernance() {
  const snapshot = state.snapshot || {};
  const nudges = snapshot.nudges || null;
  const pollSource = snapshot.sources?.nudges || {};
  const governance = state.governance;
  const pollState = safeGovernancePollState(pollSource.state);
  const current = pollState === "current";
  const candidates = current && Array.isArray(nudges?.candidates) ? nudges.candidates : [];
  const aggregate = current ? safeGovernanceAggregate(nudges?.source_health?.aggregate) : "unavailable";
  const reconciliation = validateReconciliation(governance?.reconciliation);

  $("governanceCurrentState").textContent = governanceStateChip(current, aggregate, pollState, candidates.length, nudges?.source_health, reconciliation);
  $("governanceCurrentCount").textContent = current ? String(candidates.length) : "--";
  $("governanceSummary").textContent = governanceSummaryCopy(current, aggregate, pollState, candidates.length, nudges?.source_health, reconciliation);
  renderReconciliationCard(reconciliation);
  $("governanceCurrentBlock").hidden = !current || candidates.length === 0;
  if (!current) {
    renderGovernanceEmpty("governanceCurrentList", "Current risk and process reminders are unavailable.");
  } else if (candidates.length === 0 && aggregate === "ready") {
    renderGovernanceEmpty("governanceCurrentList", "No current risk and process reminders.");
  } else if (candidates.length === 0) {
    renderGovernanceEmpty("governanceCurrentList", "Waiting for required information — an empty list does not mean the checks passed.");
  } else {
    $("governanceCurrentList").replaceChildren(...candidates.map(governanceCandidateElement));
  }

  renderGovernanceSourceHealth(pollSource, nudges?.source_health, current);
  renderGovernanceContext(nudges?.context, current);
  renderGovernanceCoverage(nudges?.confirmed_flow_coverage, current);
  renderGovernanceHistory(governance?.occurrences);
  renderGovernanceDelivery(governance);
  renderGovernanceControlStatus();
}

function renderReconciliationCard(reconciliation) {
  const view = reconciliationView(reconciliation);
  const check = state.reconciliationCheck || { busy: false, state: "", error: false };
  const card = $("reconciliationCard");
  card.dataset.state = view.key;
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
  if (!reconciliation) {
    return {
      key: "unavailable",
      label: "Unavailable",
      title: "Latest report status unavailable",
      summary: "Canary cannot confirm the latest IBKR report right now. It will keep trying automatically.",
    };
  }
  const { report, evaluation } = reconciliation;
  if (state.reconciliationCheck?.busy || report.state === "checking" || evaluation.state === "checking") {
    return {
      key: "checking",
      label: "Checking",
      title: "Checking the latest report",
      summary: "Canary is asking IBKR for the latest daily report and will compare it automatically.",
    };
  }
  if (report.state === "action_required") {
    return {
      key: "needs_you",
      label: "Needs you",
      title: "Fix the report connection",
      summary: reconciliationSetupCopy(report.reason),
    };
  }
  if (report.state === "unavailable") {
    return {
      key: "unavailable",
      label: "Unavailable",
      title: "Latest report unavailable",
      summary: reconciliationUnavailableCopy(report),
    };
  }
  if (report.state === "retry_scheduled") {
    return {
      key: "retrying",
      label: "Retrying",
      title: report.reason === "report_not_ready" && report.coverage_to ? "Report recheck will retry" : report.reason === "report_not_ready" ? "Latest report not ready yet" : "Report check will retry",
      summary: reconciliationRetryCopy(report),
    };
  }
  if (evaluation.state === "attention_required" || evaluation.state === "failed") {
    return {
      key: "needs_you",
      label: "Needs you",
      title: "Report comparison needs attention",
      summary: reconciliationEvaluationCopy(evaluation),
    };
  }
  if (report.state === "current" && evaluation.state === "complete") {
    return {
      key: "up_to_date",
      label: "Up to date",
      title: "Latest report checked",
      summary: "The daily broker report is current and Canary finished the automatic comparison.",
    };
  }
  if (report.state === "current" && evaluation.reason === "account_value_pending") {
    return {
      key: "waiting",
      label: "Waiting",
      title: "Report received",
      summary: "The report is current. Canary is waiting for today's account value before it compares the totals; no action is needed.",
    };
  }
  if (report.state === "due") {
    return {
      key: "waiting",
      label: "Waiting",
      title: "Latest report is due",
      summary: "Canary is ready to check the latest daily report and will do so automatically.",
    };
  }
  return {
    key: "waiting",
    label: "Waiting",
    title: "Waiting for the daily report",
    summary: report.reason === "before_daily_window"
      ? "Canary will check after the daily IBKR report window opens and before your morning report. Nothing needs your attention."
      : "Canary is waiting for the information needed to finish the daily report check.",
  };
}

function reconciliationSetupCopy(reason) {
  if (["token_missing", "token_invalid", "token_expired"].includes(reason)) {
    return "Open IBKR Client Portal → Reporting → Flex Queries on this Mac, renew the Flex Web Service token, replace the local token file (normally ~/.config/ibkr/flex-token), then tap Check again.";
  }
  if (["query_missing", "query_invalid"].includes(reason)) {
    return "Open IBKR Client Portal → Reporting → Flex Queries on this Mac and copy the active Activity Flex Query ID into ~/.config/ibkr/config.toml. Restart Canary, then tap Check again.";
  }
  if (reason === "flex_disabled") {
    return "Open ~/.config/ibkr/config.toml on this Mac, set Flex reports to enabled and add the Activity Flex Query ID. Restart Canary; the check will then run automatically.";
  }
  if (reason === "ip_restricted") {
    return "Allow this Mac's public IP for Flex reports in IBKR Client Portal, then tap Check again.";
  }
  if (reason === "service_inactive") {
    return "Reactivate Flex Web Service in IBKR Client Portal on this Mac, then tap Check again.";
  }
  if (["response_invalid", "report_invalid"].includes(reason)) {
    return "IBKR returned a report Canary could not safely use. Tap Check again; if it repeats, recreate the Flex query in IBKR Client Portal.";
  }
  if (["storage_failed", "projection_failed"].includes(reason)) {
    return "Canary could not save and compare the report on this Mac. Restart Canary, then tap Check again.";
  }
  return "Canary could not complete the report check safely. Tap Check again.";
}

function reconciliationUnavailableCopy(report) {
  const manual = report.can_check_now ? " You can also tap Check again." : "";
  if (report.reason === "authority_unavailable") {
    return "Canary cannot read its local report record. Restart Canary on this Mac. If this remains unavailable, repair the local Canary data store before relying on the report check.";
  }
  if (report.reason === "network_unavailable") return `This Mac could not reach IBKR. Check its internet connection.${manual}`;
  if (["service_busy", "rate_limited"].includes(report.reason)) return `IBKR is temporarily busy. Canary will keep trying automatically.${manual}`;
  return `Canary cannot confirm the latest IBKR report right now. It will keep trying automatically.${manual}`;
}

function reconciliationRetryCopy(report) {
  const next = report.next_attempt_at ? ` at ${reconciliationTimeLabel(report.next_attempt_at)}` : " soon";
  const manual = report.can_check_now ? "; you can also check now." : ".";
  if (report.reason === "report_not_ready" && report.coverage_to) return `Canary still has the report through ${reconciliationDateLabel(report.coverage_to)}. IBKR did not finish the re-read. Canary will try again${next}${manual}`;
  if (report.reason === "report_not_ready") return `IBKR has not published the first usable report yet. Canary will try again${next}${manual}`;
  if (report.reason === "coverage_pending") return `The daily report check has not finished yet. Canary will try again${next}${manual}`;
  if (report.reason === "network_unavailable") return `This Mac could not reach IBKR. Canary will try again${next}${manual}`;
  if (["service_busy", "rate_limited"].includes(report.reason)) return `IBKR asked Canary to wait. Canary will try again${next}${manual}`;
  if (["response_invalid", "report_invalid"].includes(report.reason)) return `IBKR returned a report Canary could not safely use. Canary will retry${next}${manual} If this repeats, recreate the Activity Flex Query in IBKR Client Portal.`;
  if (["storage_failed", "projection_failed"].includes(report.reason)) return `Canary could not save or compare the report on this Mac. It will retry${next}${manual} If this repeats, restart Canary and check that the Mac has free disk space.`;
  return `The report could not be checked. Canary will try again${next}${manual}`;
}

function reconciliationEvaluationCopy(evaluation) {
  if (evaluation.reason === "exceptions_need_review") {
    return "Canary found a cash movement it could not match. Open the morning report on this Mac, review that movement, record or resolve it there, then tap Check again.";
  }
  if (evaluation.reason === "account_value_mismatch") {
    return "The broker report and the account value for that report date do not match. Open the morning report on this Mac, review the figures, then tap Check again.";
  }
  if (evaluation.reason === "policy_unapproved") {
    return "The broker report arrived, but the local comparison settings are incomplete. Review and approve the missing reconciliation setting on this Mac; this is separate from fetching the report.";
  }
  return "Canary could not finish comparing the report. Tap Check again.";
}

function reconciliationMeta(reconciliation) {
  if (!reconciliation) return "";
  const report = reconciliation.report;
  const facts = [];
  if (report.coverage_to) facts.push(`Report through ${reconciliationDateLabel(report.coverage_to)}`);
  if (report.last_completed_at) facts.push(`Last checked ${reconciliationTimeLabel(report.last_completed_at)}`);
  if (report.next_attempt_at && report.retry_automatic) facts.push(`Next automatic try ${reconciliationTimeLabel(report.next_attempt_at)}`);
  return facts.join(" · ");
}

function governanceCandidateElement(candidate = {}) {
  const row = document.createElement("div");
  row.className = `governance-row governance-row--${candidate.severity === "act" ? "act" : candidate.severity === "watch" ? "watch" : "unknown"}`;
  const copy = document.createElement("div");
  const title = document.createElement("b");
  title.textContent = typeof candidate.title === "string" ? candidate.title : "";
  const body = document.createElement("p");
  body.textContent = typeof candidate.body === "string" ? candidate.body : "";
  const meta = document.createElement("span");
  const severity = candidate.severity === "act" ? "needs action" : candidate.severity === "watch" ? "review" : "";
  const destination = candidate.destination === "brief" ? "morning report" : candidate.destination === "alerts" ? "Alerts" : candidate.destination === "monitor" ? "Monitor" : "";
  meta.textContent = [severity, destination].filter(Boolean).join(" · ");
  copy.append(title, body);
  row.append(copy, meta);
  return row;
}

function renderGovernanceSourceHealth(pollSource = {}, sourceHealth = {}, current) {
  const target = $("governanceSourceHealth");
  const pollStateKnown = ["current", "stale", "not_observed", "unavailable"].includes(pollSource.state);
  const pollState = safeGovernancePollState(pollSource.state);
  const pollReason = !pollStateKnown
    ? "invalid_health"
    : typeof pollSource.reason === "string" && pollSource.reason
      ? safeGovernanceReason(pollSource.reason, "invalid_health")
      : "";
  const pollFacts = [governancePollStateCopy(pollState)];
  if (pollReason) pollFacts.push(governanceReasonCopy(pollReason));
  if (pollSource.updated_at) pollFacts.push(`updated ${governanceTime(pollSource.updated_at)}`);
  if (pollSource.last_success_at) pollFacts.push(`last successful ${governanceTime(pollSource.last_success_at)}`);
  if (!current) {
    target.textContent = pollFacts.join(" · ");
    return;
  }
  const aggregate = safeGovernanceAggregate(sourceHealth?.aggregate);
  const aggregateCopy = aggregate === "ready" ? "ready" : aggregate === "degraded" ? "some checks need attention" : "waiting for inputs";
  const parts = [`${aggregateCopy} · latest update ${pollFacts.join(" · ")}`];
  // Healthy inputs collapse to one line; only inputs that are not ok earn a row.
  const unhealthy = [];
  for (const name of governanceInputNames) {
    const input = sourceHealth?.[name] || {};
    const status = ["ok", "inactive", "unapproved", "stale", "unavailable", "error"].includes(input.status) ? input.status : "error";
    if (status === "ok") continue;
    const reason = safeGovernanceReason(input.reason, "invalid_health");
    const asOf = input.as_of ? ` · ${governanceTime(input.as_of)}` : "";
    unhealthy.push(`${governanceInputCopy(name)}: ${governanceInputStatusCopy(status)}${reason ? ` · ${governanceReasonCopy(reason)}` : ""}${asOf}`);
  }
  parts.push(unhealthy.length === 0 ? "all needed information is ready" : unhealthy.join("\n"));
  target.textContent = parts.join("\n");
}

function governanceInputCopy(name) {
  return ({
    policy: "Reminders",
    reconciliation: "Daily report",
    capital: "Account value",
    pins: "Saved approvals",
    cadence: "Schedule",
    confirmed_flow: "Payment records",
  })[name] || "Input";
}

function governanceInputStatusCopy(status) {
  return ({
    inactive: "not enabled",
    unapproved: "not enabled",
    stale: "out of date",
    unavailable: "unavailable",
    error: "could not be checked",
  })[status] || "could not be checked";
}

function governancePollStateCopy(value) {
  return ({
    current: "current",
    stale: "out of date",
    not_observed: "waiting for first check",
    unavailable: "unavailable",
  })[value] || "unavailable";
}

function governanceReasonCopy(value) {
  return ({
    not_observed: "not checked yet",
    poll_stale: "latest update is late",
    transport_unavailable: "the Mac could not reach the service",
    policy_unapproved: "reminders are not enabled",
    process_reminders_not_enabled: "reminders are not enabled",
    cadence_unapproved: "the automatic schedule is not enabled",
    evidence_stale: "information is out of date",
    source_unavailable: "information unavailable",
    evaluation_error: "check failed",
    coverage_unavailable: "payment history unavailable",
    cutover_review_required: "one-time review needed",
    invalid_health: "details unavailable",
  })[value] || "details unavailable";
}

function renderGovernanceContext(context, current) {
  const target = $("governanceContext");
  if (!current || !context) {
    target.textContent = "Extra warning details unavailable.";
    return;
  }
  const parts = [];
  if (context.shadow && Number.isFinite(context.shadow.count)) {
    parts.push(`Warning-only observations ${Math.trunc(context.shadow.count)}`);
  }
  if (context.drawdown) {
    const tier = context.drawdown.tier === "block" ? "limit reached" : "unavailable";
    const used = context.drawdown.consumed_pct === null || !Number.isFinite(context.drawdown.consumed_pct)
      ? "measurement unavailable"
      : `${context.drawdown.consumed_pct.toFixed(1)}% used`;
    parts.push(`Drawdown ${tier} · ${used}`);
  }
  target.textContent = parts.length > 0 ? parts.join(" · ") : "No extra warning details.";
}

// The coverage block surfaces inline only while it needs the operator (the
// one-time cutover review); the reviewed state stays available in the
// evidence disclosure.
function renderGovernanceCoverage(coverage, current) {
  const block = $("governanceCoverageBlock");
  const target = $("governanceCoverage");
  const detail = $("governanceCoverageDetail");
  const button = $("governanceCutoverReviewButton");
  const unresolved = current && coverage?.pre_cutover_flows_unreviewed === true;
  button.hidden = !unresolved;
  if (!current || !coverage?.coverage_from) {
    block.hidden = true;
    target.textContent = "Payment history unavailable.";
    detail.textContent = "Payment history unavailable.";
    return;
  }
  block.hidden = !unresolved && !state.governanceCutoverReview.state;
  target.textContent = unresolved ? "Older payments need a one-time review." : "Older payment history is reviewed.";
  detail.textContent = `Checked from ${governanceTime(coverage.coverage_from)} · older payments ${unresolved ? "need review" : "reviewed"}`;
}

function renderGovernanceHistory(occurrences) {
  const rows = Array.isArray(occurrences) ? occurrences : [];
  $("governanceHistoryCount").textContent = String(rows.length);
  if (rows.length === 0) {
    renderGovernanceEmpty("governanceHistoryList", state.governance === null ? "Governance history not observed." : "No governance history recorded.");
    return;
  }
  $("governanceHistoryList").replaceChildren(...rows.map(governanceOccurrenceElement));
}

function governanceOccurrenceElement(occurrence = {}) {
  const row = governanceCandidateElement(occurrence);
  row.classList.add("governance-row--history");
  const status = document.createElement("small");
  const lifecycle = governanceOccurrenceLifecycle(occurrence);
  const at = lifecycle === "resolved" ? occurrence.resolved_at : occurrence.occurred_at;
  status.textContent = `${lifecycle} · ${governanceTime(at)}`;
  row.append(status);
  return row;
}

function governanceOccurrenceLifecycle(occurrence = {}) {
  return occurrence.resolved_at ? "resolved" : "active";
}

function renderGovernanceDelivery(governance) {
  const health = governance?.delivery_health || {};
  const healthState = ["healthy", "suppressed", "degraded", "unavailable", "overflow"].includes(health.state) ? health.state : "unavailable";
  const healthClass = safeGovernanceTransportClass(health.class);
  const healthAt = governanceTimestamp(health.updated_at);
  const lastKnown = healthClass ? `${healthState} · ${healthClass}` : healthState;
  if (!healthAt) {
    $("governanceDeliveryHealth").textContent = "unavailable · updated not observed";
  } else if (state.governanceRefreshSucceeded === false) {
    $("governanceDeliveryHealth").textContent = `retained · refresh unavailable · last known ${lastKnown} · updated ${governanceTime(health.updated_at)}`;
  } else {
    $("governanceDeliveryHealth").textContent = `${lastKnown} · updated ${governanceTime(health.updated_at)}`;
  }

  const diagnostic = governance?.diagnostic || {};
  const diagnosticState = safeGovernanceTransportClass(diagnostic.state) || "not_observed";
  const lastAccepted = health.last_push_service_acceptance_at
    ? `last push-service acceptance ${governanceTime(health.last_push_service_acceptance_at)}`
    : "last push-service acceptance not observed";
  $("governanceDeliveryDetail").textContent = [
    lastAccepted,
    `diagnostic ${diagnosticState}${diagnostic.at ? ` · ${governanceTime(diagnostic.at)}` : ""}`,
  ].join("\n");
}

function renderGovernanceControlStatus() {
  const safeTest = state.safeNotificationTest;
  $("safeNotificationTestButton").disabled = safeTest.busy;
  $("safeNotificationTestStatus").textContent = safeTest.busy ? "Safe notification test pending." : safeTest.state;
  $("safeNotificationTestStatus").classList.toggle("governance-action-status--error", safeTest.error);

  const cutover = state.governanceCutoverReview;
  $("governanceCutoverReviewButton").disabled = cutover.busy;
  $("governanceCutoverReviewStatus").textContent = cutover.busy ? "Saving your review and refreshing the latest status." : cutover.state;
  $("governanceCutoverReviewStatus").classList.toggle("governance-action-status--error", cutover.error);
}

function scheduleGovernanceRefresh(options = {}) {
  if (!state.authenticated) return false;
  const delayMs = Math.max(0, Number(options.delayMs) || 0);
  const minIntervalMs = options.minIntervalMs === undefined
    ? governanceSnapshotRefreshMinInterval
    : Math.max(0, Number(options.minIntervalMs) || 0);
  const now = Date.now();
  const throttleDelay = Math.max(0, minIntervalMs - (now - state.governanceLastRefreshAt));
  const dueAt = now + Math.max(delayMs, throttleDelay);
  const ensureTrailing = options.ensureTrailing === true;
  let timerEnsureTrailing = ensureTrailing;
  if (state.governanceRefreshTimer) {
    timerEnsureTrailing ||= state.governanceRefreshTimerEnsureTrailing;
    state.governanceRefreshTimerEnsureTrailing = timerEnsureTrailing;
    if (state.governanceRefreshDueAt <= dueAt) return true;
    clearTimeout(state.governanceRefreshTimer);
  }
  state.governanceRefreshDueAt = dueAt;
  state.governanceRefreshTimerEnsureTrailing = timerEnsureTrailing;
  state.governanceRefreshTimer = setTimeout(() => {
    const trailing = state.governanceRefreshTimerEnsureTrailing;
    state.governanceRefreshTimer = null;
    state.governanceRefreshDueAt = 0;
    state.governanceRefreshTimerEnsureTrailing = false;
    refreshGovernance({ ensureTrailing: trailing });
  }, Math.max(0, dueAt - Date.now()));
  return true;
}

async function refreshGovernance(options = {}) {
  if (!state.authenticated) return false;
  if (state.governanceRefreshInFlight) {
    if (options.ensureTrailing === true) state.governanceRefreshAfterFlight = true;
    return state.governanceRefreshInFlight;
  }
  state.governanceLastRefreshAt = Date.now();
  state.governanceRefreshInFlight = (async () => {
    try {
      const res = await fetch("/api/governance", { credentials: "include" });
      if (!res.ok) {
        state.governanceRefreshSucceeded = false;
        renderGovernance();
        return false;
      }
      const governance = validateGovernanceResponse(await res.json());
      if (!governance) {
        state.governanceRefreshSucceeded = false;
        renderGovernance();
        return false;
      }
      state.governance = governance;
      state.governanceRefreshSucceeded = true;
      renderGovernance();
      return true;
    } catch {
      state.governanceRefreshSucceeded = false;
      renderGovernance();
      return false;
    } finally {
      state.governanceRefreshInFlight = null;
      if (state.governanceRefreshAfterFlight) {
        state.governanceRefreshAfterFlight = false;
        scheduleGovernanceRefresh({ minIntervalMs: 0 });
      }
    }
  })();
  return state.governanceRefreshInFlight;
}

async function sendReconciliationCheck(options = {}) {
  const outcome = state.reconciliationCheck || (state.reconciliationCheck = { busy: false, state: "", error: false });
  if (outcome.busy || !state.authenticated) return false;
  outcome.busy = true;
  outcome.state = "";
  outcome.error = false;
  renderGovernance();
  const pollDelayMs = options.pollDelayMs === undefined ? 750 : Math.max(0, Number(options.pollDelayMs) || 0);
  const maxPolls = options.maxPolls === undefined ? 20 : Math.max(1, Math.min(40, Number(options.maxPolls) || 1));
  let completed = false;
  try {
    const first = await fetch("/api/recon/check", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      credentials: "include",
      body: JSON.stringify({}),
    });
    if (!first.ok || !applyReconciliationResponse(await first.json())) throw new Error("check unavailable");
    completed = reconciliationIsTerminal(state.governance?.reconciliation);
    for (let attempt = 0; !completed && attempt < maxPolls; attempt++) {
      await new Promise((resolve) => setTimeout(resolve, pollDelayMs));
      const status = await fetch("/api/recon/status", { credentials: "include" });
      if (!status.ok || !applyReconciliationResponse(await status.json())) throw new Error("status unavailable");
      completed = reconciliationIsTerminal(state.governance?.reconciliation);
    }
    outcome.state = completed
      ? reconciliationCompletionCopy(state.governance?.reconciliation)
      : "Canary is still checking. This screen will update when it finishes.";
    return true;
  } catch {
    outcome.state = "The report could not be checked right now. Try again.";
    outcome.error = true;
    return false;
  } finally {
    outcome.busy = false;
    renderGovernance();
    await refreshGovernance();
  }
}

function applyReconciliationResponse(value) {
  const candidate = value && typeof value === "object" && !Array.isArray(value) && Object.prototype.hasOwnProperty.call(value, "reconciliation")
    ? value.reconciliation
    : value;
  const reconciliation = validateReconciliation(candidate);
  if (!reconciliation) return false;
  state.governance = { ...(state.governance || {}), reconciliation };
  renderGovernance();
  return true;
}

function reconciliationIsTerminal(reconciliation) {
  if (!reconciliation) return false;
  if (["due", "checking"].includes(reconciliation.report.state)) return false;
  return reconciliation.evaluation.state !== "checking";
}

function reconciliationCompletionCopy(reconciliation) {
  if (!reconciliation) return "Latest report status is unavailable.";
  if (reconciliation.report.state === "current" && reconciliation.evaluation.state === "complete") return "Latest report check completed.";
  if (reconciliation.report.state === "retry_scheduled") return "Automatic retry scheduled. You can check again sooner if needed.";
  if (reconciliation.report.state === "action_required") return "Follow the steps above, then check again.";
  if (reconciliation.evaluation.state === "attention_required" || reconciliation.evaluation.state === "failed") return "Review the steps above, then check again.";
  if (reconciliation.report.state === "waiting") return "Automatic check is scheduled.";
  return "Latest report status updated.";
}

async function sendSafeNotificationTest() {
  const outcome = state.safeNotificationTest;
  if (outcome.busy) return;
  outcome.busy = true;
  outcome.state = "";
  outcome.error = false;
  renderGovernanceControlStatus();
  try {
    const res = await fetch("/api/push/test", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      credentials: "include",
      body: JSON.stringify({}),
    });
    const body = res.ok ? await res.json() : {};
    if (!res.ok) throw new Error("safe test unavailable");
    const transportState = safeGovernanceTransportClass(body.state);
    if (body.push_service_accepted === true && transportState === "push_service_accepted") {
      outcome.state = "Push-service accepted.";
    } else if (body.push_service_accepted === true && transportState === "partial_acceptance") {
      outcome.state = "Partial push-service acceptance.";
    } else if (transportState === "suppressed") {
      outcome.state = "Safe notification test suppressed.";
    } else {
      outcome.state = `Safe notification test failed${transportState ? ` · ${transportState}` : ""}.`;
      outcome.error = true;
    }
  } catch {
    outcome.state = "Safe notification test unavailable.";
    outcome.error = true;
  } finally {
    outcome.busy = false;
    renderGovernanceControlStatus();
    refreshGovernance();
  }
}

async function sendGovernanceCutoverReview() {
  const outcome = state.governanceCutoverReview;
  if (outcome.busy) return;
  outcome.busy = true;
  outcome.state = "";
  outcome.error = false;
  renderGovernanceControlStatus();
  try {
    const res = await fetch("/api/governance/cutover-review", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      credentials: "include",
      body: JSON.stringify({}),
    });
    const body = res.ok ? await res.json() : {};
    if (!res.ok || !applyGovernanceCutoverReceipt(body)) throw new Error("cutover review unavailable");
    outcome.state = body.already_reviewed === true ? "Older payments were already marked reviewed." : "Older payments marked reviewed.";
    scheduleGovernanceRefresh({ delayMs: 1500, minIntervalMs: 0, ensureTrailing: true });
  } catch {
    outcome.state = "Could not save the older payment review.";
    outcome.error = true;
  } finally {
    outcome.busy = false;
    renderGovernance();
  }
}

function applyGovernanceCutoverReceipt(receipt) {
  if (!receipt || receipt.ok !== true || typeof receipt.already_reviewed !== "boolean" || receipt.evidence !== "paired_device_foreground_render_review") return false;
  const reviewedAt = governanceTimestamp(receipt.reviewed_at);
  const coverageFrom = governanceTimestamp(receipt.coverage_from);
  if (!reviewedAt || !coverageFrom || coverageFrom > reviewedAt || !state.snapshot?.nudges) return false;
  state.governanceCutoverReceipt = {
    reviewed_at: receipt.reviewed_at,
    coverage_from: receipt.coverage_from,
  };
  state.snapshot = {
    ...state.snapshot,
    nudges: {
      ...state.snapshot.nudges,
      confirmed_flow_coverage: {
        coverage_from: receipt.coverage_from,
        pre_cutover_flows_unreviewed: false,
      },
    },
  };
  return true;
}

function applyGovernanceCutoverOverlay(snapshot) {
  const receipt = state.governanceCutoverReceipt;
  if (!receipt || !snapshot?.nudges) return snapshot;
  const reviewedAt = governanceTimestamp(receipt.reviewed_at);
  const authorityAt = governanceTimestamp(snapshot.nudges.as_of);
  if (!reviewedAt || !authorityAt) {
    state.governanceCutoverReceipt = null;
    return snapshot;
  }
  if (snapshot.nudges.confirmed_flow_coverage?.pre_cutover_flows_unreviewed === false && authorityAt >= reviewedAt) {
    state.governanceCutoverReceipt = null;
    return snapshot;
  }
  if (authorityAt > reviewedAt) {
    state.governanceCutoverReceipt = null;
    return snapshot;
  }
  return {
    ...snapshot,
    nudges: {
      ...snapshot.nudges,
      confirmed_flow_coverage: {
        coverage_from: receipt.coverage_from,
        pre_cutover_flows_unreviewed: false,
      },
    },
  };
}

function safeGovernanceAggregate(value) {
  return ["ready", "suppressed", "degraded"].includes(value) ? value : "suppressed";
}

function safeGovernancePollState(value) {
  return ["current", "stale", "not_observed", "unavailable"].includes(value) ? value : "unavailable";
}

function safeGovernanceReason(value, fallback = "invalid_health") {
  const reasons = new Set([
    "", "not_observed", "poll_stale", "transport_unavailable", "policy_unapproved", "cadence_unapproved",
    "evidence_stale", "source_unavailable", "evaluation_error", "coverage_unavailable",
    "cutover_review_required", "process_reminders_not_enabled", "invalid_health",
  ]);
  return reasons.has(value) ? value : fallback;
}

function safeGovernanceTransportClass(value) {
  return governanceTransportClasses.has(value) ? value : "";
}

function safeReconciliationDate(value) {
  if (typeof value !== "string" || !/^\d{4}-\d{2}-\d{2}$/.test(value)) return "";
  const parsed = new Date(`${value}T12:00:00Z`);
  return Number.isFinite(parsed.getTime()) && parsed.toISOString().slice(0, 10) === value ? value : "";
}

function safeReconciliationTime(value) {
  if (typeof value !== "string" || !value) return "";
  return Number.isFinite(Date.parse(value)) ? value : "";
}

function reconciliationDateLabel(value) {
  const safe = safeReconciliationDate(value);
  if (!safe) return "date unavailable";
  const at = new Date(`${safe}T12:00:00Z`);
  return at.toLocaleDateString(undefined, { month: "short", day: "numeric", year: "numeric", timeZone: "UTC" });
}

function reconciliationTimeLabel(value) {
  const safe = safeReconciliationTime(value);
  if (!safe) return "a later time";
  return new Date(safe).toLocaleString(undefined, { month: "short", day: "numeric", hour: "2-digit", minute: "2-digit" });
}

function governanceTime(value) {
  const at = new Date(value);
  if (Number.isNaN(at.getTime())) return "not observed";
  return `${at.getFullYear()}-${String(at.getMonth() + 1).padStart(2, "0")}-${String(at.getDate()).padStart(2, "0")} ${shortTime(value)}`;
}

function governanceTimestamp(value) {
  const timestamp = Date.parse(value || "");
  return Number.isFinite(timestamp) ? timestamp : 0;
}

function renderGovernanceEmpty(id, copy) {
  const empty = document.createElement("div");
  empty.className = "empty-row";
  empty.textContent = copy;
  $(id).replaceChildren(empty);
}

async function enablePush() {
  if (!canUseWebPush()) {
    state.pushInspection.state = "unsupported";
    renderAlertMode();
    return;
  }
  state.pushInspection.busy = true;
  try {
    const permission = await globalThis.Notification.requestPermission();
    if (permission !== "granted") return;
    const registration = await navigator.serviceWorker.ready;
    const existing = await registration.pushManager.getSubscription();
    const subscription = existing || await registration.pushManager.subscribe({
      userVisibleOnly: true,
      applicationServerKey: b64urlToBytes(state.vapidPublicKey),
    });
    const res = await fetch("/api/push/subscribe", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      credentials: "include",
      body: JSON.stringify(subscription),
    });
    if (!res.ok) throw new Error("subscription unavailable");
  } catch {
    state.pushInspection.state = "status unavailable";
  } finally {
    state.pushInspection.busy = false;
    await refreshPushState();
  }
}

function notificationStateLabel() {
  return state.pushInspection.state;
}

async function refreshPushState() {
  let label = "status unavailable";
  if (!canUseWebPush()) {
    label = "unsupported";
  } else if (globalThis.Notification.permission === "denied") {
    label = "permission blocked";
  } else if (globalThis.Notification.permission !== "granted") {
    label = "permission not granted";
  } else {
    try {
      const registration = await navigator.serviceWorker.ready;
      const subscription = await registration.pushManager.getSubscription();
      label = subscription ? "browser subscribed" : "permission granted but not subscribed";
    } catch {
      label = "status unavailable";
    }
  }
  state.pushInspection.state = label;
  renderAlertMode();
  return label;
}

function hasNotifications() {
  return typeof globalThis.Notification === "function";
}

function canUseWebPush() {
  return hasNotifications() && "PushManager" in globalThis && !!navigator.serviceWorker;
}

export { applyGovernanceCutoverOverlay, applyGovernanceCutoverReceipt, applyReconciliationResponse, canUseWebPush, enablePush, governanceOccurrenceLifecycle, hasNotifications, notificationStateLabel, reconciliationIsTerminal, reconciliationView, refreshGovernance, refreshPushState, renderAlertMode, renderGovernance, scheduleGovernanceRefresh, sendGovernanceCutoverReview, sendReconciliationCheck, sendSafeNotificationTest, setAlertMode, validateAlertSettings, validateGovernanceResponse, validateReconciliation };
