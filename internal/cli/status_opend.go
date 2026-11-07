package cli

import (
	"fmt"
	"io"
	"maps"
	"slices"
	"strings"

	"github.com/osauer/canary/v2/internal/rpc"
)

// renderOpenDRows prints OpenD's facts in place of the TWS and API rows.
func renderOpenDRows(env *Env, out io.Writer, res *rpc.HealthResult) {
	o := res.OpenD
	statusHealthRow(env, out, "OpenD", formatOpenDValue(env, res))
	if o.ServerVersion != 0 {
		statusHealthRow(env, out, "Protocol", fmt.Sprintf("serverVer %d · Canary minimum %d", o.ServerVersion, o.MinServerVersion))
	}
	if len(o.Markets) > 0 {
		statusHealthRow(env, out, "Markets", strings.Join(o.Markets, ", ")+" tradable on this account")
	}
	if len(o.Markets) > 0 {
		statusHealthRow(env, out, "Broker access", "stocks on "+strings.Join(o.Markets, ", ")+"; options and futures not served on OpenD")
	}
	if len(o.QuoteRights) > 0 {
		statusHealthRow(env, out, "Quote rights", formatOpenDQuoteRights(o.QuoteRights))
	}
	if o.SubscriptionQuota > 0 || o.HistoryQuota > 0 {
		statusHealthRow(env, out, "Quotas", fmt.Sprintf("subscriptions %d of %d · history %d of %d (7-day window)", o.SubscriptionsUsed, o.SubscriptionQuota, o.HistoryUsed, o.HistoryQuota))
	}
	if len(o.NotServed) > 0 {
		statusHealthRow(env, out, "Not on OpenD", strings.Join(o.NotServed, ", "))
	}
}

// formatOpenDValue is OpenD's own state in words, or what it is waiting for.
func formatOpenDValue(env *Env, res *rpc.HealthResult) string {
	o := res.OpenD
	switch {
	case o.ProgramStatus == "":
		return env.red("not connected")
	case o.ProgramStatus != "Ready":
		return env.yellow(openDProgramStatusWords(o.ProgramStatus))
	}
	login := func(name string, in bool) string {
		if in {
			return name + " server logged in"
		}
		return env.yellow(name + " server not logged in")
	}
	version := fmt.Sprintf("%d.%d", o.ServerVersion/100, o.ServerVersion%100)
	return fmt.Sprintf("%s (build %d) · %s · %s", version, o.BuildNo, login("quote", o.QuoteLoggedIn), login("trade", o.TradeLoggedIn))
}

// openDProgramStatusWords says what OpenD is waiting for.
func openDProgramStatusWords(status string) string {
	switch status {
	case "UnAgreeDisclaimer":
		return "waiting for the disclaimer in OpenD"
	case "Loaded", "Loging", "NessaryDataPreparing":
		return "OpenD is starting"
	case "NeedPicVerifyCode", "NeedPhoneVerifyCode":
		return "waiting for a verification code in OpenD"
	case "LoginFailed":
		return "login failed in OpenD"
	case "ForceUpdate":
		return "OpenD requires a newer Canary"
	case "ForceLogout":
		return "OpenD was logged out: log in again"
	}
	return "OpenD reports " + status
}

func formatOpenDQuoteRights(rights map[string]string) string {
	parts := make([]string, 0, len(rights))
	for _, market := range slices.Sorted(maps.Keys(rights)) {
		level := rights[market]
		switch level {
		case "Level1":
			level = "LV1"
		case "Level2":
			level = "LV2"
		case "Level3":
			level = "LV3"
		case "No", "Unknow":
			level = "none"
		}
		parts = append(parts, market+" "+level)
	}
	return strings.Join(parts, " · ")
}
