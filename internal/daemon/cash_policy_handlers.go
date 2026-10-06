package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/BurntSushi/toml"

	"github.com/osauer/canary/v2/internal/daemon/corestore"
	"github.com/osauer/canary/v2/internal/rpc"
)

const (
	// cashPolicyReceiptType names a save's receipt in the event table.
	cashPolicyReceiptType = "cash_policy_saved"
	// cashPolicyRequestLimit bounds a check or apply request: the changes
	// are a few dozen keys, the envelope one signed confirmation.
	cashPolicyRequestLimit = 256 << 10
	// cashPolicyBookTTL is how long the facts' live inputs are reused: a
	// check follows each pause in typing, and the account read is a broker
	// request.
	cashPolicyBookTTL = 10 * time.Second
)

// cashPolicyRead is one read of the protection policy file: its bytes and
// revision, its state against the policy Canary runs, the parsed file when it
// parses, and the keys it writes (legacy places count at the new ones).
type cashPolicyRead struct {
	path     string
	data     []byte
	revision string
	state    string
	message  string
	legacy   bool
	file     protectionPolicy
	defined  map[string]bool
	inForce  protectionPolicy
}

// writable reports whether a save may write this file: Canary runs it as
// written, or adopts it at its next reread.
func (r cashPolicyRead) writable() bool {
	return r.state == rpc.CashPolicyFileOK || r.state == rpc.CashPolicyFileAhead
}

// readCashPolicy reads the file the protection policy manager reads and
// states it against the policy in force.
func (s *Server) readCashPolicy() (cashPolicyRead, error) {
	m := s.protectionPolicies
	if m == nil || strings.TrimSpace(m.path) == "" {
		return cashPolicyRead{}, errBadRequest("no protection policy file is configured")
	}
	active, _ := m.Active()
	r := cashPolicyRead{path: m.path, inForce: active, revision: "none"}
	data, err := os.ReadFile(m.path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		r.state, r.message = rpc.CashPolicyFileMissing, "there is no protection policy file yet; Canary writes it at its next start"
		return r, nil
	case err != nil:
		r.state, r.message = rpc.CashPolicyFileRefused, err.Error()
		return r, nil
	}
	r.data, r.revision = data, cashPolicyDigest(data)
	p, _, err := parseProtectionPolicy(data)
	if err != nil {
		r.state, r.message = rpc.CashPolicyFileRefused, err.Error()
		return r, nil
	}
	var raw map[string]any
	md, _ := toml.Decode(string(data), &raw)
	r.file, r.defined, r.legacy = p, definedTOMLKeys(data), md.IsDefined("buckets", "cash_sweep")
	switch {
	case p.PolicyVersion == active.PolicyVersion && sameEffectivePolicy(effectiveProtectionPolicy(p), effectiveProtectionPolicy(active)):
		r.state = rpc.CashPolicyFileOK
	case p.PolicyVersion > active.PolicyVersion || !m.adoptedFromFile():
		r.state = rpc.CashPolicyFileAhead
		r.message = fmt.Sprintf("the file holds version %d; Canary runs version %d and adopts the file at its next reread, within 30 seconds", p.PolicyVersion, active.PolicyVersion)
	default:
		r.state = rpc.CashPolicyFileDrift
		r.message = fmt.Sprintf("the file was edited without raising policy_version, so Canary keeps version %d in force and has not adopted the edits", active.PolicyVersion)
	}
	return r, nil
}

// cashPolicyBookCache keeps the facts' live inputs for cashPolicyBookTTL.
type cashPolicyBookCache struct {
	mu   sync.Mutex
	at   time.Time
	book cashPolicyBook
}

// cashPolicyBook gathers the facts' live inputs: the account read, the order
// cap in force, the interest rates from the broker's statements and, while
// leveling is on, the planned repayments from the latest proposals.
func (s *Server) cashPolicyBook(ctx context.Context) cashPolicyBook {
	if s.cashPolicyBookForTest != nil {
		return s.cashPolicyBookForTest()
	}
	now := s.nowUTC()
	c := &s.cashPolicyCache
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.at.IsZero() && now.Sub(c.at) >= 0 && now.Sub(c.at) < cashPolicyBookTTL {
		return c.book
	}
	b := cashPolicyBook{at: now, cash: map[string]float64{}, fx: map[string]float64{}}
	acct, err := s.handleAccountSummary(ctx)
	if err != nil || acct == nil {
		b.ledgerReason = "the account could not be read"
	} else {
		base, ledger, reason := cashSweepLedgerAt(acct, now)
		b.base, b.ledgerReason, b.check = base, reason, PolicyCheckBookFrom(acct, nil)
		for ccy, row := range ledger {
			if row.Observed {
				b.cash[ccy] = row.TradeDate
			}
			if positiveFinite(row.ExchangeRate) {
				b.fx[ccy] = row.ExchangeRate
			}
		}
		if base != "" {
			b.fx[base] = 1
		}
		if positiveFinite(acct.NetLiquidation) {
			b.nlv = acct.NetLiquidation
		}
	}
	if limits := s.orderLimitsInForce(b.base); limits.Complete {
		b.orderCap = limits.CapBase
		if b.base == "" {
			b.base = limits.BaseCurrency
		}
	} else {
		b.orderCapReason = nonEmptyString(limits.Summary, "[order_limits] is incomplete")
	}
	if e := s.tradeProposals; e != nil {
		toBase := func(ccy string) (float64, bool) { r := b.fx[ccy]; return r, r > 0 }
		b.rates, b.ratesThrough, b.ratesReason = e.currencyLevelingRates(ctx, s.currentBrokerStateScope(), toBase, now)
		b.leveling = e.Snapshot(false).CurrencyLeveling
	} else {
		b.ratesReason = "the broker's statements are not read in this process"
	}
	c.at, c.book = now, b
	return b
}

// cashPolicyFileSet is the policy files a check reads beside the protection
// file: the configured ones. Without a configuration it names the protection
// file alone and never falls back to the default paths.
func (s *Server) cashPolicyFileSet(path string) PolicyFileSet {
	if s.cfg == nil {
		return PolicyFileSet{Protection: path}
	}
	set := PolicyFileSetFor(s.cfg)
	set.Protection = path
	return set
}

// cashPolicyFindingsFor runs `canary policy check` over the protection file
// as data (the file, or a draft) and keeps its cash findings.
func (s *Server) cashPolicyFindingsFor(path string, data []byte, b cashPolicyBook) []rpc.CashPolicyFinding {
	if data == nil {
		return []rpc.CashPolicyFinding{}
	}
	in := PolicyCheckInput{Now: b.at, Files: s.cashPolicyFileSet(path), ProtectionData: data, Book: b.check}
	if s.cfg != nil {
		in.Trading = s.effectiveTradingConfig()
	}
	in.OrderCapOverride = s.orderLimitsFloorOverride(s.nowUTC())
	if in.Book == nil {
		in.BookSkipped = "the account could not be read"
	}
	return cashPolicyFindings(CheckPolicy(in))
}

// cashPolicySnapshot assembles policy.cash.get's answer from one read.
func (s *Server) cashPolicySnapshot(ctx context.Context, r cashPolicyRead) rpc.CashPolicySnapshot {
	b := s.cashPolicyBook(ctx)
	view, defined := r.inForce, cashPolicyDefinedIn(r.inForce)
	if r.writable() {
		view, defined = r.file, func(key string) bool { return r.defined[key] }
	}
	facts, sections := cashPolicyFacts(view, b)
	sections.Leveling.Present, sections.Sweep.Present = view.Cash.Leveling != nil, view.Cash.Sweep != nil
	currencies := b.currencies(view)
	out := rpc.CashPolicySnapshot{Revision: r.revision, Path: r.path, FileState: r.state, Message: r.message, LegacyLayout: r.legacy,
		PolicyVersion: r.file.PolicyVersion, InForceVersion: r.inForce.PolicyVersion, Writable: r.writable(), AsOf: b.at, BaseCurrency: b.base,
		Sections: sections, Settings: cashPolicySettingsFor(view, defined, currencies, facts), Currencies: cashPolicyCurrencyRows(view, b, currencies),
		RatesThrough: b.ratesThrough, Findings: []rpc.CashPolicyFinding{}}
	window := r.inForce.Cash.confirmationWindow()
	out.ConfirmationWindowSeconds, out.Confirmation = int64(window/time.Second), cashPolicyConfirmationSentence(window)
	if r.writable() {
		out.Findings = s.cashPolicyFindingsFor(r.path, r.data, b)
		if !s.cashPolicyReceiptsReady() {
			out.Writable = false
			out.Message = "Canary's state store is unavailable, so a save could not be recorded"
		}
	}
	return out
}

func (s *Server) cashPolicyReceiptsReady() bool {
	return s.coreStore != nil && s.coreStore.Health().Ready
}

func (s *Server) handleCashPolicyGet(ctx context.Context, _ *rpc.Request) (*rpc.CashPolicySnapshot, error) {
	r, err := s.readCashPolicy()
	if err != nil {
		return nil, err
	}
	out := s.cashPolicySnapshot(ctx, r)
	return &out, nil
}

// decodeCashPolicyRequest reads a check or apply request strictly.
func decodeCashPolicyRequest(raw json.RawMessage, dst any) error {
	if len(raw) > cashPolicyRequestLimit {
		return errBadRequest("cash policy request too large")
	}
	if err := decodeStrictPlatformSettingsJSON(raw, dst); err != nil {
		return errBadRequest("invalid cash policy request")
	}
	return nil
}

func (s *Server) handleCashPolicyCheck(ctx context.Context, req *rpc.Request) (*rpc.CashPolicyCheckResult, error) {
	var in rpc.CashPolicyCheckRequest
	if err := decodeCashPolicyRequest(req.Params, &in); err != nil {
		return nil, err
	}
	r, err := s.readCashPolicy()
	if err != nil {
		return nil, err
	}
	out := &rpc.CashPolicyCheckResult{Revision: r.revision, Errors: map[string]string{}, Changes: []rpc.CashPolicyChange{}, Consequences: []string{},
		Findings: []rpc.CashPolicyFinding{}, Facts: map[string]string{}, PolicyVersion: r.file.PolicyVersion}
	if in.ExpectedRevision != r.revision {
		out.Conflict = true
		return out, nil
	}
	if !r.writable() {
		return nil, &rpc.Error{Code: rpc.CodePolicyUnwritable, Message: cashPolicyMessage(r.message, "")}
	}
	d := planCashPolicyDraft(r, in.Changes)
	out.Errors = d.errors
	b := s.cashPolicyBook(ctx)
	for _, e := range d.edits {
		out.Changes = append(out.Changes, rpc.CashPolicyChange{Key: e.key, Label: e.spec.label, Unit: e.spec.unit, Currency: e.ccy, From: e.from, FromSource: e.fromSource, To: e.to,
			FromText: e.spec.fromText(e.from, e.fromSource, b.base, e.ccy), ToText: e.spec.valueText(e.to, b.base, e.ccy)})
	}
	facts, _ := cashPolicyFacts(d.policy, b)
	out.Facts = facts
	if len(d.errors) > 0 || len(d.edits) == 0 {
		return out, nil
	}
	out.Consequences = cashPolicyConsequences(r.file, d.policy, d.edits, b)
	out.Findings = s.cashPolicyFindingsFor(r.path, d.data, b)
	out.SavedVersion = r.file.PolicyVersion + 1
	out.Terms, out.Digest = cashPolicyTermsFor(r.revision, d.edits)
	out.BaseCurrency = b.base
	return out, nil
}

// cashPolicyConfirmationSentence says what [cash] confirmation_window in
// force means for a save from Desk; the file decides it.
func cashPolicyConfirmationSentence(window time.Duration) string {
	if window <= 0 {
		return "Every save asks your passkey or companion. You can allow a few minutes without asking in Canary's protection policy file (confirmation_window in [cash])."
	}
	d := cashPolicyDuration(window)
	return "For " + d + " after your passkey or companion confirms a save, further saves from the same Desk console that let no more reach the broker need no new confirmation. You can change the " + d + " in Canary's protection policy file (confirmation_window in [cash])."
}

// cashPolicyReliance is owner question 1 as the owner answered it
// (2026-10-06 15:31 CEST: "Cache the decision for 5 or 10 minutes, if not
// serious concerns"). Every save carries a confirmation reference. One that
// relies on an earlier save, rather than on the owner's device now, is
// accepted only when the save lets no more reach the broker (any consequence
// is the serious concern and needs the device), when Canary recorded that
// earlier save confirmed freshly by the same credential, and inside the
// window Canary works out itself: the earlier receipt's time plus [cash]
// confirmation_window in force now. Desk binds the reliance to its console
// session. It returns the earlier save's receipt and the window's end, nil
// for a fresh confirmation, or Canary's refusal.
func (s *Server) cashPolicyReliance(ctx context.Context, c *rpc.CashPolicyConfirmation, consequences int, window time.Duration, now time.Time) (*cashPolicyReceipt, time.Time, error) {
	if c.ConfirmedBy == "" {
		return nil, time.Time{}, nil
	}
	refuse := func(why string) error {
		return &rpc.Error{Code: rpc.CodeConfirmationRequired, Message: why + " Confirm on your device; nothing was written."}
	}
	switch {
	case consequences > 0:
		return nil, time.Time{}, refuse("This save lets more reach the broker, so it needs a fresh confirmation.")
	case window <= 0:
		return nil, time.Time{}, refuse("[cash] confirmation_window is 0s, so every save needs a fresh confirmation.")
	case !cashPriorityRequestID.MatchString(c.ConfirmedBy):
		return nil, time.Time{}, refuse("This save names no save it can rely on.")
	}
	event, found, err := s.coreStore.GetEvent(ctx, daemonStateScope, cashPolicyEventKey(c.ConfirmedBy))
	if err != nil {
		return nil, time.Time{}, fmt.Errorf("cash policy receipt unavailable")
	}
	var earlier cashPolicyReceipt
	if !found || event.Type != cashPolicyReceiptType || decodeStrictPlatformSettingsJSON(event.PayloadJSON, &earlier) != nil || earlier.RequestID != c.ConfirmedBy {
		return nil, time.Time{}, refuse("Canary holds no save this one can rely on.")
	}
	if earlier.ConfirmedBy != "" || earlier.Credential != c.Credential || event.OccurredAt.After(now) {
		return nil, time.Time{}, refuse("The save this one relies on was not confirmed on your device by the same credential.")
	}
	until := event.OccurredAt.Add(window)
	if !now.Before(until) {
		return nil, time.Time{}, refuse("The confirmation this save relies on ended at " + cashPolicyClock(until) + ".")
	}
	return &earlier, until, nil
}

// cashPolicyOriginAllowed reports whether origin may save cash policy: only
// Desk's console, which stamps no human origin and carries the owner's device
// confirmation (empty reads agent). A terminal edits the file itself; a
// paired device, like for the freeze, has no authority over the controls that
// govern later writes; and the daemon's own executors never write policy.
func cashPolicyOriginAllowed(origin string) bool {
	return origin == "" || origin == rpc.OrderOriginAgent
}

// cashPolicyReceipt is one save's receipt: the terms and their digest, each
// changed key's value before, the version written, the revision of the bytes
// written, the backup, and the owner's confirmation as Desk sent it (kept for
// audit only; Canary cannot verify its signature), with the earlier save it
// relied on, when it did. ConfirmedUntil is the end of the window the save
// relied on, or for a fresh confirmation the end of the window it opened at
// the window in force then (later reliances use the window in force at
// their own time).
type cashPolicyReceipt struct {
	Version         int            `json:"version"`
	RequestID       string         `json:"request_id"`
	Terms           string         `json:"terms"`
	Digest          string         `json:"digest"`
	Before          map[string]any `json:"before"`
	FromVersion     int            `json:"from_version"`
	SavedVersion    int            `json:"saved_version"`
	WrittenRevision string         `json:"written_revision"`
	Backup          string         `json:"backup"`
	DeskActionID    string         `json:"desk_action_id"`
	Credential      string         `json:"credential"`
	Envelope        string         `json:"envelope"`
	ConfirmedBy     string         `json:"confirmed_by,omitempty"`
	ConfirmedUntil  time.Time      `json:"confirmed_until,omitzero"`
}

func (s *Server) handleCashPolicyApply(ctx context.Context, req *rpc.Request) (*rpc.CashPolicyApplyResult, error) {
	var in rpc.CashPolicyApplyRequest
	if err := decodeCashPolicyRequest(req.Params, &in); err != nil {
		return nil, err
	}
	if !cashPriorityRequestID.MatchString(in.RequestID) {
		return nil, errBadRequest("an immutable request id is required")
	}
	if !cashPolicyOriginAllowed(in.Origin) {
		return nil, errBadRequest("cash policy saves come only from Desk's console with your device confirmation; a terminal edits the file itself")
	}
	terms, err := decodeCashPolicyTerms(in.Terms)
	if err != nil {
		return nil, err
	}
	if in.Digest != cashPolicyDigest([]byte(in.Terms)) {
		return nil, errBadRequest("the digest does not match the terms")
	}
	// Every save carries a confirmation reference (cashPolicyReliance).
	if c := in.Confirmation; c == nil || strings.TrimSpace(c.DeskActionID) == "" || strings.TrimSpace(c.Credential) == "" || strings.TrimSpace(c.Envelope) == "" {
		return nil, &rpc.Error{Code: rpc.CodeConfirmationRequired, Message: "A save needs your confirmation on your device; nothing was written."}
	}
	m := s.protectionPolicies
	if m == nil || strings.TrimSpace(m.path) == "" {
		return nil, errBadRequest("no protection policy file is configured")
	}
	if !s.cashPolicyReceiptsReady() {
		return nil, &rpc.Error{Code: rpc.CodePolicyUnwritable, Message: "Canary's state store is unavailable, so a save could not be recorded; nothing was written."}
	}
	receipt, replay, err := s.applyCashPolicy(ctx, m, in, terms)
	if err != nil {
		return nil, err
	}
	r, err := s.readCashPolicy()
	if err != nil {
		return nil, err
	}
	active, _ := m.Active()
	out := &rpc.CashPolicyApplyResult{CashPolicySnapshot: s.cashPolicySnapshot(ctx, r), RequestID: in.RequestID, SavedVersion: receipt.SavedVersion,
		InForce: active.PolicyVersion >= receipt.SavedVersion, Replay: replay, ConfirmedUntil: receipt.ConfirmedUntil}
	return out, nil
}

// applyCashPolicy is the write, under the manager's file lock so no reread
// runs between the read, the write and the reload: it checks the receipt,
// the revision and the loader's validation again, writes the changed lines
// with their provenance after a backup, reloads the manager and records the
// receipt. A retried request with the same terms writes nothing and answers
// with its receipt.
func (s *Server) applyCashPolicy(ctx context.Context, m *protectionPolicyManager, in rpc.CashPolicyApplyRequest, terms cashPolicyTerms) (cashPolicyReceipt, bool, error) {
	m.fileMu.Lock()
	defer m.fileMu.Unlock()
	eventKey := cashPolicyEventKey(in.RequestID)
	event, found, err := s.coreStore.GetEvent(ctx, daemonStateScope, eventKey)
	if err != nil {
		return cashPolicyReceipt{}, false, fmt.Errorf("cash policy receipt unavailable")
	}
	if found {
		var receipt cashPolicyReceipt
		if event.Type != cashPolicyReceiptType || decodeStrictPlatformSettingsJSON(event.PayloadJSON, &receipt) != nil || receipt.Version != 1 || receipt.RequestID != in.RequestID {
			return cashPolicyReceipt{}, false, fmt.Errorf("invalid cash policy receipt")
		}
		if receipt.Digest != in.Digest {
			return cashPolicyReceipt{}, false, &rpc.Error{Code: rpc.CodeRequestReused, Message: "This request id already saved other changes; nothing was written."}
		}
		return receipt, true, nil
	}
	r, err := s.readCashPolicy()
	if err != nil {
		return cashPolicyReceipt{}, false, err
	}
	if r.revision != terms.ExpectedRevision {
		return cashPolicyReceipt{}, false, &rpc.Error{Code: rpc.CodeSettingsConflict, Message: "The policy file changed since it was read; nothing was written."}
	}
	if !r.writable() {
		return cashPolicyReceipt{}, false, &rpc.Error{Code: rpc.CodePolicyUnwritable, Message: cashPolicyMessage(r.message, "") + " Nothing was written."}
	}
	d := planCashPolicyDraft(r, terms.Changes)
	if len(d.errors) > 0 {
		var parts []string
		for _, key := range slices.Sorted(maps.Keys(d.errors)) {
			parts = append(parts, strings.TrimSpace(key+" "+d.errors[key]))
		}
		return cashPolicyReceipt{}, false, &rpc.Error{Code: rpc.CodePolicyInvalid, Message: strings.Join(parts, "; ")}
	}
	if again, _ := cashPolicyTermsFor(r.revision, d.edits); again != in.Terms {
		return cashPolicyReceipt{}, false, errBadRequest("the terms are not what Canary's check of this file returns")
	}
	now := s.nowUTC()
	window := r.inForce.Cash.confirmationWindow()
	relied, until, err := s.cashPolicyReliance(ctx, in.Confirmation, len(cashPolicyConsequences(r.file, d.policy, d.edits, cashPolicyBook{})), window, now)
	if err != nil {
		return cashPolicyReceipt{}, false, err
	}
	if relied == nil && window > 0 {
		until = now.Add(window)
	}
	note := &cashPolicyNote{at: cashPolicyStamp(now), confirmed: cashPolicyConfirmedBy(in.Confirmation, relied, until)}
	out, err := editCashPolicyFile(r.data, d.edits, r.file.PolicyVersion, note)
	if err != nil {
		return cashPolicyReceipt{}, false, err
	}
	after, _, err := parseProtectionPolicy(out)
	if err != nil {
		return cashPolicyReceipt{}, false, fmt.Errorf("the saved file would not parse; nothing written: %w", err)
	}
	if err := verifyCashPolicyEdit(r.file, after, d.edits); err != nil {
		return cashPolicyReceipt{}, false, fmt.Errorf("the saved file would not say what the changes mean (%v); nothing written", err)
	}
	backup, err := writeCashPolicyBackup(r.path, r.data, now)
	if err != nil {
		return cashPolicyReceipt{}, false, fmt.Errorf("backup before saving: %w; nothing written", err)
	}
	if err := writePrivateFile(r.path, out); err != nil {
		return cashPolicyReceipt{}, false, fmt.Errorf("write %s: %w", r.path, err)
	}
	m.reloadHeld()
	before := map[string]any{}
	for _, e := range d.edits {
		if e.fromSource == rpc.CashPolicySourceFile {
			before[e.key] = e.from
		} else {
			before[e.key] = nil
		}
	}
	receipt := cashPolicyReceipt{Version: 1, RequestID: in.RequestID, Terms: in.Terms, Digest: in.Digest, Before: before, FromVersion: r.file.PolicyVersion,
		SavedVersion: after.PolicyVersion, WrittenRevision: cashPolicyDigest(out), Backup: backup, DeskActionID: in.Confirmation.DeskActionID,
		Credential: in.Confirmation.Credential, Envelope: in.Confirmation.Envelope, ConfirmedBy: in.Confirmation.ConfirmedBy, ConfirmedUntil: until}
	raw, _ := json.Marshal(receipt)
	input := corestore.EventInput{ScopeKey: daemonStateScope, EventKey: eventKey, Type: cashPolicyReceiptType, Action: coreEventActionUpdate,
		Origin: rpc.OrderOriginAgent, OccurredAt: now, PayloadJSON: raw}
	// The file is written: the receipt must not fail on the request's own
	// deadline, or a retry would find the file moved and no receipt.
	receiptCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if _, err := s.coreStore.AppendEvents(receiptCtx, []corestore.EventInput{input}); err != nil {
		return cashPolicyReceipt{}, false, fmt.Errorf("saved as policy_version %d, but its receipt could not be recorded: %v", after.PolicyVersion, err)
	}
	return receipt, false, nil
}

// cashPolicyEventKey names a request's receipt in the event table.
func cashPolicyEventKey(requestID string) string {
	sum := sha256.Sum256([]byte(requestID))
	return "cash-policy:" + hex.EncodeToString(sum[:])
}

// writeCashPolicyBackup copies the file as it was before a save beside it,
// owner-only, never over an earlier backup.
func writeCashPolicyBackup(path string, data []byte, now time.Time) (string, error) {
	stamp := now.UTC().Format("20060102T150405Z")
	for i := range 10 {
		backup := path + ".bak-desk-" + stamp
		if i > 0 {
			backup += fmt.Sprintf("-%d", i+1)
		}
		err := writePrivateFileExclusive(backup, data)
		if err == nil {
			return backup, nil
		}
		if !errors.Is(err, os.ErrExist) {
			return "", err
		}
	}
	return "", errors.New("ten backups already carry this second")
}

// cashPolicyStamp is a save's time in Europe/Berlin, 2026-10-06 14:05 CEST.
func cashPolicyStamp(t time.Time) string {
	loc, err := time.LoadLocation("Europe/Berlin")
	if err != nil {
		return t.UTC().Format("2006-01-02 15:04 UTC")
	}
	return t.In(loc).Format("2006-01-02 15:04 MST")
}

// cashPolicyConfirmedBy names how the owner confirmed a save, with the
// start of Desk's action id, for the provenance comment; a save that relied
// on an earlier confirmation names that save's action and the window's end.
func cashPolicyConfirmedBy(c *rpc.CashPolicyConfirmation, relied *cashPolicyReceipt, until time.Time) string {
	how, earlier := "confirmed on your device", "your device's confirmation"
	switch {
	case strings.HasPrefix(c.Credential, "passkey"):
		how, earlier = "confirmed with your passkey", "your passkey confirmation"
	case strings.HasPrefix(c.Credential, "companion"):
		how, earlier = "confirmed in the companion", "the companion's confirmation"
	}
	if relied != nil {
		return "relying on " + earlier + " of action " + cashPolicyShortID(relied.DeskActionID) + " until " + cashPolicyClock(until) +
			" (action " + cashPolicyShortID(c.DeskActionID) + ")"
	}
	return how + " (action " + cashPolicyShortID(c.DeskActionID) + ")"
}

// cashPolicyShortID is the start of a Desk action id, for a comment.
func cashPolicyShortID(id string) string {
	var out strings.Builder
	for _, r := range id {
		if out.Len() == 8 {
			break
		}
		if r < 128 && (r >= '0' && r <= '9' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r == '-') {
			out.WriteRune(r)
		}
	}
	return out.String()
}

// cashPolicyClock is a time of day in Europe/Berlin, 14:15 CEST.
func cashPolicyClock(t time.Time) string {
	loc, err := time.LoadLocation("Europe/Berlin")
	if err != nil {
		return t.UTC().Format("15:04 UTC")
	}
	return t.In(loc).Format("15:04 MST")
}
