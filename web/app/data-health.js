import { $, readJSONOrText } from "./shared.js";

let busy = false;
let lastReport = null;
let lastReceivedAt = null;
let lastAttemptAt = 0;
let deliveryFailed = false;

export function updateDataHealthReceipt() {
  const receipt = $("dataHealthReceipt");
  if (!receipt || busy) return;
  if (deliveryFailed || (lastReport && !(Date.parse(lastReport.valid_until) > Date.now()))) {
    receipt.textContent = lastReport ? "Showing the last Canary assessment. A current report has not been received; current health is unconfirmed." : "Canary's report could not be received. Source health is unverified.";
  } else if (lastReport && lastReceivedAt) {
    receipt.textContent = `Report received ${lastReceivedAt.toLocaleTimeString()}. Canary checked it ${new Date(lastReport.as_of).toLocaleTimeString()}.`;
  }
}

// Rows and counts are daemon-authored. Only receipt/error state belongs here.
export async function refreshDataHealth({ minIntervalMs = 0 } = {}) {
  if (busy || Date.now() - lastAttemptAt < minIntervalMs) return;
  lastAttemptAt = Date.now();
  busy = true;
  const receipt = $("dataHealthReceipt");
  if (!receipt) { busy = false; return; }
  receipt.textContent = "Reading Canary's report…";
  try {
    let first = null;
    let offset = 0;
    const sources = [];
    for (let page = 0; page < 128; page++) {
      const query = new URLSearchParams({ offset: String(offset), limit: "24" });
      if (first) query.set("revision", first.revision);
      const response = await fetch(`/api/data/health?${query}`, { credentials: "include", signal: AbortSignal.timeout(10000) });
      const report = await readJSONOrText(response);
      if (!response.ok || report?.schema_version !== 1 || !report.summary || !Array.isArray(report.sources) || report.offset !== offset || (first && report.revision !== first.revision)) throw new Error("Canary report unavailable or unsupported");
      if (!first) first = report;
      sources.push(...report.sources);
      if (report.complete) {
        if (sources.length !== first.summary.total) throw new Error("Incomplete Canary source coverage");
        lastReport = { ...first, sources };
        lastReceivedAt = new Date();
        deliveryFailed = false;
        renderDataHealth(lastReport);
        return;
      }
      if (!Number.isInteger(report.next_offset) || report.next_offset !== sources.length || report.next_offset <= offset) throw new Error("Incomplete Canary report");
      offset = report.next_offset;
    }
    throw new Error("Report coverage exceeds the display limit");
  } catch {
    deliveryFailed = true;
  } finally { busy = false; updateDataHealthReceipt(); }
}

export function renderDataHealth(report) {
  const summary = $("dataHealthSummary");
  const list = $("dataHealthSources");
  if (!summary || !list) return;
  summary.textContent = report.summary.label;
  list.replaceChildren();
  for (const source of report.sources) {
    const item = document.createElement("details");
    item.className = "data-health-source";
    item.dataset.state = source.state || "unknown";
    const title = document.createElement("summary");
    title.textContent = `${source.name} — ${source.receiving || "Unverified"}`;
    item.append(title);
    const text = [source.provider, ...(source.affects || []), source.detail, source.action];
    if (source.access) text.push(`Live access: ${source.access.reason} (IBKR ${source.access.code}). Retry ${new Date(source.access.retry_at).toLocaleTimeString()}.`);
    if (source.source_at) text.push(`Source time: ${new Date(source.source_at).toLocaleString()}`);
    else if (source.source_date) text.push(`Source date: ${source.source_date}`);
    else text.push("Source time: unknown");
    if (source.received_at) text.push(`Data received: ${new Date(source.received_at).toLocaleString()}`);
    if (source.checked_at) text.push(`Checked: ${new Date(source.checked_at).toLocaleString()}`);
    for (const line of text.filter(Boolean)) {
      const p = document.createElement("p");
      p.textContent = line;
      item.append(p);
    }
    list.append(item);
  }
}
