package futu

import (
	"context"
	"encoding/binary"
	"errors"
	"testing"
	"time"

	qotcommon "github.com/osauer/canary/v2/pkg/futu/internal/pb/qot_common"
)

// FrameProtocol reads the protocol id out of a raw frame.
func FrameProtocol(raw []byte) uint32 { return binary.LittleEndian.Uint32(raw[2:]) }

func replayed(t *testing.T, name string) (*connection, chan error, context.Context) {
	t.Helper()
	conn, done := replayConnection(t, name, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	t.Cleanup(cancel)
	if _, err := handshake(ctx, conn, fixtureCfg()); err != nil {
		t.Fatal(err)
	}
	return conn, done, ctx
}

func TestReplaySubscribeBasicAndPush(t *testing.T) {
	c := NewConnector(Config{Account: 1000001, ClientVersion: 333})
	nc, done := replaySocket(t, readFixture(t, "qot_sub_ok"))
	conn := newConnection(nc, c.pacer, nil)
	conn.onPush = func(f Frame) { c.handlePush(conn, f) }
	go conn.serve()
	c.mu.Lock()
	c.conn = conn
	c.epoch = 1
	c.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err := handshake(ctx, conn, fixtureCfg()); err != nil {
		t.Fatal(err)
	}
	binding, err := c.Subscribe(ctx, "HK.00700")
	if err != nil || binding.Epoch != 1 {
		t.Fatalf("subscribe %+v %v", binding, err)
	}
	quotes, err := c.BasicQuotes(ctx, "HK.00700")
	if err != nil || len(quotes) != 1 || quotes[0].Last <= 0 || quotes[0].Delivery != DeliveryStream {
		t.Fatalf("basic %+v %v", quotes, err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for len(c.QuoteCache()) == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if cached := c.QuoteCache()["HK.00700"]; cached.Last != quotes[0].Last {
		t.Fatalf("pushed quote %+v", cached)
	}
}

func TestReplaySubscribeQuotaSpent(t *testing.T) {
	c := NewConnector(Config{Account: 1000001, ClientVersion: 333})
	nc, done := replaySocket(t, readFixture(t, "qot_sub_quota_spent"))
	conn := newConnection(nc, c.pacer, nil)
	go conn.serve()
	c.mu.Lock()
	c.conn = conn
	c.epoch = 1
	c.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err := handshake(ctx, conn, fixtureCfg()); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Subscribe(ctx, "HK.00700"); !errors.Is(err, ErrQuotaExhausted) {
		t.Fatalf("subscribe: %v", err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if c.Load().LastRefusal == "" {
		t.Fatal("the refusal was not recorded")
	}
}

func TestReplaySnapshot(t *testing.T) {
	conn, done, ctx := replayed(t, "qot_snapshot_ok")
	quotes, err := readSnapshots(ctx, conn, []string{"HK.00700", "US.AAPL"})
	if err != nil || len(quotes) != 2 || quotes[0].Bid <= 0 || quotes[1].Code != "US.AAPL" {
		t.Fatalf("snapshots %+v %v", quotes, err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestReplayHistoryAndItsQuota(t *testing.T) {
	conn, done, ctx := replayed(t, "qot_history_ok")
	zone := time.FixedZone("HKT", 8*3600)
	from, _ := time.ParseInLocation(barLayout, fixtureHistoryFrom, zone)
	to, _ := time.ParseInLocation(barLayout, fixtureHistoryTo, zone)
	bars, err := readHistoryBars(ctx, conn, "HK.00700", BarDay, from, to)
	if err != nil || len(bars) != 5 || bars[0].Close <= 0 {
		t.Fatalf("bars %+v %v", bars, err)
	}
	used, remaining, err := readHistoryQuota(ctx, conn)
	if err != nil || used != 1 || remaining != 99 {
		t.Fatalf("quota %d %d %v", used, remaining, err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestReplayStaticInfo(t *testing.T) {
	conn, done, ctx := replayed(t, "qot_static_ok")
	got, err := readInstruments(ctx, conn, []string{"HK.00700"})
	if err != nil || len(got) != 1 || got[0].LotSize != 100 || got[0].Type != "Eqty" {
		t.Fatalf("instruments %+v %v", got, err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestReplayFunds(t *testing.T) {
	conn, done, ctx := replayed(t, "trd_funds_ok")
	funds, err := readFunds(ctx, conn, fixtureSimHeader(), "", false)
	if err != nil || funds.Currency != "HKD" || funds.TotalAssets <= 0 || len(funds.CashRows) != 3 {
		t.Fatalf("funds %+v %v", funds, err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestReplayPositions(t *testing.T) {
	conn, done, ctx := replayed(t, "trd_positions_ok")
	rows, err := readPositions(ctx, conn, fixtureSimHeader(), false)
	if err != nil || len(rows) != 1 || rows[0].Code != "HK.00700" || rows[0].Quantity != 300 {
		t.Fatalf("positions %+v %v", rows, err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestReplayOrdersAndFills(t *testing.T) {
	conn, done, ctx := replayed(t, "trd_orders_ok")
	orders, err := readOrders(ctx, conn, fixtureSimHeader(), false)
	if err != nil || len(orders) != 1 || orders[0].Status != "Filled_All" {
		t.Fatalf("orders %+v %v", orders, err)
	}
	fills, err := readFills(ctx, conn, fixtureSimHeader(), false)
	if err != nil || len(fills) != 1 || fills[0].OrderID != orders[0].ID {
		t.Fatalf("fills %+v %v", fills, err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestReplayMaxQuantities(t *testing.T) {
	conn, done, ctx := replayed(t, "trd_max_ok")
	got, err := readMaxQuantities(ctx, conn, fixtureSimHeader(), "00700", 412.6)
	if err != nil || got.CashBuy <= 0 || got.PositionSell != 300 {
		t.Fatalf("max %+v %v", got, err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}

	conn, done, ctx = replayed(t, "trd_max_cash_refused")
	_, err = readMaxQuantities(ctx, conn, fixtureRealHeader(), "02800", 23.44)
	var reply *Error
	if !errors.As(err, &reply) || reply.Session() {
		t.Fatalf("cash refusal %v", err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestCodesRoundTripAndTradeCodesJoin(t *testing.T) {
	for _, code := range []string{"HK.00700", "US.AAPL", "SH.600000", "SZ.000001", "SG.D05", "JP.7203", "AU.BHP", "MY.1155", "CA.RY"} {
		sec, err := security(code)
		if err != nil {
			t.Fatal(err)
		}
		if got := codeOf(sec); got != code {
			t.Errorf("%s round-tripped to %s", code, got)
		}
	}
	for _, bad := range []string{"", "AAPL", "XX.AAPL", "US."} {
		if _, err := security(bad); !errors.Is(err, ErrInvalidCode) {
			t.Errorf("%q: %v", bad, err)
		}
	}
	if got := codeOf(&qotcommon.Security{Market: new(int32(999)), Code: new("X")}); got != "X" {
		t.Errorf("unknown market: %s", got)
	}
	if got := tradeCode("HK.00700", 2); got != "HK.00700" {
		t.Errorf("a row with its prefix keeps it: %s", got)
	}
	if got := tradeCode("AAPL", 2); got != "US.AAPL" {
		t.Errorf("a bare row joins its market: %s", got)
	}
	if got := tradeCode("X", 999); got != "X" {
		t.Errorf("an unknown market stays bare: %s", got)
	}
	if !timeFromSeconds(0).IsZero() {
		t.Error("no timestamp is no time")
	}
	if got := timeFromSeconds(1.5); got.Nanosecond() != 500000000 {
		t.Errorf("fractional timestamp %v", got)
	}
}
