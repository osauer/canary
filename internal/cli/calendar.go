package cli

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/osauer/canary/v2/internal/rpc"
)

func runCalendar(ctx context.Context, env *Env, args []string) int {
	fs := flagSet(env, "calendar")
	jsonOut := fs.Bool("json", false, "emit machine-readable JSON")
	market := fs.String("market", "us", "market calendar: us, us-options, or de")
	date := fs.String("date", "", "local market date YYYY-MM-DD (default: today)")
	next := fs.Int("next", 14, "number of calendar days to include (1-400)")
	if err := fs.Parse(args); err != nil {
		return parseExit(err)
	}
	if fs.NArg() > 0 {
		return failUnexpectedArgs(env, fs)
	}
	if *next <= 0 {
		return fail(env, "calendar: --next must be positive")
	}
	params := rpc.MarketCalendarParams{
		Market: strings.TrimSpace(*market),
		Date:   strings.TrimSpace(*date),
		Days:   *next,
	}
	var res rpc.MarketCalendarResult
	if err := env.Conn.Call(ctx, rpc.MethodMarketCalendar, params, &res); err != nil {
		return fail(env, "calendar: %v", err)
	}
	if *jsonOut {
		return printJSON(env, res)
	}
	return renderCalendarText(env, &res)
}

func renderCalendarText(env *Env, r *rpc.MarketCalendarResult) int {
	out := env.Stdout
	today := time.Now()
	fmt.Fprintln(out)
	fmt.Fprintf(out, "%s calendar  ·  %s\n", r.Label, r.Timezone)
	fmt.Fprintln(out)
	renderCalendarSessionLine(env, r.Session, today)
	if r.Session.SourceURL != "" {
		fmt.Fprintf(out, "  Source:        %s\n", r.Session.SourceURL)
	}
	fmt.Fprintf(out, "  Coverage:      %s to %s\n", r.CoverageStart, r.CoverageEnd)

	if len(r.Sessions) > 1 {
		fmt.Fprintln(out)
		dateLabels := make([]string, len(r.Sessions))
		dateWidth := visibleLen("DATE")
		for i, s := range r.Sessions {
			dateLabels[i] = calendarDateLabel(s.Date, today)
			dateWidth = max(dateWidth, visibleLen(dateLabels[i]))
		}
		header := fmt.Sprintf("  %-*s  %-11s  %-20s  %s", dateWidth, "DATE", "STATE", "HOURS", "REASON")
		fmt.Fprintln(out, env.dim(header))
		fmt.Fprintln(out, env.dim(strings.Repeat("─", visibleLen(header))))
		for i, s := range r.Sessions {
			row := fmt.Sprintf("  %-*s  %-11s  %-20s  %s",
				dateWidth, dateLabels[i], s.State, marketSessionHours(s), nonEmpty(s.Reason, ""))
			if calendarSessionClosedDay(s) {
				row = env.dim(row)
			}
			fmt.Fprintln(out, row)
		}
	}
	fmt.Fprintln(out)
	return 0
}

func renderCalendarSessionLine(env *Env, s rpc.MarketSession, today time.Time) {
	out := env.Stdout
	state := s.State
	if s.IsOpen {
		state = env.green(state)
	} else if s.State == "holiday" || s.State == "early_close" || s.State == "unknown" {
		state = env.yellow(state)
	}
	fmt.Fprintf(out, "  Session:       %s  %s\n", calendarDateLabel(s.Date, today), state)
	if hours := marketSessionHours(s); hours != "" {
		fmt.Fprintf(out, "  Hours:         %s\n", hours)
	}
	if s.Reason != "" {
		fmt.Fprintf(out, "  Reason:        %s\n", s.Reason)
	}
	if s.NextOpen != nil {
		next := marketSessionTime(s, *s.NextOpen).Format("2006-01-02 15:04 MST")
		fmt.Fprintf(out, "  Next open:     %s\n", next)
	}
	if s.Notes != "" {
		fmt.Fprintf(out, "  Notes:         %s\n", s.Notes)
	}
}

type calendarDateStyle int

const (
	calendarDateISO calendarDateStyle = iota
	calendarDateDayMonthYear
)

func calendarDateLabel(date string, today time.Time) string {
	return formatCalendarDateLabel(date, today, calendarDateStyleFromEnv())
}

func formatCalendarDateLabel(date string, today time.Time, style calendarDateStyle) string {
	day, err := time.ParseInLocation("2006-01-02", strings.TrimSpace(date), time.Local)
	if err != nil {
		return date
	}
	layout := "Mon 2006-01-02"
	if style == calendarDateDayMonthYear {
		layout = "Mon 02-01-2006"
	}
	label := day.Format(layout)
	localToday := today.In(time.Local)
	if day.Year() == localToday.Year() && day.Month() == localToday.Month() && day.Day() == localToday.Day() {
		label += " (today)"
	}
	return label
}

func calendarDateStyleFromEnv() calendarDateStyle {
	for _, key := range []string{"LC_TIME", "LC_ALL", "LANG"} {
		raw := os.Getenv(key)
		if calendarLocaleConfigured(raw) {
			return calendarDateStyleFromLocale(raw)
		}
	}
	return calendarDateISO
}

func calendarDateStyleFromLocale(raw string) calendarDateStyle {
	locale := normalizedCalendarLocale(raw)
	if locale == "" || locale == "C" || locale == "POSIX" {
		return calendarDateISO
	}
	parts := strings.Split(locale, "_")
	if len(parts) >= 2 {
		switch strings.ToUpper(parts[1]) {
		case "DE":
			return calendarDateDayMonthYear
		case "US":
			return calendarDateISO
		}
	}
	if len(parts) >= 1 && strings.EqualFold(parts[0], "de") {
		return calendarDateDayMonthYear
	}
	return calendarDateISO
}

func calendarLocaleConfigured(raw string) bool {
	locale := normalizedCalendarLocale(raw)
	return locale != "" && locale != "C" && locale != "POSIX"
}

func normalizedCalendarLocale(raw string) string {
	locale := strings.TrimSpace(raw)
	locale = strings.Split(locale, ".")[0]
	locale = strings.Split(locale, "@")[0]
	return strings.ReplaceAll(locale, "-", "_")
}

func calendarSessionClosedDay(s rpc.MarketSession) bool {
	return s.State == "closed" || s.State == "holiday"
}

func marketSessionHours(s rpc.MarketSession) string {
	if s.Open.IsZero() || s.Close.IsZero() {
		return ""
	}
	open := marketSessionTime(s, s.Open)
	closeT := marketSessionTime(s, s.Close)
	return open.Format("15:04") + "-" + closeT.Format("15:04 MST")
}

func marketSessionTime(s rpc.MarketSession, t time.Time) time.Time {
	if s.Timezone == "" {
		return t
	}
	loc, err := time.LoadLocation(s.Timezone)
	if err != nil {
		return t
	}
	return t.In(loc)
}

func quoteSessionHint(env *Env, s *rpc.MarketSession) string {
	if s == nil {
		return ""
	}
	parts := []string{s.Label}
	if s.State != "" {
		parts = append(parts, s.State)
	}
	if s.Reason != "" {
		parts = append(parts, s.Reason)
	}
	if s.NextOpen != nil {
		parts = append(parts, "next open "+marketSessionTime(*s, *s.NextOpen).Format("2006-01-02 15:04 MST"))
	}
	return env.yellow(strings.Join(parts, " · "))
}
