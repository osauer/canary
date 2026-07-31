import { $, labelize, money, pct, protectionWriteConfirmation, protectionWriteUnavailableReason, readJSONOrText, renderFreshnessTimestamp } from "./shared.js";
import { state } from "./state.js";

function warningMessages(warnings = []) {
  return warnings.map((warning) => {
    if (!warning) return "";
    if (typeof warning === "string") return warning;
    return warning.message || warning.code || JSON.stringify(warning);
  }).filter(Boolean);
}

function renderOpenOrders() {
  const list = $("ordersOpenList");
  const orders = state.ordersOpen?.orders || [];
  renderFreshnessTimestamp("ordersAsOf", state.ordersOpen?.as_of, { staleMinutes: 15, fallback: "--" });
  const count = $("ordersOpenCount");
  count.textContent = orders.length === 1 ? "1 open" : `${orders.length} open`;
  count.classList.toggle("is-zero", orders.length === 0);
  if (orders.length === 0) {
    const empty = document.createElement("div");
    empty.className = "empty-row";
    empty.textContent = "None working.";
    list.replaceChildren(empty);
    return;
  }
  list.replaceChildren(...orders.map(openOrderRowElement));
}

function openOrderRowElement(order) {
  const row = document.createElement("div");
  row.className = "open-order-row pd-tile pd-order";
  if (order.reconciliation_severity === "critical") row.classList.add("open-order-row--critical");
  // A journal row the desk previewed and never transmitted is advisory, not a
  // working order: it takes the advisory lamp, and nothing else on this face
  // lamps at all. The lamp follows the served lifecycle word.
  if (orderIsPreviewState(order)) {
    row.classList.add("pd-tile--info");
    const bar = document.createElement("span");
    bar.className = "pd-tile__bar";
    bar.setAttribute("aria-hidden", "true");
    row.append(bar);
  }
  const id = orderIdentity(order);
  const edit = openOrderEdit(order);
  const trading = state.snapshot?.trading || {};
  const modifyGate = orderModifyGate(order, trading);
  const cancelGate = orderCancelGate(order, trading);
  if (modifyGate.reduceOnly) {
    // The only legal modify for a mismatched protective order is quantity ->
    // covered position with the trail verbatim; pin the edit state so the
    // preview body cannot drift from that shape (the daemon rejects any
    // other shape server-side regardless).
    edit.quantity = orderReduceToQuantity(order);
    edit.limit_price = order.limit_price > 0 ? order.limit_price : null;
    edit.trailing_percent = order.trail?.trailing_percent > 0 ? order.trail.trailing_percent : null;
    edit.trailing_amount = order.trail?.trailing_amount > 0 ? order.trail.trailing_amount : null;
    edit.initial_stop = order.trail?.initial_stop_price > 0 ? order.trail.initial_stop_price : null;
    edit.limit_offset = order.trail?.limit_offset > 0 ? order.trail.limit_offset : null;
  }

  const main = document.createElement("div");
  main.className = "open-order-row__main";
  const title = document.createElement("b");
  title.className = "pd-tile__legend";
  title.textContent = orderIdentityLegend(order);
  const meta = document.createElement("span");
  meta.className = "pd-tile__reading";
  meta.textContent = orderReadingLine(order);
  meta.title = [
    order.lifecycle_status,
    order.send_state,
    order.order_ref,
    order.account,
    order.endpoint,
  ].filter(Boolean).join(" / ") || "journal view";
  main.append(title, meta);
  const riskCopy = orderMismatchCopy(order);
  if (riskCopy) {
    const risk = document.createElement("small");
    risk.className = "open-order-row__risk";
    risk.textContent = riskCopy;
    main.append(risk);
  }

  const editBox = document.createElement("div");
  editBox.className = "open-order-row__edit";

  const qty = document.createElement("input");
  qty.type = "number";
  qty.min = "1";
  qty.max = String(orderReductionMax(order) || 1);
  qty.step = "1";
  qty.value = String(edit.quantity || orderReductionMax(order) || 1);
  qty.setAttribute("aria-label", `Reduction quantity for ${order.symbol || id}`);
  qty.disabled = !modifyGate.ready || Boolean(modifyGate.reduceOnly) || edit.busy;
  qty.addEventListener("change", () => {
    const maxQty = orderReductionMax(order) || 1;
    edit.quantity = Math.min(maxQty, Math.max(1, Math.trunc(Number(qty.value || 1))));
    edit.preview = null;
    edit.result = null;
    edit.error = "";
    renderOpenOrders();
  });

  const priceInputs = orderIsTrail(order)
    ? [
        order.trail?.trailing_amount > 0
          ? orderEditField("Trail amt", orderEditNumberInput(order, edit, modifyGate, "trailing_amount", "Trailing amount", "Trail amt"))
          : orderEditField("Trail %", orderEditNumberInput(order, edit, modifyGate, "trailing_percent", "Trailing percent", "Trail %")),
        orderEditField("Stop", orderEditNumberInput(order, edit, modifyGate, "initial_stop", "Initial stop price", "Stop")),
        ...(String(order.order_type || "").toUpperCase() === "TRAIL LIMIT"
          ? [orderEditField("Offset", orderEditNumberInput(order, edit, modifyGate, "limit_offset", "Limit offset", "Offset"))]
          : []),
      ]
    : [orderEditField("Limit", orderEditNumberInput(order, edit, modifyGate, "limit_price", "Limit price", "Limit"))];

  // The order's fixed shape (type, time in force, side) now reads on the
  // engraved identity legend and reading line above, so the edit box carries
  // only the fields the desk can actually change.
  editBox.append(orderEditField("Qty", qty), ...priceInputs);

  const controls = document.createElement("div");
  controls.className = "open-order-row__controls";
  const previewLabel = modifyGate.reduceOnly
    ? `Reduce stop to ${orderReduceToQuantity(order)}`
    : "Preview change";
  const previewButton = orderActionButton(previewLabel, modifyGate.ready && !edit.busy, modifyGate.reason);
  previewButton.addEventListener("click", () => previewOrderModify(order));
  const applyButton = orderActionButton("Apply change", modifyGate.ready && modifyPreviewReady(edit.preview) && !edit.busy, modifyApplyDisabledReason(modifyGate, edit.preview));
  applyButton.addEventListener("click", () => applyOrderModify(order));
  const cancelButton = orderActionButton("Cancel order", cancelGate.ready && !edit.busy, cancelGate.reason);
  cancelButton.addEventListener("click", () => cancelOpenOrder(order));
  controls.append(previewButton, applyButton, cancelButton);

  const status = document.createElement("small");
  status.className = "open-order-row__status";
  status.textContent = openOrderStatusLine(order, edit, modifyGate, cancelGate);

  row.append(main, editBox, controls, status);
  return row;
}

// The identity legend is engraved served fact and nothing else: the
// instrument, the side and size the journal recorded, and the order type.
function orderIdentityLegend(order = {}) {
  const side = [order.action, order.quantity].filter((part) => part || part === 0).join(" ");
  const parts = [order.symbol || order.order_ref, side, order.order_type]
    .map((part) => String(part ?? "").trim())
    .filter(Boolean);
  return parts.join(" · ") || "--";
}


// The reading line is the order's own figures: served price terms, served
// time in force, served lifecycle words. No status is re-derived here.
function orderReadingLine(order = {}) {
  const parts = [
    ...orderPriceFigures(order),
    order.tif,
    orderLifecycleLabel(order.lifecycle_status),
    orderSendStateLabel(order.send_state),
  ].map((part) => String(part ?? "").trim()).filter(Boolean);
  return parts.join(" · ") || "journal view";
}


// Price figures carry the order's own currency; an order without a served
// currency prints a bare amount rather than borrowing a label it never had.
function orderPriceFigures(order = {}) {
  const currency = order.currency || "";
  const trail = order.trail || {};
  const figures = [];
  if (order.limit_price > 0) figures.push(`limit ${money(order.limit_price, currency)}`);
  if (trail.trailing_amount > 0) figures.push(`trail ${money(trail.trailing_amount, currency)}`);
  else if (trail.trailing_percent > 0) figures.push(`trail ${pct(trail.trailing_percent)}`);
  if (trail.initial_stop_price > 0) figures.push(`stop ${money(trail.initial_stop_price, currency)}`);
  if (trail.limit_offset > 0) figures.push(`offset ${money(trail.limit_offset, currency)}`);
  return figures;
}


// Preview state is the served journal word: previewed, never transmitted.
function orderIsPreviewState(order = {}) {
  return String(order.lifecycle_status || "").trim().toLowerCase() === "previewed";
}

function orderEditField(labelText, input) {
  const field = document.createElement("label");
  field.className = "open-order-row__field";
  const caption = document.createElement("span");
  caption.textContent = labelText;
  field.append(caption, input);
  return field;
}

function orderEditNumberInput(order, edit, modifyGate, field, label, placeholder) {
  const input = document.createElement("input");
  input.type = "number";
  input.min = "0";
  input.step = "0.01";
  input.value = typeof edit[field] === "number" ? String(edit[field]) : "";
  input.placeholder = placeholder;
  input.setAttribute("aria-label", `${label} for ${order.symbol || orderIdentity(order)}`);
  input.disabled = !modifyGate.ready || Boolean(modifyGate.reduceOnly) || edit.busy;
  input.addEventListener("change", () => {
    const next = Number(input.value || 0);
    edit[field] = Number.isFinite(next) && next > 0 ? next : null;
    edit.preview = null;
    edit.result = null;
    edit.error = "";
    renderOpenOrders();
  });
  return input;
}

function orderActionButton(label, enabled, reason) {
  const button = document.createElement("button");
  button.type = "button";
  button.className = "text-button";
  button.textContent = label;
  button.disabled = !enabled;
  button.title = enabled ? label : reason || `${label} unavailable`;
  return button;
}

async function previewOrderModify(order) {
  const id = orderIdentity(order);
  const edit = openOrderEdit(order);
  const trading = state.snapshot?.trading || {};
  const gate = orderModifyGate(order, trading);
  if (!gate.ready) {
    edit.error = gate.reason;
    renderOpenOrders();
    return;
  }
  edit.busy = "preview";
  edit.error = "";
  edit.result = null;
  edit.cancelResult = null;
  renderOpenOrders();
  try {
    const res = await fetch(`/api/orders/${encodeURIComponent(id)}/preview-modify`, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      credentials: "include",
      body: JSON.stringify(modifyPreviewBody(order, edit)),
    });
    const body = await readJSONOrText(res);
    if (!res.ok) throw new Error(body.error || body.message || String(body));
    edit.preview = body;
  } catch (err) {
    edit.preview = null;
    edit.error = err.message;
  } finally {
    edit.busy = "";
    renderOpenOrders();
  }
}

async function applyOrderModify(order) {
  const id = orderIdentity(order);
  const edit = openOrderEdit(order);
  const trading = state.snapshot?.trading || {};
  const gate = orderModifyGate(order, trading);
  if (!gate.ready || !modifyPreviewReady(edit.preview)) {
    edit.error = modifyApplyDisabledReason(gate, edit.preview);
    renderOpenOrders();
    return;
  }
  const modifyConfirmation = protectionWriteConfirmation();
  if (!modifyConfirmation) {
    edit.error = "Trading account/mode unavailable; cannot confirm broker write.";
    renderOpenOrders();
    return;
  }
  edit.busy = "modify";
  edit.error = "";
  renderOpenOrders();
  try {
    const res = await fetch(`/api/orders/${encodeURIComponent(id)}/modify`, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      credentials: "include",
      body: JSON.stringify({
        preview_token: edit.preview.preview_token,
        confirm_account: modifyConfirmation.account,
        confirm_mode: modifyConfirmation.mode,
      }),
    });
    const body = await readJSONOrText(res);
    if (!res.ok) throw new Error(body.error || body.message || String(body));
    edit.result = body;
    edit.preview = null;
    await refreshOpenOrders();
  } catch (err) {
    edit.error = err.message;
  } finally {
    edit.busy = "";
    renderOpenOrders();
  }
}

async function cancelOpenOrder(order) {
  const id = orderIdentity(order);
  const edit = openOrderEdit(order);
  const trading = state.snapshot?.trading || {};
  const gate = orderCancelGate(order, trading);
  if (!gate.ready) {
    edit.error = gate.reason;
    renderOpenOrders();
    return;
  }
  const cancelConfirmation = protectionWriteConfirmation();
  if (!cancelConfirmation) {
    edit.error = "Trading account/mode unavailable; cannot confirm broker write.";
    renderOpenOrders();
    return;
  }
  edit.busy = "cancel";
  edit.error = "";
  renderOpenOrders();
  try {
    const res = await fetch(`/api/orders/${encodeURIComponent(id)}/cancel`, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      credentials: "include",
      body: JSON.stringify({
        confirm_account: cancelConfirmation.account,
        confirm_mode: cancelConfirmation.mode,
      }),
    });
    const body = await readJSONOrText(res);
    if (!res.ok) throw new Error(body.error || body.message || String(body));
    edit.cancelResult = body;
    await refreshOpenOrders();
  } catch (err) {
    edit.error = err.message;
  } finally {
    edit.busy = "";
    renderOpenOrders();
  }
}

async function refreshOpenOrders() {
  try {
    const res = await fetch("/api/orders/open", { credentials: "include" });
    if (!res.ok) return;
    state.ordersOpen = await res.json();
    renderOpenOrders();
  } catch {
    // Open orders are read-only context; the live snapshot remains primary.
  }
}

function orderIdentity(order) {
  return String(order.order_ref || order.reserved_order_id || order.perm_id || order.preview_token_id || order.symbol || "").trim();
}

function openOrderEdit(order) {
  const id = orderIdentity(order);
  if (!state.openOrderEdits[id]) {
    state.openOrderEdits[id] = {
      quantity: orderReductionMax(order) || 1,
      limit_price: order.limit_price > 0 ? order.limit_price : null,
      trailing_percent: order.trail?.trailing_percent > 0 ? order.trail.trailing_percent : null,
      trailing_amount: order.trail?.trailing_amount > 0 ? order.trail.trailing_amount : null,
      initial_stop: order.trail?.initial_stop_price > 0 ? order.trail.initial_stop_price : null,
      limit_offset: order.trail?.limit_offset > 0 ? order.trail.limit_offset : null,
      preview: null,
      result: null,
      cancelResult: null,
      error: "",
      busy: "",
    };
  }
  return state.openOrderEdits[id];
}

function orderReductionMax(order) {
  const remaining = Number(order.remaining || 0);
  const quantity = Number(order.quantity || 0);
  return Math.max(0, Math.floor(remaining > 0 ? remaining : quantity));
}

function orderModifyGate(order, trading) {
  if (!orderIdentity(order)) return { ready: false, reason: "Order id unavailable" };
  if (!trading.can_write) return { ready: false, reason: "Broker writes are not enabled by trading.status" };
  if ("modify_eligible" in order && order.modify_eligible !== true) {
    if (order.open !== false && orderReduceToQuantity(order) > 0) {
      // Position-mismatch rows disable generic modify but keep exactly one
      // shape: reduce the stop to the covered position, trail unchanged.
      // The daemon enforces the same constraint server-side.
      return { ready: true, reduceOnly: true, reason: "Position no longer covers this stop; only a reduce to the held quantity is allowed" };
    }
    if (order.reconciliation_kind === "short_entry_full") {
      return { ready: false, reason: "Position is flat; cancel this protective order instead of modifying it" };
    }
    return { ready: false, reason: "This order is not modify eligible" };
  }
  if (order.open === false) return { ready: false, reason: "Only open orders can be modified" };
  const orderType = String(order.order_type || "LMT").toUpperCase();
  if (orderType !== "LMT" && orderType !== "TRAIL" && orderType !== "TRAIL LIMIT") {
    return { ready: false, reason: "Canary mitigation UI supports LMT, TRAIL, and TRAIL LIMIT changes" };
  }
  if (orderReductionMax(order) <= 0) return { ready: false, reason: "No remaining quantity available to reduce" };
  if (orderIsTrail(order)) return { ready: true, reason: "Preview a reduction-only quantity or trail re-price; the broker order ID is kept" };
  return { ready: true, reason: "Preview a reduction-only quantity or LMT price change" };
}

function orderIsTrail(order) {
  const orderType = String(order.order_type || "").toUpperCase();
  return orderType === "TRAIL" || orderType === "TRAIL LIMIT";
}

function orderReduceToQuantity(order) {
  const value = Number(order.reduce_to_quantity || 0);
  return Number.isFinite(value) && value > 0 ? value : 0;
}

function orderMismatchCopy(order) {
  if (!order.reconciliation_kind) return "";
  const qty = Number(order.remaining || order.quantity || 0);
  const risk = Number(order.short_risk_quantity || 0);
  const selling = String(order.action || "").toUpperCase() === "SELL";
  const verb = selling ? "sells" : "buys";
  const side = selling ? "short" : "long";
  if (order.reconciliation_kind === "short_entry_excess") {
    return `Stop ${verb} ${qty} but the position covers ${orderReduceToQuantity(order)}. Triggering would open a ${risk}-share ${side} position.`;
  }
  return `Position is flat. Triggering would open a ${risk}-share ${side} position. Cancel this stop.`;
}

function orderCancelGate(order, trading) {
  if (!orderIdentity(order)) return { ready: false, reason: "Order id unavailable" };
  if (!tradingCancelAllowed(trading)) return { ready: false, reason: protectionWriteUnavailableReason(trading) };
  if (!trading.mode || !trading.account) return { ready: false, reason: "Broker account and mode are unavailable" };
  if ("cancel_eligible" in order && order.cancel_eligible !== true) return { ready: false, reason: "This order is not cancel eligible" };
  if (order.open === false) return { ready: false, reason: "Only open orders can be cancelled" };
  return { ready: true, reason: "Cancel this journal-backed open order after confirmation" };
}

function tradingCancelAllowed(trading = {}) {
  if (trading.can_write) return true;
  const blockers = trading.write_blockers || trading.blockers || [];
  if (blockers.length === 0) return false;
  return blockers.every((blocker) => String(blocker?.code || "").toLowerCase() === "trading_frozen");
}

function modifyPreviewBody(order, edit) {
  const body = {
    action: order.action || "",
    quantity: Math.min(orderReductionMax(order) || 1, Math.max(1, Math.trunc(Number(edit.quantity || 1)))),
    order_type: order.order_type || "LMT",
    tif: order.tif || "DAY",
  };
  if (orderIsTrail(order)) {
    const trail = {};
    if (edit.trailing_amount > 0) trail.trailing_amount = edit.trailing_amount;
    else if (edit.trailing_percent > 0) trail.trailing_percent = edit.trailing_percent;
    if (edit.initial_stop > 0) trail.initial_stop_price = edit.initial_stop;
    if (String(order.order_type || "").toUpperCase() === "TRAIL LIMIT" && edit.limit_offset > 0) trail.limit_offset = edit.limit_offset;
    body.trail = trail;
  } else {
    const limit = Number(edit.limit_price || 0);
    body.limit_price = Number.isFinite(limit) && limit > 0 ? limit : undefined;
  }
  return body;
}

function modifyPreviewReady(preview) {
  return Boolean(preview?.submit_eligible && previewToken(preview));
}

function modifyApplyDisabledReason(gate, preview) {
  if (!gate.ready) return gate.reason;
  if (!preview) return "Preview change first";
  if (!preview.submit_eligible) return "Modify preview is not submit eligible";
  if (!previewToken(preview)) return "Modify preview did not mint a preview token";
  return "Apply previewed change after confirmation";
}

const ORDER_LIFECYCLE_LABELS = {
  previewed: "Previewed",
  pending_submit: "Pending submit",
  pre_submitted: "Pre-submitted",
  submitted: "Working",
  partially_filled: "Partially filled",
  filled: "Filled",
  pending_cancel: "Pending cancel",
  cancelled: "Cancelled",
  rejected: "Rejected",
  inactive: "Inactive",
  unknown_reconcile_required: "Needs reconcile",
  expired_inferred: "Expired (inferred)",
  closed_reconciled: "Closed (reconciled)",
};

const ORDER_SEND_STATE_LABELS = {
  reserved: "Reserved",
  send_attempted: "Send attempted",
  broker_acknowledged: "At broker",
  uncertain_send: "Uncertain send",
  terminal: "Terminal",
};

function orderLifecycleLabel(value) {
  const key = String(value || "").toLowerCase();
  if (!key) return "";
  return ORDER_LIFECYCLE_LABELS[key] || labelize(key);
}

function orderSendStateLabel(value) {
  const key = String(value || "").toLowerCase();
  if (!key) return "";
  return ORDER_SEND_STATE_LABELS[key] || labelize(key);
}

function openOrderStatusLine(order, edit, modifyGate, cancelGate) {
  if (edit.busy === "preview") return "Previewing change.";
  if (edit.busy === "modify") return "Applying previewed change.";
  if (edit.busy === "cancel") return "Cancelling order.";
  if (edit.error) return edit.error;
  if (edit.result) return `Modify result: ${edit.result.accepted ? "accepted" : "not accepted"}${edit.result.message ? " / " + edit.result.message : ""}`;
  if (edit.cancelResult) return `Cancel result: ${edit.cancelResult.accepted ? "accepted" : "not accepted"}${edit.cancelResult.message ? " / " + edit.cancelResult.message : ""}`;
  if (edit.preview) return modifyPreviewLine(edit.preview);
  const reasons = [modifyGate.ready ? "" : modifyGate.reason, cancelGate.ready ? "" : cancelGate.reason].filter(Boolean);
  return reasons.length ? reasons.join(" / ") : `Open ${order.action || "--"} ${order.quantity || "--"} ${order.symbol || "--"}`;
}

function modifyPreviewLine(preview) {
  const parts = [
    preview.submit_eligible ? "submit eligible" : "not submit eligible",
    preview.what_if?.status ? `WhatIf ${preview.what_if.status}` : "",
    preview.preview_token_id ? `token ${preview.preview_token_id}` : "no submit token",
    preview.what_if?.message || "",
    warningMessages(preview.warnings).join(" / "),
  ].filter(Boolean);
  return "Preview change: " + parts.join(" / ");
}

function previewToken(preview) {
  return String(preview?.preview_token || "").trim();
}

export { ORDER_LIFECYCLE_LABELS, ORDER_SEND_STATE_LABELS, applyOrderModify, cancelOpenOrder, modifyApplyDisabledReason, modifyPreviewBody, modifyPreviewLine, modifyPreviewReady, openOrderEdit, openOrderRowElement, openOrderStatusLine, orderActionButton, orderCancelGate, orderEditField, orderEditNumberInput, orderIdentity, orderIdentityLegend, orderIsPreviewState, orderIsTrail, orderLifecycleLabel, orderModifyGate, orderPriceFigures, orderReadingLine, orderReductionMax, orderSendStateLabel, previewOrderModify, previewToken, refreshOpenOrders, renderOpenOrders, tradingCancelAllowed };
