package spx

import (
	"slices"
	"time"
)

// Companies is an equal-company research measure, separate from risk breadth.
type Companies struct {
	Method        string   `json:"method"`
	CompanyCount  int      `json:"company_count"`
	Coverage50    int      `json:"coverage_50"`
	PctAbove50DMA *float64 `json:"pct_above_50dma"`
	CoverageAD    int      `json:"coverage_ad"`
	Rising        int      `json:"rising"`
	Falling       int      `json:"falling"`
	Unchanged     int      `json:"unchanged"`
	RisingPct     *float64 `json:"rising_pct"`
}

// CompanyRepresentative fixes one share line per known dual-class issuer.
// Missing representatives stay missing; another class is never substituted.
func CompanyRepresentative(symbol string) string {
	switch symbol {
	case "GOOG":
		return "GOOGL"
	case "FOX":
		return "FOXA"
	case "NWS":
		return "NWSA"
	default:
		return symbol
	}
}

func computeCompanies(members []string, windows map[string]ConstituentWindow, session string, now time.Time) *Companies {
	keys := make([]string, 0, len(members))
	listed := make(map[string]bool, len(members))
	for _, s := range members {
		listed[s] = true
		keys = append(keys, CompanyRepresentative(s))
	}
	slices.Sort(keys)
	keys = slices.Compact(keys)
	p := &Companies{Method: "representative-company-v1", CompanyCount: len(keys)}
	previous, above := precedingSession(session), 0
	for _, s := range keys {
		if !listed[s] {
			continue
		}
		w, ok := windows[s]
		if !ok {
			continue
		}
		if w.LastBarAt == session && len(w.Closes) >= 50 {
			values := w.Closes[len(w.Closes)-50:]
			sum, valid := 0.0, true
			for _, c := range values {
				sum += c
				valid = valid && validParticipationClose(c)
			}
			if valid && validParticipationClose(sum) {
				p.Coverage50++
				if values[49] >= sum/50 {
					above++
				}
			}
		}
		var current, prior *Bar
		for i := range w.Bars {
			b := &w.Bars[i]
			if !validParticipationClose(b.Close) || b.ObservedAt.IsZero() || b.ObservedAt.After(now) {
				continue
			}
			if b.Date == session {
				current = b
			}
			if b.Date == previous {
				prior = b
			}
		}
		if current == nil || prior == nil {
			continue
		}
		p.CoverageAD++
		switch {
		case current.Close > prior.Close:
			p.Rising++
		case current.Close < prior.Close:
			p.Falling++
		default:
			p.Unchanged++
		}
	}
	if p.Coverage50 > 0 {
		p.PctAbove50DMA = new(100 * float64(above) / float64(p.Coverage50))
	}
	if p.CoverageAD > 0 {
		p.RisingPct = new(100 * float64(p.Rising) / float64(p.CoverageAD))
	}
	return p
}

// TapeCompanies reads retained inputs without another constituent sweep.
// Older undated closes are not assigned invented dates to backfill averages.
func (e *Engine) TapeCompanies(dates []string, now time.Time) map[string]*Companies {
	e.mu.RLock()
	defer e.mu.RUnlock()
	out := make(map[string]*Companies)
	for _, date := range dates {
		for _, h := range e.history {
			if h.Date != date || h.Participation == nil || len(h.Participation.Members) == 0 {
				continue
			}
			if h.Participation.Companies != nil {
				out[date] = h.Participation.Companies
			} else {
				out[date] = computeCompanies(h.Participation.Members, e.windows, date, now)
			}
			break
		}
	}
	return out
}
