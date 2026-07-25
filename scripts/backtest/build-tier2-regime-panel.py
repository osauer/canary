#!/usr/bin/env python3
"""Build a Tier 2 confirmation-proxy regime panel from Tier 1 rows."""

from __future__ import annotations

import argparse
import bisect
import datetime as dt
import hashlib
import json
import math
import urllib.error
import urllib.request
from pathlib import Path
from zoneinfo import ZoneInfo


BERLIN = ZoneInfo("Europe/Berlin")
RAW_DEFAULT = Path("/private/tmp/ibkr-backtest-data/raw")
ETF_SYMBOLS = ["SPY", "HYG", "RSP", "IWM", "QQQ", "QQQE", "LQD", "IEF", "TLT", "SHY"]
PROXY_PAIRS = [
    ("rsp_spy", "participation", "RSP", "SPY", -2.0, True, "equal-weight S&P 500 underperforming cap-weight SPY"),
    ("iwm_spy", "participation", "IWM", "SPY", -4.0, True, "small caps underperforming SPY"),
    ("qqqe_qqq", "participation", "QQQE", "QQQ", -2.0, True, "equal-weight Nasdaq 100 underperforming QQQ"),
    ("hyg_lqd", "credit_proxy", "HYG", "LQD", -1.5, True, "high-yield credit underperforming investment-grade credit"),
    ("hyg_ief", "credit_proxy", "HYG", "IEF", -2.0, True, "high-yield credit underperforming intermediate Treasuries"),
    ("lqd_tlt", "credit_proxy_context", "LQD", "TLT", -2.0, False, "context only: mixes credit spread, duration, and rate effects"),
    ("tlt_ief", "rates_proxy", "TLT", "IEF", -4.0, True, "long duration underperforming intermediate duration"),
    ("shy_ief", "rates_proxy", "SHY", "IEF", 1.0, True, "front-end Treasury stress versus intermediate duration"),
]


def parse_args() -> argparse.Namespace:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--input", type=Path, default=Path("build/backtest/regime_pit_panel_tier1.jsonl"))
    parser.add_argument("--output", type=Path, default=Path("build/backtest/regime_pit_panel_tier2.jsonl"))
    parser.add_argument("--sources-output", type=Path, default=Path("build/backtest/backtest_sources_tier2.jsonl"))
    parser.add_argument("--raw-dir", type=Path, default=RAW_DEFAULT)
    parser.add_argument("--start", default="2016-01-01")
    parser.add_argument("--end", default="2026-05-31")
    parser.add_argument("--no-fetch", action="store_true", help="Use existing raw files only.")
    return parser.parse_args()


def today_stamp() -> str:
    return dt.datetime.now(BERLIN).strftime("%Y-%m-%d %H:%M %Z")


def clean_float(value: object) -> float | None:
    if value is None:
        return None
    text = str(value).strip().replace("$", "").replace(",", "")
    if text in {"", ".", "N/A", "null", "None"}:
        return None
    try:
        out = float(text)
    except ValueError:
        return None
    if math.isnan(out):
        return None
    return out


def as_date(value: str) -> dt.date:
    for fmt in ("%Y-%m-%d", "%m/%d/%Y"):
        try:
            return dt.datetime.strptime(value, fmt).date()
        except ValueError:
            pass
    raise ValueError(f"unsupported date {value!r}")


def rounded(value: float | None, digits: int = 4) -> float | None:
    if value is None:
        return None
    return round(value, digits)


def pct_change(now: float | None, before: float | None) -> float | None:
    if now is None or before in (None, 0):
        return None
    return (now - before) / before * 100.0


def sha256_file(path: Path) -> str:
    h = hashlib.sha256()
    with path.open("rb") as f:
        for chunk in iter(lambda: f.read(1024 * 1024), b""):
            h.update(chunk)
    return h.hexdigest()


def nasdaq_url(symbol: str, start: str, end: str) -> str:
    return (
        f"https://api.nasdaq.com/api/quote/{symbol}/historical"
        f"?assetclass=etf&fromdate={start}&todate={end}&limit=9999"
    )


def fetch_sources(raw_dir: Path, start: str, end: str, no_fetch: bool) -> list[dict]:
    raw_dir.mkdir(parents=True, exist_ok=True)
    records = []
    for symbol in ETF_SYMBOLS:
        filename = f"nasdaq_{symbol.lower()}.json"
        path = raw_dir / filename
        url = nasdaq_url(symbol, start, end)
        status = "cached"
        error = ""
        if not no_fetch:
            request = urllib.request.Request(
                url,
                headers={
                    "User-Agent": "Mozilla/5.0 ibkr-backtest-tier2",
                    "Accept": "application/json,*/*",
                    "Referer": "https://www.nasdaq.com/",
                },
            )
            try:
                with urllib.request.urlopen(request, timeout=30) as response:
                    path.write_bytes(response.read())
                status = "fetched"
            except (urllib.error.URLError, TimeoutError, OSError) as exc:
                error = str(exc)
                status = "fetch_failed_cached" if path.exists() else "fetch_failed_missing"
        record = {
            "provider": "Nasdaq public historical endpoint",
            "kind": "tier2_proxy_prices",
            "series": [symbol],
            "url": url,
            "filename": filename,
            "path": str(path),
            "status": status,
            "error": error,
        }
        if path.exists():
            record["bytes"] = path.stat().st_size
            record["sha256"] = sha256_file(path)
        elif no_fetch:
            record["status"] = "cached_missing"
        records.append(record)
    return records


def read_jsonl(path: Path) -> list[dict]:
    rows = []
    with path.open() as f:
        for line in f:
            line = line.strip()
            if line:
                rows.append(json.loads(line))
    return rows


def read_nasdaq(path: Path) -> dict[dt.date, dict]:
    data = json.loads(path.read_text())
    rows = data["data"]["tradesTable"]["rows"]
    out = {}
    for row in rows:
        date = as_date(row["date"])
        close = clean_float(row.get("close"))
        if close is None:
            continue
        out[date] = {
            "open": clean_float(row.get("open")) or close,
            "high": clean_float(row.get("high")) or close,
            "low": clean_float(row.get("low")) or close,
            "close": close,
        }
    return out


def load_prices(raw_dir: Path) -> dict[str, dict[dt.date, dict]]:
    out = {}
    for symbol in ETF_SYMBOLS:
        path = raw_dir / f"nasdaq_{symbol.lower()}.json"
        if path.exists():
            out[symbol] = read_nasdaq(path)
    return out


def common_pair_dates(prices: dict[str, dict], left: str, right: str) -> list[dt.date]:
    return sorted(set(prices.get(left, {})) & set(prices.get(right, {})))


def latest_pair_date(dates: list[dt.date], date: dt.date, max_stale_days: int = 3) -> dt.date | None:
    pos = bisect.bisect_right(dates, date)
    if pos == 0:
        return None
    found = dates[pos - 1]
    if (date - found).days > max_stale_days:
        return None
    return found


def pair_ratio(prices: dict[str, dict], left: str, right: str, date: dt.date) -> float | None:
    left_close = prices[left][date]["close"]
    right_close = prices[right][date]["close"]
    if right_close == 0:
        return None
    return left_close / right_close


def pair_change(prices: dict[str, dict], pair_dates: list[dt.date], left: str, right: str, date: dt.date, lookback: int = 20) -> dict:
    found = latest_pair_date(pair_dates, date)
    if found is None:
        return {"status": "unavailable"}
    pos = bisect.bisect_left(pair_dates, found)
    if pos < lookback:
        return {"status": "unavailable", "as_of_date": found.isoformat()}
    prior = pair_dates[pos - lookback]
    now_ratio = pair_ratio(prices, left, right, found)
    prior_ratio = pair_ratio(prices, left, right, prior)
    return {
        "status": "ok",
        "as_of_date": found.isoformat(),
        "ratio": rounded(now_ratio, 6),
        "return_20d_pct": rounded(pct_change(now_ratio, prior_ratio), 4),
    }


def rolling_drawdown(prices: dict[dt.date, dict], dates: list[dt.date], date: dt.date, window: int) -> float | None:
    found = latest_pair_date(dates, date)
    if found is None:
        return None
    pos = bisect.bisect_right(dates, found)
    window_dates = dates[max(0, pos - window) : pos]
    highs = [prices[d]["close"] for d in window_dates]
    if not highs:
        return None
    return pct_change(prices[found]["close"], max(highs))


def tier2_labels(row: dict, spy_prices: dict[dt.date, dict], spy_dates: list[dt.date]) -> dict:
    features = row.get("tier1_features", {})
    daily = features.get("daily_spy_change_pct")
    low = features.get("same_day_spy_low_change_pct")
    date = as_date(row["date"])
    drawdown_20d = rolling_drawdown(spy_prices, spy_dates, date, 20)
    drawdown_60d = rolling_drawdown(spy_prices, spy_dates, date, 60)
    stress_reasons = []
    if isinstance(daily, (int, float)) and daily <= -3.0:
        stress_reasons.append("same_day_spy_close<=-3pct")
    if isinstance(low, (int, float)) and low <= -4.0:
        stress_reasons.append("same_day_spy_low<=-4pct")
    if drawdown_20d is not None and drawdown_20d <= -5.0:
        stress_reasons.append("spy_close_drawdown_20d<=-5pct")
    if drawdown_60d is not None and drawdown_60d <= -8.0:
        stress_reasons.append("spy_close_drawdown_60d<=-8pct")
    tier1_target = row.get("target", {})
    watch = bool(tier1_target.get("stress")) or bool(stress_reasons)
    return {
        "watch": watch,
        "stress": bool(stress_reasons),
        "stress_reasons": stress_reasons,
        "watch_reasons": ["tier1_forward_or_current_stress"] if bool(tier1_target.get("stress")) else [],
        "spy_drawdown_20d_pct": rounded(drawdown_20d, 4),
        "spy_drawdown_60d_pct": rounded(drawdown_60d, 4),
    }


def build_proxy_features(row: dict, prices: dict[str, dict], pair_dates: dict[str, list[dt.date]]) -> dict:
    date = as_date(row["date"])
    groups = {}
    red_count = 0
    yellow_count = 0
    for key, group, left, right, red_threshold, active, description in PROXY_PAIRS:
        if left not in prices or right not in prices:
            proxy = {"status": "unavailable", "description": description}
        else:
            proxy = pair_change(prices, pair_dates[key], left, right, date)
            proxy["description"] = description
            proxy["left"] = left
            proxy["right"] = right
        ret = proxy.get("return_20d_pct")
        band = ""
        if isinstance(ret, (int, float)):
            if red_threshold < 0:
                if ret <= red_threshold:
                    band = "red"
                elif ret <= red_threshold / 2.0:
                    band = "yellow"
                else:
                    band = "green"
            else:
                if ret >= red_threshold:
                    band = "red"
                elif ret >= red_threshold / 2.0:
                    band = "yellow"
                else:
                    band = "green"
        proxy["band"] = band
        proxy["active_confirmation"] = active
        red_count += int(active and band == "red")
        yellow_count += int(active and band == "yellow")
        groups.setdefault(group, {})[key] = proxy
    group_bands = {}
    for group, proxies in groups.items():
        bands = [proxy.get("band", "") for proxy in proxies.values() if proxy.get("active_confirmation")]
        if "red" in bands:
            group_bands[group] = "red"
        elif "yellow" in bands:
            group_bands[group] = "yellow"
        elif "green" in bands:
            group_bands[group] = "green"
        else:
            group_bands[group] = ""
    return {
        "proxy_groups": groups,
        "proxy_group_bands": group_bands,
        "proxy_red_count": red_count,
        "proxy_yellow_count": yellow_count,
    }


def build_rows(args: argparse.Namespace, source_records: list[dict]) -> tuple[list[dict], list[dict]]:
    tier1_rows = read_jsonl(args.input)
    prices = load_prices(args.raw_dir)
    pair_dates = {
        key: common_pair_dates(prices, left, right)
        for key, _group, left, right, _red_threshold, _active, _description in PROXY_PAIRS
    }
    spy_dates = sorted(prices.get("SPY", {}))
    out = []
    for row in tier1_rows:
        row = dict(row)
        tier1_target = dict(row.get("target", {}))
        labels = tier2_labels(row, prices["SPY"], spy_dates)
        proxy_features = build_proxy_features(row, prices, pair_dates)
        row["tier1_target"] = tier1_target
        row["tier2_features"] = {
            "label_version": "tier2-v1",
            "labels": labels,
            **proxy_features,
        }
        row["target"] = {
            "stress": labels["stress"],
            "scope": "market",
            "kind": "observable market stress" if labels["stress"] else "not observable stress",
            "window_days": 0,
            "notes": "Tier 2 stress label uses current/past SPY tape only; Tier 1 forward label is preserved in tier1_target.",
        }
        if labels["stress"]:
            row["target"]["days_to_stress"] = 0
        out.append(row)
    source_rows = []
    source_gaps = [
        "Tier 2 uses ETF confirmation proxies, not official S&P 500 breadth.",
        "Tier 2 uses credit and rates ETF proxies where official history is incomplete.",
        "dealer_gamma remains excluded: no trusted point-in-time gamma snapshot.",
        "MOVE/rates-vol remains excluded: no clean point-in-time source.",
    ]
    for record in source_records:
        source_rows.append(
            {
                "dataset": "tier2_regime_proxy_panel",
                "retrieved_at": today_stamp(),
                "source": record,
                "source_gaps": source_gaps,
            }
        )
    source_rows.append(
        {
            "dataset": "tier2_regime_proxy_panel",
            "retrieved_at": today_stamp(),
            "derived_artifact": str(args.output),
            "input_artifact": str(args.input),
            "rows": len(out),
            "proxy_pairs": [pair[0] for pair in PROXY_PAIRS],
            "active_confirmation_proxy_pairs": [pair[0] for pair in PROXY_PAIRS if pair[5]],
            "target_rule": "observable stress when same-day SPY damage or trailing SPY drawdown is already present",
            "source_gaps": source_gaps,
        }
    )
    return out, source_rows


def write_jsonl(path: Path, rows: list[dict]) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    with path.open("w") as f:
        for row in rows:
            f.write(json.dumps(row, separators=(",", ":")) + "\n")


def main() -> int:
    args = parse_args()
    source_records = fetch_sources(args.raw_dir, args.start, args.end, args.no_fetch)
    missing = [
        record["filename"]
        for record in source_records
        if record["status"] in {"fetch_failed_missing", "cached_missing"}
    ]
    if missing:
        raise SystemExit("missing required Tier 2 source files: " + ", ".join(missing))
    rows, source_rows = build_rows(args, source_records)
    write_jsonl(args.output, rows)
    write_jsonl(args.sources_output, source_rows)
    stress = sum(1 for row in rows if row["target"]["stress"])
    watch = sum(1 for row in rows if row["tier2_features"]["labels"]["watch"])
    print(f"wrote {len(rows)} rows to {args.output}")
    print(f"wrote {len(source_rows)} source rows to {args.sources_output}")
    print(f"tier2_labels={{\"watch\":{watch},\"stress\":{stress},\"non_stress\":{len(rows)-stress}}}")
    for record in source_records:
        if str(record.get("status", "")).startswith("fetch_failed"):
            print(f"warning: {record['filename']} {record['status']}: {record.get('error', '')}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
