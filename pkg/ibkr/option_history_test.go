package ibkr

import (
	"strconv"
	"strings"
	"testing"
	"time"
)

// An option's closes are read by its contract id alone, as hourly bars folded
// to each New York day's last one. IBKR has no end-of-day bars for an option
// on its smart route (live, 2026-11-19: "No data of type EODChart is
// available for the exchange 'BEST' and the security type 'Option'"), so the
// request must not ask for "1 day". It carries the option's own id and
// security type and no symbol, whatever the caller put in the symbol field
// and whatever the contract cache holds for the underlying: the symbol-keyed
// daily read would settle on the stock's cached contract and return the
// stock's closes as the option's.
func TestOptionDailyTradeBarsAreRequestedByContractIDAlone(t *testing.T) {
	c, _, g := histTFixture(t)
	c.contractMu.Lock()
	c.contractCache["SYNX"] = ContractDetailsLite{Symbol: "SYNX", ConID: 4242, SecType: "STK", Exchange: "SMART", PrimaryExch: "NASDAQ", ValidExchanges: "SMART,NASDAQ"}
	c.contractMu.Unlock()
	hours := []struct {
		at    time.Time
		close string
	}{
		{time.Date(2026, 9, 28, 14, 0, 0, 0, time.UTC), "2.3"},
		{time.Date(2026, 9, 28, 19, 0, 0, 0, time.UTC), "2.4"},
		{time.Date(2026, 9, 29, 15, 0, 0, 0, time.UTC), "2.15"},
	}
	g.script(func(fields []string) {
		if fields[0] != strconv.Itoa(reqHistoricalData) {
			return
		}
		frame := []string{strconv.Itoa(msgHistoricalData), fields[1], strconv.Itoa(len(hours))}
		for _, h := range hours {
			frame = append(frame, strconv.FormatInt(h.at.Unix(), 10), h.close, h.close, h.close, h.close, "100", h.close, "4")
		}
		c.handleHistoricalData(frame)
		c.handleHistoricalDataEnd([]string{strconv.Itoa(msgHistoricalDataEnd), fields[1], strconv.FormatInt(hours[0].at.Unix(), 10), strconv.FormatInt(hours[2].at.Unix(), 10), ""})
	})

	bars, err := c.FetchOptionDailyTradeBars(t.Context(), Contract{ConID: 700123, Symbol: "SYNX", SecType: "opt", Currency: "usd"}, 400, 2*time.Second)
	if err != nil {
		t.Fatalf("option closes: %v", err)
	}
	if len(bars) != 2 || bars[0].Date != "20260928" || bars[0].Close != 2.4 || bars[1].Date != "20260929" || bars[1].Close != 2.15 {
		t.Fatalf("bars = %+v, want each day's last hourly close", bars)
	}
	frames := g.framesByID(reqHistoricalData)
	if len(frames) != 1 {
		t.Fatalf("%d historical requests sent, want one", len(frames))
	}
	frame := frames[0]
	for index, want := range map[int]string{2: "700123", 3: "", 4: "OPT", 9: "SMART", 10: "", 11: "USD", 16: "1 hour", 17: "30 D", 18: "1", 19: "TRADES", 20: "2"} {
		if len(frame) <= index || frame[index] != want {
			t.Fatalf("request field %d = %q, want %q in %s", index, frame[index], want, strings.Join(frame, "|"))
		}
	}
	if strings.Contains(strings.Join(frame, "|"), "4242") {
		t.Fatalf("the stock's contract id reached the option request: %s", strings.Join(frame, "|"))
	}
	if sent := len(g.framesByID(reqContractData)); sent != 0 {
		t.Fatalf("%d contract lookups sent; the contract id is the whole identity", sent)
	}
}

// Anything but an exact option contract is refused before a request is sent.
func TestOptionDailyTradeBarsRefuseAnInexactContract(t *testing.T) {
	c, _, g := histTFixture(t)
	for name, contract := range map[string]Contract{
		"no contract id": {Symbol: "SYNX", SecType: "OPT", Currency: "USD"},
		"a stock":        {ConID: 4242, Symbol: "SYNX", SecType: "STK", Currency: "USD"},
		"no type":        {ConID: 700123, Currency: "USD"},
	} {
		if _, err := c.FetchOptionDailyTradeBars(t.Context(), contract, 35, time.Second); err == nil {
			t.Fatalf("%s: accepted", name)
		}
	}
	if sent := len(g.framesByID(reqHistoricalData)); sent != 0 {
		t.Fatalf("%d historical requests sent for refused contracts", sent)
	}
}
