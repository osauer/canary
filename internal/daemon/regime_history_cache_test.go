package daemon

import (
	"testing"
	"time"

	ibkrlib "github.com/osauer/canary/v2/pkg/ibkr"
)

// One root can run either broker. A series the IBKR daemon saved must not
// answer on the OpenD daemon, or the reverse: on 2026-11-14 an IBKR USD/JPY
// series read as current on OpenD, under IBKR's label.
func TestRegimeHistoryOfOneBrokerDoesNotAnswerForTheOther(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	bars := []ibkrlib.HistoricalBar{{Date: now.Format("20060102"), Close: 150}}

	ibkr := newRegimeHistoryCache(dir, nil)
	ibkr.put("USD.JPY", 14, bars, now)

	opend := newRegimeHistoryCache(dir, nil)
	opend.opend = true
	if e, ok := opend.get("USD.JPY", 14); ok {
		t.Fatalf("OpenD read IBKR's series: %+v", e)
	}
	opend.put("USD.JPY", 14, []ibkrlib.HistoricalBar{{Date: now.Format("20060102"), Close: 151}}, now)

	again := newRegimeHistoryCache(dir, nil)
	e, ok := again.get("USD.JPY", 14)
	if !ok || len(e.Bars) != 1 || e.Bars[0].Close != 150 {
		t.Fatalf("IBKR's series = %+v, %v; want its own close of 150", e, ok)
	}
}
