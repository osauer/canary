import { renderProtectionPanel } from "./protection.js";
import { $, accountBaseCurrency, currentSettings, labelize, money, purgeRestoreSettingEnabled, renderFreshnessTimestamp, renderSensitiveAccountId, stockProtectionSettingEnabled } from "./shared.js";
import { state } from "./state.js";
import { currentAccountContext, renderUnderlyings } from "./underlyings.js";

function renderSettings() {
  // The type plate is identity, not preference: it stamps even when the
  // settings payload has not been served yet.
  renderSettingsPlate();
  const settings = currentSettings();
  if (!settings || !settings.kind) return;
  state.settings = settings;
  const purge = settings.features?.purge_restore?.enabled || {};
  const stockProtection = settings.features?.stock_protection?.enabled || {};
  renderFreshnessTimestamp("settingsAsOf", settings.as_of, { staleMinutes: 15 });
  $("purgeRestoreSettingState").textContent = purge.value === false ? "Workflow off" : "Workflow on";
  $("purgeRestoreSettingMeta").textContent = `Broker submission unavailable · ${settingMeta(purge)}`;
  const toggle = $("purgeRestoreToggle");
  toggle.checked = purge.value !== false;
  toggle.disabled = purge.access !== "write";
  toggle.title = purge.reason || "Toggle the purge/restore workflow preference; this never enables broker submission";
  $("stockProtectionSettingState").textContent = stockProtection.value === false ? "Disabled" : "Enabled";
  $("stockProtectionSettingMeta").textContent = settingMeta(stockProtection);
  const stockToggle = $("stockProtectionToggle");
  stockToggle.checked = stockProtection.value !== false;
  stockToggle.disabled = stockProtection.access !== "write";
  stockToggle.title = stockProtection.reason || "Runtime preference";

  const trading = settings.trading || {};
  const status = state.snapshot?.trading || {};
  $("settingsTradingStatus").textContent = tradingStatusSettingsLabel(trading, status);
  $("settingsTradingMeta").textContent = [trading.mode?.value, trading.account?.value].filter(Boolean).join(" / ") || "Config-owned";
  $("settingsTradingLimits").textContent = tradingLimitSummary(trading.limits || {});
  $("settingsTradingLimitsMeta").textContent = tradingLimitMeta(trading.limits || {});
  const quality = settings.market_data?.quality || {};
  $("settingsMarketDataStatus").textContent = labelize(quality.status || "unknown");
  $("settingsMarketDataMeta").textContent = quality.summary || "Observed compact summary";
  $("settingsBuildStatus").textContent = settings.build?.channel?.value || "stable";
  $("settingsBuildMeta").textContent = settings.build?.experimental_trading_note || "Build-controlled capability";
  renderProtectionSettings(settings.auto_trade || {}, state.snapshot?.auto_trade || {});
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

function tradingLimitMeta(limits = {}) {
  const fields = [limits.max_notional, limits.max_option_contracts, limits.allow_stock_short, limits.allow_option_sell_to_open].filter(Boolean);
  const writable = fields.some((field) => field.access === "write");
  const firstReason = fields.map((field) => field.reason).find(Boolean);
  if (writable) return "Runtime overrides writable";
  return firstReason || "Config/build controlled";
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

async function setPurgeRestoreEnabled(enabled) {
  const previous = purgeRestoreSettingEnabled();
  state.settings = {
    ...currentSettings(),
    features: {
      ...(currentSettings().features || {}),
      purge_restore: {
        ...(currentSettings().features?.purge_restore || {}),
        enabled: {
          ...(currentSettings().features?.purge_restore?.enabled || {}),
          value: enabled,
        },
      },
    },
  };
  if (state.snapshot) state.snapshot.settings = state.settings;
  renderSettings();
  renderUnderlyings(state.snapshot?.positions || {}, state.snapshot?.account || {});
  try {
    const res = await fetch("/api/settings", {
      method: "PATCH",
      headers: { "Content-Type": "application/json" },
      credentials: "include",
      body: JSON.stringify({ features: { purge_restore: { enabled } } }),
    });
    if (!res.ok) throw new Error(await res.text());
    state.settings = await res.json();
    if (state.snapshot) state.snapshot.settings = state.settings;
  } catch (err) {
    state.settings = {
      ...currentSettings(),
      features: {
        ...(currentSettings().features || {}),
        purge_restore: {
          ...(currentSettings().features?.purge_restore || {}),
          enabled: {
            ...(currentSettings().features?.purge_restore?.enabled || {}),
            value: previous,
          },
        },
      },
    };
    if (state.snapshot) state.snapshot.settings = state.settings;
    state.underlyingNotice = "Settings update failed: " + err.message;
  }
  renderSettings();
  renderUnderlyings(state.snapshot?.positions || {}, state.snapshot?.account || {});
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

export { renderProtectionSettings, renderSettings, renderSettingsPlate, setPurgeRestoreEnabled, setStockProtectionEnabled, settingMeta, settingsPolicyFileLabel, tradingLimitMeta, tradingLimitSummary, tradingStatusSettingsLabel };
