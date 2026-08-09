# Alerts and notifications

Updated: 2026-07-25 20:23 CEST

An alert is a daemon-owned fact with a lifecycle. The daemon decides whether a condition exists, how serious it is, and whether it opened, escalated, or recovered. The paired app records that decision, decides whether your chosen notification level permits a push, and owns every delivery attempt and receipt. The app never recomputes severity, dwell, escalation, or recovery.

## What raises one

Nine producers can raise an alert, and the list is fixed in the daemon: Stress, Regime, Rulebook, risk policy, Protection, order integrity, reconciliation, governance, and Data Health. Delivery health sits outside that universe on purpose, because it is a downstream result, so a transport failure never counts toward source coverage.

Each producer classifies its condition at one of four severities, in rank order: `observe`, `watch`, `act`, `urgent`. Thresholds stay with the producer; [Sensors](../understand/sensors.md) covers where each one gets its evidence.

Daemon-owned heartbeats re-observe the sources every 30 seconds, so a condition can open with no app or CLI window anywhere. A source that cannot express both an active fact and a trustworthy negative counts as uncovered, and an uncovered source can neither clear a condition nor authorize a push.

## Where it goes

Every occurrence enters the Action Queue of [the paired app](app.md), under a
single read-through cursor shared by all paired devices. The queue shows fixed
app-authored copy; producer keys, targets, and receipts stay private.

The CLI has no alerts or stress command. `canary brief` and `canary rules` carry
the decision-relevant facts as rows.

Phone notifications are configured under Settings → Phone notifications. The level is global for the app host and every paired device; browser permission and the push subscription belong to each browser or installed app separately.

| Setting | Stored value | New occurrences that may push |
| --- | --- | --- |
| Off | `none` | None. The inbox and unread history still work. |
| Action required | `act_only` | `act` and `urgent`. |
| Watch + action | `watch_and_act` | `watch`, `act`, and `urgent`. |

`observe` is inbox-only in every mode, including Watch + action. No setting makes it page.

Eligibility is sampled and frozen when an occurrence is first recorded. Turning notifications up later does not arm something suppressed at creation; the daemon has to produce a new occurrence or a qualifying escalation. Changing the mode changes delivery only, never producer policy, source freshness, risk limits, or whether the condition exists.

## The gate is per source

A durable occurrence is sent only when its scope has a delivery baseline, its state is open or an escalation, its severity is allowed by the current mode, its own source row is covered and inside its producer-authored freshness deadline, the occurrence is still in the current daemon snapshot, the target holds no accepted receipt for it, and an active subscription, signing keys, and a sender all exist.

Because that check is per source, an unrelated source being unavailable cannot suppress an independently current candidate. Silence is treated more strictly: an empty candidate list means **clear** only when the whole expected source set is complete and current, and otherwise the snapshot state is `unknown`, which is not a quiet all-clear.

The first complete, current snapshot in each account-and-mode scope sets that baseline. Conditions already active at that moment stay in the inbox as existing context but are never pushed, so a cutover never pages you with a backlog.

## What a delivery receipt proves

The dispatcher reserves one occurrence-and-target attempt before transport is allowed and records at most one accepted receipt per scope, occurrence, and target, so replayed snapshots, repeated polls, and restarts cannot produce a duplicate push. Retryable transport failures wait 1, 5, and 15 minutes after the first three attempts; a fourth is terminal. A definite rejection is never retried, and a dead subscription is retired so it cannot keep degrading healthy targets.

**A push-service acceptance means the push service accepted the message, and nothing more.** It is not evidence that a device displayed the notification, and it is not evidence that a person read it. The app says so on the page, beside the receipt: "This does not prove the phone displayed it or that it was read." A crash after the final send but before the outcome is persisted is recorded as `interrupted_uncertain` rather than resolved either way. Physical receipt is checked on the physical device or not at all.

Delivery health is derived from every active target's durable attempts, so one healthy target cannot hide another's retry or rejection. A degraded or blocked state banners at the top of the page.

## What a tap does

A tap navigates to one of three allowlisted routes: the Monitor tab, the Brief tab, or the Alerts tab. Destinations are a frozen enum, and any other value, including a hostile payload string, falls back to Monitor. A URL inside a payload is never followed.

A tap never marks anything read. Reading happens only through the app's own acknowledgement path, which requires an authenticated, visible, fully rendered Alerts view plus a short dwell or an in-view interaction, and advances the cursor only across the exact set the page rendered.

An occurrence nobody acknowledges is not lost: unread rows survive compaction indefinitely, and only read, ended history is removed, after 90 days.

## What an alert cannot do

An alert never authorizes an order. The push payload carries fixed app-authored title and body plus an allowlist of severity, kind, destination, display id, and app URL, with no producer prose, symbols, order references, or account fields on the wire. Nothing in the alert path can place, modify, or cancel an order, change a freeze or a limit, approve a risk policy, or turn a degraded observation into a decision. Setting the mode to Off stops new push eligibility and changes no broker order, freeze, limit, or guardrail.
