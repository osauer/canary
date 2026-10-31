# Desk automatic authority groundwork

Prepared 2026-10-09 17:50 CEST from the owner's Desk interview in
`01a11d08-684a-70c3-bed6-ab84135953f8`. This source increment is not routed,
installed, armed or commissioned. Its focused tests use synthetic data only.

## Accepted contract

Desk's existing AUTO mode schedules work. A separate device-confirmed mandate
selects protection/reductions or protection/reductions plus stock additions.
The scope persists until disarmed or downgraded. MANUAL pauses it; resuming AUTO
requires reconciliation and current broker-policy checks. No new daily budget
or allocation cap is introduced. Existing policy remains binding, including
cash, commitments, margin and applicable exposure warnings for automatic adds.

Additions and ordinary stock reductions use a fixed submission-time midpoint
limit, IBKR Adaptive and DAY validity. Adaptive priority is a Desk setting:
Patient, Normal (approved initial value), or Urgent. Existing protective order
semantics remain unchanged. Unsupported Adaptive requests are withheld, with
no plain-limit or market fallback. An order already sent is never repriced by
a settings change. Existing broker orders and manual actions survive an Auto
lock; the locked position remains included in all portfolio risk calculations.

## Ownership and authority

Desk/Torok owns deterministic rule selection, operating mode, locks and rule
history. Canary owns device verification, the persisted mandate, revocation,
financial gates, broker commitments, attempt identity and reconciliation.
The model and MCP receive no execution capability. No CLI strategy engine is
added; a future private backend handoff must use the existing broker write path.

A persistent mandate is needed because an exact-order confirmation or queued
reduction does not authorise subsequent fresh entry-rule episodes. Its signed
terms bind the account/mode, store authority epoch, maximum scope, controller
capability hash, authority generation and accepted execution semantics. The
private controller capability is never browser/model output or a command-line
argument. The signed mandate explicitly delegates deterministic selection to
the Desk controller; Canary does not independently establish a strategy signal.

Control/confirmation and the final wire guard share the authority lock. The
wire reader lease lasts until the transport finishes. Restrictive control
changes require the existing capability and generation but do not require
executable preferences; enabling requires valid priority and preference hash.
A daemon restart preserves scope but clears effective Running until a reconciled
controller supplies a new request ID. Confirmation replay returns current state
and cannot rearm a revoked mandate. A replaced store invalidates old signatures.

## A breach of the mandate holds full scope

Owner decision 2026-10-10 07:51 CEST: after a loss brake, automatic
additions come back only through a review. Since 2026-10-31 (build B5, at
the owner's instruction: one loss ladder, the mandate's) the brake this
reads is the mandate's breach, not the `[capital]` ladder's latch. Each
breach episode the mandate's reduction opens (two consecutive reduce
verdicts; it ends when the loss measure is back within the loss room) holds
a full mandate at protection (status `held_by: "mandate_breach"`); the hold
outlasts the episode's end until the owner confirms full again on a device
in Desk. While an episode is open, or the verdict cannot be measured,
confirming full is refused; protection can be armed. The hold is derived at
read: the last episode the reduction opened, kept in a daemon state
document across restarts, against the episode recorded at confirmation, so
no event hook can miss a breach. The controller sees the lower scope and
`authority_changed` at the wire for entries; protection and reductions keep
running. Manual trading follows the `[capital]` ladder's brake alone.

## Current source and remaining connection

Implemented building blocks: signed-term verification, persisted authority and
control generations, an optional final-wire lease, Adaptive encoding and
WhatIf/callback checks, journal projection and algorithm drift refusal.
Since 2026-10-10 the daemon dispatches the four authority methods, reached
only through the Go client (`DeskAuthority`, `PrepareDeskAuthority`,
`ConfirmDeskAuthority`, `ControlDeskAuthority`), like the cash policy methods:
outside the MCP catalogue and the CLI, so the controller capability never
appears in argv. A daemon without them answers unknown-method, which Desk
reads as automatic trading unavailable. Since 2026-10-19
`desk.execution.submit` with the automatic authorisation constructs the wire
fence on the production write path (`desk_execution_submit.go`,
`binding.deskAuthority`): the controller's submission holds a grant under
`brokerWriteMu`, and the wire guard re-reads the mandate under the wire lock
before the first byte (`desk-execution.md`).

Before connection: persist episode consumption and pre-dispatch commitments;
reconcile attempts against the existing order journal; attach the authority to
exact drafts and existing protection/reduction submission; coordinate existing
protection scheduling; expose only the private controller handoff; connect
Desk's device review, Torok controller, preferences/mode fences and Activity;
then exercise crashes, concurrent manual orders, scope changes, partial fills,
unsupported instruments and reconnects. Full repository gates are required before committing this groundwork; a
no-trade installation smoke remains outstanding until deployment. Live commissioning is not authorised by
this build request.
