package futusim

import (
	"errors"
	"math"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/osauer/canary/v2/pkg/futu"
)

// The fixture holds nothing that looks like account data: the account-data
// gate's IBKR account-id pattern and placeholder allowlist
// (scripts/check-no-account-data.sh, check 3) find nothing, no cash figure
// is a whole multiple of 100 (the gate's rounded money band, check 6, can
// only match such figures), and the account ids are small synthetic
// integers. The gate's money and private-terms checks themselves need the
// live daemon and the owner's term list, so they run in the pre-commit
// hook on the staged fixture, not here.
func TestFixtureHoldsNoAccountData(t *testing.T) {
	raw, err := os.ReadFile("testdata/book.json")
	if err != nil {
		t.Fatal(err)
	}
	idRe := regexp.MustCompile(`(?i)(^|[^[:alnum:]_])(D?U[0-9]{6,9})([^[:alnum:]]|$)`)
	allow := regexp.MustCompile(`(?i)^(D?U1234567|D?U7654321|DU123456|DU0000000|D?U1111111|D?U2222222|D?U6666666|D?U9999999)$`)
	for _, m := range idRe.FindAllStringSubmatch(string(raw), -1) {
		if !allow.MatchString(m[2]) {
			t.Errorf("fixture carries an account-id shaped token")
		}
	}
	b := Default()
	for _, a := range b.Accounts() {
		if a.ID >= 10_000_000 {
			t.Errorf("account id %d is not a small synthetic integer", a.ID)
		}
		for currency, cash := range a.Cash {
			if math.Mod(cash, 100) == 0 {
				t.Errorf("account %d %s cash is a whole multiple of 100; the money band could match it", a.ID, currency)
			}
		}
	}
	for _, in := range b.Instruments() {
		if !strings.HasPrefix(in.Name, "SYNTH ") {
			t.Errorf("instrument %s is not named as synthetic", in.Code)
		}
	}
	if raw[len(raw)-1] != '\n' {
		t.Error("fixture lacks a final newline")
	}
}

// Two books from the same fixture on the same clock read the same quotes
// and fill the same order at the same price; a different seed does not.
func TestBookIsDeterministicForAFixedClockAndSeed(t *testing.T) {
	one, two := Default(), Default()
	if one.Seed() != two.Seed() || one.Seed() == 0 {
		t.Fatalf("seeds %d %d", one.Seed(), two.Seed())
	}
	for _, step := range []time.Duration{0, 7 * time.Second, 90 * time.Second, 25 * time.Hour} {
		one.Clock.Advance(step)
		two.Clock.Advance(step)
		for _, in := range one.Instruments() {
			q1, err := one.Quote(in.Code)
			if err != nil {
				t.Fatal(err)
			}
			q2, _ := two.Quote(in.Code)
			if q1 != q2 {
				t.Fatalf("%s after %s: %+v vs %+v", in.Code, step, q1, q2)
			}
			if q1.Bid >= q1.Last || q1.Ask <= q1.Last || q1.Last <= 0 {
				t.Fatalf("%s quote %+v", in.Code, q1)
			}
			if r := math.Mod(math.Round(q1.Last/in.Tick*1e6)/1e6, 1); r != 0 {
				t.Fatalf("%s last %v is not on the %v tick", in.Code, q1.Last, in.Tick)
			}
		}
	}
	// The fixture's clock is 09:30 in Hong Kong on a Wednesday: the HK walk
	// moves while the US market, closed at that hour, holds its price.
	fresh := Default()
	hk0, _ := fresh.Quote("HK.00700")
	us0, _ := fresh.Quote("US.AAPL")
	if !hk0.Open || us0.Open || !fresh.MarketOpen("HK") || fresh.MarketOpen("US") || fresh.MarketOpen("XX") {
		t.Fatalf("market state at the fixture clock: HK %v US %v", hk0.Open, us0.Open)
	}
	fresh.Clock.Advance(time.Minute)
	hk1, _ := fresh.Quote("HK.00700")
	us1, _ := fresh.Quote("US.AAPL")
	if hk1.Volume == hk0.Volume || us1.Last != us0.Last || us1.Volume != us0.Volume {
		t.Fatalf("HK %+v -> %+v, US %+v -> %+v", hk0, hk1, us0, us1)
	}
	// Reading an earlier instant replays the walk from its start.
	fresh.Clock.Advance(-time.Minute)
	if back, _ := fresh.Quote("HK.00700"); back != hk0 {
		t.Fatalf("replay %+v, want %+v", back, hk0)
	}
	// A weekend holds every market.
	weekend := NewClock(time.Date(2026, 11, 7, 6, 0, 0, 0, time.UTC))
	for _, m := range []string{"HK", "US", "SG"} {
		if marketOpen(m, weekend.Now()) || openSeconds(m, weekend.Now(), weekend.Now().Add(24*time.Hour)) != 0 {
			t.Fatalf("%s open on a Saturday", m)
		}
	}

	raw, _ := os.ReadFile("testdata/book.json")
	other, err := Parse([]byte(strings.Replace(string(raw), `"seed": 20261104`, `"seed": 7`, 1)))
	if err != nil {
		t.Fatal(err)
	}
	other.Clock.Advance(time.Minute)
	if q, _ := other.Quote("HK.00700"); q.Last == hk1.Last && q.Volume == hk1.Volume {
		t.Fatalf("a different seed walked the same path: %+v", q)
	}
}

func TestOrdersFillAgainstTheWalkAndBookIntoPositionsAndCash(t *testing.T) {
	b := Default()
	const sim, real = 1000001, 1000002
	q, _ := b.Quote("HK.00700")
	cash0, _ := b.Cash(sim)

	// A market buy fills at the ask at once and the cost averages in.
	o, fills, err := b.PlaceOrder(sim, "HK.00700", Buy, 200, nil)
	if err != nil || o.Status != OrderFilled || len(fills) != 1 || fills[0].Price != q.Ask || fills[0].Quantity != 200 {
		t.Fatalf("market buy: %+v %+v %v", o, fills, err)
	}
	positions, _ := b.Positions(sim)
	var held Position
	for _, p := range positions {
		if p.Code == "HK.00700" {
			held = p
		}
	}
	wantCost := (398.4*300 + q.Ask*200) / 500
	if held.Quantity != 500 || math.Abs(held.Cost-wantCost) > 1e-9 {
		t.Fatalf("position %+v, want 500 at %v", held, wantCost)
	}
	cash1, _ := b.Cash(sim)
	if math.Abs((cash0["HKD"]-cash1["HKD"])-q.Ask*200) > 1e-6 || cash1["USD"] != cash0["USD"] {
		t.Fatalf("cash %v -> %v", cash0, cash1)
	}

	// A limit sell above the bid waits, then fills at the limit once the
	// walk reaches it (the clock is advanced until it does).
	limit := q.Bid * 1.0005
	o, fills, err = b.PlaceOrder(sim, "HK.00700", Sell, 100, &limit)
	if err != nil || o.Status != OrderOpen || len(fills) != 0 || *o.Limit != roundTick(limit, 0.2) {
		t.Fatalf("limit sell: %+v %+v %v", o, fills, err)
	}
	var filled []Fill
	for range 3600 {
		b.Clock.Advance(time.Second)
		if filled = b.Settle(); len(filled) > 0 {
			break
		}
	}
	if len(filled) != 1 || filled[0].OrderID != o.ID || filled[0].Price != *o.Limit {
		t.Fatalf("limit fill %+v", filled)
	}
	orders, _ := b.Orders(sim)
	if len(orders) != 2 || orders[1].Status != OrderFilled || orders[1].Filled != 100 {
		t.Fatalf("orders %+v", orders)
	}
	all, _ := b.Fills(sim)
	if len(all) != 2 {
		t.Fatalf("fills %+v", all)
	}

	// A new instrument opens a position; selling it all closes it.
	if _, _, err := b.PlaceOrder(sim, "HK.00005", Buy, 400, nil); err != nil {
		t.Fatal(err)
	}
	if _, _, err := b.PlaceOrder(sim, "HK.00005", Sell, 400, nil); err != nil {
		t.Fatal(err)
	}
	positions, _ = b.Positions(sim)
	for _, p := range positions {
		if p.Code == "HK.00005" {
			t.Fatalf("closed position still listed: %+v", p)
		}
	}

	// Cancel: open only, once.
	far := 1.0
	o, _, _ = b.PlaceOrder(sim, "HK.00700", Buy, 100, &far)
	if c, err := b.CancelOrder(o.ID); err != nil || c.Status != OrderCancelled {
		t.Fatalf("cancel: %+v %v", c, err)
	}
	if _, err := b.CancelOrder(o.ID); !errors.Is(err, ErrOrderClosed) {
		t.Fatalf("second cancel: %v", err)
	}
	if _, err := b.CancelOrder(999); !errors.Is(err, ErrNoOrder) {
		t.Fatalf("unknown cancel: %v", err)
	}

	// Refusals: a locked real account, an unknown account or code, a side
	// that is neither, a quantity off the lot.
	for _, tc := range []struct {
		name string
		acc  uint64
		code string
		side Side
		qty  int64
		want error
	}{
		{"locked", real, "HK.02800", Buy, 500, ErrLocked},
		{"no account", 42, "HK.00700", Buy, 100, ErrNoAccount},
		{"no instrument", sim, "HK.99999", Buy, 100, ErrNoInstrument},
		{"off lot", sim, "HK.00700", Buy, 150, ErrLotSize},
		{"zero", sim, "HK.00700", Sell, 0, ErrLotSize},
	} {
		if _, _, err := b.PlaceOrder(tc.acc, tc.code, tc.side, tc.qty, nil); !errors.Is(err, tc.want) {
			t.Errorf("%s: %v, want %v", tc.name, err, tc.want)
		}
	}
	if _, _, err := b.PlaceOrder(sim, "HK.00700", Side("hold"), 100, nil); err == nil {
		t.Error("a side that is neither was accepted")
	}
	// A closed market holds the order open.
	if o, fills, err := b.PlaceOrder(sim, "US.AAPL", Buy, 1, nil); err != nil || o.Status != OrderOpen || len(fills) != 0 {
		t.Fatalf("order on a closed market: %+v %+v %v", o, fills, err)
	}
	for _, id := range []uint64{42} {
		if _, err := b.Positions(id); !errors.Is(err, ErrNoAccount) {
			t.Fatal("positions of an unknown account")
		}
		if _, err := b.Orders(id); !errors.Is(err, ErrNoAccount) {
			t.Fatal("orders of an unknown account")
		}
		if _, err := b.Fills(id); !errors.Is(err, ErrNoAccount) {
			t.Fatal("fills of an unknown account")
		}
		if _, err := b.Cash(id); !errors.Is(err, ErrNoAccount) {
			t.Fatal("cash of an unknown account")
		}
		if _, err := b.Account(id); !errors.Is(err, ErrNoAccount) {
			t.Fatal("unknown account")
		}
	}
	if a, err := b.Account(real); err != nil || !a.Locked || a.Env != EnvReal {
		t.Fatalf("real account %+v %v", a, err)
	}
	if _, err := b.Instrument("HK.99999"); !errors.Is(err, ErrNoInstrument) {
		t.Fatal("unknown instrument")
	}
	if _, err := b.Quote("HK.99999"); !errors.Is(err, ErrNoInstrument) {
		t.Fatal("quote of an unknown instrument")
	}
}

// Quotas read the one source in pkg/futu: the subscription tier, the
// one-minute hold and the seven-day history window.
func TestQuotasFollowTheDocumentedRules(t *testing.T) {
	b := Default()
	if b.SubscriptionQuota() != futu.SubscriptionQuotaTiers[0] || b.HistoryQuota() != futu.SubscriptionQuotaTiers[0] {
		t.Fatalf("quotas %d %d", b.SubscriptionQuota(), b.HistoryQuota())
	}
	small, err := Parse([]byte(`{"clock":"2026-11-04T01:30:00Z","subscription_quota":2,"history_quota":1,
		"instruments":[{"code":"HK.00700","lot_size":100,"start":1,"tick":0.01},{"code":"HK.00005","lot_size":100,"start":1,"tick":0.01},{"code":"US.AAPL","lot_size":1,"start":1,"tick":0.01}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if err := small.Subscribe("HK.00700", 1); err != nil {
		t.Fatal(err)
	}
	if err := small.Subscribe("HK.00700", 1); err != nil {
		t.Fatalf("a held line costs again: %v", err)
	}
	if err := small.Subscribe("HK.00700", 2); err != nil {
		t.Fatal(err)
	}
	if err := small.Subscribe("HK.00005", 1); !errors.Is(err, ErrQuota) {
		t.Fatalf("third line: %v", err)
	}
	if used, remaining := small.SubscriptionUsage(); used != 2 || remaining != 0 || !small.Subscribed("HK.00700", 2) || small.Subscribed("HK.00005", 1) {
		t.Fatalf("usage %d %d", used, remaining)
	}
	if err := small.Unsubscribe("HK.00700", 1); !errors.Is(err, ErrHold) {
		t.Fatalf("release inside the hold: %v", err)
	}
	small.Clock.Advance(futu.SubscriptionMinimumHold)
	if err := small.Unsubscribe("HK.00700", 1); err != nil {
		t.Fatalf("release after the hold: %v", err)
	}
	if err := small.Unsubscribe("HK.00700", 1); !errors.Is(err, ErrNotSubscribed) {
		t.Fatalf("second release: %v", err)
	}
	if err := small.Subscribe("HK.99999", 1); !errors.Is(err, ErrNoInstrument) {
		t.Fatalf("unknown line: %v", err)
	}

	if err := small.RequestHistory("HK.00700"); err != nil {
		t.Fatal(err)
	}
	if err := small.RequestHistory("HK.00700"); err != nil {
		t.Fatalf("a repeat inside the window costs: %v", err)
	}
	if err := small.RequestHistory("US.AAPL"); !errors.Is(err, ErrHistoryQuota) {
		t.Fatalf("second instrument: %v", err)
	}
	if err := small.RequestHistory("HK.99999"); !errors.Is(err, ErrNoInstrument) {
		t.Fatalf("unknown history: %v", err)
	}
	used, remaining, detail := small.HistoryUsage()
	if used != 1 || remaining != 0 || len(detail) != 1 {
		t.Fatalf("history usage %d %d %v", used, remaining, detail)
	}
	small.Clock.Advance(futu.HistoryQuotaWindow)
	if used, _, _ := small.HistoryUsage(); used != 0 {
		t.Fatalf("quota not released after the window: %d", used)
	}
	if err := small.RequestHistory("US.AAPL"); err != nil {
		t.Fatalf("after the window: %v", err)
	}
}

func TestParseRefusesAnIncompleteBook(t *testing.T) {
	for name, raw := range map[string]string{
		"not json":         `{`,
		"no clock":         `{"instruments":[]}`,
		"instrument":       `{"clock":"2026-11-04T01:30:00Z","instruments":[{"code":"HK.1"}]}`,
		"unknown market":   `{"clock":"2026-11-04T01:30:00Z","instruments":[{"code":"JP.7203","lot_size":100,"start":1,"tick":0.01}]}`,
		"account env":      `{"clock":"2026-11-04T01:30:00Z","accounts":[{"acc_id":1,"env":"paper"}]}`,
		"position account": `{"clock":"2026-11-04T01:30:00Z","positions":[{"acc_id":1,"code":"HK.00700"}]}`,
		"position code":    `{"clock":"2026-11-04T01:30:00Z","accounts":[{"acc_id":1,"env":"real"}],"positions":[{"acc_id":1,"code":"HK.00700"}]}`,
	} {
		if _, err := Parse([]byte(raw)); err == nil {
			t.Errorf("%s: parsed", name)
		}
	}
	if _, err := Load("testdata/missing.json"); err == nil {
		t.Error("missing file loaded")
	}
	if b, err := Load("testdata/book.json"); err != nil || len(b.Accounts()) != 2 {
		t.Fatalf("load: %v", err)
	}
}

func TestRunningClockFollowsTheWallClockFromItsStart(t *testing.T) {
	start := time.Date(2026, 11, 4, 1, 30, 0, 0, time.UTC)
	c := RunningClock(start)
	now := c.Now()
	if now.Before(start) || now.Sub(start) > time.Minute {
		t.Fatalf("running clock at %s from %s", now, start)
	}
	c.Advance(time.Hour)
	if later := c.Now(); later.Sub(now) < time.Hour {
		t.Fatalf("advance on a running clock: %s -> %s", now, later)
	}
}
