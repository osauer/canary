package tui

import (
	"fmt"
	"strings"

	"github.com/osauer/canary/v2/internal/app/live"
	"github.com/osauer/canary/v2/internal/productidentity"
)

func statusLine(m *model) string {
	snap := m.snapshot
	parts := []string{styleBrand(" " + productidentity.ProductName + " "), connectionSegment(snap)}
	if snap.Calendar != nil && snap.Calendar.Session.State != "" {
		parts = append(parts, styleDim("mkt ")+snap.Calendar.Session.State)
	}
	if snap.Trading != nil {
		parts = append(parts, tradingSegment(snap))
	}
	if m.active != nil {
		parts = append(parts, styleWarn("run "+m.active.line))
	}
	if !snap.UpdatedAt.IsZero() {
		parts = append(parts, styleDim("live:"+snap.UpdatedAt.Local().Format("15:04:05")))
	}
	return strings.Join(parts, styleDim("  |  "))
}

func connectionSegment(snap live.Snapshot) string {
	if snap.Status == nil {
		return styleWarn("gateway connecting")
	}
	if !snap.Status.Connected {
		msg := "gateway down"
		if snap.Status.LastError != "" {
			msg += " " + snap.Status.LastError
		}
		return styleDanger(msg)
	}
	account := snap.Status.ConnectedAccount
	if account == "" {
		account = snap.Status.Account
	}
	if account == "" {
		account = "auto"
	}
	return styleOK("gateway up") + " " + styleStrong(account) + " " + strings.ToLower(string(snap.Status.AccountMode))
}

func tradingSegment(snap live.Snapshot) string {
	switch {
	case snap.Trading.CanWrite:
		return styleOK("trade submit")
	case snap.Trading.CanPreview:
		return styleWarn("trade preview")
	default:
		return styleDanger("trade off")
	}
}

func riskPanelLines(m *model, width, height int) []string {
	if width < 10 || height <= 0 {
		return nil
	}
	lines := []string{canaryPanelTitle(width)}
	snap := m.snapshot
	if snap.Stress != nil {
		lines = append(lines, riskRow(width, "Stress", severityStyle(string(snap.Stress.Severity))))
		if snap.Stress.Action != "" {
			lines = append(lines, riskRow(width, "Action", snap.Stress.Action))
		}
	}
	if snap.Regime != nil {
		if snap.Regime.Composite.Verdict != "" {
			lines = append(lines, riskRow(width, "Regime", snap.Regime.Composite.Verdict))
		}
		if snap.Regime.Summary.PunchLine != "" {
			lines = append(lines, fit(" "+styleDim(snap.Regime.Summary.PunchLine), width))
		}
		if len(snap.Regime.WarningDetails) > 0 {
			lines = append(lines, riskRow(width, "Warn", styleWarn(fmt.Sprintf("%d", len(snap.Regime.WarningDetails)))))
		}
	}
	if len(snap.Errors) > 0 {
		lines = append(lines, riskRow(width, "Sources", styleDanger("degraded")))
	}
	if len(lines) == 1 {
		lines = append(lines, riskRow(width, "State", styleDim("waiting for live snapshot")))
	}
	for len(lines) < height {
		lines = append(lines, "")
	}
	return lines[:height]
}

func riskRow(width int, label, value string) string {
	return fit(" "+styleDim(fmt.Sprintf("%-7s", label))+value, width)
}

func severityStyle(sev string) string {
	switch strings.ToLower(strings.TrimSpace(sev)) {
	case "ok", "low", "normal", "green":
		return styleOK(sev)
	case "high", "critical", "red":
		return styleDanger(sev)
	case "":
		return styleDim("unknown")
	default:
		return styleWarn(sev)
	}
}
