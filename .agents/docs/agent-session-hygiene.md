# Agent session hygiene

Last updated: 2026-07-18 18:23 CEST

Rationale and measured numbers behind the binding session-hygiene rules in
`AGENTS.md`. Source: the 2026-06-12 audit of two days of agent sessions in
this repo (~0.77B input tokens deduped, 96% cache reads), with each finding
adversarially verified against the raw transcripts.

## Why context size dominates cost and latency

~74% of all input tokens in the audit window were billed on API calls made
when the session context already exceeded 150k tokens. Every call whose
prompt exceeds 200k bills the *entire request* at the long-context price
tier, and time-to-first-token grows with context even when fully cached.
At 550k context, each extra tool round-trip costs ~0.4–0.5M billed tokens
and seconds of latency — the token levers and the speed levers are the same
lever.

The worst measured session ran 8 hours to 546k context with zero
compactions; ~95% of its ~111M input tokens were billed above 200k context.

## The rules, with evidence

**Compact or hand off at phase boundaries.** When work changes phase
(explore → implement → verify) or topic, and context is large, compact with
a short handoff or start a fresh session seeded with it. Corrected saving:
~40–60M tokens/day at the audit's session mix. Handoffs must restate
guardrail state: gateway pins (port/account/client id), freeze status, and
what is committed vs in-flight — over-aggressive compaction that drops
trading state is worse than a fat context.

**Never park a fat context.** Prompt cache expires after ~5 minutes idle;
resuming a 300k context overnight re-bills the full prefix at write price.
143 cold rebuilds (~27M tokens) were measured in two days, several from
sessions left open 19:00→04:30. Before a multi-hour pause, leave a 3-line
state note and end the session.

**Dispatch reviewed, independent fix batches to fresh-context agents.**
Measured benchmark from the audit: the same fix+test+commit work cost
~80k context/call in a fresh worktree agent vs 300–450k/call inline in a
fat main session — a 4–6x difference. Exploration/verification belongs in
read-only agents; long reviews run in the background while the
main session continues.

**Batch tiny probes; read by range.** 524 sub-2KB-output one-liners
(grep/ls/status) were issued from >200k contexts in two days, each paying a
full-context round-trip (~156M tokens raw). Batch independent probes 3–4:1
into one programmatic or parallel call. Use ranged reads around the
hit instead of whole-file dumps (one 35KB whole-file read was carried by
651 subsequent calls); `git diff --stat` first, then scoped diffs. Use
`apply_patch` for intentional edits so changes stay reviewable.

**Waits are background jobs, not polls.** Foreground sleeps and repeated
one-shot status greps from a large context burned ~7M tokens/day plus
12–18 min/day of wall time. Start the wait asynchronously through the current
client's background mechanism and retain its session or log handle. Background
waiters need a bounded timeout and must never bounce the daemon themselves.

**Long gates: run once, backgrounded or teed.** `make test` runs `check`
as its first prerequisite — never prefix it with `make check &&`. Never run a
long gate as a foreground pipe whose client timeout can kill the pipeline and
lose its output. Start it asynchronously, or write to a stable local log and
retain the process handle. The gateway lock prints a keepalive while waiting;
prolonged silence means the gate itself is running, not queued.

**Pre-warm the daily govulncheck scan.** The first gate run of each day
pays the cold scan (measured 8–10 min). `make govuln-prewarm-install` sets
up a 06:00 LaunchAgent so the stamp is warm before the first interactive
run.
