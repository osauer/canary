package daemon

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/osauer/canary/v2/internal/daemon/corestore"
	"github.com/osauer/canary/v2/internal/dial"
	"github.com/osauer/canary/v2/internal/rpc"
	ibkrlib "github.com/osauer/canary/v2/pkg/ibkr"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// daemonBrokerEvidenceBinding joins daemon connector publication authority to
// the Connector's exact socket/order/portfolio frontiers. It is process-local
// evidence for one final commit, never a durable identity or broker-write
// authorization.
type daemonBrokerEvidenceBinding struct {
	scope          brokerStateScope
	connector      *ibkrlib.Connector
	connectorEpoch uint64
	broker         ibkrlib.BrokerEvidenceBinding
}

// withStableBrokerEvidence linearizes a derived-state commit against socket
// replacement, order callbacks, and structural portfolio changes. Connector
// publication participates in the same barrier, so s.mu is needed only for a
// short identity check and is never held across SQLite/composer work. False
// with nil error means the captured evidence drifted and commit was not called.
func (s *Server) withStableBrokerEvidence(binding daemonBrokerEvidenceBinding, commit func() error) (bool, error) {
	if s != nil && s.stableBrokerEvidenceForTest != nil {
		return s.stableBrokerEvidenceForTest(binding, commit)
	}
	if s == nil || binding.connector == nil || commit == nil || !brokerScopeConcrete(binding.scope) {
		return false, nil
	}
	var commitErr error
	committed := binding.connector.WithStableBrokerEvidence(binding.broker, func() bool {
		s.mu.Lock()
		if s.connector != binding.connector || s.connectorEpoch != binding.connectorEpoch {
			s.mu.Unlock()
			return false
		}
		ep := s.endpoint
		configuredAccount := ""
		port := ep.Port
		if s.cfg != nil {
			configuredAccount = s.cfg.Gateway.Account
			if port == 0 && s.cfg.Gateway.Port != nil {
				port = *s.cfg.Gateway.Port
			}
		}
		currentScope := brokerStateScopeFromSnapshot(configuredAccount, ep.Account, port, binding.connector.AccountID())
		if !sameBrokerScope(binding.scope, currentScope) {
			s.mu.Unlock()
			return false
		}
		s.mu.Unlock()
		commitErr = commit()
		return commitErr == nil
	})
	return committed, commitErr
}

// withStableBrokerAndOrderEvidence extends the broker barrier through the
// exact local order-event head used to derive Protection or Order Integrity.
// Lock order is binding Connector -> order journal -> SQLite composer. False
// with nil error means either frontier drifted and commit was not called.
func (s *Server) withStableBrokerAndOrderEvidence(binding daemonBrokerEvidenceBinding, orderJournal *orderJournalStore, expectedOrderHead int64, commit func() error) (bool, error) {
	if s == nil || orderJournal == nil || orderJournal != s.orderJournal || expectedOrderHead < 0 || commit == nil {
		return false, nil
	}
	journalCommitted := false
	brokerCommitted, err := s.withStableBrokerEvidence(binding, func() error {
		var journalErr error
		journalCommitted, journalErr = orderJournal.WithStableAuthorityHead(expectedOrderHead, commit)
		return journalErr
	})
	return brokerCommitted && journalCommitted, err
}

// withConnectorEvidencePublication changes the daemon's connector identity
// while both the outgoing and incoming Connector barriers participate. The
// caller supplies the exact expected current pointer; false means another
// lifecycle transition won the race and no mutation ran. mutateLocked runs
// under s.mu and must not call Connector methods.
func (s *Server) withConnectorEvidencePublication(expected, next *ibkrlib.Connector, mutateLocked func()) bool {
	if s == nil || mutateLocked == nil {
		return false
	}
	applied := false
	wrapped := func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.connector == expected {
			mutateLocked()
			applied = true
		}
	}
	if expected != nil {
		expected.WithBrokerEvidenceMutation(func() {
			if next != nil && next != expected {
				next.WithBrokerEvidenceMutation(wrapped)
				return
			}
			wrapped()
		})
	} else if next != nil {
		next.WithBrokerEvidenceMutation(wrapped)
	} else {
		wrapped()
	}
	return applied
}

const authorityWatermarkVersion = 1

type authorityWatermarkFile struct {
	Version int                     `json:"version"`
	Head    corestore.AuthorityHead `json:"head"`
}

func loadAuthorityWatermark(path string) (*corestore.AuthorityHead, error) {
	if strings.TrimSpace(path) == "" {
		return nil, fmt.Errorf("authority watermark path is empty")
	}
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("inspect authority watermark: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return nil, fmt.Errorf("authority watermark must be a regular private file")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read authority watermark: %w", err)
	}
	var file authorityWatermarkFile
	if err := json.Unmarshal(raw, &file); err != nil {
		return nil, fmt.Errorf("decode authority watermark: %w", err)
	}
	if file.Version != authorityWatermarkVersion || strings.TrimSpace(file.Head.AuthorityEpoch) == "" || file.Head.HeadGeneration < 0 || file.Head.LastEventSeq < 0 || file.Head.SignerGeneration < 1 {
		return nil, fmt.Errorf("authority watermark is invalid")
	}
	return &file.Head, nil
}

func writeAuthorityWatermark(path string, head corestore.AuthorityHead) error {
	if strings.TrimSpace(head.AuthorityEpoch) == "" || head.HeadGeneration < 0 || head.LastEventSeq < 0 || head.SignerGeneration < 1 {
		return fmt.Errorf("refuse invalid authority watermark")
	}
	raw, err := json.Marshal(authorityWatermarkFile{Version: authorityWatermarkVersion, Head: head})
	if err != nil {
		return fmt.Errorf("encode authority watermark: %w", err)
	}
	encoded := append(raw, '\n')
	// A maintenance-only schema upgrade can publish a new physical database
	// without changing the authority head. Preserve the already-durable
	// watermark byte-for-byte in that case rather than physically re-stamping
	// identical authority.
	unchanged := false
	if info, statErr := os.Lstat(path); statErr == nil &&
		info.Mode()&os.ModeSymlink == 0 && info.Mode().IsRegular() &&
		info.Mode().Perm()&0o077 == 0 {
		if existing, readErr := os.ReadFile(path); readErr == nil && bytes.Equal(existing, encoded) {
			unchanged = true
		}
	}
	if !unchanged {
		if err := writePrivateStateAtomic(path, encoded); err != nil {
			return fmt.Errorf("write authority watermark: %w", err)
		}
	}
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return fmt.Errorf("open authority watermark for sync: %w", err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return fmt.Errorf("sync authority watermark: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close authority watermark: %w", err)
	}
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return fmt.Errorf("open authority watermark directory: %w", err)
	}
	if err := dir.Sync(); err != nil {
		_ = dir.Close()
		return fmt.Errorf("sync authority watermark directory: %w", err)
	}
	if err := dir.Close(); err != nil {
		return fmt.Errorf("close authority watermark directory: %w", err)
	}
	return nil
}

type brokerStateScope struct {
	Account string
	Mode    string
}

func (s *Server) currentBrokerStateScope() brokerStateScope {
	if s == nil {
		return brokerStateScope{}
	}
	s.mu.Lock()
	ep := s.endpoint
	c := s.connector
	s.mu.Unlock()

	configuredAccount := ""
	if s.cfg != nil {
		configuredAccount = s.cfg.Gateway.Account
	}
	connectedAccount := ""
	if c != nil {
		connectedAccount = c.AccountID()
	}
	port := ep.Port
	if port == 0 && s.cfg != nil && s.cfg.Gateway.Port != nil {
		port = *s.cfg.Gateway.Port
	}
	return brokerStateScopeFromSnapshot(configuredAccount, ep.Account, port, connectedAccount)
}

func brokerStateScopeFromSnapshot(configuredAccount, endpointAccount string, port int, connectedAccount string) brokerStateScope {
	account := strings.TrimSpace(configuredAccount)
	if account == "" {
		account = strings.TrimSpace(endpointAccount)
	}
	if !brokerScopeAccountConcrete(account) {
		if connected := strings.TrimSpace(connectedAccount); brokerScopeAccountConcrete(connected) {
			account = connected
		}
	}
	return brokerStateScope{
		Account: account,
		Mode:    accountModeForStatus(port, account),
	}
}

func brokerScopeAccountConcrete(account string) bool {
	account = strings.TrimSpace(account)
	if account == "" || strings.EqualFold(account, "All") {
		return false
	}
	// A managedAccounts frame can carry several accounts (comma-separated)
	// for multi-account logins. That is a session aggregate, not a single
	// account identity — anything that does not trim to one token is
	// non-concrete and scoped state fails closed.
	return !strings.ContainsAny(account, ", \t")
}

// brokerScopeConcrete reports whether the scope names one concrete account
// with a known paper/live mode — the only identity scoped trading state may
// bind to.
func brokerScopeConcrete(scope brokerStateScope) bool {
	if !brokerScopeAccountConcrete(scope.Account) {
		return false
	}
	switch scope.Mode {
	case rpc.AccountModePaper, rpc.AccountModeLive:
		return true
	default:
		return false
	}
}

func sameBrokerScope(a, b brokerStateScope) bool {
	return strings.EqualFold(strings.TrimSpace(a.Account), strings.TrimSpace(b.Account)) &&
		strings.EqualFold(strings.TrimSpace(a.Mode), strings.TrimSpace(b.Mode))
}

func brokerScopedModeMatches(rowMode, scopeMode string) bool {
	scopeMode = strings.TrimSpace(scopeMode)
	if scopeMode == "" || scopeMode == rpc.AccountModeUnknown {
		return true
	}
	return strings.EqualFold(strings.TrimSpace(rowMode), scopeMode)
}

func brokerScopeIsUnfiltered(scope brokerStateScope) bool {
	return strings.TrimSpace(scope.Account) == "" && strings.TrimSpace(scope.Mode) == ""
}

func brokerScopedAccountMatches(rowAccount string, scope brokerStateScope) bool {
	if brokerScopeIsUnfiltered(scope) {
		return true
	}
	if !brokerScopeAccountConcrete(scope.Account) {
		return false
	}
	return strings.EqualFold(strings.TrimSpace(rowAccount), scope.Account)
}

func orderViewMatchesBrokerScope(view rpc.OrderView, scope brokerStateScope) bool {
	return brokerScopedAccountMatches(view.Account, scope) &&
		brokerScopedModeMatches(view.Mode, scope.Mode)
}

func defaultTradingStatePath(filename string) (string, error) {
	if v := os.Getenv("XDG_STATE_HOME"); v != "" {
		return filepath.Join(v, "ibkr", filename), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve user home: %w", err)
	}
	return filepath.Join(home, ".local", "state", "ibkr", filename), nil
}

// defaultDaemonDatabasePath delegates to dial so the CLI, which sizes this
// file to budget how long it waits for the daemon to publish its socket,
// cannot drift from where the daemon actually opens it.
func defaultDaemonDatabasePath() (string, error) {
	return dial.DefaultAuthorityPath()
}

func ensurePrivateStateDir(path string) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("mkdir %s: %w", dir, err)
	}
	return nil
}

func writePrivateStateAtomic(path string, data []byte) error {
	if err := ensurePrivateStateDir(path); err != nil {
		return err
	}
	base := filepath.Base(path)
	tmp, err := os.CreateTemp(filepath.Dir(path), base+".tmp.*")
	if err != nil {
		return fmt.Errorf("create temp: %w", err)
	}
	tmpPath := tmp.Name()
	defer func() {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
	}()
	if _, err := tmp.Write(data); err != nil {
		return fmt.Errorf("write temp: %w", err)
	}
	if err := tmp.Chmod(0o600); err != nil {
		return fmt.Errorf("chmod temp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("rename %s: %w", path, err)
	}
	return nil
}

// ErrPersistenceInUse means another process owns the daemon state database.
// It is deliberately distinct from ErrAlreadyRunning: two daemons using
// different socket paths must still fail visibly when they resolve to the same
// daemon.db.
var ErrPersistenceInUse = errors.New("another Canary daemon owns the daemon state database")

// persistenceLock serializes every daemon that resolves to one authoritative
// state database. Unlike the socket-specific instance pidfile, this lock file
// is never removed: unlinking a flock file after unlock permits an inode race
// in which two later processes can each lock a different file at the same
// pathname.
type persistenceLock struct {
	path string
	f    *os.File
}

func acquirePersistenceLock(databasePath string) (*persistenceLock, error) {
	if databasePath == "" {
		return nil, errors.New("daemon database path is required")
	}
	path := databasePath + ".lock"
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("mkdir persistence lock dir: %w", err)
	}
	if info, err := os.Lstat(path); err == nil && info.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("persistence lock must not be a symbolic link: %s", path)
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("inspect persistence lock: %w", err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open persistence lock: %w", err)
	}
	if err := f.Chmod(0o600); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("secure persistence lock: %w", err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, ErrPersistenceInUse
		}
		return nil, fmt.Errorf("flock persistence lock: %w", err)
	}
	if err := f.Truncate(0); err != nil {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
		return nil, fmt.Errorf("truncate persistence lock: %w", err)
	}
	if _, err := f.WriteString(strconv.Itoa(os.Getpid()) + "\n"); err != nil {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
		return nil, fmt.Errorf("write persistence lock pid: %w", err)
	}
	if err := f.Sync(); err != nil {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
		return nil, fmt.Errorf("sync persistence lock: %w", err)
	}
	return &persistenceLock{path: path, f: f}, nil
}

// Release unlocks and closes the lock. The stable lock pathname remains.
func (l *persistenceLock) Release() {
	if l == nil || l.f == nil {
		return
	}
	_ = syscall.Flock(int(l.f.Fd()), syscall.LOCK_UN)
	_ = l.f.Close()
	l.f = nil
}

// ErrTradingDisabled is returned when the local order-entry gate is closed or
// an order-write handler is intentionally unavailable. The dispatcher returns
// this as CodeTradingDisabled rather than unknown_method so clients get a
// clear safety refusal instead of a method-typo guess.
var ErrTradingDisabled = errors.New("trading disabled")

// ErrAlreadyRunning means another Canary daemon holds the instance lock for this
// socket path. Callers (cmd/canaryd) treat this as an expected, non-fatal
// condition: a duplicate start, exit cleanly.
var ErrAlreadyRunning = errors.New("another Canary daemon holds the instance lock")

// instanceLock is a flock-backed pidfile. Lifetime is bound to the daemon
// process; on Stop() we release the flock and remove the pidfile.
type instanceLock struct {
	path string
	f    *os.File
}

// acquireInstanceLock takes a non-blocking exclusive flock on
// <socketDir>/ibkr.lock and writes the current PID. Returns
// ErrAlreadyRunning if the lock is contended.
func acquireInstanceLock(socketPath string) (*instanceLock, error) {
	path := dial.LockPath(socketPath)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("mkdir lock dir: %w", err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open lock file: %w", err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, ErrAlreadyRunning
		}
		return nil, fmt.Errorf("flock: %w", err)
	}
	if err := f.Truncate(0); err != nil {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
		return nil, fmt.Errorf("truncate lock file: %w", err)
	}
	if _, err := f.WriteString(strconv.Itoa(os.Getpid()) + "\n"); err != nil {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
		return nil, fmt.Errorf("write pid: %w", err)
	}
	return &instanceLock{path: path, f: f}, nil
}

// Release unlocks the flock, closes the file, and removes the pidfile.
// Safe to call multiple times.
func (l *instanceLock) Release() {
	if l == nil || l.f == nil {
		return
	}
	_ = syscall.Flock(int(l.f.Fd()), syscall.LOCK_UN)
	_ = l.f.Close()
	_ = os.Remove(l.path)
	l.f = nil
}
