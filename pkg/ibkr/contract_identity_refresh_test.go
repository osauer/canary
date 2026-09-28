package ibkr

import (
	"bufio"
	"context"
	"encoding/binary"
	"errors"
	"strconv"
	"sync"
	"testing"
	"time"
)

// retiredIdentityGateway answers like a gateway that lists a symbol under
// listed and serves history only for served; any other conID draws code 200.
type retiredIdentityGateway struct {
	c              *Connector
	conn           *Connection
	listed, served string

	mu      sync.Mutex
	history []string // conIDs of reqHistoricalData, in order
	lookups int      // reqContractDetails sent
}

func (g *retiredIdentityGateway) Write(p []byte) (int, error) {
	for off := 0; off+4 <= len(p); {
		n := int(binary.BigEndian.Uint32(p[off : off+4]))
		fields := g.conn.decodeMessage(p[off+4 : off+4+n])
		off += 4 + n
		switch fields[0] {
		case strconv.Itoa(reqHistoricalData):
			reqID, _ := strconv.Atoi(fields[1])
			g.mu.Lock()
			g.history = append(g.history, fields[2])
			g.mu.Unlock()
			if fields[2] != g.served {
				g.c.failHistoricalRequest(reqID, &HistoricalRequestError{Code: 200, Message: "No security definition has been found for the request"})
				continue
			}
			g.c.handleHistoricalData([]string{strconv.Itoa(msgHistoricalData), fields[1], "2",
				"20260924", "80", "81", "79", "80.5", "1000", "80.2", "10",
				"20260925", "80.5", "82", "80", "81.5", "1200", "81", "12", ""})
		case strconv.Itoa(reqContractData):
			reqID, _ := strconv.Atoi(fields[2])
			g.mu.Lock()
			g.lookups++
			g.mu.Unlock()
			g.conn.dispatchHandlers(msgContractData, contractDetailsWakeupFrame(reqID, "OKE", "STK", g.listed), g.conn.BrokerSessionEpoch())
			prewarmTestEnd(g.conn, reqID)
		}
	}
	return len(p), nil
}

func newRetiredIdentityConnector(t *testing.T, listed, served string, cachedConID int) (*Connector, *retiredIdentityGateway) {
	t.Helper()
	c, conn, _, _ := newMissTestConnector(t)
	c.mu.Lock()
	c.ready = true
	c.mu.Unlock()
	g := &retiredIdentityGateway{c: c, conn: conn, listed: listed, served: served}
	conn.writer = bufio.NewWriter(g)
	if cachedConID != 0 {
		c.SeedContractDetails("OKE", ContractDetailsLite{Symbol: "OKE", SecType: "STK", Exchange: "SMART", PrimaryExch: "NYSE", Currency: "USD", ConID: cachedConID, LocalSymbol: "OKE", TradingClass: "OKE"})
	}
	return c, g
}

// A cached conID that IBKR retired drew code 200 on every read, and the
// breadth lane skipped the symbol for the session, every session.
func TestDailyHistoryReplacesRetiredCachedIdentity(t *testing.T) {
	buf := captureConnectorLogs(t)
	c, g := newRetiredIdentityConnector(t, "921971937", "921971937", 10794)

	bars, err := c.FetchHistoricalDailyBars(context.Background(), "OKE", 30, 5*time.Second)
	if err != nil {
		t.Fatalf("read = %v, want bars under the current identity", err)
	}
	if len(bars) != 2 {
		t.Fatalf("bars = %d, want 2", len(bars))
	}
	g.mu.Lock()
	history, lookups := append([]string(nil), g.history...), g.lookups
	g.mu.Unlock()
	if len(history) != 2 || history[0] != "10794" || history[1] != "921971937" {
		t.Fatalf("history requests by conID = %v, want the retired then the current identity", history)
	}
	if lookups != 1 {
		t.Fatalf("contract lookups = %d, want exactly one refresh", lookups)
	}
	if cached := c.cachedContractDetail("OKE"); cached == nil || cached.ConID != 921971937 {
		t.Fatalf("cached identity = %+v, want conID 921971937", cached)
	}
	if lines := logLines(buf, "Contract identity for OKE changed"); len(lines) != 1 {
		t.Fatalf("identity change warnings = %q, want one", lines)
	}
}

// When IBKR still lists the rejected conID, the rejection is the answer:
// one lookup, no second history request, and the definition verdict.
func TestDailyHistoryKeepsVerdictWhenIdentityUnchanged(t *testing.T) {
	c, g := newRetiredIdentityConnector(t, "10794", "", 10794)

	_, err := c.FetchHistoricalDailyBars(context.Background(), "OKE", 30, 5*time.Second)
	if !errors.Is(err, ErrContractNoDefinition) {
		t.Fatalf("read = %v, want the definition verdict", err)
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if len(g.history) != 1 || g.lookups != 1 {
		t.Fatalf("history=%v lookups=%d, want one of each", g.history, g.lookups)
	}
}

// An identity fetched fresh for this read is not retried: the lookup just
// answered, so a code 200 against it is the verdict.
func TestDailyHistoryDoesNotRefreshAFreshIdentity(t *testing.T) {
	c, g := newRetiredIdentityConnector(t, "10794", "", 0)

	_, err := c.FetchHistoricalDailyBars(context.Background(), "OKE", 30, 5*time.Second)
	if !errors.Is(err, ErrContractNoDefinition) {
		t.Fatalf("read = %v, want the definition verdict", err)
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if len(g.history) != 1 || g.lookups != 1 {
		t.Fatalf("history=%v lookups=%d, want one of each", g.history, g.lookups)
	}
}
