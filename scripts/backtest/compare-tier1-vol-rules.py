#!/usr/bin/env python3
"""Compare candidate red-volatility rules on Tier 1/Tier 2 regime panels."""

from __future__ import annotations

import argparse
import json
from pathlib import Path


CLUSTER_NAMES = ["vol", "credit", "funding", "fx", "gamma", "breadth"]
BAND_RANK = {"": 0, "green": 1, "yellow": 2, "red": 3}


def parse_args() -> argparse.Namespace:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--pit", type=Path, default=Path("build/backtest/regime_pit_panel_tier1.jsonl"))
    parser.add_argument("--compact", type=Path, default=Path("build/backtest/regime_backtest_tier1.jsonl"))
    return parser.parse_args()


def read_jsonl(path: Path) -> list[dict]:
    rows = []
    with path.open() as f:
        for line in f:
            line = line.strip()
            if line:
                rows.append(json.loads(line))
    return rows


def strongest(*bands: str) -> str:
    best = ""
    for band in bands:
        if BAND_RANK.get(band or "", 0) > BAND_RANK.get(best, 0):
            best = band or best
    return best


def has_independent_red(bands: list[str], self_index: int) -> bool:
    return any(i != self_index and band == "red" for i, band in enumerate(bands))


def clusters(row: dict) -> list[str]:
    regime = row["regime"]
    raw = [
        strongest(regime.get("vix_term_structure", {}).get("band", ""), regime.get("vol_of_vol", {}).get("band", "")),
        strongest(regime.get("hyg_spy_divergence", {}).get("band", ""), regime.get("credit_spreads", {}).get("band", "")),
        strongest(regime.get("funding_stress", {}).get("band", "")),
        strongest(regime.get("usd_jpy", {}).get("band", "")),
        strongest(regime.get("gamma_zero", {}).get("band", "")),
        strongest(regime.get("breadth", {}).get("band", "")),
    ]
    out = list(raw)
    if regime.get("hyg_spy_divergence", {}).get("band") == "red" and regime.get("credit_spreads", {}).get("band") != "red" and not has_independent_red(raw, 1):
        out[1] = "yellow"
    if regime.get("usd_jpy", {}).get("band") == "red" and not has_independent_red(raw, 3):
        out[3] = "yellow"
    return out


def red_names(bands: list[str]) -> list[str]:
    return [CLUSTER_NAMES[i] for i, band in enumerate(bands) if band == "red"]


def is_severe_isolated_vol(row: dict, pit: dict, bands: list[str]) -> bool:
    names = red_names(bands)
    if names != ["vol"]:
        return False
    regime = row["regime"]
    features = pit.get("tier1_features", {})
    vix_ratio = regime.get("vix_term_structure", {}).get("ratio")
    vvix = regime.get("vol_of_vol", {}).get("last")
    vix_change = regime.get("vix_term_structure", {}).get("vix_change_pct")
    spy_change = features.get("daily_spy_change_pct")
    return any(
        [
            number_at_least(vix_ratio, 1.00),
            number_at_least(vvix, 120.0),
            number_at_least(vix_change, 20.0),
            number_at_most(spy_change, -1.0),
        ]
    )


def has_tier2_proxy_confirmation(pit: dict) -> bool:
    group_bands = pit.get("tier2_features", {}).get("proxy_group_bands", {})
    return any(band == "red" for band in group_bands.values())


def has_tier2_stress_confirmation(row: dict, pit: dict) -> bool:
    regime = row["regime"]
    features = pit.get("tier1_features", {})
    vix_ratio = regime.get("vix_term_structure", {}).get("ratio")
    vvix = regime.get("vol_of_vol", {}).get("last")
    vix_change = regime.get("vix_term_structure", {}).get("vix_change_pct")
    spy_change = features.get("daily_spy_change_pct")
    spy_low = features.get("same_day_spy_low_change_pct")
    return any(
        [
            has_tier2_proxy_confirmation(pit),
            number_at_least(vix_ratio, 1.10),
            number_at_least(vvix, 150.0),
            number_at_least(vix_change, 40.0),
            number_at_most(spy_change, -2.0),
            number_at_most(spy_low, -3.0),
        ]
    )


def signal_tier2_severity_split(row: dict, pit: dict, bands: list[str]) -> bool:
    names = red_names(bands)
    if bool(names) and names != ["vol"]:
        return True
    if names != ["vol"]:
        return False
    return is_severe_isolated_vol(row, pit, bands) or has_tier2_proxy_confirmation(pit)


def signal_tier2_confirmed_red(row: dict, pit: dict, bands: list[str]) -> bool:
    return "red" in bands and has_tier2_stress_confirmation(row, pit)


def number_at_least(value, threshold: float) -> bool:
    return isinstance(value, (int, float)) and value >= threshold


def number_at_most(value, threshold: float) -> bool:
    return isinstance(value, (int, float)) and value <= threshold


def target_primary(row: dict) -> bool:
    target = row.get("target", {})
    return bool(target.get("stress")) and target.get("scope", "market") == "market"


def target_tier1_primary(row: dict, pit: dict) -> bool:
    target = pit.get("tier1_target") or row.get("target", {})
    return bool(target.get("stress")) and target.get("scope", "market") == "market"


def target_forward_drawdown(row: dict, pit: dict) -> bool:
    target = pit.get("tier1_target") or row.get("target", {})
    drawdown = target.get("max_spy_drawdown_pct")
    return target.get("scope", "market") == "market" and isinstance(drawdown, (int, float)) and drawdown <= -5.0


def target_tier2_stress(row: dict, pit: dict) -> bool:
    labels = pit.get("tier2_features", {}).get("labels", {})
    if labels:
        return bool(labels.get("stress"))
    return target_primary(row)


def target_tier2_watch(row: dict, pit: dict) -> bool:
    labels = pit.get("tier2_features", {}).get("labels", {})
    if labels:
        return bool(labels.get("watch"))
    return target_tier1_primary(row, pit)


def signal_current(row: dict, pit: dict, bands: list[str]) -> bool:
    return "red" in bands


def signal_confirm_only(row: dict, pit: dict, bands: list[str]) -> bool:
    names = red_names(bands)
    return bool(names) and names != ["vol"]


def signal_severity_split(row: dict, pit: dict, bands: list[str]) -> bool:
    return signal_confirm_only(row, pit, bands) or is_severe_isolated_vol(row, pit, bands)


def signal_watch(row: dict, pit: dict, bands: list[str]) -> bool:
    return "red" in bands or bands.count("yellow") >= 3


def score(rows: list[tuple[dict, dict, list[str]]], signal_fn, target_fn) -> dict:
    out = {"tp": 0, "fp": 0, "tn": 0, "fn": 0, "signals": 0, "targets": 0, "rows": 0}
    for row, pit, bands in rows:
        target = target_fn(row, pit)
        signal = signal_fn(row, pit, bands)
        out["rows"] += 1
        out["signals"] += int(signal)
        out["targets"] += int(target)
        if signal and target:
            out["tp"] += 1
        elif signal and not target:
            out["fp"] += 1
        elif not signal and target:
            out["fn"] += 1
        else:
            out["tn"] += 1
    out["precision"] = ratio(out["tp"], out["tp"] + out["fp"])
    out["recall"] = ratio(out["tp"], out["tp"] + out["fn"])
    out["false_alarm_rate"] = ratio(out["fp"], out["fp"] + out["tn"])
    return out


def ratio(numerator: int, denominator: int) -> float | None:
    if denominator == 0:
        return None
    return round(numerator / denominator, 4)


def fmt_pct(value: float | None) -> str:
    if value is None:
        return "n/a"
    return f"{value * 100:.1f}%"


def print_metrics(title: str, rows: list[tuple[dict, dict, list[str]]], target_fn) -> None:
    print(title)
    rules = [
        ("watch_any_red_or_3_yellow", signal_watch),
        ("current_any_red_cluster", signal_current),
        ("confirm_only_no_isolated_vol", signal_confirm_only),
        ("runtime_visible_severity_split", signal_severity_split),
    ]
    if any("tier2_features" in pit for _row, pit, _bands in rows):
        rules.append(("tier2_proxy_severity_split", signal_tier2_severity_split))
        rules.append(("tier2_confirmed_red", signal_tier2_confirmed_red))
    for name, fn in rules:
        m = score(rows, fn, target_fn)
        print(
            f"  {name:29s} rows={m['rows']:4d} targets={m['targets']:4d} "
            f"signals={m['signals']:4d} tp={m['tp']:4d} fp={m['fp']:4d} fn={m['fn']:4d} "
            f"precision={fmt_pct(m['precision'])} recall={fmt_pct(m['recall'])} false_alarm={fmt_pct(m['false_alarm_rate'])}"
        )


def examples(rows: list[tuple[dict, dict, list[str]]], signal_fn, target_fn, want_signal: bool, want_target: bool, limit: int = 8) -> list[str]:
    out = []
    for row, pit, bands in rows:
        signal = signal_fn(row, pit, bands)
        target = target_fn(row, pit)
        if signal == want_signal and target == want_target:
            features = pit.get("tier1_features", {})
            tier2 = pit.get("tier2_features", {})
            regime = row.get("regime", {})
            proxy_bands = ",".join(k for k, v in tier2.get("proxy_group_bands", {}).items() if v == "red") or "-"
            out.append(
                f"{row['date']} {row.get('market_cluster','')} kind={row.get('target', {}).get('kind', '')} "
                f"red={','.join(red_names(bands)) or '-'} vix_ratio={regime.get('vix_term_structure', {}).get('ratio')} "
                f"vvix={regime.get('vol_of_vol', {}).get('last')} vix_chg={regime.get('vix_term_structure', {}).get('vix_change_pct')} "
                f"spy={features.get('daily_spy_change_pct')} spy_low={features.get('same_day_spy_low_change_pct')} "
                f"fwd_dd={(pit.get('tier1_target') or row.get('target', {})).get('max_spy_drawdown_pct')} proxy_red={proxy_bands}"
            )
        if len(out) >= limit:
            break
    return out


def main() -> int:
    args = parse_args()
    pit_by_date = {row["date"]: row for row in read_jsonl(args.pit)}
    compact = read_jsonl(args.compact)
    joined = []
    for row in compact:
        pit = pit_by_date[row["date"]]
        joined.append((row, pit, clusters(row)))
    splits = {
        "all": joined,
        "tuning_pre_2024": [item for item in joined if item[1].get("tier1_features", {}).get("split") == "tuning"],
        "holdout_2024_plus": [item for item in joined if item[1].get("tier1_features", {}).get("split") == "holdout"],
    }
    for split_name, rows in splits.items():
        print_metrics(f"\nTier 1 primary target, {split_name}", rows, target_tier1_primary)
    for split_name, rows in splits.items():
        print_metrics(f"\nForward-drawdown-only target, {split_name}", rows, target_forward_drawdown)
    if any("tier2_features" in pit for _row, pit, _bands in joined):
        for split_name, rows in splits.items():
            print_metrics(f"\nTier 2 stress target, {split_name}", rows, target_tier2_stress)
        for split_name, rows in splits.items():
            print_metrics(f"\nTier 2 watch target, {split_name}", rows, target_tier2_watch)
    holdout = splits["holdout_2024_plus"]
    print("\nTier 2 confirmed-red false positives on holdout stress target:")
    for line in examples(holdout, signal_tier2_confirmed_red, target_tier2_stress, True, False):
        print("  " + line)
    print("\nTier 2 confirmed-red misses on holdout stress target:")
    for line in examples(holdout, signal_tier2_confirmed_red, target_tier2_stress, False, True):
        print("  " + line)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
