package ibkr

import (
	"bufio"
	"strconv"
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
			if len(frame) > 0 && frame[0] == strconv.Itoa(reqAcctData) {
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
	if err != nil || len(rows) != syntheticBookRows || !health.DownloadShortAt.IsZero() || !health.InitialCompletedAt.IsZero() || health.RequestedAt.IsZero() {
		t.Fatalf("self-heal did not return one fresh unprimed capture: rows=%d short=%v completed=%v requested=%v err=%v",
			len(rows), health.DownloadShortAt, health.InitialCompletedAt, health.RequestedAt, err)
	}
	if got := subscribes(); got != 3 {
		t.Fatalf("short download did not resubscribe after the throttle: %d subscriptions", got)
	}
}
