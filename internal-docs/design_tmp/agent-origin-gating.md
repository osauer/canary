# Agent-origin gating for broker writes

Updated: 2026-06-20 00:00 CEST (policy flip: live agent-origin broker writes
are allowed through the same gated broker-write paths as human writes; origin
remains audit metadata and an extension point. Earlier: 2026-06-11 08:15 CEST.)
Status: implemented

Contract per `docs/templates/daemon-cli-trading-contract.md`.

## Scope

- **Goal:** every broker write carries origin metadata for audit and future
  origin-specific policy, while agent-origin paper and live writes are allowed
  only through the existing gated broker-write paths. Trading capability,
  route pins, preview tokens, freeze state, broker WhatIf/eligibility, and the
  local journal remain the authorization boundary.
- **User-facing command/tool/API:** `ibkr proposals submit`, `ibkr order
  place|modify`, `ibkr purge … / purge restore --execute`, app HTTP write
  endpoints, MCP `ibkr_order_preview` (token redaction only).
- **Owner layer:** origin *detection* is adapter-owned (CLI process state, app
  pairing); broker-write authorization is daemon policy in the broker-write
  choke point. Config/build stay the owners of trading capability; origin is
  request metadata, never persisted config.
- **Existing behavior:** RPC envelope carries no caller identity; CLI, MCP
  server, and any same-uid process are indistinguishable on the daemon socket.
  Claude/Codex hook layers gate some Bash verbs client-side only.

## Authority

| Concept | Authoritative source | Typed field/contract | Renderer/tool | Fallback / unavailable |
|---|---|---|---|---|
| Request origin | invoking adapter at call time | `origin` field on broker-write params (`agent`, `human-tty`, `human-paired-device`) | journaled per order event; available to policy hooks | missing/unknown → treated as `agent` for audit |
| Broker-write policy | daemon `brokerWriteAuthorization` | `can_write`, `write_blockers`, submit/place errors | `ibkr trading status`, write responses | connected gateway plus config/build/pins/freeze/journal/broker checks decide |
| ~~Live human confirmation~~ | removed 2026-06-11 | was: typed `live/<account>` ack, compared verbatim | — | human origins write on live with preview token + pins only |
| Agent detection (CLI) | process env + stdin | env markers `CLAUDECODE`, `CLAUDE_CODE_ENTRYPOINT`, `CODEX_SANDBOX`, `OPENAI_CODEX`, or `IBKR_AGENT_CONTEXT=1`, or `!isatty(stdin)` | n/a | any marker or non-TTY → `agent` |
| App origin | paired-device auth in `internal/app` | app sets `human-paired-device` on daemon calls | SPA | unauthenticated callers never reach writes |

## Mechanism

1. **CLI** resolves origin once at startup into `cli.Env`. `IBKR_AGENT_CONTEXT`
   can only force the *more restrictive* classification (`agent`); there is no
   env/flag that yields `human-tty`. Gets `// docgen:env` comment.
2. **RPC**: `Origin string` added to `OrderPlaceParams`, `OrderModifyParams`,
   `OrderCancelParams` (journaled),
   `TradeProposalSubmitParams`, `PurgeExecuteParams`,
   `PurgeRestoreExecuteParams`, and platform-settings update params for
   `[trading]` limit keys.
3. **Daemon**: `brokerWriteAuthorization` (single choke point used by place,
   modify, proposal submit, purge execute, restore execute) authorizes writes
   from all origins using connected-gateway readiness, trading mode, build
   capability, gateway/account/client pins, journal availability, freeze state,
   preview tokens, and broker checks.
   **Cancel is freeze-exempt** (strictly risk-reducing); origin still
   journaled. `trading.freeze` and every trading-limit settings write
   (`max_notional`, `max_option_contracts`, `allow_stock_short`,
   `allow_option_sell_to_open`) require a human-terminal origin in disabled,
   paper, and live modes; agent, missing, and paired-device origins are refused.
4. **MCP**: `ibkr_order_preview` redacts the raw `preview_token` (returns
   `preview_token_id` only), aligning with the proposal surface's
   `sanitizeProposalPreview`. Closes the mint-over-MCP → redeem-over-Bash
   laundering path in both modes; agents place/modify through the gated CLI
   token path.
5. **Journal**: every write attempt records `origin`, giving the audit trail
   for "who placed this".
6. **Hook layer (client-side, defense in depth, this repo's plugin):**
   `hooks/ibkr-pre-tool-use.sh` gates write verbs on `trading status --json`
   readiness for paper or live routes; it bans shell composition only for
   write-verb invocations so read-only `ibkr … --json | jq` stays usable.

## Safety invariants (supersedes the template's blanket line)

- Agent-origin requests may place, modify, close, submit, or transmit broker
  orders only when the full broker-write gate passes. There is no origin-only
  live hard block.
- Paper and live agent writes remain gated by the full existing stack: trading
  build, mode, connected gateway, pinned port/account/client-id, route
  confirmation, single-use WhatIf-accepted preview tokens where applicable,
  journal availability, and `trading.freeze`.
- Preview tokens are not submit eligibility; both fields stay separate.
- Origin is honest-by-construction for compliant agents and is an interlock,
  not a security boundary: a same-uid adversary can forge params or edit
  config. Documented in SECURITY.md.
- Missing origin == `agent`. New adapters must opt *in* to a human origin.

## Residual risks (accepted, documented)

- A paired PWA request is stamped `human-paired-device`; the daemon cannot tell
  whether browser automation clicked it. This remains accepted for user-driven
  app operation, but it is not an agent authorization path. Project agents keep
  paired-browser QA read-only and route any explicit current-turn broker-write
  request through the agent-origin gated CLI. The preview token and
  server-validated `confirm_account`/`confirm_mode` fields still gate app writes.
- Direct socket callers can claim `human-tty`. Same-uid trust boundary as
  today; the preview-token invariant, gateway pins with session cross-check,
  and the `trading.freeze` switch still apply on top (the config ack stack
  was removed 2026-06-11).

## Before/After artifact

Before: a simulated live gate refused agent origin with an origin-only blocker
before preview.
After: paper and live origins share the same base authorization; unit fixtures
assert no origin blocker, and proposal submit reaches preview/WhatIf before any
broker transmit attempt.

## Verification

- Unit: authorization matrix (origin × mode × verb incl. cancel freeze
  exemption), CLI origin detection (env/TTY), MCP redaction, settings gating,
  hook composition/readiness gates.
- `make check` (incl. docs-regen for the env var + MCP description), and a
  `-tags trading` test leg so the enforced path actually compiles under CI.
- `make smoke` vs paper TWS; paper E2E proposals submit as agent (must pass).

## Rollback

- Revert: daemon origin policy hook, CLI env detection, MCP redaction, hook
  script. No runtime state: origin is per-request metadata; journal entries
  with origin are additive. User-facing change reverts to hard-blocking
  agent-origin live writes.
