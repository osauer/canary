package macrosource

import (
	"context"
	"errors"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/osauer/canary/v2/internal/rpc"
)

var nyfedMonth = regexp.MustCompile(`(?is)<td\b[^>]*class="ts-data-table-head"[^>]*>\s*<div\b[^>]*>([A-Za-z]+ [0-9]{4})</div>\s*</td>`)
var nyfedTable = regexp.MustCompile(`(?is)<table\b[^>]*class="research-table-1col greyborder"[^>]*>(.*?)</table>`)
var nyfedCell = regexp.MustCompile(`(?is)<td\b[^>]*>(.*?)</td>`)

// nyfedTimeAfterLink and nyfedTimeInLink each match one release entry: a link
// and its "(HH:MM)" Eastern time label. The calendar usually prints the label
// after the link, sometimes behind a PDF icon; since October 2026 it also
// prints some labels inside the link text. Both are anchored at the entry's
// own link.
var nyfedTimeAfterLink = regexp.MustCompile(`(?is)^<a\b[^>]*>(.*?)</a>\s*(?:<img\b[^>]*>\s*)?<br\s*/?>\s*\(([0-9]{2}:[0-9]{2})\)`)
var nyfedTimeInLink = regexp.MustCompile(`(?is)^<a\b[^>]*>(.*?)\s*\(([0-9]{2}:[0-9]{2})\)\s*</a>`)
var nyfedAnchor = regexp.MustCompile(`(?is)<a\b`)
var nyfedAnchorEnd = regexp.MustCompile(`(?i)</a>`)
var nyfedDay = regexp.MustCompile(`^([0-9]{2})(?:\s|$)`)
var nyfedNext = regexp.MustCompile(`(?is)<a\b[^>]*href="(/research/calendars/i-[a-z]{3}[0-9]{2}\.html)"[^>]*>\s*NEXT MONTH`)
var nyfedMonthPath = regexp.MustCompile(`^/research/calendars/i-[a-z]{3}[0-9]{2}\.html$`)

func calendarSourceURL(s Spec, raw string) bool {
	if raw == s.URL {
		return true
	}
	u, err := url.Parse(raw)
	return s.Kind == "nyfed" && err == nil && SafeURL(raw) && u.Hostname() == "www.newyorkfed.org" && u.RawQuery == "" && u.Fragment == "" && nyfedMonthPath.MatchString(u.Path)
}

// parseNYFed reads one monthly New York Fed calendar page. The page must name
// one month that agrees with its URL, state Eastern time and list every
// weekday; any such defect fails the page. A release link that matches neither
// entry shape is skipped and counted in Batch.SkippedItems, and a page left
// with no release fails.
func parseNYFed(s Spec, raw string, now time.Time) (Batch, error) {
	months := nyfedMonth.FindAllStringSubmatch(raw, -1)
	tables := nyfedTable.FindAllStringSubmatch(raw, -1)
	if len(months) != 1 || len(tables) != 1 || !strings.Contains(raw, "all Eastern Time") {
		return Batch{}, errors.New("calendar source: New York Fed calendar format or timezone changed")
	}
	month, table := months[0], tables[0]
	loc, err := time.LoadLocation(s.Timezone)
	if err != nil {
		return Batch{}, errors.New("calendar source: New York Fed calendar timezone invalid")
	}
	first, err := time.ParseInLocation("January 2006", month[1], loc)
	if err != nil {
		return Batch{}, errors.New("calendar source: New York Fed calendar month invalid")
	}
	if u, err := url.Parse(s.URL); err != nil || nyfedMonthPath.MatchString(u.Path) && u.Path != "/research/calendars/i-"+strings.ToLower(first.Format("Jan06"))+".html" {
		return Batch{}, errors.New("calendar source: New York Fed calendar URL and month disagree")
	}
	out := Batch{WindowStart: first.Format(time.DateOnly), WindowEnd: first.AddDate(0, 1, -1).Format(time.DateOnly)}
	seenDays := map[int]bool{}
	for _, cell := range nyfedCell.FindAllStringSubmatch(table[1], -1) {
		text := plain(cell[1])
		dayMatch := nyfedDay.FindStringSubmatch(text)
		if len(dayMatch) != 2 {
			if nyfedAnchor.MatchString(cell[1]) {
				return Batch{}, errors.New("calendar source: New York Fed release omitted its date")
			}
			continue
		}
		day, _ := strconv.Atoi(dayMatch[1])
		date := time.Date(first.Year(), first.Month(), day, 0, 0, 0, 0, loc)
		if date.Month() != first.Month() || seenDays[day] {
			return Batch{}, errors.New("calendar source: New York Fed calendar day invalid")
		}
		seenDays[day] = true
		anchors := nyfedAnchor.FindAllStringIndex(cell[1], -1)
		for i, at := range anchors {
			// Bound each entry by the next link so no title can absorb a
			// neighboring release.
			end := len(cell[1])
			if i+1 < len(anchors) {
				end = anchors[i+1][0]
			}
			title, label, ok := nyfedRelease(cell[1][at[0]:end])
			if !ok {
				// One entry in an unknown shape must not discard the month's
				// other releases; the count is disclosed with the batch.
				out.SkippedItems++
				continue
			}
			// A recognized entry with an impossible clock is a data defect,
			// not an unknown shape, and still fails the page.
			tm, err := time.Parse("15:04", label)
			if err != nil {
				return Batch{}, errors.New("calendar source: New York Fed release time invalid")
			}
			event := rpc.MacroEvent{Title: title, Category: "Economic release", Date: date.Format(time.DateOnly), TimePrecision: "source_label", TimeLabel: label}
			// This calendar mixes morning labels with afternoon labels such as
			// R-Star's "02:00". Without a meridiem, 01-12 cannot become an instant.
			if tm.Hour() == 0 || tm.Hour() > 12 {
				event.ScheduledAt = time.Date(date.Year(), date.Month(), date.Day(), tm.Hour(), tm.Minute(), 0, 0, loc)
				event.TimePrecision = "instant"
			}
			out.Events = append(out.Events, event)
		}
	}
	// A partial HTML response must not establish that missing weekdays are clear.
	for day := first; day.Month() == first.Month(); day = day.AddDate(0, 0, 1) {
		if day.Weekday() != time.Saturday && day.Weekday() != time.Sunday && !seenDays[day.Day()] {
			return Batch{}, errors.New("calendar source: New York Fed calendar weekdays incomplete")
		}
	}
	if len(out.Events) == 0 && out.SkippedItems > 0 {
		return Batch{}, errors.New("calendar source: New York Fed release time or format invalid")
	}
	return out, nil
}

// nyfedRelease reads the release entry in segment, which starts at the entry's
// link and ends before the next link in its day cell. It returns the raw title
// markup and the "HH:MM" label, or ok false when the entry has no title or
// matches neither entry shape.
func nyfedRelease(segment string) (title, label string, ok bool) {
	m := nyfedTimeAfterLink.FindStringSubmatch(segment)
	if m == nil {
		m = nyfedTimeInLink.FindStringSubmatch(segment)
	}
	// A title that spans a closing tag belongs to markup neither shape models.
	if m == nil || nyfedAnchorEnd.MatchString(m[1]) || plain(m[1]) == "" {
		return "", "", false
	}
	return m[1], m[2], true
}

func (c *Client) fetchNYFed(ctx context.Context, spec Spec, now time.Time) (Batch, error) {
	// Read only consecutive next-month links published by the calendar. The
	// landing page may still name last month at rollover; its published successor
	// can establish the current month without guessing an archive URL.
	res, err := c.read(ctx, spec.URL, Batch{})
	if err != nil {
		return Batch{}, err
	}
	raw := res.body
	batch, err := Parse(spec, raw, now)
	if err != nil {
		return Batch{}, err
	}
	loc, _ := time.LoadLocation(spec.Timezone)
	today := now.In(loc).Format(time.DateOnly)
	first, _ := time.Parse(time.DateOnly, batch.WindowStart)
	if today < batch.WindowStart || today >= first.AddDate(0, 2, 0).Format(time.DateOnly) {
		return Batch{}, errors.New("calendar source: New York Fed calendar does not cover the current date")
	}
	if today > batch.WindowEnd {
		batch, raw, err = c.fetchNYFedNext(ctx, spec, batch, raw, now)
		if err != nil {
			return Batch{}, err
		}
	}
	if now.In(loc).AddDate(0, 0, 7).Format(time.DateOnly) <= batch.WindowEnd {
		return batch, nil
	}
	more, _, err := c.fetchNYFedNext(ctx, spec, batch, raw, now)
	if err != nil {
		return Batch{}, err
	}
	batch.Events = append(batch.Events, more.Events...)
	batch.WindowEnd = more.WindowEnd
	batch.SkippedItems += more.SkippedItems
	if err := ValidateBatch(spec, batch, now); err != nil {
		return Batch{}, err
	}
	return batch, nil
}

func (c *Client) fetchNYFedNext(ctx context.Context, spec Spec, prior Batch, raw []byte, now time.Time) (Batch, []byte, error) {
	links := nyfedNext.FindAllStringSubmatch(string(raw), -1)
	if len(links) != 1 {
		return Batch{}, nil, errors.New("calendar source: New York Fed next-month calendar link unavailable")
	}
	end, _ := time.Parse(time.DateOnly, prior.WindowEnd)
	nextMonth := end.AddDate(0, 0, 1)
	// Check the link's month before requesting it, then verify the response
	// heading as well. An allowed host alone cannot establish provenance.
	if links[0][1] != "/research/calendars/i-"+strings.ToLower(nextMonth.Format("Jan06"))+".html" {
		return Batch{}, nil, errors.New("calendar source: New York Fed next-month link is not consecutive")
	}
	next := spec
	next.URL = "https://www.newyorkfed.org" + links[0][1]
	res, err := c.read(ctx, next.URL, Batch{})
	if err != nil {
		return Batch{}, nil, err
	}
	raw = res.body
	more, err := Parse(next, raw, now)
	if err != nil {
		return Batch{}, nil, err
	}
	if more.WindowStart != nextMonth.Format(time.DateOnly) {
		return Batch{}, nil, errors.New("calendar source: New York Fed next-month calendar is not consecutive")
	}
	return more, raw, nil
}
