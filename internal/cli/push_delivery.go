package cli

import (
	"fmt"
	"strings"
	"time"

	"github.com/osauer/canary/v2/internal/rpc"
)

// pushReportStaleAfter is how long the daemon's copy of the app's proof may
// age before status says the app host stopped reporting (the app relays on
// every change and on a five-minute heartbeat).
const pushReportStaleAfter = 15 * time.Minute

// PushDeliveryLine renders the one-line phone push verdict shared by
// `canary status` and `canary app status`: how long alert pushes have been
// silent, whether a device ever witnessed a push, and what blocks delivery.
// ok is true only when a paired device has reported a push displayed or
// opened and nothing currently blocks delivery. Push-service acceptance is
// never a witness.
func PushDeliveryLine(p rpc.PushDeliveryProof, now time.Time) (string, bool) {
	return pushDeliveryLine(p, now, time.Local)
}

func pushDeliveryLine(p rpc.PushDeliveryProof, now time.Time, loc *time.Location) (string, bool) {
	stamp := func(at time.Time) string { return at.In(loc).Format("2006-01-02 15:04") }
	parts := make([]string, 0, 6)
	switch {
	case p.SilentSince == nil:
		parts = append(parts, "no alert push accepted on record")
	case now.Sub(*p.SilentSince) >= 24*time.Hour:
		days := int(now.Sub(*p.SilentSince) / (24 * time.Hour))
		parts = append(parts, fmt.Sprintf("silent since %s (%d %s)", stamp(*p.SilentSince), days, plural(days, "day", "days")))
	default:
		parts = append(parts, "last alert push "+stamp(*p.SilentSince))
	}
	witness, witnessed := p.LastWitness()
	if witnessed {
		label := fmt.Sprintf("%s on %s %s", witness.Event, witness.Device, stamp(witness.At))
		if witness.Kind == rpc.PushNoticeKindDiagnostic {
			label += " (test)"
		}
		parts = append(parts, label)
	} else {
		parts = append(parts, "never witnessed on a device")
	}
	// The silence clock covers accepted alert pushes; name the last push too
	// when it says something the clock does not: a refusal or a test push.
	if sent := p.LastSent; sent != nil && (!sent.Accepted || sent.Kind == rpc.PushNoticeKindDiagnostic) {
		class := sent.Class
		if sent.HTTPStatus != 0 {
			class = fmt.Sprintf("%s %d", class, sent.HTTPStatus)
		}
		label := fmt.Sprintf("last push %s %s", class, stamp(sent.At))
		if sent.Kind == rpc.PushNoticeKindDiagnostic {
			label += " (test)"
		}
		parts = append(parts, label)
	}
	if p.IntakeRejectedSince != nil {
		parts = append(parts, "alert intake rejected since "+stamp(*p.IntakeRejectedSince))
	}
	if p.SubscriptionExpiredAt != nil && (!witnessed || p.SubscriptionExpiredAt.After(witness.At)) {
		parts = append(parts, "a subscription expired "+stamp(*p.SubscriptionExpiredAt)+"; enable notifications on the phone")
	}
	switch {
	case p.Mode == "none":
		parts = append(parts, "notifications off")
	case p.ActiveSubscriptions == 0:
		parts = append(parts, "no push subscription")
	}
	if p.Dispatcher != "healthy" && p.IntakeRejectedSince == nil {
		dispatcher := "dispatcher " + p.Dispatcher
		if p.DispatcherClass != "" {
			dispatcher += " (" + p.DispatcherClass + ")"
		}
		parts = append(parts, dispatcher)
	}
	ok := witnessed && p.Dispatcher == "healthy" && p.IntakeRejectedSince == nil && p.ActiveSubscriptions > 0 && p.Mode != "none"
	return strings.Join(parts, " · "), ok
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// formatPushDeliveryValue is the `canary status` row: the shared verdict,
// plus a note when the app host has stopped reporting to the daemon.
func formatPushDeliveryValue(env *Env, status rpc.PushDeliveryStatus, now time.Time) string {
	return formatPushDeliveryValueIn(env, status, now, time.Local)
}

func formatPushDeliveryValueIn(env *Env, status rpc.PushDeliveryStatus, now time.Time, loc *time.Location) string {
	line, ok := pushDeliveryLine(status.Proof, now, loc)
	if age := now.Sub(status.ReceivedAt); age > pushReportStaleAfter {
		line += " · app host last reported " + status.ReceivedAt.In(loc).Format("2006-01-02 15:04")
		ok = false
	}
	if ok {
		return env.green(line)
	}
	return env.yellow(line)
}
