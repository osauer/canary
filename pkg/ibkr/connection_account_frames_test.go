package ibkr

import (
	"strconv"
	"testing"
	"time"
)

// wireTAccountConn is a ready connection whose Gateway has named the managed
// accounts; it never redials on a disconnect.
func wireTAccountConn(t *testing.T, managed string) *Connection {
	t.Helper()
	conn, _ := newReadyWireTestConnection(t)
	conn.config.AutoReconnect = false
	conn.processMessage(twsGatewayFrame(msgManagedAccts, "1", managed))
	return conn
}

// wireTRow is one instrument row as the Gateway streams it: the same
// contract fields serve the position (61) and updatePortfolio (7) frames.
type wireTRow struct {
	Account, Symbol, SecType, Expiry, Strike, Right, Multiplier, Currency string
	ConID                                                                 int
	Position, Price, Value, AvgCost, Unrealized, Realized                 float64
}

func wireTFloat(v float64) string { return strconv.FormatFloat(v, 'f', -1, 64) }

// positionFrame follows EDecoder.processPositionDataMsg v3: version, account,
// conId, symbol, secType, lastTradeDate, strike, right, multiplier, exchange,
// currency, localSymbol, tradingClass, position, avgCost.
func (r wireTRow) positionFrame() []byte {
	return twsGatewayFrame(msgPosition, "3", r.Account, strconv.Itoa(r.ConID), r.Symbol, r.SecType, r.Expiry, r.Strike, r.Right, r.Multiplier,
		"SMART", r.Currency, r.Symbol, r.Symbol, wireTFloat(r.Position), wireTFloat(r.AvgCost))
}

// portfolioFrame follows EDecoder.processPortfolioValueMsg v8: version,
// conId, symbol, secType, lastTradeDate, strike, right, multiplier,
// primaryExch, currency, localSymbol, tradingClass, position, marketPrice,
// marketValue, averageCost, unrealizedPNL, realizedPNL, accountName.
func (r wireTRow) portfolioFrame() []byte {
	return twsGatewayFrame(msgPortfolioValue, "8", strconv.Itoa(r.ConID), r.Symbol, r.SecType, r.Expiry, r.Strike, r.Right, r.Multiplier,
		"", r.Currency, r.Symbol, r.Symbol, wireTFloat(r.Position), wireTFloat(r.Price), wireTFloat(r.Value), wireTFloat(r.AvgCost),
		wireTFloat(r.Unrealized), wireTFloat(r.Realized), r.Account)
}

func wireTStockRow(account string, conID int, symbol, currency string, position, price float64) wireTRow {
	return wireTRow{Account: account, ConID: conID, Symbol: symbol, SecType: "STK", Currency: currency,
		Position: position, Price: price, Value: position * price, AvgCost: price - 1, Unrealized: position, Realized: 0}
}

func wireTOptionRow(account string, conID int, symbol string, position float64) wireTRow {
	return wireTRow{Account: account, ConID: conID, Symbol: symbol, SecType: "OPT", Expiry: "20261218", Strike: "100", Right: "C", Multiplier: "100", Currency: "USD",
		Position: position, Price: 3.2, Value: position * 320, AvgCost: 2.9, Unrealized: 30, Realized: 0}
}

func wireTDownloadEnd(conn *Connection, account string) {
	conn.processMessage(twsGatewayFrame(msgAcctDownloadEnd, "1", account))
}

func wireTAccountValue(conn *Connection, key, value, currency, account string) {
	conn.processMessage(twsGatewayFrame(msgAcctValue, "2", key, value, currency, account))
}

// --- 4. reqPositions snapshot ----------------------------------------------

// A reqPositions stream fills an isolated snapshot: rows arrive after the
// request, a zero row removes its contract, positionEnd publishes the
// result, and a row outside the request window changes nothing. The
// streaming account-updates book is never touched by it.
func TestReqPositionsSnapshotLifecycle(t *testing.T) {
	conn := wireTAccountConn(t, "DU1234567")
	stock := wireTStockRow("DU1234567", 4242, "SYNX", "USD", 100, 50.5)
	option := wireTOptionRow("DU1234567", 4343, "ZETA", -2)
	closed := wireTStockRow("DU1234567", 4444, "ZETA", "EUR", 0, 10)

	conn.processMessage(stock.positionFrame()) // before the request: nothing listens
	if conn.GetPositionsSnapshot() != nil {
		t.Fatal("a position row before reqPositions produced a snapshot")
	}
	if err := conn.RequestPositions(); err != nil {
		t.Fatalf("RequestPositions: %v", err)
	}
	for _, row := range []wireTRow{stock, option, closed} {
		conn.processMessage(row.positionFrame())
	}
	if conn.GetPositionsSnapshot() != nil {
		t.Fatal("snapshot published before positionEnd")
	}
	conn.processMessage(twsGatewayFrame(msgPositionEnd, "1"))
	if err := conn.WaitForPositionsEnd(time.Second); err != nil {
		t.Fatalf("positionEnd not signalled: %v", err)
	}

	snapshot := conn.GetPositionsSnapshot()
	if len(snapshot) != 2 {
		t.Fatalf("snapshot rows = %d, want 2: %#v", len(snapshot), snapshot)
	}
	got := snapshot["DU1234567|STK:4242"]
	if got == nil || got.Position != 100 || got.AverageCost != 49.5 || got.Contract.Currency != "USD" || got.Contract.Exchange != "SMART" || got.Account != "DU1234567" {
		t.Fatalf("stock row = %+v", got)
	}
	opt := snapshot["DU1234567|OPT:4343"]
	if opt == nil || opt.Position != -2 || opt.AverageCost != 2.9 || opt.Contract.Strike != 100 || opt.Contract.Right != "C" || opt.Contract.Expiry != "20261218" || opt.Contract.Multiplier != 100 {
		t.Fatalf("option row = %+v", opt)
	}
	if _, present := snapshot["DU1234567|STK:4444"]; present {
		t.Fatal("a zero-position row stayed in the snapshot")
	}
	if streamed := conn.GetPositions(); len(streamed) != 0 {
		t.Fatalf("reqPositions rows leaked into the account-updates book: %d", len(streamed))
	}

	late := wireTStockRow("DU1234567", 4545, "SYNX", "USD", 7, 1)
	conn.processMessage(late.positionFrame())
	if len(conn.GetPositionsSnapshot()) != 2 {
		t.Fatal("a late position row changed the completed snapshot")
	}
	// Short frames are refused without touching anything.
	conn.processMessage(twsGatewayFrame(msgPosition, "3", "DU1234567", "4242"))
	if len(conn.GetPositionsSnapshot()) != 2 {
		t.Fatal("a short position frame changed the snapshot")
	}
}

// Regression: a stock row on the reqPositions stream was decoded with
// Multiplier 100 because the wire frame always carries the option slots
// (empty) and the decoder took the field count for the options layout.
// The Gateway's position frame (EDecoder v3) has the same 15 fields for every
// secType; a stock leaves multiplier empty. The streaming updatePortfolio
// decoder normalises that to one unit; the reqPositions decoder must agree.
func TestReqPositionsStockRowKeepsUnitMultiplier(t *testing.T) {
	conn := wireTAccountConn(t, "DU1234567")
	if err := conn.RequestPositions(); err != nil {
		t.Fatalf("RequestPositions: %v", err)
	}
	conn.processMessage(wireTStockRow("DU1234567", 4242, "SYNX", "USD", 100, 50.5).positionFrame())
	conn.processMessage(twsGatewayFrame(msgPositionEnd, "1"))
	got := conn.GetPositionsSnapshot()["DU1234567|STK:4242"]
	if got == nil {
		t.Fatal("stock row missing from the snapshot")
	}
	if got.Contract.Multiplier != 1 {
		t.Fatalf("stock Multiplier = %d, want 1 (the streaming decoder's unit normalisation)", got.Contract.Multiplier)
	}
}

// --- 4. reqAccountUpdates download -----------------------------------------

// The initial download stages rows until accountDownloadEnd, then publishes
// them as one generation; a second subscription's download replaces the
// first wholesale, so a contract absent from the new download is gone.
func TestAccountUpdatesDownloadPublishesAndSecondDownloadReplaces(t *testing.T) {
	conn := wireTAccountConn(t, "DU1234567")
	if err := conn.RequestAccountUpdates("DU1234567"); err != nil {
		t.Fatalf("RequestAccountUpdates: %v", err)
	}
	wireTAccountValue(conn, "NetLiquidation", "100000", "USD", "DU1234567")
	conn.processMessage(wireTStockRow("DU1234567", 4242, "SYNX", "USD", 100, 50).portfolioFrame())
	conn.processMessage(wireTStockRow("DU1234567", 4343, "ZETA", "EUR", 200, 20).portfolioFrame())
	if rows, health := conn.GetPositionsWithPortfolioHealth(); len(rows) != 0 || !health.InitialCompletedAt.IsZero() {
		t.Fatalf("rows published before accountDownloadEnd: rows=%d completed=%v", len(rows), health.InitialCompletedAt)
	}
	wireTDownloadEnd(conn, "DU1234567")
	rows, health := conn.GetPositionsWithPortfolioHealth()
	if len(rows) != 2 || health.InitialCompletedAt.IsZero() || !health.DownloadShortAt.IsZero() || health.Account != "DU1234567" {
		t.Fatalf("first download: rows=%d health=%+v", len(rows), health)
	}
	zeta := rows["DU1234567|STK:4343"]
	if zeta == nil || zeta.Contract.Currency != "EUR" || zeta.Contract.Multiplier != 1 || zeta.Position != 200 || zeta.MarketPrice != 20 || zeta.MarketValue != 4000 || zeta.AverageCost != 19 || zeta.UnrealizedPNL != 200 {
		t.Fatalf("EUR stock row = %+v", zeta)
	}
	if synx := rows["DU1234567|STK:4242"]; synx == nil || synx.Contract.Symbol != "SYNX" || synx.Contract.ConID != 4242 {
		t.Fatalf("USD stock row = %+v", synx)
	}
	firstGeneration := conn.PortfolioProjectionGeneration()

	if err := conn.RequestAccountUpdates("DU1234567"); err != nil {
		t.Fatalf("resubscribe: %v", err)
	}
	if rows, _ := conn.GetPositionsWithPortfolioHealth(); len(rows) != 2 {
		t.Fatalf("resubscribe dropped the published rows early: %d", len(rows))
	}
	conn.processMessage(wireTStockRow("DU1234567", 4343, "ZETA", "EUR", 150, 21).portfolioFrame())
	wireTDownloadEnd(conn, "DU1234567")
	rows, health = conn.GetPositionsWithPortfolioHealth()
	if len(rows) != 1 || health.InitialCompletedAt.IsZero() {
		t.Fatalf("second download: rows=%d health=%+v", len(rows), health)
	}
	if zeta := rows["DU1234567|STK:4343"]; zeta == nil || zeta.Position != 150 || zeta.MarketPrice != 21 {
		t.Fatalf("replaced row = %+v", zeta)
	}
	if _, stale := rows["DU1234567|STK:4242"]; stale {
		t.Fatal("a contract absent from the second download survived it")
	}
	if conn.PortfolioProjectionGeneration() <= firstGeneration {
		t.Fatal("replacing the book did not advance the projection generation")
	}
}

// After the download, rows stream straight into the book: a zero row removes
// its contract, a new contract is added, and both move the structural
// generation; a price-only update changes the row but not the generation.
func TestAccountUpdatesSteadyStateRows(t *testing.T) {
	conn := wireTAccountConn(t, "DU1234567")
	if err := conn.RequestAccountUpdates("DU1234567"); err != nil {
		t.Fatal(err)
	}
	conn.processMessage(wireTStockRow("DU1234567", 4242, "SYNX", "USD", 100, 50).portfolioFrame())
	wireTDownloadEnd(conn, "DU1234567")
	generation := conn.PortfolioProjectionGeneration()

	conn.processMessage(wireTStockRow("DU1234567", 4242, "SYNX", "USD", 100, 51).portfolioFrame())
	if row, _ := conn.GetPosition("DU1234567|STK:4242"); row == nil || row.MarketPrice != 51 {
		t.Fatalf("price update not applied: %+v", row)
	}
	if g := conn.PortfolioProjectionGeneration(); g != generation {
		t.Fatalf("a price-only update moved the generation %d -> %d", generation, g)
	}

	conn.processMessage(wireTOptionRow("DU1234567", 4343, "ZETA", 3).portfolioFrame())
	if _, ok := conn.GetPosition("DU1234567|OPT:4343"); !ok {
		t.Fatal("new option row not added")
	}
	if conn.PortfolioProjectionGeneration() <= generation {
		t.Fatal("a new contract did not advance the generation")
	}
	generation = conn.PortfolioProjectionGeneration()

	conn.processMessage(wireTStockRow("DU1234567", 4242, "SYNX", "USD", 0, 51).portfolioFrame())
	if _, still := conn.GetPosition("DU1234567|STK:4242"); still {
		t.Fatal("a zero-position row did not remove its contract")
	}
	if conn.PortfolioProjectionGeneration() <= generation {
		t.Fatal("removing a contract did not advance the generation")
	}
	if rows, _ := conn.GetPositionsWithPortfolioHealth(); len(rows) != 1 {
		t.Fatalf("book rows = %d, want the option only", len(rows))
	}
}

// A malformed required field poisons the whole staged generation: the
// receipt records the invalid payload and accountDownloadEnd publishes
// nothing.
func TestPortfolioRowMalformedFieldInvalidatesGeneration(t *testing.T) {
	good := wireTStockRow("DU1234567", 4242, "SYNX", "USD", 100, 50)
	for _, tc := range []struct {
		name string
		bad  func(r *wireTRow) []byte
	}{
		{"option without strike", func(r *wireTRow) []byte {
			*r = wireTOptionRow("DU1234567", 4343, "ZETA", 1)
			r.Strike = ""
			return r.portfolioFrame()
		}},
		{"option without right", func(r *wireTRow) []byte {
			*r = wireTOptionRow("DU1234567", 4343, "ZETA", 1)
			r.Right = ""
			return r.portfolioFrame()
		}},
		{"future without multiplier", func(r *wireTRow) []byte { r.SecType = "FUT"; r.Expiry = "20261218"; return r.portfolioFrame() }},
		{"zero conId", func(r *wireTRow) []byte { r.ConID = 0; return r.portfolioFrame() }},
		{"non-numeric position", func(r *wireTRow) []byte {
			return twsGatewayFrame(msgPortfolioValue, "8", "4242", "SYNX", "STK", "", "", "", "", "", "USD", "SYNX", "SYNX", "abc", "50", "5000", "49", "100", "0", "DU1234567")
		}},
		{"short frame", func(r *wireTRow) []byte { return twsGatewayFrame(msgPortfolioValue, "8", "4242", "SYNX") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			conn := wireTAccountConn(t, "DU1234567")
			if err := conn.RequestAccountUpdates("DU1234567"); err != nil {
				t.Fatal(err)
			}
			conn.processMessage(good.portfolioFrame())
			row := good
			conn.processMessage(tc.bad(&row))
			_, health := conn.GetPositionsWithPortfolioHealth()
			if health.InvalidPayloadAt.IsZero() {
				t.Fatalf("malformed row left the generation valid: %+v", health)
			}
			wireTDownloadEnd(conn, "DU1234567")
			rows, health := conn.GetPositionsWithPortfolioHealth()
			if len(rows) != 0 || !health.InitialCompletedAt.IsZero() {
				t.Fatalf("poisoned generation published: rows=%d health=%+v", len(rows), health)
			}
		})
	}
}

// One login with several accounts streams them all over one subscription.
// Rows and end markers of a sibling account are dropped and leave the bound
// account's download intact; an account outside the login, or a blank one,
// latches a scope conflict that no end marker can publish through.
func TestPortfolioFramesAreScopedToTheBoundAccount(t *testing.T) {
	t.Run("sibling rows dropped", func(t *testing.T) {
		conn := wireTAccountConn(t, "DU1234567,DU7654321")
		if err := conn.RequestAccountUpdates("DU1234567"); err != nil {
			t.Fatal(err)
		}
		conn.processMessage(wireTStockRow("DU1234567", 4242, "SYNX", "USD", 100, 50).portfolioFrame())
		conn.processMessage(wireTStockRow("DU7654321", 4343, "ZETA", "USD", 5, 50).portfolioFrame())
		wireTDownloadEnd(conn, "DU7654321")
		if rows, health := conn.GetPositionsWithPortfolioHealth(); len(rows) != 0 || !health.InitialCompletedAt.IsZero() || !health.ScopeConflictAt.IsZero() {
			t.Fatalf("sibling end marker published or conflicted: rows=%d health=%+v", len(rows), health)
		}
		wireTDownloadEnd(conn, "DU1234567")
		rows, health := conn.GetPositionsWithPortfolioHealth()
		if len(rows) != 1 || health.InitialCompletedAt.IsZero() || !health.ScopeConflictAt.IsZero() {
			t.Fatalf("bound download: rows=%d health=%+v", len(rows), health)
		}
		if _, leaked := rows["DU7654321|STK:4343"]; leaked {
			t.Fatal("sibling row published into the bound account's book")
		}
	})
	for _, account := range []string{"DU2222222", ""} {
		t.Run("foreign account "+strconv.Quote(account), func(t *testing.T) {
			conn := wireTAccountConn(t, "DU1234567")
			if err := conn.RequestAccountUpdates("DU1234567"); err != nil {
				t.Fatal(err)
			}
			conn.processMessage(wireTStockRow("DU1234567", 4242, "SYNX", "USD", 100, 50).portfolioFrame())
			conn.processMessage(wireTStockRow(account, 4343, "ZETA", "USD", 5, 50).portfolioFrame())
			_, health := conn.GetPositionsWithPortfolioHealth()
			if health.ScopeConflictAt.IsZero() {
				t.Fatalf("foreign row did not latch a scope conflict: %+v", health)
			}
			wireTDownloadEnd(conn, "DU1234567")
			rows, health := conn.GetPositionsWithPortfolioHealth()
			if len(rows) != 0 || !health.InitialCompletedAt.IsZero() {
				t.Fatalf("conflicted stream published: rows=%d health=%+v", len(rows), health)
			}
		})
	}
	t.Run("foreign end marker", func(t *testing.T) {
		conn := wireTAccountConn(t, "DU1234567")
		if err := conn.RequestAccountUpdates("DU1234567"); err != nil {
			t.Fatal(err)
		}
		conn.processMessage(wireTStockRow("DU1234567", 4242, "SYNX", "USD", 100, 50).portfolioFrame())
		wireTDownloadEnd(conn, "DU2222222")
		rows, health := conn.GetPositionsWithPortfolioHealth()
		if len(rows) != 0 || health.ScopeConflictAt.IsZero() {
			t.Fatalf("foreign end marker: rows=%d health=%+v", len(rows), health)
		}
	})
}

// Account values key by tag and currency: a ledger currency becomes
// "<tag>_<CCY>", BASE and an empty currency stay the bare tag. A sibling
// account's row never overwrites the bound account's value, and a short
// frame is ignored.
func TestAccountValueCurrencyKeysAndScope(t *testing.T) {
	conn := wireTAccountConn(t, "DU1234567,DU7654321")
	if err := conn.RequestAccountUpdates("DU1234567"); err != nil {
		t.Fatal(err)
	}
	wireTAccountValue(conn, "NetLiquidation", "100000", "EUR", "DU1234567")
	wireTAccountValue(conn, "CashBalance", "5000", "USD", "DU1234567")
	wireTAccountValue(conn, "NetLiquidation", "100000", "BASE", "DU1234567")
	wireTAccountValue(conn, "AccountType", "INDIVIDUAL", "", "DU1234567")
	wireTAccountValue(conn, "NetLiquidation", "1", "EUR", "DU7654321")
	wireTAccountValue(conn, "AccountType", "JOINT", "", "DU7654321")
	conn.processMessage(twsGatewayFrame(msgAcctValue, "2", "NetLiquidation", "2"))

	for key, want := range map[string]string{
		"NetLiquidation_EUR": "100000", "CashBalance_USD": "5000", "NetLiquidation": "100000", "AccountType": "INDIVIDUAL",
	} {
		if got, ok := conn.GetAccountValue(key); !ok || got != want {
			t.Errorf("GetAccountValue(%q) = %q,%t want %q", key, got, ok, want)
		}
	}
	if _, ok := conn.GetAccountValue("NetLiquidation_USD"); ok {
		t.Error("a key never streamed is present")
	}
	if summary := conn.GetAccountSummary(); len(summary) != 4 {
		t.Errorf("summary keys = %v, want exactly the four bound-account rows", summary)
	}
}

// A single-account login whose Gateway has not named the account yet still
// records its rows: nothing is bound, so no row is foreign.
func TestAccountValueBeforeAnyBindingIsKept(t *testing.T) {
	conn, _ := newReadyWireTestConnection(t)
	wireTAccountValue(conn, "NetLiquidation", "100000", "USD", "DU1234567")
	if got, ok := conn.GetAccountValue("NetLiquidation_USD"); !ok || got != "100000" {
		t.Fatalf("GetAccountValue = %q,%t", got, ok)
	}
}
