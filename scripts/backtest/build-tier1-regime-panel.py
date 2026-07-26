#!/usr/bin/env python3
"""Build a sourced Tier 1 regime backtest panel.

Tier 1 is intentionally narrow:
- official Cboe VIX/VIX3M/VVIX history
- public ETF OHLC history for SPY/HYG
- FRED funding, FX, and credit series where the public files cover the date
- no dealer gamma and no historical constituent breadth unless a trusted
  point-in-time source is provided later

The generated rows are point-in-time inputs for:
    canary backtest build-regime --input build/backtest/regime_pit_panel_tier1.jsonl
"""

from __future__ import annotations

import argparse
import bisect
import csv
import datetime as dt
import hashlib
import json
import math
import os
import sys
import urllib.error
import urllib.request
from pathlib import Path
from zoneinfo import ZoneInfo


BASE_SOURCE_DEFS = {
    "cboe_vix.csv": {
        "provider": "Cboe",
        "kind": "volatility",
        "series": ["VIX"],
        "url": "https://cdn.cboe.com/api/global/us_indices/daily_prices/VIX_History.csv",
    },
    "cboe_vix3m.csv": {
        "provider": "Cboe",
        "kind": "volatility",
        "series": ["VIX3M"],
        "url": "https://cdn.cboe.com/api/global/us_indices/daily_prices/VIX3M_History.csv",
    },
    "cboe_vvix.csv": {
        "provider": "Cboe",
        "kind": "volatility",
        "series": ["VVIX"],
        "url": "https://cdn.cboe.com/api/global/us_indices/daily_prices/VVIX_History.csv",
    },
    "nasdaq_spy.json": {
        "provider": "Nasdaq public historical endpoint",
        "kind": "prices",
        "series": ["SPY"],
        "url": "https://api.nasdaq.com/api/quote/SPY/historical?assetclass=etf&fromdate=2016-01-01&todate=2026-05-31&limit=9999",
    },
    "nasdaq_hyg.json": {
        "provider": "Nasdaq public historical endpoint",
        "kind": "prices",
        "series": ["HYG"],
        "url": "https://api.nasdaq.com/api/quote/HYG/historical?assetclass=etf&fromdate=2016-01-01&todate=2026-05-31&limit=9999",
    },
    "fred_cp3m.csv": {
        "provider": "FRED",
        "kind": "funding",
        "series": ["DCPF3M"],
        "url": "https://fred.stlouisfed.org/graph/fredgraph.csv?id=DCPF3M",
    },
    "fred_tbill3m.csv": {
        "provider": "FRED",
        "kind": "funding",
        "series": ["DTB3"],
        "url": "https://fred.stlouisfed.org/graph/fredgraph.csv?id=DTB3",
    },
    "fred_usdjpy.csv": {
        "provider": "FRED",
        "kind": "fx",
        "series": ["DEXJPUS"],
        "url": "https://fred.stlouisfed.org/graph/fredgraph.csv?id=DEXJPUS",
    },
    "fred_hy_oas.csv": {
        "provider": "FRED",
        "kind": "credit",
        "series": ["BAMLH0A0HYM2"],
        "url": "https://fred.stlouisfed.org/graph/fredgraph.csv?id=BAMLH0A0HYM2",
    },
    "fred_ig_oas.csv": {
        "provider": "FRED",
        "kind": "credit",
        "series": ["BAMLC0A0CM"],
        "url": "https://fred.stlouisfed.org/graph/fredgraph.csv?id=BAMLC0A0CM",
    },
}

EVENT_ANCHORS = [
    ("2018-02-05", "2018 volmageddon"),
    ("2018-12-24", "2018 Q4 tightening selloff"),
    ("2019-08-05", "2019 trade-war pullback"),
    ("2020-03-12", "2020 covid crash"),
    ("2020-06-11", "2020 reopening pullback"),
    ("2021-09-20", "2021 Evergrande/taper pullback"),
    ("2022-06-13", "2022 inflation bear trend"),
    ("2022-09-13", "2022 hot CPI/rates shock"),
    ("2023-03-13", "2023 regional bank stress"),
    ("2023-10-27", "2023 rates drawdown"),
    ("2024-04-19", "2024 inflation/rates pullback"),
    ("2024-08-05", "2024 yen carry unwind"),
    ("2025-03-13", "2025 valuation pullback"),
    ("2025-04-04", "2025 tariff shock"),
    ("2026-02-18", "2026 AI bubble wobble"),
    ("2026-03-30", "2026 AI infrastructure shakeout"),
    ("2026-05-14", "2026 AI-led rally control"),
]

BERLIN = ZoneInfo("Europe/Berlin")
RAW_DEFAULT = Path("/private/tmp/ibkr-backtest-data/raw")


def parse_args() -> argparse.Namespace:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--raw-dir", type=Path, default=RAW_DEFAULT)
    parser.add_argument("--output", type=Path, default=Path("build/backtest/regime_pit_panel_tier1.jsonl"))
    parser.add_argument("--sources-output", type=Path, default=Path("build/backtest/backtest_sources_tier1.jsonl"))
    parser.add_argument("--start", default="2016-01-01")
    parser.add_argument("--end", default="2026-05-31")
    parser.add_argument("--no-fetch", action="store_true", help="Use existing raw files only.")
    parser.add_argument("--event-window", type=int, default=10, help="Trading sessions before/after named events to include.")
    parser.add_argument("--target-window", type=int, default=5, help="Primary stress-label window in trading sessions.")
    parser.add_argument("--early-window", type=int, default=20, help="Secondary early-warning outcome window in trading sessions.")
    return parser.parse_args()


def today_stamp() -> str:
    return dt.datetime.now(BERLIN).strftime("%Y-%m-%d %H:%M %Z")


def source_defs(start: str, end: str) -> dict[str, dict]:
    out = {filename: dict(meta) for filename, meta in BASE_SOURCE_DEFS.items()}
    out["nasdaq_spy.json"]["url"] = (
        "https://api.nasdaq.com/api/quote/SPY/historical"
        f"?assetclass=etf&fromdate={start}&todate={end}&limit=9999"
    )
    out["nasdaq_hyg.json"]["url"] = (
        "https://api.nasdaq.com/api/quote/HYG/historical"
        f"?assetclass=etf&fromdate={start}&todate={end}&limit=9999"
    )
    return out


def as_date(value: str) -> dt.date:
    for fmt in ("%Y-%m-%d", "%m/%d/%Y"):
        try:
            return dt.datetime.strptime(value, fmt).date()
        except ValueError:
            pass
    raise ValueError(f"unsupported date {value!r}")


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


def fetch_sources(raw_dir: Path, no_fetch: bool, sources: dict[str, dict]) -> list[dict]:
    raw_dir.mkdir(parents=True, exist_ok=True)
    records = []
    for filename, meta in sources.items():
        path = raw_dir / filename
        status = "cached"
        error = ""
        if not no_fetch:
            request = urllib.request.Request(
                meta["url"],
                headers={
                    "User-Agent": "Mozilla/5.0 ibkr-backtest-tier1",
                    "Accept": "text/csv,application/json,*/*",
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
        if not path.exists():
            records.append({**meta, "filename": filename, "path": str(path), "status": status, "error": error})
            continue
        records.append(
            {
                **meta,
                "filename": filename,
                "path": str(path),
                "status": status,
                "error": error,
                "bytes": path.stat().st_size,
                "sha256": sha256_file(path),
            }
        )
    missing = [r for r in records if r["status"] == "fetch_failed_missing"]
    if missing:
        names = ", ".join(r["filename"] for r in missing)
        raise SystemExit(f"missing required raw source files: {names}")
    return records


def read_cboe(path: Path) -> dict[dt.date, dict]:
    out = {}
    with path.open(newline="") as f:
        reader = csv.DictReader(f)
        for row in reader:
            date = as_date(row["DATE"])
            close = clean_float(row.get("CLOSE") or row.get("VVIX"))
            if close is None:
                continue
            out[date] = {
                "open": clean_float(row.get("OPEN")) or close,
                "high": clean_float(row.get("HIGH")) or close,
                "low": clean_float(row.get("LOW")) or close,
                "close": close,
            }
    return out


def read_fred(path: Path) -> dict[dt.date, float]:
    out = {}
    with path.open(newline="") as f:
        reader = csv.DictReader(f)
        columns = [c for c in (reader.fieldnames or []) if c != "observation_date"]
        if not columns:
            return out
        value_column = columns[0]
        for row in reader:
            value = clean_float(row.get(value_column))
            if value is not None:
                out[dt.date.fromisoformat(row["observation_date"])] = value
    return out


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
            "volume": clean_float(row.get("volume")),
        }
    return out


def prior_date(dates: list[dt.date], date: dt.date, offset: int = 1) -> dt.date | None:
    pos = bisect.bisect_left(dates, date)
    target = pos - offset
    if target < 0:
        return None
    return dates[target]


def next_window_dates(dates: list[dt.date], date: dt.date, window: int) -> list[dt.date]:
    pos = bisect.bisect_left(dates, date)
    return dates[pos : pos + window + 1]


def rolling_values(series: dict[dt.date, dict], dates: list[dt.date], date: dt.date, field: str, window: int) -> list[float]:
    pos = bisect.bisect_right(dates, date)
    window_dates = dates[max(0, pos - window) : pos]
    values = []
    for d in window_dates:
        value = series[d].get(field)
        if value is not None:
            values.append(value)
    return values


def latest_value(series: dict[dt.date, float], dates: list[dt.date], date: dt.date, max_stale_days: int) -> tuple[dt.date | None, float | None]:
    pos = bisect.bisect_right(dates, date)
    if pos == 0:
        return None, None
    found = dates[pos - 1]
    if (date - found).days > max_stale_days:
        return None, None
    return found, series[found]


def value_at_or_before(series: dict[dt.date, float], dates: list[dt.date], date: dt.date) -> tuple[dt.date | None, float | None]:
    pos = bisect.bisect_right(dates, date)
    if pos == 0:
        return None, None
    found = dates[pos - 1]
    return found, series[found]


def classify_vix_ratio(ratio: float | None) -> str:
    if ratio is None:
        return ""
    if ratio < 0.92:
        return "green"
    if ratio < 1.0:
        return "yellow"
    return "red"


def classify_vvix(vvix: float | None) -> str:
    if vvix is None:
        return ""
    if vvix < 90:
        return "green"
    if vvix < 110:
        return "yellow"
    return "red"


def event_labels(dates: list[dt.date], event_window: int) -> dict[dt.date, str]:
    labels = {}
    for raw_date, label in EVENT_ANCHORS:
        anchor = as_date(raw_date)
        pos = bisect.bisect_left(dates, anchor)
        if pos >= len(dates):
            continue
        if dates[pos] != anchor and pos > 0 and abs((dates[pos - 1] - anchor).days) < abs((dates[pos] - anchor).days):
            pos -= 1
        for d in dates[max(0, pos - event_window) : min(len(dates), pos + event_window + 1)]:
            labels[d] = label
    return labels


def selected_dates(all_dates: list[dt.date], vix: dict, vix3m: dict, vvix: dict, event_window: int) -> dict[dt.date, list[str]]:
    selected: dict[dt.date, list[str]] = {}
    for d in all_dates:
        ratio = vix[d]["close"] / vix3m[d]["close"] if vix3m[d]["close"] else None
        vix_band = classify_vix_ratio(ratio)
        vvix_band = classify_vvix(vvix[d]["close"])
        if vix_band in {"yellow", "red"} or vvix_band in {"yellow", "red"}:
            selected.setdefault(d, []).append("vol_yellow_red")
    for d, label in event_labels(all_dates, event_window).items():
        selected.setdefault(d, []).append("event_window:" + label)
    calm_months = set()
    for d in all_dates:
        ratio = vix[d]["close"] / vix3m[d]["close"] if vix3m[d]["close"] else None
        if classify_vix_ratio(ratio) == "green" and classify_vvix(vvix[d]["close"]) == "green":
            key = (d.year, d.month)
            if key not in calm_months:
                selected.setdefault(d, []).append("monthly_calm_control")
                calm_months.add(key)
    return selected


def target_for(date: dt.date, spy_dates: list[dt.date], spy: dict, vix: dict, vix_dates: list[dt.date], window: int, early_window: int) -> dict:
    today = spy[date]
    prev = prior_date(spy_dates, date)
    prev_close = spy[prev]["close"] if prev else None
    daily_change = pct_change(today["close"], prev_close)
    intraday_change = pct_change(today["low"], prev_close)
    future_dates = next_window_dates(spy_dates, date, window)
    lows = [spy[d]["low"] for d in future_dates if spy[d].get("low") is not None]
    max_drawdown = pct_change(min(lows), today["close"]) if lows else None
    early_future_dates = next_window_dates(spy_dates, date, early_window)
    early_lows = [spy[d]["low"] for d in early_future_dates if spy[d].get("low") is not None]
    early_max_drawdown = pct_change(min(early_lows), today["close"]) if early_lows else None
    days_to_stress = None
    if max_drawdown is not None:
        for i, d in enumerate(future_dates):
            low_drawdown = pct_change(spy[d]["low"], today["close"])
            if low_drawdown is not None and low_drawdown <= -5.0:
                days_to_stress = i
                break
    vix_future_dates = next_window_dates(vix_dates, date, window)
    vix_values = [vix[d]["close"] for d in vix_future_dates if d in vix]
    vix_shock = pct_change(max(vix_values), vix[date]["close"]) if vix_values else None
    stress = False
    reasons = []
    if max_drawdown is not None and max_drawdown <= -5.0:
        stress = True
        reasons.append("forward_20d_spy_low_drawdown<=-5pct")
    if daily_change is not None and daily_change <= -3.0:
        stress = True
        reasons.append("same_day_spy_close<=-3pct")
        days_to_stress = 0 if days_to_stress is None else days_to_stress
    if intraday_change is not None and intraday_change <= -4.0:
        stress = True
        reasons.append("same_day_spy_low<=-4pct")
        days_to_stress = 0 if days_to_stress is None else days_to_stress
    if stress:
        kind = "market stress"
    elif max_drawdown is not None and max_drawdown <= -3.0:
        kind = "shallow pullback"
    elif daily_change is not None and daily_change <= -1.5:
        kind = "one-day pullback"
    else:
        kind = "calm/control"
    target = {
        "stress": stress,
        "scope": "market",
        "kind": kind,
        "window_days": window,
        "max_spy_drawdown_pct": rounded(max_drawdown, 2),
        "vix_shock_pct": rounded(vix_shock, 2),
        "notes": f"Tier 1 label uses SPY forward {window}-session low and same-day OHLC; labels are not signal inputs.",
    }
    if days_to_stress is not None:
        target["days_to_stress"] = days_to_stress
    return {
        "target": target,
        "daily_spy_change_pct": rounded(daily_change, 4),
        "same_day_spy_low_change_pct": rounded(intraday_change, 4),
        f"forward_{early_window}d_spy_drawdown_pct": rounded(early_max_drawdown, 2),
        "label_reasons": reasons,
    }


def fred_block(source: str, date: dt.date, dates: list[dt.date], series: dict[dt.date, float], key: str, max_stale_days: int = 7) -> tuple[dict, dt.date | None, float | None]:
    as_of, value = latest_value(series, dates, date, max_stale_days)
    if value is None:
        return {"source": source, "status": "unavailable"}, None, None
    return {"source": source, "as_of_date": as_of.isoformat(), key: rounded(value, 4)}, as_of, value


def build_rows(args: argparse.Namespace, source_records: list[dict]) -> tuple[list[dict], list[dict]]:
    raw = args.raw_dir
    start = dt.date.fromisoformat(args.start)
    end = dt.date.fromisoformat(args.end)
    vix = read_cboe(raw / "cboe_vix.csv")
    vix3m = read_cboe(raw / "cboe_vix3m.csv")
    vvix = read_cboe(raw / "cboe_vvix.csv")
    spy = read_nasdaq(raw / "nasdaq_spy.json")
    hyg = read_nasdaq(raw / "nasdaq_hyg.json")
    cp3m = read_fred(raw / "fred_cp3m.csv")
    tbill3m = read_fred(raw / "fred_tbill3m.csv")
    usdjpy = read_fred(raw / "fred_usdjpy.csv")
    hy_oas = read_fred(raw / "fred_hy_oas.csv")
    ig_oas = read_fred(raw / "fred_ig_oas.csv")

    all_dates = sorted(set(vix) & set(vix3m) & set(vvix) & set(spy) & set(hyg))
    all_dates = [d for d in all_dates if start <= d <= end]
    spy_dates = sorted(spy)
    hyg_dates = sorted(hyg)
    vix_dates = sorted(vix)
    cp3m_dates = sorted(cp3m)
    tbill_dates = sorted(tbill3m)
    usdjpy_dates = sorted(usdjpy)
    hy_dates = sorted(hy_oas)
    ig_dates = sorted(ig_oas)
    selected = selected_dates(all_dates, vix, vix3m, vvix, args.event_window)
    event_by_date = event_labels(all_dates, args.event_window)

    rows = []
    for d in sorted(selected):
        ratio = vix[d]["close"] / vix3m[d]["close"] if vix3m[d]["close"] else None
        prev_vix_date = prior_date(vix_dates, d)
        prev_vix = vix[prev_vix_date]["close"] if prev_vix_date else None
        prev_spy_date = prior_date(spy_dates, d)
        prev_spy = spy[prev_spy_date]["close"] if prev_spy_date else None
        spy_change = spy[d]["close"] - prev_spy if prev_spy is not None else None
        spy_change_pct = pct_change(spy[d]["close"], prev_spy)
        vvix_20d_date = prior_date(sorted(vvix), d, offset=20)
        vvix_change_20d = pct_change(vvix[d]["close"], vvix[vvix_20d_date]["close"] if vvix_20d_date else None)
        hyg_50 = rolling_values(hyg, hyg_dates, d, "close", 50)
        spy_252 = rolling_values(spy, spy_dates, d, "close", 252)
        label = target_for(d, spy_dates, spy, vix, vix_dates, args.target_window, args.early_window)

        credit = {"source": "FRED graph CSV: ICE BofA OAS public series", "status": "unavailable"}
        hy_date, hy_value = latest_value(hy_oas, hy_dates, d, 7)
        ig_date, ig_value = latest_value(ig_oas, ig_dates, d, 7)
        if hy_value is not None:
            prev_hy_date = prior_date(hy_dates, hy_date, offset=20) if hy_date else None
            hy_20d_change = hy_value - hy_oas[prev_hy_date] if prev_hy_date else None
            credit = {
                "source": "FRED graph CSV: ICE BofA OAS public series",
                "as_of_date": hy_date.isoformat() if hy_date else d.isoformat(),
                "hy_oas": rounded(hy_value, 4),
                "hy_oas_20d_change": rounded(hy_20d_change, 4),
            }
            if ig_value is not None:
                credit["ig_oas"] = rounded(ig_value, 4)
                credit["hy_ig_spread"] = rounded(hy_value - ig_value, 4)

        cp_block, cp_date, cp_value = fred_block("FRED graph CSV: DCPF3M minus DTB3", d, cp3m_dates, cp3m, "cp_3m_rate")
        tbill_date, tbill_value = latest_value(tbill3m, tbill_dates, d, 7)
        if cp_value is not None and tbill_value is not None:
            funding = {
                "source": "FRED graph CSV: DCPF3M minus DTB3",
                "as_of_date": max(cp_date, tbill_date).isoformat() if cp_date and tbill_date else d.isoformat(),
                "cp_3m_rate": rounded(cp_value, 4),
                "tbill_3m_rate": rounded(tbill_value, 4),
                "spread_bps": rounded((cp_value - tbill_value) * 100.0, 2),
            }
        else:
            funding = cp_block

        usdjpy_date, usdjpy_value = latest_value(usdjpy, usdjpy_dates, d, 7)
        prior_fx_date, prior_fx_value = value_at_or_before(usdjpy, usdjpy_dates, d - dt.timedelta(days=7))
        if usdjpy_value is not None and prior_fx_value is not None:
            fx = {
                "source": "FRED graph CSV: DEXJPUS",
                "as_of_date": usdjpy_date.isoformat() if usdjpy_date else d.isoformat(),
                "last": rounded(usdjpy_value, 4),
                "close_7d_ago": rounded(prior_fx_value, 4),
                "weekly_change_pct": rounded(pct_change(usdjpy_value, prior_fx_value), 4),
            }
        else:
            fx = {"source": "FRED graph CSV: DEXJPUS", "status": "unavailable"}

        market_cluster = event_by_date.get(d, "Tier 1 expanded volatility/calm controls")
        selection_reasons = sorted(set(selected[d]))
        row = {
            "date": d.isoformat(),
            "case": f"tier1 {label['target']['kind']} {d.isoformat()}",
            "market_cluster": market_cluster,
            "vix_term_structure": {
                "source": "Cboe official historical CSV",
                "as_of_date": d.isoformat(),
                "vix": rounded(vix[d]["close"], 4),
                "vix3m": rounded(vix3m[d]["close"], 4),
                "ratio": rounded(ratio, 6),
                "vix_prev_close": rounded(prev_vix, 4),
                "vix_change_pct": rounded(pct_change(vix[d]["close"], prev_vix), 4),
            },
            "vol_of_vol": {
                "source": "Cboe official VVIX historical CSV",
                "as_of_date": d.isoformat(),
                "last": rounded(vvix[d]["close"], 4),
                "change_20d_pct": rounded(vvix_change_20d, 4),
            },
            "hyg_spy_divergence": {
                "source": "Nasdaq public historical endpoint",
                "as_of_date": d.isoformat(),
                "hyg_price": rounded(hyg[d]["close"], 4),
                "hyg_50dma": rounded(sum(hyg_50) / len(hyg_50), 4) if hyg_50 else None,
                "spy_price": rounded(spy[d]["close"], 4),
                "spy_52w_high": rounded(max(spy_252), 4) if spy_252 else None,
                "spy_prev_close": rounded(prev_spy, 4),
                "spy_change": rounded(spy_change, 4),
                "spy_change_pct": rounded(spy_change_pct, 4),
            },
            "credit_spreads": credit,
            "funding_stress": funding,
            "usd_jpy": fx,
            "breadth": {
                "status": "unavailable",
                "source": "Tier 1: historical constituent breadth not sourced; do not fabricate breadth",
            },
            "target": label["target"],
            "tier1_features": {
                "split": "holdout" if d >= dt.date(2024, 1, 1) else "tuning",
                "selection_reasons": selection_reasons,
                "vix_ratio_band": classify_vix_ratio(ratio),
                "vvix_band": classify_vvix(vvix[d]["close"]),
                "spy_open": rounded(spy[d]["open"], 4),
                "spy_high": rounded(spy[d]["high"], 4),
                "spy_low": rounded(spy[d]["low"], 4),
                "spy_close": rounded(spy[d]["close"], 4),
                "daily_spy_change_pct": label["daily_spy_change_pct"],
                "same_day_spy_low_change_pct": label["same_day_spy_low_change_pct"],
                f"forward_{args.early_window}d_spy_drawdown_pct": label[f"forward_{args.early_window}d_spy_drawdown_pct"],
                "label_reasons": label["label_reasons"],
            },
            "notes": "Tier 1 expanded panel: selected every vol yellow/red day, named-event windows, and monthly calm controls; gamma and breadth remain unavailable.",
        }
        rows.append(prune_none(row))

    source_gaps = [
        "dealer_gamma omitted: no trusted point-in-time gamma snapshot",
        "historical S&P 500 constituent breadth not sourced; breadth row is unavailable",
        "public FRED/ICE HY and IG OAS availability depends on retrieved file coverage",
        "Tier 1 labels use forward SPY OHLC and are for threshold testing, not live signal inputs",
    ]
    source_rows = []
    for record in source_records:
        source_rows.append(
            {
                "dataset": "tier1_regime_panel",
                "retrieved_at": today_stamp(),
                "source": record,
                "source_gaps": source_gaps,
            }
        )
    source_rows.append(
        {
            "dataset": "tier1_regime_panel",
            "retrieved_at": today_stamp(),
            "derived_artifact": str(args.output),
            "rows": len(rows),
            "start": rows[0]["date"] if rows else None,
            "end": rows[-1]["date"] if rows else None,
            "selection": "all VIX/VIX3M or VVIX yellow/red days, named-event windows, monthly calm controls",
            "target_rule": f"market stress when SPY forward {args.target_window}-session low drawdown <= -5%, same-day SPY close <= -3%, or same-day SPY low <= -4%",
            "split_rule": "tuning before 2024-01-01; holdout from 2024-01-01 onward",
            "source_gaps": source_gaps,
        }
    )
    return rows, source_rows


def prune_none(value):
    if isinstance(value, dict):
        return {k: prune_none(v) for k, v in value.items() if v is not None}
    if isinstance(value, list):
        return [prune_none(v) for v in value]
    return value


def write_jsonl(path: Path, rows: list[dict]) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    with path.open("w") as f:
        for row in rows:
            f.write(json.dumps(row, sort_keys=False, separators=(",", ":")) + "\n")


def main() -> int:
    args = parse_args()
    records = fetch_sources(args.raw_dir, args.no_fetch, source_defs(args.start, args.end))
    rows, source_rows = build_rows(args, records)
    write_jsonl(args.output, rows)
    write_jsonl(args.sources_output, source_rows)
    status_counts = {}
    split_counts = {}
    for row in rows:
        target = row["target"]
        status_counts[target["kind"]] = status_counts.get(target["kind"], 0) + 1
        split = row["tier1_features"]["split"]
        split_counts[split] = split_counts.get(split, 0) + 1
    print(f"wrote {len(rows)} rows to {args.output}")
    print(f"wrote {len(source_rows)} source rows to {args.sources_output}")
    print("target_kind_counts=" + json.dumps(status_counts, sort_keys=True))
    print("split_counts=" + json.dumps(split_counts, sort_keys=True))
    failed_fetches = [r for r in records if str(r.get("status", "")).startswith("fetch_failed")]
    for record in failed_fetches:
        print(f"warning: {record['filename']} {record['status']}: {record.get('error', '')}", file=sys.stderr)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
