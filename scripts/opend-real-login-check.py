#!/usr/bin/env python3
"""What only a real moomoo Singapore or Hong Kong login can prove, read-only.

Run it with `canary` on PATH and a daemon set to [broker] kind = "opend". It prints
shapes, labels and currency rates, never an account number, a symbol, a quantity or
an account figure, so its output can be shared. It sends no order.
"""
import json
import re
import subprocess
from collections import Counter


def read(*args):
    done = subprocess.run(["canary", *args, "--json"], capture_output=True, text=True, timeout=60)
    try:
        return json.loads(done.stdout)
    except ValueError:
        return {"unreadable": (done.stderr or done.stdout).strip()[:160]}


def scrub(text):
    return re.sub(r"\d{4,}", "[number]", str(text))[:160]


status = read("status")
opend = status.get("opend") or {}
print("1. Login and account")
print("   connected:", status.get("connected"), "| program:", opend.get("program_status"),
      "| serverVer:", opend.get("server_version"))
print("   environment:", opend.get("environment"), "| markets:", opend.get("markets"))
print("   account detected:", bool(opend.get("account_ending")),
      "| real accounts to choose from:", len(opend.get("account_choices") or []))
if status.get("last_error"):
    print("   last error:", scrub(status["last_error"]))

print("2. Quote rights (market to level)")
print("  ", opend.get("quote_rights"))

account = read("account")
print("3. Funds")
print("   base currency:", account.get("base_currency"),
      "| ledger currencies:", sorted((account.get("currency_ledger") or {}).keys()) or
      account.get("authority", {}).get("ledger_currency_count"))
print("   read:", account.get("authority", {}).get("availability"),
      "| mode:", account.get("authority", {}).get("scope", {}).get("account_mode"))
fields = account.get("authority", {}).get("fields", {})
print("   fields served:", sorted(k for k, v in fields.items() if v))
print("   fields not served:", sorted(k for k, v in fields.items() if not v))

print("   rates (base units per unit; public figures, compare them with a public rate):")
for row in account.get("currency_exposure") or []:
    print("    ", row.get("currency"), row.get("exchange_rate"),
          "worked out from the account's totals" if row.get("exchange_rate_implied") else "quoted")
if not account.get("currency_exposure"):
    print("     none")

positions = read("positions")
stocks, apart = positions.get("stocks") or [], positions.get("not_served") or []
print("4. Holdings (counts only)")
print("   by currency:", dict(Counter(r.get("currency") for r in stocks)))
print("   by exchange:", dict(Counter(r.get("primary_exchange") or r.get("exchange") for r in stocks)))
print("   price source:", dict(Counter(r.get("price_source") or "streamed" for r in stocks)))
print("   without a base value:", sum(1 for r in stocks if r.get("market_value_base") is None))
print("   held apart, by type:", dict(Counter(r.get("sec_type") for r in apart)))
ratios = []
for row in apart:
    mark, qty, value = row.get("mark"), row.get("quantity"), row.get("market_value")
    if mark and qty and value:
        ratios.append(round(abs(value) / abs(mark * qty)))
print("   value / (price x quantity) of those, the multiplier if it is carried:", sorted(set(ratios)))
print("   portfolio complete:", positions.get("authority", {}).get("portfolio_complete"))

brief = subprocess.run(["canary", "brief"], capture_output=True, text=True, timeout=120)
line = [l.strip() for l in brief.stdout.splitlines() if "quote right" in l or "quotes are snapshots" in l]
print("5. Brief's line on quotes:", scrub(line[0]) if line else "none")
