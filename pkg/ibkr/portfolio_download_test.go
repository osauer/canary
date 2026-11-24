package ibkr

import (
	"bufio"
	"strconv"
	"strings"
	"testing"
	"time"
)

// syntheticBookRows is a seven-option book worth 10,000 USD per row. With
// ExchangeRate 0.8 the stream's gross position value is 56,000 EUR.
const (
	syntheticBookRows      = 7
	syntheticRowValueUSD   = 10_000
	syntheticGrossValueEUR = "56000"
)

func feedAccountValue(conn *Connection, key, value, currency, account string) {
	conn.processMessage(conn.encodeMsg(msgAcctValue, "2", key, value, currency, account))
}

// feedAccountValues replays the account-value head of a reqAccountUpdates
// download: the gross position value is complete even when TWS has not yet
// loaded every portfolio row.
func feedAccountValues(conn *Connection, account string) {
	feedAccountValue(conn, "NetLiquidation", "100000", "EUR", account)
	feedAccountValue(conn, "GrossPositionValue", syntheticGrossValueEUR, "EUR", account)
	feedAccountValue(conn, "OptionMarketValue", strconv.Itoa(syntheticBookRows*syntheticRowValueUSD), "USD", account)
	feedAccountValue(conn, "ExchangeRate", "0.8", "USD", account)
}

func feedOptionRow(conn *Connection, account string, conID int) {
	conn.processMessage(conn.encodeMsg(msgPortfolioValue, "8",
		strconv.Itoa(conID), "SYN", "OPT", "20261218", "100", "C", "100", "", "USD", "SYN "+strconv.Itoa(conID), "SYN",
		"1", "100", strconv.Itoa(syntheticRowValueUSD), "9000", "1000", "0", account))
}

func feedDownloadEnd(conn *Connection, account string) {
	conn.processMessage(conn.encodeMsg(msgAcctDownloadEnd, "1", account))
}

func captureProjection(t *testing.T, connector *Connector) PortfolioProjectionBinding {
	t.Helper()
	binding, ok := connector.CaptureSession()
	if !ok {
		t.Fatal("capture session")
	}
	projection, ok := connector.CapturePortfolioProjectionForSession(binding)
	if !ok {
		t.Fatal("capture portfolio projection")
	}
	return projection
}

// TestReconnectShortPortfolioDownloadIsNotCurrent replays 2026-09-23: TWS,
// seconds after its own login, answered a new session's subscription with two
// of seven rows and a genuine accountDownloadEnd, while the same stream's gross
// position value already covered the whole book. The daemon reports
// freshness=current only for a receipt with InitialCompletedAt, so the partial
// generation must neither complete the receipt nor replace the published rows.
func TestReconnectShortPortfolioDownloadIsNotCurrent(t *testing.T) {
	conn, connector, _, newSocket, gate := newQueuedInstructionReconnectFixture(t)
	account := gate.Account

	if err := connector.RequestAccountUpdates(account); err != nil {
		t.Fatalf("subscribe session A: %v", err)
	}
	feedAccountValues(conn, account)
	for conID := 1; conID <= syntheticBookRows; conID++ {
		feedOptionRow(conn, account, 800000+conID)
	}
	feedDownloadEnd(conn, account)
	if a := captureProjection(t, connector); a.Health.InitialCompletedAt.IsZero() || len(a.Positions) != syntheticBookRows {
		t.Fatalf("session A complete book not current: completed=%v rows=%d", a.Health.InitialCompletedAt, len(a.Positions))
	}

	conn.resetOrderIDReadiness()
	conn.writer = bufio.NewWriter(newSocket)
	connector.onConnectionEstablished(conn)
	conn.observeNextValidOrderIDAtEpoch(500, conn.BrokerSessionEpoch())
	if err := connector.RequestAccountUpdates(account); err != nil {
		t.Fatalf("subscribe session B: %v", err)
	}
	feedAccountValues(conn, account)
	feedOptionRow(conn, account, 800001)
	feedOptionRow(conn, account, 800002)
	feedDownloadEnd(conn, account)

	short := captureProjection(t, connector)
	if !short.Health.InitialCompletedAt.IsZero() {
		t.Fatalf("partial download completed the receipt with %d of %d rows", len(short.Positions), syntheticBookRows)
	}
	if len(short.Positions) != 0 {
		t.Fatalf("partial download published %d rows", len(short.Positions))
	}
	if short.Health.DownloadShortAt.IsZero() {
		t.Fatal("short download left no typed reason on the receipt")
	}

	for conID := 3; conID <= syntheticBookRows; conID++ {
		feedOptionRow(conn, account, 800000+conID)
	}
	complete := captureProjection(t, connector)
	if complete.Health.InitialCompletedAt.IsZero() || !complete.Health.DownloadShortAt.IsZero() || len(complete.Positions) != syntheticBookRows {
		t.Fatalf("late rows did not complete the book: completed=%v short=%v rows=%d",
			complete.Health.InitialCompletedAt, complete.Health.DownloadShortAt, len(complete.Positions))
	}
	if complete.Generation <= short.Generation {
		t.Fatalf("publication did not advance the projection generation: %d -> %d", short.Generation, complete.Generation)
	}
}

// TestShortPortfolioDownloadResubscribesWhileRowsAreRetained covers the
// same-session case, such as 1101 data-loss recovery: the prior complete rows
// stay published as context, so the empty-cache self-heal never fires, and a
// short download would otherwise hold the receipt unprimed until TWS happened
// to push the missing rows.
func TestShortPortfolioDownloadResubscribesWhileRowsAreRetained(t *testing.T) {
	conn, connector, oldSocket, _, gate := newQueuedInstructionReconnectFixture(t)
	account := gate.Account
	now := time.Date(2026, 9, 23, 5, 53, 33, 0, time.UTC)
	connector.acctUpdatesNow = func() time.Time { return now }

	if err := connector.RequestAccountUpdates(account); err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	feedAccountValues(conn, account)
	for conID := 1; conID <= syntheticBookRows; conID++ {
		feedOptionRow(conn, account, 800000+conID)
	}
	feedDownloadEnd(conn, account)

	if err := connector.RequestAccountUpdates(account); err != nil {
		t.Fatalf("resubscribe: %v", err)
	}
	feedAccountValues(conn, account)
	feedOptionRow(conn, account, 800001)
	feedDownloadEnd(conn, account)

	rows, health, err := connector.CachedPositionsWithHealth()
	if err != nil || len(rows) != syntheticBookRows || health.DownloadShortAt.IsZero() || !health.InitialCompletedAt.IsZero() {
		t.Fatalf("short resubscribe: rows=%d short=%v completed=%v err=%v", len(rows), health.DownloadShortAt, health.InitialCompletedAt, err)
	}
	subscribes := func() int {
		count := 0
		for _, frame := range decodeOutboundFrames(t, conn, oldSocket.Bytes()) {
			if len(frame) > 2 && frame[0] == strconv.Itoa(reqAcctData) && frame[2] == "1" {
				count++
			}
		}
		return count
	}
	if got := subscribes(); got != 2 {
		t.Fatalf("resubscribed inside the throttle window: %d subscriptions", got)
	}

	now = now.Add(acctUpdatesResubscribeThrottle)
	rows, health, err = connector.CachedPositionsWithHealth()
	if err != nil || len(rows) != syntheticBookRows || !health.InitialCompletedAt.IsZero() {
		t.Fatalf("self-heal capture: rows=%d completed=%v err=%v", len(rows), health.InitialCompletedAt, err)
	}
	if got := subscribes(); got != 3 {
		t.Fatalf("short download did not resubscribe after the throttle: %d subscriptions", got)
	}
	for conID := 2; conID <= syntheticBookRows; conID++ {
		feedOptionRow(conn, account, 800000+conID)
	}
	if rows, health, err = connector.CachedPositionsWithHealth(); err != nil || len(rows) != syntheticBookRows || health.InitialCompletedAt.IsZero() {
		t.Fatalf("late rows after the self-heal did not complete the book: rows=%d completed=%v err=%v", len(rows), health.InitialCompletedAt, err)
	}
}

// TestSelfHealKeepsTheShortGenerationUntilRowsFootIt replays 2026-11-20
// 23:45 to 2026-11-21 03:25: after the nightly TWS restart a download ended
// with no rows, the self-heal resubscribed every 30 seconds, one download
// ended short, and TWS then answered none of the 434 resubscribes with an end
// marker while it kept sending portfolio rows. Each resubscribe emptied the
// staged rows and cleared the short marker, so no row could complete the book
// and it stayed not current until the socket dropped. A self-heal on the same
// session keeps the staged rows; late rows complete them once they foot, and
// never before.
func TestSelfHealKeepsTheShortGenerationUntilRowsFootIt(t *testing.T) {
	conn, connector, _, _, gate := newQueuedInstructionReconnectFixture(t)
	account := gate.Account
	now := time.Date(2026, 11, 20, 22, 45, 18, 0, time.UTC)
	connector.acctUpdatesNow = func() time.Time { return now }
	notCurrent := func(step string) {
		t.Helper()
		if rows, health, err := connector.CachedPositionsWithHealth(); err != nil || !health.InitialCompletedAt.IsZero() {
			t.Fatalf("%s: short book read as current: rows=%d err=%v", step, len(rows), err)
		}
	}

	if err := connector.RequestAccountUpdates(account); err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	feedAccountValues(conn, account)
	feedDownloadEnd(conn, account)
	now = now.Add(acctUpdatesResubscribeThrottle)
	notCurrent("empty download")

	for conID := 1; conID <= syntheticBookRows-2; conID++ {
		feedOptionRow(conn, account, 800000+conID)
	}
	feedDownloadEnd(conn, account)
	now = now.Add(acctUpdatesResubscribeThrottle)
	notCurrent("download ended short")
	now = now.Add(acctUpdatesResubscribeThrottle)
	notCurrent("resubscribed again")

	feedOptionRow(conn, account, 800000+syntheticBookRows-1)
	notCurrent("one row short")
	feedOptionRow(conn, account, 800000+syntheticBookRows)
	rows, health, err := connector.CachedPositionsWithHealth()
	if err != nil || health.InitialCompletedAt.IsZero() || len(rows) != syntheticBookRows {
		t.Fatalf("late rows did not complete the book: rows=%d completed=%v short=%v err=%v",
			len(rows), health.InitialCompletedAt, health.DownloadShortAt, err)
	}
}

// TestHeldPortfolioBookIsOneEpisode replays 2026-11-19 23:45 to 2026-11-20
// 05:18: after the nightly TWS restart the stream carried the gross position
// value, a download ended with no rows, and the 30-second resubscribe ran 660
// times with a warning each, none of which said whether IBKR answered. The
// hold is now one episode: a warning when the repair starts, one once it has
// lasted portfolioHeldWarnAfter naming what was sent and what came back,
// repeated every portfolioHeldWarnEvery while it lasts, and one when the book
// is current again.
func TestHeldPortfolioBookIsOneEpisode(t *testing.T) {
	buf := captureConnectorLogs(t)
	conn, connector, _, _, gate := newQueuedInstructionReconnectFixture(t)
	account := gate.Account
	now := time.Date(2026, 11, 19, 22, 45, 18, 0, time.UTC)
	connector.acctUpdatesNow = func() time.Time { return now }

	if err := connector.RequestAccountUpdates(account); err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	feedAccountValues(conn, account)
	feedDownloadEnd(conn, account)

	for range 25 {
		now = now.Add(acctUpdatesResubscribeThrottle)
		if rows, health, err := connector.CachedPositionsWithHealth(); err != nil || len(rows) != 0 || !health.InitialCompletedAt.IsZero() {
			t.Fatalf("held book read as current: rows=%d completed=%v err=%v", len(rows), health.InitialCompletedAt, err)
		}
	}
	if got := logLines(buf, "resubscribing account updates"); len(got) != 1 {
		t.Fatalf("want one warning when the repair starts, got %d:\n%s", len(got), strings.Join(got, "\n"))
	}
	held := logLines(buf, "Portfolio book held as not current")
	if len(held) != 3 {
		t.Fatalf("want the hold said after %s and every %s after, got %d:\n%s", portfolioHeldWarnAfter, portfolioHeldWarnEvery, len(held), buf.Bytes())
	}
	for _, want := range []string{"level=WARN", "for 1m0s", "3 resubscribes sent", "0 failed to send", "0 portfolio rows", "0 download end markers", "open dialog"} {
		if !strings.Contains(held[0], want) {
			t.Errorf("hold warning lacks %q: %s", want, held[0])
		}
	}
	for i, want := range []string{"for 6m0s", "for 11m0s"} {
		if !strings.Contains(held[i+1], "level=WARN") || !strings.Contains(held[i+1], want) {
			t.Errorf("repeated hold warning %d lacks %q: %s", i+1, want, held[i+1])
		}
	}
	if hold, ok := connector.PortfolioHold(); !ok || hold.Repairs != 25 || !hold.Since.Equal(time.Date(2026, 11, 19, 22, 45, 48, 0, time.UTC)) {
		t.Fatalf("hold not reported to readers: %+v %v", hold, ok)
	}

	feedAccountValues(conn, account)
	for conID := 1; conID <= syntheticBookRows; conID++ {
		feedOptionRow(conn, account, 800000+conID)
	}
	feedDownloadEnd(conn, account)
	for range 2 {
		if rows, health, err := connector.CachedPositionsWithHealth(); err != nil || len(rows) != syntheticBookRows || health.InitialCompletedAt.IsZero() {
			t.Fatalf("complete download not current: rows=%d completed=%v err=%v", len(rows), health.InitialCompletedAt, err)
		}
	}
	if _, ok := connector.PortfolioHold(); ok {
		t.Fatal("hold still reported after the book is current")
	}
	recovered := logLines(buf, "Portfolio book current again")
	if len(recovered) != 1 || !strings.Contains(recovered[0], "level=WARN") || !strings.Contains(recovered[0], "after 12m0s") || !strings.Contains(recovered[0], "25 resubscribes") {
		t.Fatalf("want one recovery warning naming the hold, got %d:\n%s", len(recovered), strings.Join(recovered, "\n"))
	}

	// A later hold is a new episode with its own first warning.
	if err := connector.RequestAccountUpdates(account); err != nil {
		t.Fatalf("resubscribe: %v", err)
	}
	feedAccountValues(conn, account)
	feedDownloadEnd(conn, account)
	now = now.Add(acctUpdatesResubscribeThrottle)
	connector.CachedPositionsWithHealth()
	if got := logLines(buf, "resubscribing account updates"); len(got) != 2 {
		t.Fatalf("a new hold did not warn once more: %d warnings", len(got))
	}
}

// TestPortfolioFootingDoesNotTakeTheUnitRateForARate replays TWS on a live
// EUR-base account, which reports ExchangeRate 1 for USD. Taken at face value,
// six of seven rows read as 107% of gross, so a download missing a seventh of
// the book completed as current. A real rate of 0.8 puts them at 86%.
func TestPortfolioFootingDoesNotTakeTheUnitRateForARate(t *testing.T) {
	realRate := func(base, currency string) (float64, bool) {
		return 0.8, base == "EUR" && currency == "USD"
	}
	feed := func(t *testing.T, rate func(string, string) (float64, bool), rows int) (*Connection, *Connector, string) {
		t.Helper()
		conn, connector, _, _, gate := newQueuedInstructionReconnectFixture(t)
		conn.config.FXRateToBase = rate
		account := gate.Account
		if err := connector.RequestAccountUpdates(account); err != nil {
			t.Fatalf("subscribe: %v", err)
		}
		feedAccountValue(conn, "NetLiquidation", "100000", "EUR", account)
		feedAccountValue(conn, "GrossPositionValue", syntheticGrossValueEUR, "EUR", account)
		feedAccountValue(conn, "OptionMarketValue", strconv.Itoa(syntheticBookRows*syntheticRowValueUSD), "USD", account)
		feedAccountValue(conn, "ExchangeRate", "1", "USD", account)
		for conID := 1; conID <= rows; conID++ {
			feedOptionRow(conn, account, 800000+conID)
		}
		feedDownloadEnd(conn, account)
		return conn, connector, account
	}

	t.Run("a short download is held on the real rate", func(t *testing.T) {
		conn, connector, account := feed(t, realRate, syntheticBookRows-1)
		short := captureProjection(t, connector)
		if !short.Health.InitialCompletedAt.IsZero() || short.Health.DownloadShortAt.IsZero() || len(short.Positions) != 0 {
			t.Fatalf("download missing a seventh of the book: completed=%v short=%v rows=%d",
				short.Health.InitialCompletedAt, short.Health.DownloadShortAt, len(short.Positions))
		}
		feedOptionRow(conn, account, 800000+syntheticBookRows)
		if complete := captureProjection(t, connector); complete.Health.InitialCompletedAt.IsZero() || len(complete.Positions) != syntheticBookRows {
			t.Fatalf("the late row did not complete the book: completed=%v rows=%d", complete.Health.InitialCompletedAt, len(complete.Positions))
		}
	})

	t.Run("a complete download is not held", func(t *testing.T) {
		_, connector, _ := feed(t, realRate, syntheticBookRows)
		if complete := captureProjection(t, connector); complete.Health.InitialCompletedAt.IsZero() || !complete.Health.DownloadShortAt.IsZero() {
			t.Fatalf("complete book held: completed=%v short=%v", complete.Health.InitialCompletedAt, complete.Health.DownloadShortAt)
		}
	})

	t.Run("with no real rate the end marker stands alone", func(t *testing.T) {
		_, connector, _ := feed(t, func(string, string) (float64, bool) { return 0, false }, syntheticBookRows-1)
		if alone := captureProjection(t, connector); alone.Health.InitialCompletedAt.IsZero() || !alone.Health.DownloadShortAt.IsZero() {
			t.Fatalf("no provable coverage must not hold the book: completed=%v short=%v", alone.Health.InitialCompletedAt, alone.Health.DownloadShortAt)
		}
	})
}

// TestHeldBookRepairEndsTheLiveSubscriptionFirst replays 2026-11-24 12:52:
// TWS, logged in a minute earlier, answered the reconnected session's
// subscription with account values and an end marker but no rows. Every
// repair repeated the subscribe on the live subscription, which TWS does not
// answer (2026-11-21: 439 repeats, no row, no end marker), so the book stayed
// held until a restart's new socket downloaded it at once. A repair ends the
// subscription before it subscribes again.
func TestHeldBookRepairEndsTheLiveSubscriptionFirst(t *testing.T) {
	conn, connector, socket, _, gate := newQueuedInstructionReconnectFixture(t)
	account := gate.Account
	now := time.Date(2026, 11, 24, 11, 52, 8, 0, time.UTC)
	connector.acctUpdatesNow = func() time.Time { return now }
	if err := connector.RequestAccountUpdates(account); err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	feedAccountValues(conn, account)
	feedDownloadEnd(conn, account)

	accountFrames := func() []string {
		var out []string
		for _, frame := range decodeOutboundFrames(t, conn, socket.Bytes()) {
			if len(frame) > 0 && frame[0] == strconv.Itoa(reqAcctData) {
				out = append(out, strings.TrimSuffix(strings.Join(frame[1:], ","), ","))
			}
		}
		return out
	}
	for _, reason := range []string{"short download", "scope conflict"} {
		before := len(accountFrames())
		if reason == "scope conflict" {
			feedOptionRow(conn, "U9999999", 800001)
		}
		now = now.Add(acctUpdatesResubscribeThrottle)
		if _, health, err := connector.CachedPositionsWithHealth(); err != nil || !health.InitialCompletedAt.IsZero() {
			t.Fatalf("%s: held book read as current (err %v)", reason, err)
		}
		got := accountFrames()[before:]
		want := []string{"2,0," + account, "2,1," + account}
		if strings.Join(got, " ") != strings.Join(want, " ") {
			t.Fatalf("%s repair sent %q, want %q", reason, got, want)
		}
	}

	// The fresh download the second subscribe asks for completes the book.
	feedAccountValues(conn, account)
	for conID := 1; conID <= syntheticBookRows; conID++ {
		feedOptionRow(conn, account, 800000+conID)
	}
	feedDownloadEnd(conn, account)
	if rows, health, err := connector.CachedPositionsWithHealth(); err != nil || len(rows) != syntheticBookRows || health.InitialCompletedAt.IsZero() {
		t.Fatalf("fresh download not current: rows=%d completed=%v err=%v", len(rows), health.InitialCompletedAt, err)
	}
}
