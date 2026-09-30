package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/osauer/canary/v2/internal/publichttp"
	ibkrlib "github.com/osauer/canary/v2/pkg/ibkr"
)

// The USD bill universe: TreasuryDirect's public list of recently auctioned
// Treasury bills (no key), read once a day while the cash sweep is enabled
// and kept in daemon.db, so a restart does not ask again. The sweep picks the
// bill maturing nearest a rung's target from it and then resolves that bill
// by CUSIP at the broker.

// treasuryDirectBillsURL is the public securities endpoint the owner named
// (decision of 2026-09-30); a variable so tests can point it elsewhere.
var treasuryDirectBillsURL = "https://www.treasurydirect.gov/TA_WS/securities/Bill?format=json"

var treasuryDirectHTTPClient = &http.Client{Timeout: 20 * time.Second}

const (
	treasuryBillUniverseKind  = "cash_sweep_us_bill_universe_v1"
	treasuryBillUniverseScope = "public-treasurydirect:bills"
	// The list is refreshed once a day; a list up to two days old still
	// serves, so one missed refresh does not stop the sweep. Older than that
	// the universe is unavailable.
	treasuryBillUniverseRefresh = 24 * time.Hour
	treasuryBillUniverseMaxAge  = 48 * time.Hour
	// A failed refresh is retried after this.
	treasuryBillUniverseRetry = 15 * time.Minute
	// treasuryBillUniverseMaxBody bounds the response read.
	treasuryBillUniverseMaxBody = 4 << 20
)

// treasuryBill is one outstanding bill: CUSIP and dates (YYYY-MM-DD).
type treasuryBill struct {
	CUSIP        string `json:"cusip"`
	IssueDate    string `json:"issue_date"`
	MaturityDate string `json:"maturity_date"`
	Term         string `json:"term,omitempty"`
}

// treasuryBillUniverseRecord is the persisted list.
type treasuryBillUniverseRecord struct {
	FetchedAt time.Time      `json:"fetched_at"`
	Bills     []treasuryBill `json:"bills"`
}

// billUniverse is the in-memory list, its last attempt and failure.
type billUniverse struct {
	mu          sync.Mutex
	record      treasuryBillUniverseRecord
	lastAttempt time.Time
	lastErr     string
	kick        chan struct{}
	fetch       func(context.Context) ([]treasuryBill, error)
}

// snapshot returns the list when it is young enough to serve, else the
// reason it is unavailable.
func (u *billUniverse) snapshot(now time.Time) ([]treasuryBill, time.Time, string) {
	if u == nil {
		return nil, time.Time{}, "TreasuryDirect's bill list has not been read"
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	switch {
	case u.record.FetchedAt.IsZero():
		reason := "TreasuryDirect's bill list has not been read yet"
		if u.lastErr != "" {
			reason = "TreasuryDirect's bill list is unreachable: " + u.lastErr
		}
		return nil, time.Time{}, reason
	case now.Sub(u.record.FetchedAt) > treasuryBillUniverseMaxAge:
		reason := fmt.Sprintf("TreasuryDirect's bill list was last read %s, more than two days ago", u.record.FetchedAt.UTC().Format(time.RFC3339))
		if u.lastErr != "" {
			reason += "; the latest read failed: " + u.lastErr
		}
		return nil, u.record.FetchedAt, reason
	}
	return slices.Clone(u.record.Bills), u.record.FetchedAt, ""
}

// due reports whether a refresh should run now.
func (u *billUniverse) due(now time.Time) bool {
	u.mu.Lock()
	defer u.mu.Unlock()
	return now.Sub(u.record.FetchedAt) >= treasuryBillUniverseRefresh && now.Sub(u.lastAttempt) >= treasuryBillUniverseRetry
}

// requestRefresh wakes the refresher without blocking.
func (u *billUniverse) requestRefresh() {
	if u == nil || u.kick == nil {
		return
	}
	select {
	case u.kick <- struct{}{}:
	default:
	}
}

// refresh reads the list once and, on success, keeps and persists it.
func (u *billUniverse) refresh(ctx context.Context, now time.Time, persist func(context.Context, treasuryBillUniverseRecord) error) error {
	u.mu.Lock()
	u.lastAttempt = now
	fetch := u.fetch
	u.mu.Unlock()
	if fetch == nil {
		fetch = fetchTreasuryDirectBills
	}
	bills, err := fetch(ctx)
	u.mu.Lock()
	defer u.mu.Unlock()
	if err != nil {
		u.lastErr = err.Error()
		return err
	}
	u.lastErr = ""
	u.record = treasuryBillUniverseRecord{FetchedAt: now.UTC(), Bills: bills}
	if persist != nil {
		if perr := persist(ctx, u.record); perr != nil {
			return fmt.Errorf("persist TreasuryDirect bill list: %w", perr)
		}
	}
	return nil
}

// fetchTreasuryDirectBills reads and parses the public list.
func fetchTreasuryDirectBills(ctx context.Context) ([]treasuryBill, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, treasuryDirectBillsURL, nil)
	if err != nil {
		return nil, err
	}
	publichttp.SetUserAgent(req)
	req.Header.Set("Accept", "application/json")
	resp, err := treasuryDirectHTTPClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("HTTP %s", resp.Status)
	}
	return parseTreasuryDirectBills(io.LimitReader(resp.Body, treasuryBillUniverseMaxBody))
}

// parseTreasuryDirectBills reads TreasuryDirect's securities JSON: an array
// of records with cusip, issueDate and maturityDate ("2006-01-02T15:04:05").
// Records that are not bills, carry no valid CUSIP or no maturity (an
// announced reopening) are skipped; a reopened CUSIP counts once, at its
// earliest issue date.
func parseTreasuryDirectBills(r io.Reader) ([]treasuryBill, error) {
	var records []struct {
		CUSIP        string `json:"cusip"`
		IssueDate    string `json:"issueDate"`
		MaturityDate string `json:"maturityDate"`
		SecurityType string `json:"securityType"`
		SecurityTerm string `json:"securityTerm"`
	}
	if err := json.NewDecoder(r).Decode(&records); err != nil {
		return nil, fmt.Errorf("decode TreasuryDirect bill list: %w", err)
	}
	byCUSIP := map[string]treasuryBill{}
	for _, rec := range records {
		cusip := strings.ToUpper(strings.TrimSpace(rec.CUSIP))
		if !strings.EqualFold(strings.TrimSpace(rec.SecurityType), "Bill") || !ibkrlib.ValidCUSIP(cusip) {
			continue
		}
		issue, okIssue := treasuryDirectDate(rec.IssueDate)
		maturity, okMaturity := treasuryDirectDate(rec.MaturityDate)
		if !okIssue || !okMaturity || !maturity.After(issue) {
			continue
		}
		bill := treasuryBill{CUSIP: cusip, IssueDate: issue.Format(time.DateOnly), MaturityDate: maturity.Format(time.DateOnly), Term: strings.TrimSpace(rec.SecurityTerm)}
		if prior, ok := byCUSIP[cusip]; ok && prior.IssueDate <= bill.IssueDate {
			continue
		}
		byCUSIP[cusip] = bill
	}
	if len(byCUSIP) == 0 {
		return nil, errors.New("TreasuryDirect's bill list held no bill with a CUSIP, issue and maturity date")
	}
	out := make([]treasuryBill, 0, len(byCUSIP))
	for _, bill := range byCUSIP {
		out = append(out, bill)
	}
	slices.SortFunc(out, func(a, b treasuryBill) int {
		return strings.Compare(a.MaturityDate+a.CUSIP, b.MaturityDate+b.CUSIP)
	})
	return out, nil
}

func treasuryDirectDate(raw string) (time.Time, bool) {
	raw = strings.TrimSpace(raw)
	if len(raw) < len(time.DateOnly) {
		return time.Time{}, false
	}
	t, err := time.Parse(time.DateOnly, raw[:len(time.DateOnly)])
	return t, err == nil
}

// billUniverse returns the server's list, loading it from daemon.db on
// first use.
func (s *Server) billUniverse() *billUniverse {
	b := s.bondSupport()
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.universe == nil {
		u := &billUniverse{kick: make(chan struct{}, 1)}
		if raw, ok, err := loadMarketState(s.coreStore, treasuryBillUniverseScope, treasuryBillUniverseKind); err == nil && ok {
			var rec treasuryBillUniverseRecord
			if json.Unmarshal(raw, &rec) == nil && !rec.FetchedAt.IsZero() && len(rec.Bills) > 0 {
				u.record = rec
			}
		}
		b.universe = u
	}
	return b.universe
}

// cashSweepWantsUSBills reports whether the policy in force sweeps USD into
// bills, the only reason to read TreasuryDirect.
func (s *Server) cashSweepWantsUSBills() bool {
	if s == nil || s.protectionPolicies == nil {
		return false
	}
	policy, _ := s.protectionPolicies.Active()
	bucket := policy.Buckets.CashSweep
	return bucket.enabled() && slices.Contains(bucket.currency("USD").Instruments, cashSweepInstrumentUSTBill)
}

// startCashSweepBillUniverse runs the daily TreasuryDirect read while the
// sweep wants USD bills. An offline or test state database never starts
// the network reader (the macro sources' seam).
func (s *Server) startCashSweepBillUniverse(ctx context.Context) {
	u := s.billUniverse()
	if !s.productionStateDatabase || s.disableMacroSources {
		return
	}
	persist := func(ctx context.Context, rec treasuryBillUniverseRecord) error {
		raw, err := json.Marshal(rec)
		if err != nil {
			return err
		}
		return saveMarketDocument(ctx, s.coreStore, treasuryBillUniverseScope, treasuryBillUniverseKind, raw)
	}
	s.macroLoopWG.Go(func() {
		ticker := time.NewTicker(treasuryBillUniverseRetry)
		defer ticker.Stop()
		for {
			if now := time.Now(); s.cashSweepWantsUSBills() && u.due(now) {
				readCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
				if err := u.refresh(readCtx, now, persist); err != nil && ctx.Err() == nil {
					s.warnf("cash sweep: TreasuryDirect bill list refresh failed; next attempt in %s: %v", treasuryBillUniverseRetry, err)
				}
				cancel()
			}
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			case <-u.kick:
			}
		}
	})
}
