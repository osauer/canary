import { renderProtectionPanel } from "./protection.js";
import { renderAll } from "./render-runtime.js";
import { $, accountBaseCurrency, currentSettings, dateFormatMode, labelize, maskAccountId, money, renderFreshnessTimestamp, renderSensitiveAccountId, stockProtectionSettingEnabled } from "./shared.js";
import { state } from "./state.js";
import { currentAccountContext, renderUnderlyings } from "./underlyings.js";

function renderSettings() {
  // The type plate is identity, not preference: it stamps even when the
  // settings payload has not been served yet.
  renderSettingsPlate();
  const settings = currentSettings();
  if (!settings || !settings.kind) return;
  state.settings = settings;
	const stockProtection = settings.features?.stock_protection?.enabled || {};
	const dateFormat = settings.display?.date_format || {};
	renderFreshnessTimestamp("settingsAsOf", settings.as_of, { staleMinutes: 15 });
  const dateSelect = $("dateFormatSelect");
  dateSelect.value = dateFormatMode(dateFormat.value);
  dateSelect.disabled = dateFormat.access !== "write" || state.dateFormatUpdate.busy;
  dateSelect.title = dateFormat.reason || "Calendar-date presentation";
  $("dateFormatSettingMeta").textContent = "Applied across every tab; relative freshness stays relative.";
  $("dateFormatSettingStatus").textContent = state.dateFormatUpdate.state;
  $("dateFormatSettingStatus").classList.toggle("governance-action-status--error", state.dateFormatUpdate.error);
  $("stockProtectionSettingState").textContent = stockProtection.value === false ? "Disabled" : "Enabled";
  $("stockProtectionSettingMeta").textContent = settingMeta(stockProtection);
  const stockToggle = $("stockProtectionToggle");
  stockToggle.checked = stockProtection.value !== false;
  stockToggle.disabled = stockProtection.access !== "write";
  stockToggle.title = stockProtection.reason || "Runtime preference";

  const trading = settings.trading || {};
  const status = state.snapshot?.trading || {};
  $("settingsTradingStatus").textContent = tradingStatusSettingsLabel(trading, status);
  renderSettingsTradingMeta(trading);
  $("settingsTradingLimits").textContent = tradingLimitSummary(trading.limits || {});
  $("settingsTradingLimitsMeta").textContent = tradingLimitMeta(trading.limits || {});
  const quality = settings.market_data?.quality || {};
  $("settingsMarketDataStatus").textContent = labelize(quality.status || "unknown");
  $("settingsMarketDataMeta").textContent = quality.summary || "Observed compact summary";
  $("settingsBuildStatus").textContent = settings.build?.channel?.value || "stable";
  const buildNote = settings.build?.experimental_trading_note || "Build-controlled capability";
  $("settingsBuildMeta").textContent = state.appVersion ? `v${String(state.appVersion).replace(/^v/, "")} · ${buildNote}` : buildNote;
  renderProtectionSettings(settings.auto_trade || {}, state.snapshot?.auto_trade || {});
}

function renderSettingsTradingMeta(trading = {}) {
  const element = $("settingsTradingMeta");
  if (!element) return;
  const mode = String(trading.mode?.value || "").trim();
  const account = String(trading.account?.value || "").trim();
  const displayAccount = account && !state.accountValueVisible ? maskAccountId(account) : account;
  element.textContent = [mode, displayAccount].filter(Boolean).join(" / ") || "Config-owned";
  element.classList.toggle("is-private", Boolean(account) && !state.accountValueVisible);
}

// The stamped type plate at the foot of the back panel: the same served
// identity the header carries, under the same privacy mask. It reads
// "CANARY · <account> · <MODE> · MADE FOR ONE DESK" — a machine built for one
// desk, and it says so.
function renderSettingsPlate() {
  const account = currentAccountContext(state.snapshot?.account || {});
  renderSensitiveAccountId("settingsPlateAccount", account.accountId, account.accountLabel);
  const mode = $("settingsPlateMode");
  if (mode) mode.textContent = account.modeLabel;
}

function settingMeta(field = {}) {
  const access = field.access || "read";
  const source = field.source || "observed";
  return field.reason ? `${access}/${source}: ${field.reason}` : `${access}/${source}`;
}

function tradingStatusSettingsLabel(trading = {}, status = {}) {
  if ((status.mode || trading.mode?.value) === "disabled") return "Disabled";
  if (status.blocked) return "Blocked";
  if (status.can_write) return "Write ready";
  if (status.can_preview) return "Preview ready";
  return "Read-only";
}

function tradingLimitSummary(limits = {}) {
  const notional = limits.max_notional?.value;
  const optionQty = limits.max_option_contracts?.value;
  const parts = [];
  // [trading].max_notional is defined in the account currency (see
  // config.Trading), so label it with the account base, never a fixed USD.
  if (typeof notional === "number") parts.push(money(notional, accountBaseCurrency(state.snapshot?.account || {})));
  if (typeof optionQty === "number") parts.push(`${optionQty} opt`);
  return parts.join(" / ") || "--";
}

// The meta line says what the figures ARE before saying who may change them:
// a bare "€10,000.00 / 5 opt" reads as an unexplained number otherwise.
function tradingLimitMeta(limits = {}) {
  const fields = [limits.max_notional, limits.max_option_contracts, limits.allow_stock_short, limits.allow_option_sell_to_open].filter(Boolean);
  const writable = fields.some((field) => field.access === "write");
  const firstReason = fields.map((field) => field.reason).find(Boolean);
  const meaning = "Per-order caps: notional / option contracts";
  if (writable) return `${meaning} · runtime overrides writable`;
  return firstReason ? `${meaning} · ${firstReason}` : `${meaning} · config/build controlled`;
}

function renderProtectionSettings(autoTrade = {}, status = {}) {
  const proposals = autoTrade.proposals_enabled || {};
  const fastPath = autoTrade.fast_path_enabled || {};
  const policy = status.policy || {};
  const hotReload = autoTrade.hot_reload || {};
  const cadence = autoTrade.proposal_cadence?.value || status.proposal_cadence || "";
  const reload = autoTrade.reload_interval?.value || status.reload_interval || "";
  $("settingsProtectionStatus").textContent = proposals.value === false ? "Proposals off" : "Manual proposals on";
  $("settingsProtectionMeta").textContent = [
    fastPath.value === false ? "fast path off" : "fast path on",
    cadence ? `cadence ${cadence}` : "",
  ].filter(Boolean).join(" / ") || "Config-owned";
  $("settingsPolicyStatus").textContent = policy.policy_id
    ? `${policy.policy_id} v${policy.policy_version || "--"}`
    : settingsPolicyFileLabel(autoTrade.policy_file?.value);
  $("settingsPolicyMeta").textContent = [
    policy.status ? `status ${labelize(policy.status)}` : "",
    hotReload.value === false ? "hot reload off" : "hot reload on",
    reload ? `reload ${reload}` : "",
  ].filter(Boolean).join(" / ") || settingMeta(autoTrade.policy_file || {});
}

function settingsPolicyFileLabel(value) {
  const raw = String(value || "").trim();
  if (!raw) return "Policy file";
  const normalized = raw.replaceAll("\\", "/");
  return normalized.split("/").filter(Boolean).pop() || raw;
}

async function setStockProtectionEnabled(enabled) {
  const previous = stockProtectionSettingEnabled();
  state.settings = {
    ...currentSettings(),
    features: {
      ...(currentSettings().features || {}),
      stock_protection: {
        ...(currentSettings().features?.stock_protection || {}),
        enabled: {
          ...(currentSettings().features?.stock_protection?.enabled || {}),
          value: enabled,
        },
      },
    },
  };
  if (state.snapshot) state.snapshot.settings = state.settings;
  renderSettings();
  renderProtectionPanel(state.snapshot?.proposals || {}, state.snapshot?.auto_trade || {}, state.snapshot?.market_events || {});
  try {
    const res = await fetch("/api/settings", {
      method: "PATCH",
      headers: { "Content-Type": "application/json" },
      credentials: "include",
      body: JSON.stringify({ features: { stock_protection: { enabled } } }),
    });
    if (!res.ok) throw new Error(await res.text());
    state.settings = await res.json();
    if (state.snapshot) state.snapshot.settings = state.settings;
  } catch (err) {
    state.settings = {
      ...currentSettings(),
      features: {
        ...(currentSettings().features || {}),
        stock_protection: {
          ...(currentSettings().features?.stock_protection || {}),
          enabled: {
            ...(currentSettings().features?.stock_protection?.enabled || {}),
            value: previous,
          },
        },
      },
    };
    if (state.snapshot) state.snapshot.settings = state.settings;
    state.underlyingNotice = "Settings update failed: " + err.message;
  }
  renderSettings();
  renderProtectionPanel(state.snapshot?.proposals || {}, state.snapshot?.auto_trade || {}, state.snapshot?.market_events || {});
}

async function setDateFormat(value) {
  const allowed = new Set(["us", "eu", "us_weekday", "eu_weekday"]);
  const nextValue = String(value || "").trim().toLowerCase();
  if (!allowed.has(nextValue) || state.dateFormatUpdate.busy) return false;
  const previous = dateFormatMode();
  const setLocal = (dateFormat) => {
    state.settings = {
      ...currentSettings(),
      display: {
        ...(currentSettings().display || {}),
        date_format: {
          ...(currentSettings().display?.date_format || {}),
          value: dateFormat,
        },
      },
    };
    if (state.snapshot) state.snapshot.settings = state.settings;
  };

  state.dateFormatUpdate = { busy: true, state: "Saving date format…", error: false };
  setLocal(nextValue);
  renderAll();
  if (state.readOnlyPreview) {
    state.dateFormatUpdate = { busy: false, state: "Preview only · not saved.", error: false };
    renderAll();
    return true;
  }
  try {
    const res = await fetch("/api/settings", {
      method: "PATCH",
      headers: { "Content-Type": "application/json" },
      credentials: "include",
      body: JSON.stringify({ display: { date_format: nextValue } }),
    });
    if (!res.ok) throw new Error(await res.text());
    state.settings = await res.json();
    if (state.snapshot) state.snapshot.settings = state.settings;
    state.dateFormatUpdate = { busy: false, state: "Date format saved.", error: false };
    renderAll();
    return true;
  } catch {
    setLocal(previous);
    state.dateFormatUpdate = { busy: false, state: "Date format was not changed.", error: true };
    renderAll();
    return false;
  }
}

export { renderProtectionSettings, renderSettings, renderSettingsPlate, renderSettingsTradingMeta, setDateFormat, setStockProtectionEnabled, settingMeta, settingsPolicyFileLabel, tradingLimitMeta, tradingLimitSummary, tradingStatusSettingsLabel };
