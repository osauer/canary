package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/osauer/canary/v2/internal/flexstmt"
	"github.com/osauer/canary/v2/internal/procinfo"
	"github.com/osauer/canary/v2/internal/xdgcache"
)

// The account-data gate (scripts/check-no-account-data.sh) and the log
// monitor (scripts/log-monitor) keep the owner's current holdings and live
// money figures out of commits and reports. They read them from two private
// files only the daemon writes, so no process but the daemon opens daemon.db
// with SQLite. An outside SQLite opener that finds no lock on the -shm's DMS
// byte resets the shared memory to 3 bytes, and a daemon still mapping it
// dies with SIGBUS: live 2026-10-29 21:29:45 CET, after which one committed
// risk-capital save was missing.
const (
	holdingsDenylistFile = "holdings-denylist"
	moneyDenylistFile    = "money-denylist"
	// noFlexPositionReport heads a holdings file with nothing to check yet.
	noFlexPositionReport = "# no Flex position report"
	// accountDataRefreshEvery paces the money file and the outside-opener
	// check. Authors round and the book moves before they commit; the gate
	// matches rounded figures within 2%, which a minute's move stays inside.
	accountDataRefreshEvery = time.Minute
	// shmDMSByte is SQLite's dead-man-switch lock byte in the -shm file,
	// (22+SQLITE_SHM_NLOCK)*4+SQLITE_SHM_NLOCK. Every open connection holds
	// it shared.
	shmDMSByte = 128
)

var holdingTickerRE = regexp.MustCompile(`^[A-Z][A-Z0-9]*(\.[A-Z0-9]+)?$`)

// accountDataFiles is the last content written, so an unchanged file is not
// rewritten, and the outside-opener probe. holdings is touched only under the
// retained-Flex gate, money and the probe only by the refresh loop.
type accountDataFiles struct {
	holdings []byte
	money    []byte
	// shm stays open for the life of the process: closing any descriptor of
	// daemon.db-shm drops every POSIX lock this process holds on it,
	// SQLite's included.
	shm    *os.File
	warned map[int]bool
}

// holdingsDenylist renders the latest Flex position report of each account
// as "# as of DAY" and one ticker per line: the symbols and their
// underlyings, an IBKR class suffix ("BRK B") cut to its root.
func holdingsDenylist(statements []flexstmt.Statement) []byte {
	type held struct{ account, day, symbol, underlying string }
	var rows []held
	latest := map[string]string{}
	for _, st := range statements {
		for _, p := range st.Positions {
			if p.ReportDate.IsZero() {
				continue
			}
			account := p.AccountID
			if account == "" {
				account = st.AccountID
			}
			day := p.ReportDate.UTC().Format(time.DateOnly)
			rows = append(rows, held{account, day, p.Symbol, p.UnderlyingSymbol})
			latest[account] = max(latest[account], day)
		}
	}
	if len(rows) == 0 {
		return []byte(noFlexPositionReport + "\n")
	}
	asOf := ""
	var tickers []string
	for _, r := range rows {
		if r.day != latest[r.account] {
			continue
		}
		asOf = max(asOf, r.day)
		for _, s := range []string{r.symbol, r.underlying} {
			root, _, _ := strings.Cut(strings.TrimSpace(s), " ")
			if holdingTickerRE.MatchString(root) {
				tickers = append(tickers, root)
			}
		}
	}
	slices.Sort(tickers)
	var b bytes.Buffer
	fmt.Fprintf(&b, "# as of %s\n", asOf)
	for _, t := range slices.Compact(tickers) {
		b.WriteString(t + "\n")
	}
	return b.Bytes()
}

// moneyDenylist renders the live book's money figures as the gate's rows:
// "nlv|VALUE|DAY" for the live account's last equity ("missing" when the
// document has an observation time but no value) and "cash|VALUE|" for each
// cash, trade-date cash and settled cash figure of the live proposal
// snapshot's sweep. Values keep their JSON spelling.
func moneyDenylist(riskCapital, proposals []byte) ([]byte, error) {
	var b bytes.Buffer
	if len(riskCapital) > 0 {
		var doc struct {
			State struct {
				AccountMode string      `json:"account_mode"`
				Equity      json.Number `json:"last_equity_base"`
				AsOf        string      `json:"last_equity_as_of"`
			} `json:"state"`
		}
		if err := json.Unmarshal(riskCapital, &doc); err != nil {
			return nil, fmt.Errorf("decode risk capital document: %w", err)
		}
		if st := doc.State; st.AccountMode == "live" && st.AsOf != "" {
			value := st.Equity.String()
			if value == "" {
				value = "missing"
			}
			fmt.Fprintf(&b, "nlv|%s|%s\n", value, st.AsOf[:min(len(st.AsOf), 10)])
		}
	}
	if len(proposals) > 0 {
		var doc struct {
			AccountMode string `json:"account_mode"`
			CashSweep   struct {
				Currencies []map[string]json.RawMessage `json:"currencies"`
			} `json:"cash_sweep"`
		}
		if err := json.Unmarshal(proposals, &doc); err != nil {
			return nil, fmt.Errorf("decode proposal snapshot: %w", err)
		}
		if doc.AccountMode == "live" {
			for _, currency := range doc.CashSweep.Currencies {
				for _, key := range []string{"cash", "trade_date_cash", "settled_cash"} {
					var n json.Number
					if raw, ok := currency[key]; ok && json.Unmarshal(raw, &n) == nil {
						fmt.Fprintf(&b, "cash|%s|\n", n)
					}
				}
			}
		}
	}
	return b.Bytes(), nil
}

// writeHoldingsDenylist follows the statement projection. The caller holds
// the retained-Flex gate.
func (s *Server) writeHoldingsDenylist(statements []flexstmt.Statement) {
	if s == nil || !s.productionStateDatabase {
		return
	}
	data := holdingsDenylist(statements)
	if bytes.Equal(data, s.accountData.holdings) {
		return
	}
	if err := writeAccountDataFile(holdingsDenylistFile, data); err != nil {
		s.warnf("account-data gate: write the holdings file: %v", err)
		return
	}
	s.accountData.holdings = data
}

// startAccountDataFiles keeps the money file current and watches for another
// process opening daemon.db with SQLite.
func (s *Server) startAccountDataFiles(ctx context.Context) {
	if s == nil || !s.productionStateDatabase {
		return
	}
	go func() {
		ticker := time.NewTicker(accountDataRefreshEvery)
		defer ticker.Stop()
		for {
			s.refreshMoneyDenylist(ctx)
			s.checkOutsideSQLiteOpeners(ctx)
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
}

func (s *Server) refreshMoneyDenylist(ctx context.Context) {
	core := s.coreStore
	if core == nil {
		return
	}
	// Without a resolved account the live book is unknown; the last file
	// stays rather than an empty one replacing it.
	scopeKey, err := riskCapitalScopeKey(s.currentBrokerStateScope())
	if err != nil {
		return
	}
	risk, _, err := core.GetStateDocument(ctx, scopeKey, stateKindRiskCapital)
	if err != nil {
		return
	}
	proposals, _, err := core.GetStateDocument(ctx, daemonStateScope, proposalStateKind)
	if err != nil {
		return
	}
	data, err := moneyDenylist(risk.JSON, proposals.JSON)
	if err != nil {
		s.warnf("account-data gate: %v", err)
		return
	}
	if bytes.Equal(data, s.accountData.money) {
		return
	}
	if err := writeAccountDataFile(moneyDenylistFile, data); err != nil {
		s.warnf("account-data gate: write the money file: %v", err)
		return
	}
	s.accountData.money = data
}

func writeAccountDataFile(name string, data []byte) error {
	path, err := xdgcache.CacheDir(name)
	if err != nil {
		return err
	}
	return writePrivateStateAtomic(path, data)
}

// checkOutsideSQLiteOpeners warns once per process found holding a lock on
// daemon.db-shm. F_GETLK never reports this process's own locks, so any
// holder is another process with daemon.db open.
func (s *Server) checkOutsideSQLiteOpeners(ctx context.Context) {
	if s.coreStore == nil || s.coreStorePath == "" {
		return
	}
	if s.accountData.shm == nil {
		f, err := os.Open(s.coreStorePath + "-shm")
		if err != nil {
			return
		}
		s.accountData.shm = f
		s.accountData.warned = map[int]bool{}
	}
	lock := syscall.Flock_t{Type: syscall.F_WRLCK, Whence: 0, Start: shmDMSByte, Len: 1}
	if err := syscall.FcntlFlock(s.accountData.shm.Fd(), syscall.F_GETLK, &lock); err != nil {
		return
	}
	pid := int(lock.Pid)
	if lock.Type == syscall.F_UNLCK || pid <= 0 || s.accountData.warned[pid] {
		return
	}
	s.accountData.warned[pid] = true
	name := procinfo.Names(ctx, []int{pid})[pid]
	s.warnf("daemon.db: another process (pid %d %s) has it open with SQLite; only the daemon may, because an outside opener resets the daemon's shared memory if the daemon's lock on it is lost (crash of 2026-10-29). Read account data through canary commands", pid, nonEmptyString(name, "unknown"))
}
