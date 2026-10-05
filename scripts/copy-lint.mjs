#!/usr/bin/env node
// Public-copy lint over rendered HTML (the built site, not the sources).
//
//   node tools/copy-lint.mjs [--rebaseline] [--skip <path-part>]...
//        [--claims-only <path-part>]... <dir|file>...
//
// --claims-only limits the claim check to pages whose path contains the part
// (a product manual states hundreds of figures the code vouches for; the
// landing and marketing pages are where unbacked figures slip in).
//
// Three checks per sentence of visible text:
//   private  a phrase from ~/.config/copy-lint/private.txt (never committed:
//            job title, employer, place names) — always fails
//   claim    a number, percentage, amount, "free" or a superlative (not step
//            labels, years, versions, times, index names) that no line
//            in claims.txt vouches for — fails unless already in the baseline
//   phrase   a line from copy-lint.txt: [fail] entries fail, [warn] entries warn
// copy-lint.baseline records today's known hits (a hash per rule+sentence);
// a hit in the baseline is reported as backlog and does not fail. New hits
// fail. --rebaseline rewrites the baseline from the current hits.
import { readFileSync, writeFileSync, existsSync, readdirSync, statSync } from "node:fs";
import { createHash } from "node:crypto";
import { join, dirname, relative } from "node:path";
import { fileURLToPath } from "node:url";
import { homedir } from "node:os";

const root = dirname(dirname(fileURLToPath(import.meta.url)));
const args = process.argv.slice(2);
const rebaseline = args.includes("--rebaseline");
const skips = [];
const claimsOnly = [];
const targets = [];
for (let i = 0; i < args.length; i++) {
  if (args[i] === "--rebaseline") continue;
  if (args[i] === "--skip") { skips.push(args[++i]); continue; }
  if (args[i] === "--claims-only") { claimsOnly.push(args[++i]); continue; }
  targets.push(args[i]);
}
if (targets.length === 0) { console.error("usage: copy-lint.mjs [--rebaseline] [--skip part]... [--claims-only part]... <dir|file>..."); process.exit(2); }

const lines = (p) => existsSync(p) ? readFileSync(p, "utf8").split("\n").map((l) => l.trim()).filter((l) => l && !l.startsWith("#")) : [];
const privatePhrases = lines(join(homedir(), ".config", "copy-lint", "private.txt"));
const claims = lines(join(root, "claims.txt")).map((l) => l.split(" — ")[0].toLowerCase());
const phraseRules = { fail: [], warn: [] };
let section = "warn";
for (const l of lines(join(root, "copy-lint.txt"))) {
  if (l === "[fail]" || l === "[warn]") { section = l.slice(1, -1); continue; }
  phraseRules[section].push(l.toLowerCase());
}
const baselinePath = join(root, "copy-lint.baseline");
const baseline = new Set(lines(baselinePath).map((l) => l.split(" ")[0]));

const htmlFiles = [];
const walk = (p) => {
  if (skips.some((s) => p.includes(s))) return;
  const st = statSync(p);
  if (st.isDirectory()) { for (const e of readdirSync(p)) walk(join(p, e)); }
  else if (p.endsWith(".html")) htmlFiles.push(p);
};
targets.forEach(walk);

const textOf = (html) => html
  .replace(/<script[\s\S]*?<\/script>/gi, " ")
  .replace(/<style[\s\S]*?<\/style>/gi, " ")
  .replace(/<(pre|code)[\s\S]*?<\/\1>/gi, " ")
  .replace(/<!--[\s\S]*?-->/g, " ")
  .replace(/<\/(p|li|h[1-6]|div|section|article|td|th|tr|figcaption|blockquote|title)>/gi, ".\n")
  .replace(/<br\s*\/?>/gi, "\n")
  .replace(/<[^>]+>/g, " ")
  .replace(/&#(\d+);/g, (_, n) => String.fromCodePoint(Number(n)))
  .replace(/&#x([0-9a-f]+);/gi, (_, n) => String.fromCodePoint(parseInt(n, 16)))
  .replace(/&nbsp;/g, " ").replace(/&amp;/g, "&").replace(/&lt;/g, "<").replace(/&gt;/g, ">").replace(/&quot;/g, '"').replace(/&#39;|&rsquo;|&lsquo;/g, "'")
  .replace(/[ \t]+/g, " ");
const sentences = (text) => text.split(/(?<=[.!?])\s+|\n+/).map((s) => s.replace(/^[\s.]+|[\s.]+$/g, "")).filter((s) => s.split(" ").length >= 3);

// A claim is a figure the reader could check. Years, versions, clock times,
// dates, ports and code-like tokens are not claims.
const notClaim = /\b\d{4}-\d{2}-\d{2}\b|\bfree float\b|\blean 4\b|\b(19|20)\d{2}\b|\bv?\d+\.\d+(\.\d+)?\b|\b\d{1,2}:\d{2}\b|\b\d{1,2}\s+(jan|feb|mar|apr|may|jun|jul|aug|sep|oct|nov|dec)\w*\b|\b\d{4,5}\b|s&p\s?500|nasdaq\s?100|^\s*\d{1,2}\s*(·|\.|-|:)?\s/gi;
const claimRe = /(^|[^\w.])(\d[\d,]*(\.\d+)?\s?(%|percent|x|×)?|[$€£]\s?\d|\d\s?(usd|eur|gbp))(?![\w.])|\bfree\b|\b(fastest|cheapest|best|largest|biggest|unique|guaranteed|no-code|award-winning|industry-leading)\b/i;
const isClaim = (s) => claimRe.test(s.replace(notClaim, " "));
const vouched = (s) => { const l = s.toLowerCase(); return claims.some((c) => c && l.includes(c)); };
const hash = (rule, s) => createHash("sha1").update(rule + "|" + s.toLowerCase().replace(/\s+/g, " ")).digest("hex").slice(0, 12);

const hits = [];
for (const f of htmlFiles) {
  const rel = relative(root, f);
  for (const s of sentences(textOf(readFileSync(f, "utf8")))) {
    const l = s.toLowerCase();
    for (const p of privatePhrases) if (l.includes(p.toLowerCase())) hits.push({ rule: "private", sev: "fail", f: rel, s, why: p });
    const claimsHere = claimsOnly.length === 0 || claimsOnly.some((c) => f.includes(c));
    if (claimsHere && isClaim(s) && !vouched(s)) hits.push({ rule: "claim", sev: "fail", f: rel, s, why: "no line in claims.txt vouches for this figure" });
    for (const p of phraseRules.fail) if (l.includes(p)) hits.push({ rule: "phrase", sev: "fail", f: rel, s, why: p });
    for (const p of phraseRules.warn) if (l.includes(p)) hits.push({ rule: "phrase", sev: "warn", f: rel, s, why: p });
  }
}

if (rebaseline) {
  const out = hits.filter((h) => h.rule !== "private").map((h) => `${hash(h.rule, h.s)} ${h.f} ${h.rule} ${h.s.slice(0, 70)}`);
  writeFileSync(baselinePath, out.length ? out.join("\n") + "\n" : "");
  console.log(`copy-lint: baseline rewritten with ${out.length} known hit(s)`);
  process.exit(0);
}

let fails = 0, backlog = 0, warns = 0;
for (const h of hits) {
  const known = h.rule !== "private" && baseline.has(hash(h.rule, h.s));
  const tag = h.rule === "private" ? "FAIL" : known ? "backlog" : h.sev === "fail" ? "FAIL" : "warn";
  if (tag === "FAIL") fails++; else if (tag === "backlog") backlog++; else warns++;
  console.log(`${tag.padEnd(7)} ${h.f}: ${h.rule} (${h.why}): "${h.s.slice(0, 160)}"`);
}
console.log(`copy-lint: ${htmlFiles.length} page(s), ${fails} new failure(s), ${backlog} in baseline, ${warns} warning(s)${privatePhrases.length ? "" : "; private list absent, self-framing check skipped"}`);
process.exit(fails ? 1 : 0);
