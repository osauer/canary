package ibkr

import (
	"sync"
	"testing"
	"time"
)

// Independent refutation: concurrent reads must remain detached and scoped
// while the original account subscription is replaced in the same socket.
func TestRefuterPassiveStreamConcurrentScopeReplacement(t *testing.T) {
	c, binding, account := streamObservationFixture(t)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for range 200 {
			c.conn.resetPortfolioStreamHealth(account, time.Now().UTC())
			feedStreamValue(c.conn, account, "CashBalance", "0", "USD")
			c.conn.resetPortfolioStreamHealth("U_OTHER_SYNTHETIC", time.Now().UTC())
			feedStreamValue(c.conn, "U_OTHER_SYNTHETIC", "$LEDGER-SettledCash", "100", "GBP")
		}
	}()
	go func() {
		defer wg.Done()
		for range 400 {
			got := c.CaptureAccountStreamObservationForSession(binding, account)
			if got == nil {
				t.Error("current socket unexpectedly retired")
				return
			}
			if got.Status == "scope_or_generation_changed" {
				if len(got.Rows) != 0 {
					t.Error("scope failure retained rows")
					return
				}
				continue
			}
			for _, row := range got.Rows {
				if row.Currency == "GBP" {
					t.Error("foreign subscription entered original account diagnostic")
					return
				}
			}
			if len(got.Rows) > 0 {
				got.Rows[0].Currency = "X_PRIVATE_MUTATION"
			}
		}
	}()
	wg.Wait()
	c.conn.resetPortfolioStreamHealth(account, time.Now().UTC())
	feedStreamValue(c.conn, account, "CashBalance", "0", "USD")
	got := c.CaptureAccountStreamObservationForSession(binding, account)
	if len(got.Rows) != 1 || got.Rows[0].Currency != "USD" {
		t.Fatal("detached read mutation contaminated new observation")
	}
}
