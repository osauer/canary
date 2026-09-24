package macrosource

import (
	"bytes"
	"cmp"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"html"
	"io"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/osauer/canary/v2/internal/rpc"
)

func parseBEA(s Spec, b []byte, now time.Time) (Batch, error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(b, &raw); err != nil {
		return Batch{}, errors.New("BEA calendar format changed")
	}
	delete(raw, "file_last_updated")
	var rows = make(map[string]struct {
		Dates []string `json:"release_dates"`
	})
	for key, value := range raw {
		var row struct {
			Dates []string `json:"release_dates"`
		}
		if err := json.Unmarshal(value, &row); err != nil {
			return Batch{}, errors.New("BEA calendar format changed")
		}
		rows[key] = row
	}
	if len(rows) == 0 {
		return Batch{}, errors.New("BEA calendar format changed")
	}
	out := Batch{}
	seen := map[string]bool{}
	for title, row := range rows {
		for _, v := range row.Dates {
			at, err := time.Parse(time.RFC3339, v)
			if err != nil {
				return Batch{}, errors.New("BEA release time is invalid")
			}
			k := title + v
			if seen[k] {
				continue
			}
			seen[k] = true
			out.Events = append(out.Events, rpc.MacroEvent{Title: title, Category: "Economic release", Date: at.Format(time.DateOnly), ScheduledAt: at, TimePrecision: "instant"})
		}
	}
	return out, nil
}
func parseICS(s Spec, raw string, now time.Time) (Batch, error) {
	raw = strings.TrimSpace(raw)
	if !strings.HasPrefix(raw, "BEGIN:VCALENDAR\n") && !strings.HasPrefix(raw, "BEGIN:VCALENDAR\r\n") {
		return Batch{}, errors.New("calendar format changed")
	}
	if !strings.HasSuffix(raw, "END:VCALENDAR") {
		// A connection closed without length framing ends the body early
		// without a read error; the next read can deliver the whole calendar.
		return Batch{}, transientPayload("calendar response incomplete")
	}
	if strings.Count(raw, "BEGIN:VCALENDAR") != 1 || strings.Count(raw, "END:VCALENDAR") != 1 {
		return Batch{}, errors.New("calendar response incomplete")
	}
	lines := strings.Split(strings.ReplaceAll(raw, "\r\n", "\n"), "\n")
	unfolded := []string{}
	for _, l := range lines {
		if (strings.HasPrefix(l, " ") || strings.HasPrefix(l, "\t")) && len(unfolded) > 0 {
			unfolded[len(unfolded)-1] += l[1:]
		} else {
			unfolded = append(unfolded, l)
		}
	}
	out := Batch{}
	var row rpc.MacroEvent
	var blsEastern *time.Location
	in := false
	for _, l := range unfolded {
		if l == "BEGIN:VEVENT" {
			if in {
				return Batch{}, errors.New("calendar event nesting invalid")
			}
			in = true
			row = rpc.MacroEvent{Category: "Economic release"}
			continue
		}
		if !in {
			continue
		}
		if l == "END:VCALENDAR" && in {
			return Batch{}, errors.New("calendar response incomplete")
		}
		if l == "END:VEVENT" {
			if row.Title == "" || row.Date == "" {
				return Batch{}, errors.New("calendar event omitted a title or date")
			}
			out.Events = append(out.Events, row)
			in = false
			continue
		}
		key, value, ok := strings.Cut(l, ":")
		if !ok {
			continue
		}
		if key == "SUMMARY" {
			row.Title = strings.NewReplacer(`\,`, ",", `\;`, ";", `\n`, " ").Replace(value)
		}
		if key == "RRULE" || key == "RDATE" || key == "EXDATE" {
			return Batch{}, errors.New("recurring calendar events unsupported")
		}
		if strings.Split(key, ";")[0] != "DTSTART" {
			continue
		}
		if row.Date != "" {
			return Batch{}, errors.New("calendar event has multiple start dates")
		}
		zone := s.Timezone
		hasZone := false
		for _, param := range strings.Split(key, ";")[1:] {
			if after, ok0 := strings.CutPrefix(param, "TZID="); ok0 {
				if hasZone || strings.Trim(after, `"`) == "" {
					return Batch{}, errors.New("calendar timezone parameters invalid")
				}
				hasZone = true
				zone = strings.Trim(after, `"`)
			}
		}
		// RFC 5545 forbids TZID on UTC instants and date-only values. Do not
		// silently select one of two contradictory clock declarations.
		if hasZone && (len(value) == 8 || strings.HasSuffix(value, "Z")) {
			return Batch{}, errors.New("calendar timezone contradicts start value")
		}
		if len(value) == 8 {
			d, err := time.Parse("20060102", value)
			if err != nil {
				return Batch{}, errors.New("calendar date invalid")
			}
			row.Date = d.Format(time.DateOnly)
			row.TimePrecision = "date"
			continue
		}
		var loc *time.Location
		var err error
		if zone == "US-Eastern" {
			if blsEastern == nil {
				blsEastern, err = blsEasternLocation(s, unfolded)
			}
			loc = blsEastern
		} else {
			loc, err = time.LoadLocation(zone)
		}
		if err != nil {
			return Batch{}, errors.New("calendar timezone unsupported")
		}
		layout := "20060102T150405"
		if strings.HasSuffix(value, "Z") {
			layout += "Z"
			loc = time.UTC
		}
		at, err := time.ParseInLocation(layout, value, loc)
		if err != nil || zone == "US-Eastern" && at.Year() < 2007 {
			return Batch{}, errors.New("calendar instant invalid")
		}
		// ParseInLocation can normalize a nonexistent spring-forward hour.
		if zone == "US-Eastern" && !strings.HasSuffix(value, "Z") && at.Format(layout) != value {
			return Batch{}, errors.New("calendar local instant does not exist")
		}
		// Go does not promise which repeated-hour occurrence it chooses.
		// The supported profile has a one-hour fallback; iCalendar chooses
		// the earlier occurrence when the wall clock occurs twice.
		if zone == "US-Eastern" && at.Add(-time.Hour).Format(layout) == value {
			at = at.Add(-time.Hour)
		}
		row.Date = at.Format(time.DateOnly)
		row.ScheduledAt = at
		row.TimePrecision = "instant"
	}
	if in {
		return Batch{}, errors.New("calendar response incomplete")
	}
	return out, nil
}

// blsEasternLocation accepts BLS's declared post-2007 US DST profile, not an
// arbitrary alias or timezone definition that happens to use the same name.
func blsEasternLocation(s Spec, lines []string) (*time.Location, error) {
	if s.ID != "bls-calendar" || s.URL != "https://www.bls.gov/schedule/news_release/bls.ics" || s.Timezone != "America/New_York" {
		return nil, errors.New("calendar timezone unsupported")
	}
	start, end := -1, -1
	for i, line := range lines {
		switch line {
		case "BEGIN:VTIMEZONE":
			if start != -1 {
				return nil, errors.New("calendar timezone definition ambiguous")
			}
			start = i
		case "END:VTIMEZONE":
			if start == -1 || end != -1 {
				return nil, errors.New("calendar timezone definition incomplete")
			}
			end = i
		}
	}
	const declared = `BEGIN:VTIMEZONE
TZID:US-Eastern
BEGIN:DAYLIGHT
TZOFFSETFROM:-0500
TZOFFSETTO:-0400
DTSTART:20070311T020000
RRULE:FREQ=YEARLY;BYMONTH=3;BYDAY=2SU
TZNAME:EDT
END:DAYLIGHT
BEGIN:STANDARD
TZOFFSETFROM:-0400
TZOFFSETTO:-0500
DTSTART:20071104T020000
RRULE:FREQ=YEARLY;BYMONTH=11;BYDAY=1SU
TZNAME:EST
END:STANDARD
END:VTIMEZONE`
	if start == -1 || end <= start || strings.Join(lines[start:end+1], "\n") != declared {
		return nil, errors.New("calendar timezone definition unsupported")
	}
	return time.LoadLocation("America/New_York")
}

func parseFed(s Spec, b []byte, now time.Time) (Batch, error) {
	var doc struct {
		Events []struct{ Month, Days, Title, Time, Type string }
	}
	if json.Unmarshal(bytes.TrimPrefix(b, []byte{239, 187, 191}), &doc) != nil {
		return Batch{}, errors.New("federal reserve calendar format changed")
	}
	out := Batch{}
	loc, _ := time.LoadLocation(s.Timezone)
	for _, r := range doc.Events {
		if r.Month == "" || r.Days == "" {
			continue
		}
		if _, err := time.Parse("2006-01", r.Month); err != nil {
			return Batch{}, errors.New("federal reserve calendar month invalid")
		}
		for part := range strings.SplitSeq(r.Days, ",") {
			ds := strings.Split(strings.TrimSpace(part), "-")
			start, err := strconv.Atoi(strings.TrimSpace(ds[0]))
			if err != nil {
				return Batch{}, errors.New("federal reserve calendar day invalid")
			}
			end := start
			if len(ds) > 2 {
				return Batch{}, errors.New("federal reserve calendar range invalid")
			}
			if len(ds) > 1 {
				end, err = strconv.Atoi(strings.TrimSpace(ds[1]))
				if err != nil || end < start || end > 31 {
					return Batch{}, errors.New("federal reserve calendar range invalid")
				}
			}
			for day := start; day <= end; day++ {
				date := r.Month + "-" + left2(day)
				d, err := time.ParseInLocation(time.DateOnly, date, loc)
				if err != nil {
					return Batch{}, errors.New("federal reserve calendar date invalid")
				}
				e := rpc.MacroEvent{Title: r.Title, Category: r.Type, Date: date, TimePrecision: "date", TimeLabel: r.Time}
				clean := strings.ToUpper(strings.ReplaceAll(strings.ReplaceAll(r.Time, ".", ""), " ", ""))
				tm, err := time.Parse("3:04PM", clean)
				if err == nil {
					e.ScheduledAt = time.Date(d.Year(), d.Month(), d.Day(), tm.Hour(), tm.Minute(), 0, 0, loc)
					e.TimePrecision = "instant"
				}
				out.Events = append(out.Events, e)
			}
		}
	}
	return out, nil
}
func left2(i int) string {
	if i < 10 {
		return "0" + strconv.Itoa(i)
	}
	return strconv.Itoa(i)
}

var ecbRows = regexp.MustCompile(`(?s)<dt>(.*?)</dt>\s*<dd>(.*?)</dd>`)
var ecbEvent = regexp.MustCompile(`(?s)<strong>Event:</strong>(.*?)<br\s*/?>`)
var ecbTime = regexp.MustCompile(`(?s)<strong>Time:</strong>(.*?)<br\s*/?>`)
var htmlTags = regexp.MustCompile(`<[^>]*>`)

func plain(v string) string { return tidy(html.UnescapeString(htmlTags.ReplaceAllString(v, " "))) }
func parseECB(s Spec, b []byte, now time.Time) (Batch, error) {
	out := Batch{}
	for _, r := range ecbRows.FindAllStringSubmatch(string(b), -1) {
		date, err := time.Parse("Monday, 2 January 2006", plain(r[1]))
		if err != nil {
			return Batch{}, errors.New("ECB calendar date format changed")
		}
		title := ecbEvent.FindStringSubmatch(r[2])
		if len(title) < 2 {
			return Batch{}, errors.New("ECB calendar event format changed")
		}
		e := rpc.MacroEvent{Title: plain(title[1]), Date: date.Format(time.DateOnly), Category: "ECB calendar", TimePrecision: "date"}
		if label := ecbTime.FindStringSubmatch(r[2]); len(label) > 1 {
			e.TimeLabel = plain(label[1])
			e.TimePrecision = "source_label"
		}
		out.Events = append(out.Events, e)
	}
	return out, nil
}
func parseRSS(s Spec, b []byte, now time.Time) (Batch, error) {
	var feed struct {
		XMLName xml.Name
		Channel struct {
			Items []struct {
				Title   string `xml:"title"`
				Link    string `xml:"link"`
				PubDate string `xml:"pubDate"`
			} `xml:"item"`
		} `xml:"channel"`
	}
	decoder := xml.NewDecoder(bytes.NewReader(b))
	decoder.CharsetReader = func(charset string, input io.Reader) (io.Reader, error) {
		if strings.EqualFold(charset, "us-ascii") {
			return input, nil
		}
		return nil, errors.New("publication encoding unsupported")
	}
	if decoder.Decode(&feed) != nil || feed.XMLName.Local != "rss" {
		return Batch{}, errors.New("publication feed format changed")
	}
	out := Batch{}
	// One malformed item must not discard its valid neighbours: it is skipped
	// and counted for disclosure, and the first cause fails a feed left with no
	// valid item. A publication dated after receipt still fails the whole feed
	// because it implicates the receipt clock, not only that item.
	var skipped error
	for _, r := range feed.Channel.Items {
		link := strings.TrimSpace(r.Link)
		// A retained BEA item omits the scheme on its own official host.
		// Canonicalize only that fixed host; never resolve arbitrary feed text.
		if strings.HasPrefix(link, "www.bea.gov/") && (s.ID == "bea-news") {
			link = "https://" + link
		}
		if !sourceLink(s, link) || plain(r.Title) == "" {
			out.SkippedItems++
			skipped = cmp.Or(skipped, errors.New("publication title or source link invalid"))
			continue
		}
		p := rpc.MacroPublication{Title: plain(r.Title), SourceURL: link}
		if strings.TrimSpace(r.PubDate) != "" {
			at, err := publicationTime(s, strings.TrimSpace(r.PubDate))
			if err != nil {
				out.SkippedItems++
				skipped = cmp.Or(skipped, err)
				continue
			}
			p.PublishedAt = at
		}
		// An item dated ahead of this clock is accepted once that time passes.
		if !p.PublishedAt.IsZero() && p.PublishedAt.After(now.Add(time.Minute)) {
			return Batch{}, transientPayload("publication time is in the future")
		}
		out.Publications = append(out.Publications, p)
	}
	if len(out.Publications) == 0 && skipped != nil {
		return Batch{}, skipped
	}
	return out, nil
}

// SkippedDisclosure describes the publication-feed items that parsing omitted
// from b, or returns "" when it kept every item. Callers show it beside the
// retained batch; it is not a failure of that batch.
func (b Batch) SkippedDisclosure() string {
	switch {
	case b.SkippedItems <= 0:
		return ""
	case b.SkippedItems == 1:
		return "1 feed item skipped: invalid title, link or publication date"
	}
	return fmt.Sprintf("%d feed items skipped: invalid title, link or publication date", b.SkippedItems)
}

func publicationTime(s Spec, value string) (time.Time, error) {
	for _, layout := range []string{time.RFC1123Z, "Mon, 2 Jan 2006 15:04:05 -0700", time.RFC822Z, time.RFC3339} {
		if at, err := time.Parse(layout, value); err == nil {
			return at, nil
		}
	}
	loc, err := time.LoadLocation(s.Timezone)
	if err != nil {
		return time.Time{}, errors.New("publication timezone unsupported")
	}
	for _, layout := range []string{time.RFC1123, "Mon, 2 Jan 2006 15:04:05 MST", time.RFC822} {
		if at, err := time.ParseInLocation(layout, value, loc); err == nil {
			abbreviation, offset := at.Zone()
			if offset == 0 && abbreviation != "UTC" && abbreviation != "GMT" {
				continue
			}
			return at, nil
		}
	}
	return time.Time{}, errors.New("publication date format changed")
}
