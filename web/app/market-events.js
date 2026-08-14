import { $, labelize, normalizeSymbol, shortTimeWithZone } from "./shared.js";
import { state } from "./state.js";
import { borrowSourceHealth, marketEventHealthVisible, positionsHaveShortStock, relevantMarketEventHealth } from "./exposure-relevance.js";

function renderMarketFlagRail(id, items) {
  const rail = $(id);
  if (!rail) return;
  const chips = (items || []).map((item) => item.sourceHealth ? marketSourceHealthChip(item.sourceHealth) : marketFlagChip(item.flag, item.options || {})).filter(Boolean);
  rail.hidden = chips.length === 0;
  rail.replaceChildren(...chips);
}

function marketFlagRow(flags) {
  const active = (flags || []).filter(marketEventFlagVisible);
  if (active.length === 0) return null;
  const row = document.createElement("div");
  row.className = "market-flag-row";
  row.replaceChildren(...active.map((flag) => marketFlagChip(flag, { compact: true })));
  return row;
}

function marketFlagChip(flag = {}, options = {}) {
  if (!flag || !flag.id) return null;
  const chip = document.createElement("span");
  chip.className = `market-flag-chip market-flag-chip--${marketEventTone(flag)}`;
  chip.textContent = options.label || marketEventLabel(flag, options);
  chip.title = marketEventTitle(flag);
  return chip;
}

function marketSourceHealthChip(health = {}) {
  if (!marketEventHealthVisible(health)) return null;
  const chip = document.createElement("span");
  chip.className = "market-flag-chip market-flag-chip--muted";
  chip.textContent = `${marketEventSourceLabel(health.source)} ${labelize(health.status || "unknown")}`;
  chip.title = [
    health.source,
    health.as_of ? `as of ${shortTimeWithZone(health.as_of)}` : "",
    ...(health.notes || []),
  ].filter(Boolean).join(" · ");
  return chip;
}

function marketEventFlagsForSymbol(symbol, events = {}) {
  const target = normalizeSymbol(symbol);
  if (!target) return [];
  const bySymbol = events.by_symbol || {};
  for (const [key, flags] of Object.entries(bySymbol)) {
    if (normalizeSymbol(key) === target) {
      return (flags || []).filter(marketEventFlagVisible);
    }
  }
  return [];
}

function marketEventFlagVisible(flag = {}) {
  const status = String(flag.status || "").toLowerCase();
  return status === "active" || status === "recent" || status === "stale" || status === "unknown" || status === "degraded";
}

function protectionEffectiveMarketFlags(proposal = {}, events = {}) {
  const out = [];
  const seen = new Set();
  const add = (flag = {}) => {
    if (!marketEventFlagVisible(flag)) return;
    const key = `${flag.id || ""}|${flag.symbol || ""}|${flag.status || ""}`;
    if (seen.has(key)) return;
    seen.add(key);
    out.push(flag);
  };
  for (const flag of proposal.market_flags || []) add(flag);
  for (const flag of marketEventFlagsForSymbol(proposal.symbol || proposal.contract?.symbol, events)) add(flag);
  return out;
}

function protectionEffectiveBlockers(proposal = {}, events = {}) {
  const blockers = [...(proposal.blockers || [])];
  const snapshotBlocker = proposalSnapshotBlocker();
  if (snapshotBlocker) blockers.unshift(snapshotBlocker);
  const eventBlocker = protectionMarketEventBlocker(proposal, events);
  if (eventBlocker) blockers.unshift(eventBlocker);
  return blockers;
}

function proposalSnapshotBlocker() {
  return (state.snapshot?.proposals?.blockers || [])[0] || null;
}

function protectionMarketEventBlocker(proposal = {}, events = {}) {
  for (const flag of protectionEffectiveMarketFlags(proposal, events)) {
    const id = String(flag.id || "");
    const status = String(flag.status || "").toLowerCase();
    if (status !== "active") continue;
    if (id === "halt_regulatory_or_news" || id === "luld_pause" || flag.role === "hard_blocker" || flag.severity === "block") {
      return {
        code: `market_event_${id || "blocker"}`,
        message: `${flag.label || marketEventIDLabel(id)} is active for ${flag.symbol || proposal.symbol || "this symbol"}; refresh proposals after it clears`,
      };
    }
  }
  return null;
}

function underlyingHeroMarketFlags(rows, events = {}) {
  const heldSymbols = new Set(rows.filter((row) => !row.virtual).map((row) => row.symbol));
  const counts = new Map();
  for (const row of rows) {
    if (row.virtual || !heldSymbols.has(row.symbol)) continue;
    for (const flag of row.marketFlags || []) {
      if (!marketEventFlagVisible(flag)) continue;
      const key = flag.id;
      const existing = counts.get(key) || { flag, count: 0 };
      existing.count += 1;
      counts.set(key, existing);
    }
  }
  const items = [...counts.values()].map(({ flag, count }) => ({
    flag,
    options: { label: `${flag.label || marketEventIDLabel(flag.id)} ${count}` },
  }));
  if (items.length > 0) return items;
  return marketEventHealthItems(events);
}

function marketEventHealthItems(events = {}) {
  return relevantMarketEventHealth(events, state.snapshot?.positions)
    .map((sourceHealth) => ({ sourceHealth }));
}

// Borrow-inventory / borrow-fee feed health only changes a decision when
// the book can be forced to cover — i.e. it holds short stock (the only
// daemon consumer is buy-to-cover proposal friction). For an all-long
// book a permanently unreachable borrow feed is noise, not risk
// disclosure, so those health chips stay hidden until a short stock
// position exists. Active borrow flags on held names still render.
function bookHasShortStock() {
  return positionsHaveShortStock(state.snapshot?.positions);
}

function marketEventLabel(flag = {}, options = {}) {
  const base = flag.label || marketEventIDLabel(flag.id);
  if (options.compact) return base;
  return base;
}

function marketEventIDLabel(id = "") {
  switch (id) {
    case "borrow_inventory_tight": return "Borrow tight";
    case "borrow_fee_extreme": return "Fee extreme";
    case "reg_sho_threshold": return "Reg SHO";
    case "luld_pause":
    case "luld_pause_recent": return "LULD";
    case "halt_regulatory_or_news": return "Halt";
    default: return labelize(id || "flag");
  }
}

function marketEventTone(flag = {}) {
  const severity = String(flag.severity || "").toLowerCase();
  if (severity === "block") return "hard";
  if (severity === "act" || severity === "watch") return "friction";
  return "muted";
}

function marketEventTitle(flag = {}) {
  return [
    flag.symbol,
    flag.status ? labelize(flag.status) : "",
    flag.source || "",
    flag.as_of ? `as of ${shortTimeWithZone(flag.as_of)}` : "",
    ...(flag.details || []),
  ].filter(Boolean).join(" · ");
}

function marketEventSourceLabel(source = "") {
  const normalized = String(source || "").toLowerCase();
  if (normalized.includes("borrow_inventory")) return "Borrow";
  if (normalized.includes("borrow_fee")) return "Fee";
  if (normalized.includes("reg_sho")) return "Reg SHO";
  if (normalized.includes("halt")) return "Halts";
  if (normalized.includes("market_events")) return "Flags";
  return labelize(source || "Source");
}

export { bookHasShortStock, borrowSourceHealth, marketEventFlagVisible, marketEventFlagsForSymbol, marketEventHealthItems, marketEventHealthVisible, marketEventIDLabel, marketEventLabel, marketEventSourceLabel, marketEventTitle, marketEventTone, marketFlagChip, marketFlagRow, marketSourceHealthChip, proposalSnapshotBlocker, protectionEffectiveBlockers, protectionEffectiveMarketFlags, protectionMarketEventBlocker, renderMarketFlagRail, underlyingHeroMarketFlags };
