package spx

import (
	"crypto/sha256"
	"encoding/hex"
	"math"
	"slices"
	"strings"
	"time"

	"github.com/osauer/canary/v2/internal/marketcal"
)

// Participation is an observed constituent comparison, not a risk verdict.
// Members records the universe actually used here, not historical membership.
// RecordedAt identifies this computed revision; InputObservedAt is the latest
// acquisition in its dated input pairs. Neither claims exchange publication time.
type Participation struct {
	Method          string    `json:"method"`
	RecordedAt      time.Time `json:"recorded_at"`
	InputObservedAt time.Time `json:"input_observed_at,omitzero"`
	MembershipID    string    `json:"membership_id"`
	Members         []string  `json:"members"`
	PctAbove20DMA   *float64  `json:"pct_above_20dma"`
	Coverage20      int       `json:"coverage_20"`
	Advancing       int       `json:"advancing"`
	Declining       int       `json:"declining"`
	Unchanged       int       `json:"unchanged"`
	CoverageAD      int       `json:"coverage_ad"`
	AdvancePct      *float64  `json:"advance_pct"`
	AdvancingVolume float64   `json:"advancing_volume"`
	DecliningVolume float64   `json:"declining_volume"`
	UnchangedVolume float64   `json:"unchanged_volume"`
	CoverageVolume  int       `json:"coverage_volume"`
	UpVolumePct     *float64  `json:"up_volume_pct"`
}

func computeParticipation(members []string, windows map[string]ConstituentWindow, session string, now time.Time) *Participation {
	universe := slices.Clone(members)
	slices.Sort(universe)
	universe = slices.Compact(universe)
	hash := sha256.Sum256([]byte(strings.Join(universe, "\n")))
	p := &Participation{Method: "constituent-participation-v1", RecordedAt: now, Members: universe, MembershipID: hex.EncodeToString(hash[:])}
	previous := precedingSession(session)
	above20 := 0
	for _, symbol := range universe {
		w, ok := windows[symbol]
		if !ok || w.LastBarAt != session {
			continue
		}
		if len(w.Closes) >= 20 {
			closes := w.Closes[len(w.Closes)-20:]
			sum, valid := 0.0, true
			for _, close := range closes {
				if !validParticipationClose(close) {
					valid = false
				}
				sum += close
			}
			if valid && !math.IsInf(sum, 0) {
				p.Coverage20++
				if closes[19] >= sum/20 {
					above20++
				}
			}
		}
		var current, prior *Bar
		for i := range w.Bars {
			b := &w.Bars[i]
			if b.Date == session {
				current = b
			}
			if previous != "" && b.Date == previous {
				prior = b
			}
		}
		if current == nil || prior == nil || !validParticipationClose(current.Close) || !validParticipationClose(prior.Close) || current.ObservedAt.IsZero() || prior.ObservedAt.IsZero() || current.ObservedAt.After(now) || prior.ObservedAt.After(now) {
			continue
		}
		p.CoverageAD++
		p.InputObservedAt = laterTime(p.InputObservedAt, laterTime(current.ObservedAt, prior.ObservedAt))
		direction := 0
		switch {
		case current.Close > prior.Close:
			p.Advancing++
			direction = 1
		case current.Close < prior.Close:
			p.Declining++
			direction = -1
		default:
			p.Unchanged++
		}
		if current.Volume == nil || *current.Volume < 0 {
			continue
		}
		p.CoverageVolume++
		volume := float64(*current.Volume)
		switch direction {
		case 1:
			p.AdvancingVolume += volume
		case -1:
			p.DecliningVolume += volume
		default:
			p.UnchangedVolume += volume
		}
	}
	if p.Coverage20 > 0 {
		p.PctAbove20DMA = new(100 * float64(above20) / float64(p.Coverage20))
	}
	if changed := p.Advancing + p.Declining; changed > 0 {
		p.AdvancePct = new(100 * float64(p.Advancing) / float64(changed))
	}
	if changedVolume := p.AdvancingVolume + p.DecliningVolume; changedVolume > 0 {
		p.UpVolumePct = new(100 * p.AdvancingVolume / changedVolume)
	}
	return p
}

func precedingSession(day string) string {
	at, err := time.Parse("2006-01-02", day)
	if err != nil {
		return ""
	}
	r, err := marketcal.New().Query(marketcal.Query{Market: marketcal.MarketUSEquity, Date: at.AddDate(0, 0, -14).Format("2006-01-02"), Days: 15})
	if err != nil || len(r.Sessions) == 0 || r.Sessions[len(r.Sessions)-1].Close.IsZero() {
		return ""
	}
	previous := ""
	for _, s := range r.Sessions {
		if s.Date < day && !s.Close.IsZero() {
			previous = s.Date
		}
	}
	return previous
}

func validParticipationClose(v float64) bool { return v > 0 && !math.IsNaN(v) && !math.IsInf(v, 0) }
func laterTime(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}

func mergeParticipationBars(existing, incoming []Bar) []Bar {
	byDate := make(map[string]Bar, len(existing)+len(incoming))
	for _, b := range existing {
		byDate[b.Date] = b
	}
	for _, b := range incoming {
		if old, ok := byDate[b.Date]; ok && old.Close == b.Close && equalParticipationVolume(old.Volume, b.Volume) {
			continue // An unchanged reread must not refresh acquisition time.
		}
		if b.Volume != nil && *b.Volume < 0 {
			b.Volume = nil
		}
		byDate[b.Date] = b
	}
	out := make([]Bar, 0, len(byDate))
	for _, b := range byDate {
		out = append(out, b)
	}
	slices.SortFunc(out, func(a, b Bar) int { return strings.Compare(a.Date, b.Date) })
	if len(out) > MaxHistoryPoints {
		out = out[len(out)-MaxHistoryPoints:]
	}
	return out
}

func equalParticipationVolume(a, b *int64) bool {
	return a == nil && b == nil || a != nil && b != nil && *a == *b
}
func equalParticipationBars(a, b []Bar) bool {
	return slices.EqualFunc(a, b, func(a, b Bar) bool {
		return a.Date == b.Date && a.Close == b.Close && a.ObservedAt.Equal(b.ObservedAt) && equalParticipationVolume(a.Volume, b.Volume)
	})
}
