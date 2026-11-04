package futusim

import (
	"fmt"
	"hash/fnv"
	"math"
	"slices"
	"time"
)

// Candle periods the book serves beyond a whole number of minutes.
const (
	// PeriodDay is one candle per trading day.
	PeriodDay = -1
	// PeriodWeek is one candle per week, at its last trading day.
	PeriodWeek = -2
	// PeriodMonth is one candle per month, at its last trading day.
	PeriodMonth = -3
)

// prevCloseFactor places the synthetic previous close a little under the start.
const prevCloseFactor = 0.99

// historicSwing is the amplitude of the synthetic price before the book's
// clock starts, and historicCycle its period; the path meets the instrument's
// start exactly at the book's first instant, so the walk continues from it.
const (
	historicSwing = 0.06
	historicCycle = 13 * 24 * time.Hour
)

// BookBar is one synthetic candle; At is its closing instant.
type BookBar struct {
	At     time.Time
	Open   float64
	High   float64
	Low    float64
	Close  float64
	Volume int64
}

// FundsView is one account's funds in one currency, at the walk's marks.
type FundsView struct {
	Currency      string
	TotalAssets   float64
	Cash          float64
	MarketValue   float64
	UnrealizedPnL float64
	Currencies    []string
	ByCurrency    map[string]float64
}

// convert turns an amount between the book's currencies through its HKD rates.
func (b *Book) convert(amount float64, from, to string) float64 {
	if from == to {
		return amount
	}
	fromRate, toRate := b.fx[from], b.fx[to]
	if fromRate <= 0 || toRate <= 0 {
		return 0
	}
	return amount * fromRate / toRate
}

// Funds values an account in a currency: its cash in every currency and its
// positions at the walk.
func (b *Book) Funds(accountID uint64, currency string) (FundsView, error) {
	now := b.Clock.Now()
	b.mu.Lock()
	defer b.mu.Unlock()
	account, err := b.account(accountID)
	if err != nil {
		return FundsView{}, err
	}
	view := FundsView{Currency: currency, ByCurrency: make(map[string]float64, len(account.Cash))}
	for name, amount := range account.Cash {
		view.Currencies = append(view.Currencies, name)
		view.ByCurrency[name] = amount
		view.Cash += b.convert(amount, name, currency)
	}
	slices.Sort(view.Currencies)
	for _, p := range b.positions {
		if p.AccountID != accountID || p.Quantity == 0 {
			continue
		}
		in, err := b.instrument(p.Code)
		if err != nil {
			return FundsView{}, err
		}
		q, err := b.quote(p.Code, now)
		if err != nil {
			return FundsView{}, err
		}
		view.MarketValue += b.convert(q.Last*float64(p.Quantity), in.Currency, currency)
		view.UnrealizedPnL += b.convert((q.Last-p.Cost)*float64(p.Quantity), in.Currency, currency)
	}
	view.TotalAssets = view.Cash + view.MarketValue
	return view, nil
}

// releaseLine drops a held line whatever its age; the server calls it when the
// connection that held the line has closed.
func (b *Book) releaseLine(code string, subType int32) {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.subscriptions, subscriptionKey(code, subType))
}

// positionID is a position's stable synthetic id.
func positionID(p Position) uint64 {
	h := fnv.New64a()
	fmt.Fprintf(h, "%d|%s", p.AccountID, p.Code)
	return h.Sum64() >> 16
}

// priceAt is the instrument's price at an instant: the synthetic history before
// the book's first instant, the walk from it on.
func (b *Book) priceAt(in Instrument, at time.Time) float64 {
	if !at.Before(b.epoch) {
		last, _ := b.walks[in.Code].at(openSeconds(in.Market(), b.epoch, at))
		return last
	}
	angle := 2 * math.Pi * float64(b.epoch.Sub(at)) / float64(historicCycle)
	return roundTick(in.Start*(1+historicSwing*math.Sin(angle)), in.Tick)
}

// barCloses lists the closing instants of the period's candles inside a window.
func barCloses(market string, period int, from, to time.Time) []time.Time {
	table := sessionTables[market]
	loc := zone(table.zone)
	var closes []time.Time
	start := from.In(loc)
	day := time.Date(start.Year(), start.Month(), start.Day(), 0, 0, 0, 0, loc)
	for ; !day.After(to); day = day.AddDate(0, 0, 1) {
		weekday := day.Weekday()
		if weekday == time.Saturday || weekday == time.Sunday {
			continue
		}
		last := table.sessions[len(table.sessions)-1]
		dayClose := day.Add(time.Duration(last.close) * time.Minute)
		switch period {
		case PeriodDay:
			closes = append(closes, dayClose)
		case PeriodWeek:
			if weekday == time.Friday {
				closes = append(closes, dayClose)
			}
		case PeriodMonth:
			next := day.AddDate(0, 0, 1)
			for next.Weekday() == time.Saturday || next.Weekday() == time.Sunday {
				next = next.AddDate(0, 0, 1)
			}
			if next.Month() != day.Month() {
				closes = append(closes, dayClose)
			}
		default:
			for _, s := range table.sessions {
				for minute := s.open + period; minute <= s.close; minute += period {
					closes = append(closes, day.Add(time.Duration(minute)*time.Minute))
				}
			}
		}
	}
	return slices.DeleteFunc(closes, func(at time.Time) bool { return at.Before(from) || at.After(to) })
}

// barsAt builds the candle closing at each instant, deterministically.
func (b *Book) barsAt(in Instrument, period int, closes []time.Time) []BookBar {
	length := time.Duration(period) * time.Minute
	if period <= 0 {
		length = 24 * time.Hour
	}
	bars := make([]BookBar, 0, len(closes))
	for _, at := range closes {
		open, closePrice := b.priceAt(in, at.Add(-length)), b.priceAt(in, at)
		h := fnv.New64a()
		fmt.Fprintf(h, "%d|%s|%d", b.seed, in.Code, at.Unix())
		bars = append(bars, BookBar{
			At:     at,
			Open:   open,
			High:   roundTick(max(open, closePrice)+in.Tick, in.Tick),
			Low:    roundTick(min(open, closePrice)-in.Tick, in.Tick),
			Close:  closePrice,
			Volume: int64(h.Sum64()%1000+1) * in.LotSize,
		})
	}
	return bars
}

// HistoryBars serves the candles between two of OpenD's time strings, read in
// the market's own zone.
func (b *Book) HistoryBars(code string, period int, begin, end string) ([]BookBar, *time.Location, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	in, err := b.instrument(code)
	if err != nil {
		return nil, nil, err
	}
	loc := zone(sessionTables[in.Market()].zone)
	from, err := time.ParseInLocation(wireTime, begin, loc)
	if err != nil {
		return nil, nil, fmt.Errorf("begin time: %w", err)
	}
	to, err := time.ParseInLocation(wireTime, end, loc)
	if err != nil {
		return nil, nil, fmt.Errorf("end time: %w", err)
	}
	return b.barsAt(in, period, barCloses(in.Market(), period, from, to)), loc, nil
}

// RecentBars serves the latest count candles at the clock's instant.
func (b *Book) RecentBars(code string, period, count int) ([]BookBar, *time.Location, error) {
	now := b.Clock.Now()
	b.mu.Lock()
	defer b.mu.Unlock()
	in, err := b.instrument(code)
	if err != nil {
		return nil, nil, err
	}
	span := time.Duration(max(count, 1)) * 24 * time.Hour
	if period > 0 {
		span = time.Duration(count+1) * time.Duration(period) * time.Minute * 8
	}
	if period == PeriodWeek {
		span *= 7
	}
	if period == PeriodMonth {
		span *= 31
	}
	closes := barCloses(in.Market(), period, now.Add(-span), now)
	if len(closes) > count {
		closes = closes[len(closes)-count:]
	}
	return b.barsAt(in, period, closes), zone(sessionTables[in.Market()].zone), nil
}

// SetSubscriptionQuota changes the OpenD-wide quota, as a test uses to make it
// spent by another client.
func (b *Book) SetSubscriptionQuota(quota int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.subscriptionQuota = quota
}

// SetHistoryQuota changes the seven-day quota.
func (b *Book) SetHistoryQuota(quota int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.historyQuota = quota
}

// SetRealLocked locks or unlocks the book's real accounts for trading, a switch for
// a test of the lock refusal; the simulator itself never unlocks on request.
func (b *Book) SetRealLocked(locked bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for i := range b.accounts {
		if b.accounts[i].Env == EnvReal {
			b.accounts[i].Locked = locked
		}
	}
}

// FollowWallClock starts the book's clock and its quote walk at the instant,
// following the wall clock from then on; the process simulator uses it.
func (b *Book) FollowWallClock(at time.Time) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.epoch = at.UTC()
	b.Clock = RunningClock(at)
}
