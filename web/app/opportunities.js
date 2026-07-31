import { renderAll } from "./app.js";
import { protectionEmptyRow } from "./protection-coverage.js";
import { goDurationMinutes, protectionContractLabel } from "./protection.js";
import { $, blockerText, hasNumericValue, labelize, money, normalizeSymbol, numberRead, renderFreshnessTimestamp, shortPreviewMessage } from "./shared.js";
import { state } from "./state.js";

// Opportunities are exception-shaped: no standing surface. The advisory bar
// exists only while the daemon serves exercises, and it exists at the top of
// the Protection sheet — the one place a reduce-only desk acts on option
// positions. A clean book renders nothing at all, anywhere.
function renderOpportunitiesPanel(opportunities = {}) {
  const panel = $("opportunitiesPanel");
  if (!panel) return;
  const detail = $("opportunitiesDetailPanel");
  const toggle = $("opportunitiesToggle");
  const rows = opportunities.opportunities || [];
  const counts = opportunities.counts || {};
  const total = counts.total ?? rows.length ?? 0;
  panel.hidden = total <= 0;
  panel.dataset.open = String(state.opportunitiesOpen);
  detail.hidden = !state.opportunitiesOpen;
  toggle.textContent = state.opportunitiesOpen ? "Hide opportunities" : "Show opportunities";
  toggle.setAttribute("aria-expanded", String(state.opportunitiesOpen));
  renderOpportunitiesTimestamp(opportunities);
  $("opportunitiesCount").textContent = String(total);
  const gainCurrency = counts.expected_gain_currency || rows.find((row) => row.expected_gain_currency)?.expected_gain_currency || "";
  const gainEl = $("opportunitiesExpectedGain");
  const hasGain = total > 0 && hasNumericValue(counts.expected_gain);
  gainEl.textContent = hasGain ? money(counts.expected_gain, gainCurrency) : "--";
  gainEl.title = hasGain && !gainCurrency
    ? "Opportunity gains span mixed or unknown currencies; the sum is shown without a currency label."
    : "";
  const refresh = $("opportunitiesRefreshButton");
  refresh.disabled = state.opportunitySnapshotBusy;
  refresh.title = state.opportunitySnapshotBusy ? "Refreshing opportunities" : "Refresh opportunity snapshot";
  const reason = opportunityReason(opportunities);
  const refreshReason = opportunitySnapshotRefreshReason();
  const reasonText = [reason, refreshReason].filter(Boolean).join(" · ");
  const reasonEl = $("opportunitiesReason");
  reasonEl.textContent = reasonText;
  reasonEl.hidden = !reasonText;
  if (!state.opportunitiesOpen) return;
  $("opportunitiesRows").replaceChildren(...(rows.length > 0
    ? rows.map(opportunityRow)
    : [protectionEmptyRow("No option exercise opportunities.")]));
  if (opportunityNeedsSnapshotSync(opportunities)) {
    queueOpportunitySnapshotSync();
  }
}

function renderOpportunitiesTimestamp(opportunities = {}) {
  const cadence = goDurationMinutes(opportunities.status?.refresh_cadence) ?? 2;
  const staleMinutes = Math.ceil(cadence + Math.max(3, cadence / 3));
  renderFreshnessTimestamp("opportunitiesAsOf", opportunities.as_of, { staleMinutes, quietWhenFresh: true });
}

function opportunityReason(opportunities = {}) {
  const blocker = (opportunities.blockers || opportunities.status?.blockers || opportunities.policy_status?.blockers || [])[0];
  if (blocker) return blockerText(blocker);
  const policy = opportunities.policy_status || opportunities.status?.policy || {};
  if (policy.status && policy.status !== "active" && policy.status !== "default") {
    return `Policy ${policy.status}${policy.policy_id ? ` · ${policy.policy_id} v${policy.policy_version || "--"}` : ""}`;
  }
  return "";
}

function opportunitySnapshotRefreshReason() {
  if (state.opportunitySnapshotBusy) return "Refreshing opportunities";
  return state.opportunitySnapshotNotice || "";
}

function opportunityNeedsSnapshotSync(opportunities = {}) {
  if (!state.opportunitiesOpen || state.opportunitySnapshotBusy) return false;
  if (opportunities.status?.enabled === false) return false;
  const policyStatus = String(opportunities.policy_status?.status || opportunities.status?.policy?.status || "").toLowerCase();
  if (policyStatus === "drift" || policyStatus === "error") return false;
  const revision = String(opportunities.revision || "");
  const blockerCodes = [
    ...(opportunities.blockers || []),
    ...(opportunities.status?.blockers || []),
  ].map((blocker) => String(blocker.code || ""));
  if (blockerCodes.some(opportunityTransientSnapshotBlocker)) return true;
  if ((opportunities.opportunities || []).length > 0) return false;
  return !revision || revision === "empty";
}

function opportunityTransientSnapshotBlocker(code = "") {
  return [
    "account_unavailable",
    "positions_unavailable",
    "positions_pending",
    "trading_status_unavailable",
  ].includes(code);
}

function queueOpportunitySnapshotSync() {
  const now = Date.now();
  if (state.opportunitySnapshotBusy || now - state.opportunitySnapshotLastAt < 10000) return;
  state.opportunitySnapshotBusy = true;
  state.opportunitySnapshotLastAt = now;
  state.opportunitySnapshotNotice = "";
  setTimeout(() => {
    syncOpportunitySnapshot();
  }, 0);
}

async function syncOpportunitySnapshot() {
  try {
    const res = await fetch("/api/opportunities", { credentials: "include", cache: "no-store" });
    if (!res.ok) throw new Error(await res.text());
    const opportunities = await res.json();
    applyOpportunitySnapshot(opportunities);
    const count = opportunities.counts?.total ?? (opportunities.opportunities || []).length;
    state.opportunitySnapshotNotice = count > 0 ? "" : "No exercise opportunities available yet";
  } catch (err) {
    state.opportunitySnapshotNotice = "Opportunity refresh failed: " + shortPreviewMessage(err.message);
  } finally {
    state.opportunitySnapshotBusy = false;
    renderAll();
  }
}

function opportunityRow(opportunity) {
  const row = document.createElement("div");
  row.className = "opportunity-row";
  const blocked = (opportunity.blockers || []).length > 0;
  const previewKey = opportunityPreviewStateKey(opportunity);
  const previewBusy = state.opportunityPreviewBusy === previewKey;
  const previewResult = state.opportunityPreviews[previewKey] || null;
  const previewGate = opportunityPreviewGate(opportunity);
  const submitGate = opportunitySubmitGate(opportunity, previewResult);

  const copy = document.createElement("div");
  copy.className = "opportunity-row__copy";
  const bucket = document.createElement("span");
  bucket.className = "opportunity-row__bucket";
  bucket.textContent = opportunityBucketLabel(opportunity);
  const title = document.createElement("b");
  title.className = "opportunity-row__title";
  title.textContent = opportunityTitle(opportunity);
  copy.append(bucket, title);
  const metrics = opportunityMetricRow(opportunity);
  if (metrics) copy.append(metrics);
  const blockerText = blocked ? opportunityBlockerText(opportunity.blockers) : "";
  if (blockerText) {
    const blocker = document.createElement("small");
    blocker.className = "opportunity-row__blocker";
    blocker.textContent = blockerText;
    copy.append(blocker);
  }
  const previewText = opportunityPreviewText(previewResult);
  if (previewText) {
    const preview = document.createElement("small");
    preview.className = "opportunity-row__preview";
    preview.textContent = previewText;
    copy.append(preview);
  }
  const submitState = document.createElement("small");
  submitState.className = opportunitySubmitStateClass({ gate: submitGate });
  submitState.textContent = `Exercise submission unavailable · ${submitGate.reason}`;
  copy.append(submitState);
  const actions = document.createElement("div");
  actions.className = "opportunity-row__actions";
  const preview = document.createElement("button");
  preview.type = "button";
  preview.className = "opportunity-preview";
  preview.textContent = previewBusy ? "Reviewing" : "Review";
  preview.disabled = blocked || previewBusy || !previewGate.ready;
  preview.title = blocked ? opportunityBlockerText(opportunity.blockers) : previewGate.reason;
  preview.addEventListener("click", () => previewOpportunityExercise(opportunity));
  actions.append(preview);
  const ignore = document.createElement("button");
  ignore.type = "button";
  ignore.className = "opportunity-ignore";
  ignore.textContent = "Ignore";
  ignore.title = "Ignore this opportunity; no broker instruction is sent";
  ignore.addEventListener("click", () => ignoreOpportunity(opportunity));
  actions.append(ignore);
  row.append(copy, actions);
  return row;
}

function opportunityBucketLabel(opportunity = {}) {
  if (opportunity.bucket === "option_exercise") return "Option exercise";
  return labelize(opportunity.bucket || "--");
}

function opportunityTitle(opportunity = {}) {
  return [
    "Exercise candidate",
    opportunity.quantity || 0,
    opportunity.symbol || "--",
    protectionContractLabel(opportunity.contract || {}),
  ].filter(Boolean).join(" ");
}

function opportunityMetricRow(opportunity = {}) {
  const metrics = [];
  const currency = opportunity.expected_gain_currency || "";
  if (hasNumericValue(opportunity.expected_gain)) {
    metrics.push(["gain", `gain ${money(opportunity.expected_gain, currency)}`]);
  }
  if (hasNumericValue(opportunity.intrinsic_value)) {
    metrics.push(["intrinsic", `intrinsic ${money(opportunity.intrinsic_value, currency)}`]);
  }
  if (hasNumericValue(opportunity.close_value)) {
    metrics.push(["close", `close ${money(opportunity.close_value, currency)}`]);
  }
  const effect = String(opportunity.position_effect || "").trim();
  if (effect) metrics.push(["effect", `effect ${labelize(effect)}`]);
  const postExerciseRisk = opportunityPostExerciseRiskMetrics(opportunity);
  metrics.push(...postExerciseRisk);
  if (metrics.length === 0) return null;
  const wrap = document.createElement("small");
  wrap.className = "opportunity-row__metrics";
  for (const [kind, text] of metrics) {
    const item = document.createElement("span");
    item.className = `opportunity-row__metric${kind === "gain" ? " opportunity-row__metric--gain" : ""}${kind === "review" || kind === "risk" ? " opportunity-row__metric--risk" : ""}`;
    item.textContent = text;
    wrap.append(item);
  }
  return wrap;
}

function opportunityPostExerciseRiskMetrics(opportunity = {}) {
  const risk = opportunity.post_exercise_risk || null;
  if (!risk) return [];
  const metrics = [];
  const underlying = normalizeSymbol(risk.underlying || opportunity.underlying_contract?.symbol || opportunity.symbol || "");
  metrics.push(["exposure", `${underlying || "underlying"} ${numberRead(risk.before_quantity)}→${numberRead(risk.after_quantity)} shares`]);
  const riskChange = opportunityPostExerciseRiskChangeLabel(risk);
  if (riskChange) metrics.push(["risk", riskChange]);
  if (risk.protection_review_needed) {
    metrics.push(["review", "protection review"]);
  } else if (risk.protection_coverage_state) {
    metrics.push(["coverage", `coverage ${labelize(risk.protection_coverage_state)}`]);
  }
  return metrics;
}

function opportunityPostExerciseRiskChangeLabel(risk = {}) {
  if (risk.risk_opened) return "risk opened";
  if (risk.risk_increased) return "risk increased";
  if (risk.risk_flipped) return "risk flipped";
  const change = String(risk.risk_change || "").toLowerCase();
  if (change === "reduced") return "risk reduced";
  if (change === "closed") return "risk closed";
  return change && change !== "unknown" ? `risk ${labelize(change)}` : "";
}

function opportunityPreviewGate(opportunity = {}) {
	const blocker = (opportunity.blockers || [])[0];
	if (blocker) return { ready: false, reason: `${blocker.code}: ${blocker.message}` };
	return { ready: true, reason: "Review the current exercise evidence; no token is minted and no broker instruction is sent" };
}

function opportunitySubmitGate(opportunity = {}, previewResult = null) {
	void opportunity;
	void previewResult;
	return {
		ready: false,
		reason: "Use TWS after reviewing the resulting position; exact option-to-underlying risk policy and durable one-shot authority are not approved",
	};
}

function opportunityPreviewStateKey(opportunity = {}) {
	return `${opportunity.key || ""}@${opportunity.revision || ""}`;
}

function opportunityPreviewText(result = null) {
  if (!result) return "";
  if (result.local && result.pending) return "Reviewing exercise evidence; no broker instruction is sent";
  if (result.pending) return "Reviewing exercise evidence; no broker instruction is sent";
  const blocker = (result.blockers || [])[0];
  if (blocker) return `Review blocked · ${blocker.code}: ${blocker.message}`;
  return "Exercise review returned · submission remains unavailable";
}

function opportunitySubmitStateText({ result = null, gate = {}, busy = false, previewResult = null } = {}) {
  if (busy) return "Exercise submission unavailable";
  if (result) return opportunitySubmitResultText(result);
  if (previewResult?.pending) return "";
  return `Exercise submission unavailable · ${gate.reason || opportunitySubmitGate().reason}`;
}

function opportunitySubmitStateClass({ result = null, gate = {}, busy = false } = {}) {
  const classes = ["opportunity-row__submit-state"];
  if (busy || result?.accepted) {
    classes.push("opportunity-row__submit-state--ready");
  } else if (result?.blockers?.length || (gate && gate.ready === false)) {
    classes.push("opportunity-row__submit-state--blocked");
  }
  return classes.join(" ");
}

function opportunitySubmitResultText(result = {}) {
  const blocker = (result.blockers || [])[0];
  if (blocker) return `Exercise submission unavailable · ${blocker.code}: ${blocker.message}`;
  return "Exercise submission unavailable · use TWS after reviewing the resulting position";
}

function opportunityBlockerText(blockers = []) {
  if (blockers.length === 0) return "Opportunity is blocked";
  return blockers.map(blockerText).join("; ");
}

function opportunityPreviewStale(result = {}, opportunity = {}) {
  return Boolean(result.opportunity?.revision && opportunity.revision && result.opportunity.revision !== opportunity.revision);
}

async function previewOpportunityExercise(opportunity) {
  const previewKey = opportunityPreviewStateKey(opportunity);
  state.opportunityPreviewBusy = previewKey;
  state.opportunityPreviews = {
    ...state.opportunityPreviews,
    [previewKey]: { local: true, pending: true, opportunity, as_of: new Date().toISOString() },
  };
  renderOpportunitiesPanel(state.snapshot?.opportunities || {});
  try {
    const res = await fetch("/api/opportunities/preview-exercise", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      credentials: "include",
      body: JSON.stringify({
        key: opportunity.key,
        revision: opportunity.revision,
        quantity: opportunity.quantity,
        timeout_ms: opportunityPreviewTimeoutMs(opportunity),
      }),
    });
    if (!res.ok) throw new Error(await res.text());
    const result = await res.json();
    state.opportunityPreviews = {
      ...state.opportunityPreviews,
      [previewKey]: result,
    };
  } catch (err) {
    state.opportunityPreviews = {
      ...state.opportunityPreviews,
      [previewKey]: {
        blockers: [{ code: "preview_failed", message: err.message }],
        as_of: new Date().toISOString(),
      },
    };
  } finally {
    if (state.opportunityPreviewBusy === previewKey) state.opportunityPreviewBusy = "";
    renderOpportunitiesPanel(state.snapshot?.opportunities || {});
  }
}

async function submitOpportunityExercise(opportunity) {
  const previewKey = opportunityPreviewStateKey(opportunity);
  const gate = opportunitySubmitGate(opportunity, state.opportunityPreviews[previewKey] || null);
  state.opportunitySubmits = {
    ...state.opportunitySubmits,
    [previewKey]: {
      blockers: [{ code: "exercise_submission_unavailable", message: gate.reason }],
      as_of: new Date().toISOString(),
    },
  };
  renderOpportunitiesPanel(state.snapshot?.opportunities || {});
}

function opportunityPreviewTimeoutMs() {
  return 5000;
}

async function ignoreOpportunity(opportunity) {
  const res = await fetch("/api/opportunities/ignore", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    credentials: "include",
    body: JSON.stringify({ key: opportunity.key, revision: opportunity.revision }),
  });
  if (!res.ok) throw new Error(await res.text());
  await refreshOpportunities();
}

async function refreshOpportunities() {
  state.opportunitySnapshotBusy = true;
  renderOpportunitiesPanel(state.snapshot?.opportunities || {});
  try {
    const res = await fetch("/api/opportunities/refresh", { method: "POST", credentials: "include" });
    if (!res.ok) throw new Error(await res.text());
    const opportunities = await res.json();
    applyOpportunitySnapshot(opportunities);
    state.opportunitySnapshotNotice = "";
    renderAll();
  } catch (err) {
    state.opportunitySnapshotNotice = "Opportunity refresh failed: " + shortPreviewMessage(err.message);
    renderOpportunitiesPanel(state.snapshot?.opportunities || {});
  } finally {
    state.opportunitySnapshotBusy = false;
    renderOpportunitiesPanel(state.snapshot?.opportunities || {});
  }
}

function applyOpportunitySnapshot(opportunities = {}) {
  state.snapshot = {
    ...(state.snapshot || {}),
    opportunities,
    trading: opportunities.trading || state.snapshot?.trading,
  };
}

export { applyOpportunitySnapshot, ignoreOpportunity, opportunityBlockerText, opportunityBucketLabel, opportunityMetricRow, opportunityNeedsSnapshotSync, opportunityPostExerciseRiskChangeLabel, opportunityPostExerciseRiskMetrics, opportunityPreviewGate, opportunityPreviewStale, opportunityPreviewStateKey, opportunityPreviewText, opportunityPreviewTimeoutMs, opportunityReason, opportunityRow, opportunitySnapshotRefreshReason, opportunitySubmitGate, opportunitySubmitResultText, opportunitySubmitStateClass, opportunitySubmitStateText, opportunityTitle, opportunityTransientSnapshotBlocker, previewOpportunityExercise, queueOpportunitySnapshotSync, refreshOpportunities, renderOpportunitiesPanel, renderOpportunitiesTimestamp, submitOpportunityExercise, syncOpportunitySnapshot };
