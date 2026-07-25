# Alerts and notifications

Status: planned. This file is the brief for the page, not the page itself.

**Audience.** Someone who wants to be told when something matters and is not
willing to watch a screen to find out.

**Questions the page has to answer**

- What raises an alert: which sensors, which thresholds, which state changes.
- Where an alert goes: the CLI, the paired app inbox, push notification.
- How delivery is proven, and what an unproven delivery means. A sensor
  measuring is not the same as a phone buzzing.
- Which alerts are opt-in, and how to turn the volume up or down.
- What happens to an alert nobody acknowledges.
- What tapping a notification opens.

**Draw from**

- `docs/design/alert-regime-production.md`.
- `docs/design/notification-tap-landing.md`.
- `docs/design/risk-governance-nudges.md` for the nudge class.
- The active alert inbox contract enforced by `make app-active-alert-inbox-check`.

**Boundaries to keep**

- An alert never authorizes an order.
- Do not promise delivery guarantees the transport does not provide.
