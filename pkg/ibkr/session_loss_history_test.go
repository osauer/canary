package ibkr

import (
	"errors"
	"io"
	"testing"
	"time"
)

// A history read sent on a session that ends cannot be answered: TWS answers
// a request only on the session that carried it. The read ends with its
// session, as an unavailable connection, whether TWS ended it (its nightly
// restart), the owner stopped the connector, or the answer-path supervisor
// dropped the session. Until 2026-10-08 it waited out its own budget instead:
// a breadth read sent at 23:45:00.0, just before TWS's restart, held the
// breadth refresh open until 23:47:00.
func TestHistoryReadEndsWithItsSession(t *testing.T) {
	for _, tc := range []struct {
		name string
		end  func(*Connector, *Connection)
	}{
		{"lost", func(_ *Connector, conn *Connection) { conn.handleDisconnection(io.EOF) }},
		{"stopped", func(c *Connector, _ *Connection) { _ = c.Stop() }},
		{"dropped", func(c *Connector, _ *Connection) { c.DropSession(errors.New("history unanswered")) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, conn, _, _ := newMissTestConnector(t)
			conn.config.AutoReconnect = false // as the daemon's lanes run; no redial outlives the test
			c.mu.Lock()
			c.running = true
			c.mu.Unlock()
			c.attachConnectionHooks(conn)
			contract := Contract{ConID: 90123, Symbol: "SYNTH", SecType: "STK", Exchange: "SMART", Currency: "USD"}
			read := make(chan error, 1)
			go func() {
				_, err := c.FetchHistoricalDailyBarsWithContract(t.Context(), contract, 10, 30*time.Second)
				read <- err
			}()
			for deadline := time.Now().Add(2 * time.Second); c.AnswerPath(time.Now()).InFlight == 0; time.Sleep(5 * time.Millisecond) {
				if time.Now().After(deadline) {
					t.Fatal("no history request was sent")
				}
			}

			tc.end(c, conn)
			var err error
			select {
			case err = <-read:
			case <-time.After(2 * time.Second):
				t.Fatal("history read outlived its session")
			}
			if !errors.Is(err, ErrIBKRUnavailable) {
				t.Fatalf("a read cut by the session's end is an unavailable connection: %v", err)
			}
			if path := c.AnswerPath(time.Now()); path.InFlight != 0 || !path.LastAnswerAt.IsZero() || path.Timeouts != 0 {
				t.Fatalf("an ended read counted as pending, answered or timed out: %+v", path)
			}
		})
	}
}
