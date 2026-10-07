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
	"github.com/osauer/canary/v2/internal/risk"
	"github.com/osauer/canary/v2/internal/rpc"
)

const (
	// cashPolicyReceiptType names a save's receipt in the event table.
	cashPolicyReceiptType = "cash_policy_saved"
	// cashPolicyRequestLimit bounds a check or apply request: the changes
	// are a few dozen keys, the envelope one signed confirmation with the
	// review it covers.
	cashPolicyRequestLimit = 256 << 10
	// cashPolicyBookTTL is how long the facts' live inputs are reused: a
	// check follows each pause in typing, and the account read is a broker
	// request.
	cashPolicyBookTTL = 10 * time.Second
	// cashPolicyReceiptVersion is the receipt record written since the order
	// caps joined the settings; version 1 receipts are read as before.
	cashPolicyReceiptVersion = 2
	// orderLimitsRevisionEventKind is the governance event journaled for
	// each constitution revision policy.cash.apply writes.
	orderLimitsRevisionEventKind = "order_limits_revision"
)

// cashPolicyRead is one read of the two files these settings edit: the
// protection policy file (its bytes and revision, its state against the
// policy Canary runs, the parsed file when it parses, and the keys it writes,
// legacy places counting at the new ones) and, under con, the risk
// constitution. revision names both files' bytes.
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
	con      cashPolicyConstitutionRead
}

// cashPolicyConstitutionRead is the risk constitution as these settings read
// it. present is false while no constitution manager runs, and the order
// limits are then out of scope. file is nil when the file is missing or
// refused; inForce is the policy the manager runs, nil when none does.
type cashPolicyConstitutionRead struct {
	present  bool
	path     string
	data     []byte
	revision string
	state    string
	message  string
	file     *risk.Constitution
	defined  map[string]bool
	inForce  *risk.Constitution
}

// writable reports whether a save may write the protection file: Canary runs
// it as written, or adopts it at its next reread.
func (r cashPolicyRead) writable() bool {
	return r.state == rpc.CashPolicyFileOK || r.state == rpc.CashPolicyFileAhead
}

// writable reports the same for the constitution.
func (c cashPolicyConstitutionRead) writable() bool {
	return c.present && c.file != nil && (c.state == rpc.CashPolicyFileOK || c.state == rpc.CashPolicyFileAhead)
}

// view is what the settings show: each file when a save may write it, else
// the policy in force.
func (r cashPolicyRead) view() cashPolicyState {
	st := cashPolicyState{p: r.inForce}
	if r.writable() {
		st.p = r.file
	}
	if r.con.present {
		st.c = r.con.inForce
		if r.con.writable() {
			st.c = r.con.file
		}
	}
	return st
}

// definedKey reports whether the viewed file writes a key.
func (r cashPolicyRead) definedKey(key string) bool {
	if strings.HasPrefix(key, risk.OrderLimitsTable+".") {
		if r.con.writable() {
			return r.con.defined[key]
		}
		return cashPolicyDefinedIn(cashPolicyState{c: r.con.inForce})(key)
	}
	if r.writable() {
		return r.defined[key]
	}
	return cashPolicyDefinedIn(cashPolicyState{p: r.inForce})(key)
}

// readCashPolicy reads the file the protection policy manager reads and the
// constitution the risk policy manager reads, and states each against the
// policy in force.
func (s *Server) readCashPolicy() (cashPolicyRead, error) {
	r, err := s.readProtectionFile()
	if err != nil {
		return r, err
	}
	r.con = s.readConstitutionFile()
	if r.con.present {
		r.revision += "+" + r.con.revision
	}
	return r, nil
}

// readProtectionFile reads the protection policy file alone; revision is
// its digest.
func (s *Server) readProtectionFile() (cashPolicyRead, error) {
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
		r.message = fmt.Sprintf("the file holds version %d; Canary runs version %d and adopts the file at its next reread%s", p.PolicyVersion, active.PolicyVersion, m.rereadWithin())
	default:
		r.state = rpc.CashPolicyFileDrift
		r.message = fmt.Sprintf("the file was edited without raising policy_version, so Canary keeps version %d in force and has not adopted the edits", active.PolicyVersion)
	}
	return r, nil
}

// readConstitutionFile reads the risk constitution the way its manager does
// and states it against the policy in force. Without a manager the order
// limits are out of scope.
func (s *Server) readConstitutionFile() cashPolicyConstitutionRead {
	m := s.riskPolicies
	if m == nil || strings.TrimSpace(m.path) == "" {
		return cashPolicyConstitutionRead{}
	}
	snap := m.snapshot()
	c := cashPolicyConstitutionRead{present: true, path: m.path, inForce: snap.policy, revision: "none"}
	data, err := os.ReadFile(m.path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		c.state, c.message = rpc.CashPolicyFileMissing, "there is no risk policy file yet; Canary writes it at its next start"
		return c
	case err != nil:
		c.state, c.message = rpc.CashPolicyFileRefused, err.Error()
		return c
	}
	c.data, c.revision = data, cashPolicyDigest(data)
	parsed, err := parseConstitutionFile(data)
	if err != nil {
		c.state, c.message = rpc.CashPolicyFileRefused, err.Error()
		return c
	}
	c.file, c.defined = parsed, definedTOMLKeys(data)
	within := ", within " + cashPolicyDuration(snap.reloadInterval)
	switch {
	case snap.policy == nil:
		c.state = rpc.CashPolicyFileAhead
		c.message = fmt.Sprintf("the file holds version %d; Canary runs no risk policy yet and adopts the file at its next reread%s", parsed.PolicyVersion, within)
	case parsed.PolicyVersion == snap.policy.PolicyVersion && parsed.EffectiveFingerprintKey() == snap.policy.EffectiveFingerprintKey():
		c.state = rpc.CashPolicyFileOK
	case parsed.PolicyVersion > snap.policy.PolicyVersion:
		c.state = rpc.CashPolicyFileAhead
		c.message = fmt.Sprintf("the file holds version %d; Canary runs version %d and adopts the file at its next reread%s", parsed.PolicyVersion, snap.policy.PolicyVersion, within)
	default:
		c.state = rpc.CashPolicyFileDrift
		c.message = fmt.Sprintf("the file was edited without raising policy_version, so Canary keeps version %d in force and has not adopted the edits", snap.policy.PolicyVersion)
	}
	return c
}

// rereadWithin says how soon the manager rereads the file: within its reload
// interval while hot reload is on. Otherwise only a restart or a save from
// Desk rereads it, and no interval is promised.
func (m *protectionPolicyManager) rereadWithin() string {
	if !m.hotReload {
		return ""
	}
	return ", within " + cashPolicyDuration(m.reloadInterval)
}

// cashPolicyBookCache keeps the facts' live inputs for cashPolicyBookTTL.
type cashPolicyBookCache struct {
	mu   sync.Mutex
	at   time.Time
	book cashPolicyBook
}

// cashPolicyBook gathers the facts' live inputs: the account and positions
// reads (the sweep's cash input, as the planner assembles it), the order cap
// in force, the interest rates from the broker's statements and, while
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
		// Holdings are classified by the sweep policy in force, as the
		// planner does; a failed positions read leaves them out, as there.
		pos, _ := s.handlePositionsList(ctx, &rpc.Request{})
		var bucket *protectionCashSweepPolicy
		if m := s.protectionPolicies; m != nil {
			active, _ := m.Active()
			bucket = active.Cash.Sweep
		}
		cashSweepReadCash(&b.sweep, bucket, acct, pos, now)
		b.base, b.ledgerReason, b.check = b.sweep.BaseCurrency, b.sweep.LedgerReason, PolicyCheckBookFrom(acct, pos)
		for ccy, row := range b.sweep.Ledger {
			if row.Observed {
				b.cash[ccy] = row.TradeDate
			}
			if positiveFinite(row.ExchangeRate) {
				b.fx[ccy] = row.ExchangeRate
			}
		}
		if b.base != "" {
			b.fx[b.base] = 1
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
// file and the constitution the managers read, and never falls back to the
// default paths.
func (s *Server) cashPolicyFileSet(path string) PolicyFileSet {
	set := PolicyFileSet{Protection: path}
	if s.cfg != nil {
		set = PolicyFileSetFor(s.cfg)
		set.Protection = path
	}
	if m := s.riskPolicies; m != nil && strings.TrimSpace(m.path) != "" {
		set.Constitution = m.path
	}
	return set
}

// cashPolicyFindingsFor runs `canary policy check` over the protection file
// and the constitution as data (the files, or a draft) and keeps the
// findings the screen shows.
func (s *Server) cashPolicyFindingsFor(path string, data, conData []byte, b cashPolicyBook) []rpc.CashPolicyFinding {
	if data == nil {
		return []rpc.CashPolicyFinding{}
	}
	in := PolicyCheckInput{Now: b.at, Files: s.cashPolicyFileSet(path), ProtectionData: data, ConstitutionData: conData, Book: b.check}
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
	view := r.view()
	facts, sections := cashPolicyFacts(view.p, b)
	sections.Leveling.Present, sections.Sweep.Present = view.p.Cash.Leveling != nil, view.p.Cash.Sweep != nil
	currencies := b.currencies(view.p)
	out := rpc.CashPolicySnapshot{Revision: r.revision, Path: r.path, FileState: r.state, Message: r.message, LegacyLayout: r.legacy,
		PolicyVersion: r.file.PolicyVersion, InForceVersion: r.inForce.PolicyVersion, Writable: r.writable(), AsOf: b.at, BaseCurrency: b.base,
		Sections: sections, Settings: cashPolicySettingsFor(view, r.definedKey, currencies, facts), Currencies: cashPolicyCurrencyRows(view.p, b, currencies),
		RatesThrough: b.ratesThrough, Findings: []rpc.CashPolicyFinding{}}
	window := r.inForce.Cash.confirmationWindow()
	out.ConfirmationWindowSeconds, out.Confirmation = int64(window/time.Second), cashPolicyConfirmationSentence(window)
	if r.writable() {
		out.Findings = s.cashPolicyFindingsFor(r.path, r.data, r.con.data, b)
		if !s.cashPolicyReceiptsReady() {
			out.Writable = false
			out.Message = "Canary's state store is unavailable, so a save could not be recorded"
		}
	}
	if r.con.present {
		con := &rpc.CashPolicyFile{Path: r.con.path, FileState: r.con.state, Message: r.con.message, Writable: r.con.writable() && s.cashPolicyReceiptsReady()}
		if r.con.file != nil {
			con.PolicyVersion = r.con.file.PolicyVersion
		}
		if r.con.inForce != nil {
			con.InForceVersion = r.con.inForce.PolicyVersion
		}
		out.Constitution = con
		limits := s.orderLimitsInForce(b.base)
		out.Sections.OrderLimits = rpc.CashPolicySection{Present: limits.Complete, Fact: "Order cap in force: " + limits.Summary + "."}
		if !limits.Complete {
			out.Sections.OrderLimits.Fact = sentence(limits.Summary) + "."
		}
		keys, message := cashPolicyDeviceKeys(r.con.inForce)
		device := &rpc.CashPolicyDevice{Verifiable: []string{}, Message: message}
		for _, k := range keys {
			device.Verifiable = append(device.Verifiable, k.Credential())
		}
		out.Device = device
	}
	preset := s.cashPolicyPresetFor(r)
	out.Preset = &preset
	out.Presets = cashPolicyPresetOptions(view, b)
	out.Restore = s.cashPolicyRestore(ctx, r, preset)
	return out
}

// cashPolicyPresetFor derives the stance of one read: from each file when a
// save may write it, else from the policy in force, with a note naming the
// file in another state.
func (s *Server) cashPolicyPresetFor(r cashPolicyRead) rpc.CashPolicyPreset {
	var notes []string
	if !r.writable() {
		notes = append(notes, "the cash settings are compared from the policy in force ("+nonEmptyString(r.message, r.state)+")")
	}
	if r.con.present && !r.con.writable() {
		notes = append(notes, "the order caps are compared from the policy in force ("+nonEmptyString(r.con.message, r.con.state)+")")
	}
	return cashPolicyStance(cashPolicyPresetTables(), cashPolicyCoveredValues(r.view()), notes...)
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
	caps := d.capEdits()
	if len(caps) > 0 && !r.con.writable() {
		return nil, &rpc.Error{Code: rpc.CodePolicyUnwritable, Message: "Order caps: " + cashPolicyMessage(nonEmptyString(r.con.message, "the risk policy file cannot be written from here"), "")}
	}
	b := s.cashPolicyBook(ctx)
	for _, e := range d.edits {
		out.Changes = append(out.Changes, rpc.CashPolicyChange{Key: e.key, Label: e.spec.label, Unit: e.spec.unit, Currency: e.ccy, From: e.from, FromSource: e.fromSource, To: e.to,
			FromText: e.spec.fromText(e.from, e.fromSource, b.base, e.ccy), ToText: e.spec.valueText(e.to, b.base, e.ccy)})
	}
	facts, _ := cashPolicyFacts(d.policy, b)
	out.Facts = facts
	before := s.cashPolicyPresetFor(r)
	out.PresetFrom = before.ID
	if len(d.errors) > 0 || len(d.edits) == 0 {
		out.PresetTo = before.ID
		return out, nil
	}
	out.PresetTo = cashPolicyStance(cashPolicyPresetTables(), cashPolicyCoveredValues(d.state())).ID
	out.Consequences = cashPolicyConsequences(r.view(), d.state(), d.edits, b)
	out.Findings = s.cashPolicyFindingsFor(r.path, d.data, d.conData, b)
	out.SavedVersion = r.file.PolicyVersion + 1
	if len(caps) > 0 {
		out.ConstitutionPolicyVersion, out.ConstitutionSavedVersion = r.con.file.PolicyVersion, r.con.file.PolicyVersion+1
	}
	out.DeviceRequired = len(out.Consequences) > 0 || len(caps) > 0
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
	return "For " + d + " after your passkey or companion confirms a save, further saves from the same Desk console that let no more reach the broker need no new confirmation. You can change the " + d + " in Canary's protection policy file (confirmation_window in [cash]). A change to an order cap always asks your device."
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
// changed key's value before (both files' keys), the versions written, the
// revision of the bytes written (both files), the backup, and the owner's
// confirmation as Desk sent it, with the credential Canary verified itself
// when it could (Verified; empty for a confirmation kept for audit only),
// with the earlier save it relied on, when it did. ConfirmedUntil is the end
// of the window the save relied on, or for a fresh confirmation the end of
// the window it opened at the window in force then (later reliances use the
// window in force at their own time). PresetBefore and PresetAfter are the
// stance by value before and after the save; Constitution the constitution
// half of a save that changed an order cap; Partial a save that stopped after
// its first file.
type cashPolicyReceipt struct {
	Version         int                            `json:"version"`
	RequestID       string                         `json:"request_id"`
	Terms           string                         `json:"terms"`
	Digest          string                         `json:"digest"`
	Before          map[string]any                 `json:"before"`
	FromVersion     int                            `json:"from_version"`
	SavedVersion    int                            `json:"saved_version"`
	WrittenRevision string                         `json:"written_revision"`
	Backup          string                         `json:"backup"`
	DeskActionID    string                         `json:"desk_action_id"`
	Credential      string                         `json:"credential"`
	Envelope        string                         `json:"envelope"`
	ConfirmedBy     string                         `json:"confirmed_by,omitempty"`
	ConfirmedUntil  time.Time                      `json:"confirmed_until,omitzero"`
	PresetBefore    string                         `json:"preset_before,omitempty"`
	PresetAfter     string                         `json:"preset_after,omitempty"`
	Verified        string                         `json:"verified,omitempty"`
	Constitution    *cashPolicyConstitutionReceipt `json:"constitution,omitempty"`
	Partial         *rpc.CashPolicyPartial         `json:"partial,omitempty"`
}

// cashPolicyConstitutionReceipt is the constitution half of a receipt.
// GovernanceEvent says whether the revision's governance event was recorded
// ("recorded") or why not: the file is written and in force by then, and
// nothing can be rolled back (old bytes would read as drift), so the receipt,
// which the restore and the retry read, carries the fact beside the log line.
type cashPolicyConstitutionReceipt struct {
	FromVersion     int    `json:"from_version"`
	SavedVersion    int    `json:"saved_version"`
	WrittenRevision string `json:"written_revision"`
	Backup          string `json:"backup"`
	GovernanceEvent string `json:"governance_event"`
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
		InForce: receipt.SavedVersion == 0 || active.PolicyVersion >= receipt.SavedVersion, Replay: replay, ConfirmedUntil: receipt.ConfirmedUntil,
		Verified: receipt.Verified, Partial: receipt.Partial}
	if receipt.Constitution != nil {
		out.ConstitutionSavedVersion = receipt.Constitution.SavedVersion
		if snap := s.riskPolicies.snapshot(); snap.policy == nil || snap.policy.PolicyVersion < receipt.Constitution.SavedVersion {
			out.InForce = false
		}
	}
	if receipt.Partial != nil {
		out.InForce = false
	}
	return out, nil
}

// applyCashPolicy is the write, under both managers' file locks (the
// constitution's first, then the protection file's) so no reread runs
// between the read, the writes and the reloads: it checks the receipt, the
// revision and the loaders' validation again, writes the constitution first
// (an order cap change, with the owner's device confirmation verified by
// Canary itself and a governance event), then the protection file, each with
// its changed lines and their provenance after a backup, reloads each
// manager and records the receipt. A failure after the constitution was
// written is a partial save, reported as such and never rolled back (old
// bytes would read as drift). A retried request with the same terms writes
// nothing and answers with its receipt.
func (s *Server) applyCashPolicy(ctx context.Context, m *protectionPolicyManager, in rpc.CashPolicyApplyRequest, terms cashPolicyTerms) (cashPolicyReceipt, bool, error) {
	if rm := s.riskPolicies; rm != nil {
		rm.fileMu.Lock()
		defer rm.fileMu.Unlock()
	}
	m.fileMu.Lock()
	defer m.fileMu.Unlock()
	eventKey := cashPolicyEventKey(in.RequestID)
	event, found, err := s.coreStore.GetEvent(ctx, daemonStateScope, eventKey)
	if err != nil {
		return cashPolicyReceipt{}, false, fmt.Errorf("cash policy receipt unavailable")
	}
	if found {
		var receipt cashPolicyReceipt
		if event.Type != cashPolicyReceiptType || decodeStrictPlatformSettingsJSON(event.PayloadJSON, &receipt) != nil || receipt.Version < 1 || receipt.Version > cashPolicyReceiptVersion || receipt.RequestID != in.RequestID {
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
	caps, cash := d.capEdits(), d.cashEdits()
	if len(caps) > 0 && !r.con.writable() {
		return cashPolicyReceipt{}, false, &rpc.Error{Code: rpc.CodePolicyUnwritable, Message: "Order caps: " + cashPolicyMessage(nonEmptyString(r.con.message, "the risk policy file cannot be written from here"), "") + " Nothing was written."}
	}
	now := s.nowUTC()
	window := r.inForce.Cash.confirmationWindow()
	consequences := len(cashPolicyConsequences(r.view(), d.state(), d.edits, cashPolicyBook{}))
	relied, until, verified, err := s.cashPolicyConfirm(ctx, r, in, len(caps) > 0, consequences, window, now)
	if err != nil {
		return cashPolicyReceipt{}, false, err
	}
	if relied == nil && window > 0 {
		until = now.Add(window)
	}
	presetBefore := s.cashPolicyPresetFor(r).ID
	presetAfter := cashPolicyStance(cashPolicyPresetTables(), cashPolicyCoveredValues(d.state())).ID
	note := &cashPolicyNote{at: cashPolicyStamp(now), confirmed: cashPolicyConfirmedBy(in.Confirmation, relied, until), preset: cashPolicyPresetProvenance(presetAfter)}
	before := map[string]any{}
	for _, e := range d.edits {
		if e.fromSource == rpc.CashPolicySourceFile {
			before[e.key] = e.from
		} else {
			before[e.key] = nil
		}
	}
	receipt := cashPolicyReceipt{Version: cashPolicyReceiptVersion, RequestID: in.RequestID, Terms: in.Terms, Digest: in.Digest, Before: before, FromVersion: r.file.PolicyVersion,
		SavedVersion: r.file.PolicyVersion, DeskActionID: in.Confirmation.DeskActionID, Credential: in.Confirmation.Credential, Envelope: in.Confirmation.Envelope,
		ConfirmedBy: in.Confirmation.ConfirmedBy, ConfirmedUntil: until, PresetBefore: presetBefore, PresetAfter: presetAfter, Verified: verified}
	protectionData, conData := r.data, r.con.data

	// Both files are prepared before either is written, so a draft either
	// file refuses writes nothing.
	var conOut, cashOut []byte
	var conAfter *risk.Constitution
	if len(caps) > 0 {
		conOut, err = editConstitutionFile(r.con.data, caps, r.con.file.PolicyVersion, note)
		if err != nil {
			return cashPolicyReceipt{}, false, err
		}
		conAfter, err = parseConstitutionFile(conOut)
		if err != nil {
			return cashPolicyReceipt{}, false, fmt.Errorf("the saved risk policy file would not parse; nothing written: %w", err)
		}
		if err := verifyConstitutionEdit(r.con.file, conAfter, caps); err != nil {
			return cashPolicyReceipt{}, false, fmt.Errorf("the saved risk policy file would not say what the changes mean (%v); nothing written", err)
		}
	}
	var cashAfter protectionPolicy
	if len(cash) > 0 {
		cashOut, err = editCashPolicyFile(r.data, cash, r.file.PolicyVersion, note)
		if err != nil {
			return cashPolicyReceipt{}, false, err
		}
		cashAfter, _, err = parseProtectionPolicy(cashOut)
		if err != nil {
			return cashPolicyReceipt{}, false, fmt.Errorf("the saved file would not parse; nothing written: %w", err)
		}
		if err := verifyCashPolicyEdit(r.file, cashAfter, cash); err != nil {
			return cashPolicyReceipt{}, false, fmt.Errorf("the saved file would not say what the changes mean (%v); nothing written", err)
		}
	}

	// The constitution first, in both directions (design §7.2).
	if len(caps) > 0 {
		backup, err := writeCashPolicyBackup(r.con.path, r.con.data, now)
		if err != nil {
			return cashPolicyReceipt{}, false, fmt.Errorf("backup before saving the risk policy file: %w; nothing written", err)
		}
		if err := writePrivateFile(r.con.path, conOut); err != nil {
			return cashPolicyReceipt{}, false, fmt.Errorf("write %s: %w; nothing written", r.con.path, err)
		}
		s.riskPolicies.reloadHeld()
		conData = conOut
		receipt.Constitution = &cashPolicyConstitutionReceipt{FromVersion: r.con.file.PolicyVersion, SavedVersion: conAfter.PolicyVersion, WrittenRevision: cashPolicyDigest(conOut), Backup: backup}
		receipt.Constitution.GovernanceEvent = s.journalOrderLimitsRevision(ctx, now, in, r.con.file, conAfter, caps, receipt)
	}
	if len(cash) > 0 {
		err = s.writeCashPolicyHalf(m, r, cashOut, now, &receipt)
		switch {
		case err == nil:
			protectionData = cashOut
			receipt.SavedVersion = cashAfter.PolicyVersion
		case receipt.Constitution != nil:
			// The constitution is written and in force; the cash half is not.
			// The stance now reads by value, and the receipt and the result
			// say which half saved (design §7.2).
			receipt.Partial = &rpc.CashPolicyPartial{Saved: rpc.CashPolicySectionOrderLimits, NotSaved: "cash", Reason: cashPolicyMessage(err.Error(), "")}
			receipt.PresetAfter = cashPolicyStance(cashPolicyPresetTables(), cashPolicyCoveredValues(cashPolicyState{p: r.file, c: conAfter})).ID
		default:
			return cashPolicyReceipt{}, false, err
		}
	}
	receipt.WrittenRevision = cashPolicyDigest(protectionData)
	if r.con.present {
		receipt.WrittenRevision += "+" + cashPolicyDigest(conData)
	}
	raw, _ := json.Marshal(receipt)
	input := corestore.EventInput{ScopeKey: daemonStateScope, EventKey: eventKey, Type: cashPolicyReceiptType, Action: coreEventActionUpdate,
		Origin: rpc.OrderOriginAgent, OccurredAt: now, PayloadJSON: raw}
	// The files are written: the receipt must not fail on the request's own
	// deadline, or a retry would find the files moved and no receipt.
	receiptCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if _, err := s.coreStore.AppendEvents(receiptCtx, []corestore.EventInput{input}); err != nil {
		return cashPolicyReceipt{}, false, fmt.Errorf("saved as policy_version %d, but its receipt could not be recorded: %v", receipt.SavedVersion, err)
	}
	return receipt, false, nil
}

// writeCashPolicyHalf writes the protection file: a backup, the bytes, the
// reload. It fills the receipt's backup.
func (s *Server) writeCashPolicyHalf(m *protectionPolicyManager, r cashPolicyRead, out []byte, now time.Time, receipt *cashPolicyReceipt) error {
	if s.cashPolicyWriteFault != nil {
		if err := s.cashPolicyWriteFault(r.path); err != nil {
			return err
		}
	}
	backup, err := writeCashPolicyBackup(r.path, r.data, now)
	if err != nil {
		return fmt.Errorf("backup before saving: %w", err)
	}
	if err := writePrivateFile(r.path, out); err != nil {
		return fmt.Errorf("write %s: %w", r.path, err)
	}
	m.reloadHeld()
	receipt.Backup = backup
	return nil
}

// cashPolicyConfirm decides how a save is confirmed. A save that changes an
// order cap takes only a fresh confirmation Canary verifies itself against
// the constitution's pinned keys, in both directions and with no window
// (owner decision 2026-10-07 08:33 CEST). Any other save may rely on an
// earlier confirmation (cashPolicyReliance); a fresh one is verified when
// the envelope carries a signature and a key is pinned, and kept for audit
// only otherwise, as before. It returns the relied-on receipt and the
// window's end, and the credential verified.
func (s *Server) cashPolicyConfirm(ctx context.Context, r cashPolicyRead, in rpc.CashPolicyApplyRequest, capChange bool, consequences int, window time.Duration, now time.Time) (*cashPolicyReceipt, time.Time, string, error) {
	c := in.Confirmation
	keys, message := cashPolicyDeviceKeys(r.con.inForce)
	if capChange {
		if c.ConfirmedBy != "" {
			return nil, time.Time{}, "", &rpc.Error{Code: rpc.CodeConfirmationRequired, Message: "Changing an order cap needs your device every time; a save cannot rely on an earlier confirmation. Nothing was written."}
		}
		if len(keys) == 0 {
			return nil, time.Time{}, "", &rpc.Error{Code: rpc.CodeConfirmationUnverifiable, Message: message + " Nothing was written."}
		}
		verified, err := verifyCashPolicyDevice(keys, c, in.Terms)
		if err != nil {
			if errors.Is(err, errCashPolicyEnvelopeUnsigned) {
				return nil, time.Time{}, "", &rpc.Error{Code: rpc.CodeConfirmationUnverifiable, Message: "Changing an order cap needs a confirmation Canary can verify itself, and this one carries no signature over the save; nothing was written."}
			}
			return nil, time.Time{}, "", &rpc.Error{Code: rpc.CodeConfirmationUnverifiable, Message: "Canary could not verify your device's confirmation: " + err.Error() + ". Nothing was written."}
		}
		return nil, time.Time{}, verified, nil
	}
	relied, until, err := s.cashPolicyReliance(ctx, c, consequences, window, now)
	if err != nil {
		return nil, time.Time{}, "", err
	}
	if relied != nil || len(keys) == 0 || !cashPolicyEnvelopeSigned(c.Envelope) {
		return relied, until, "", nil
	}
	verified, err := verifyCashPolicyDevice(keys, c, in.Terms)
	if err != nil {
		return nil, time.Time{}, "", &rpc.Error{Code: rpc.CodeConfirmationUnverifiable, Message: "Canary could not verify your device's confirmation: " + err.Error() + ". Nothing was written."}
	}
	return nil, until, verified, nil
}

// journalOrderLimitsRevision records a constitution revision written from
// Desk as a governance event in risk_policy_events: the request and the
// Desk action, the credential and whether Canary verified it, the versions,
// the fingerprint after and each cap before and after (design §7.2). Today
// only status transitions reach that journal; this adds the writes. It
// returns "recorded", or why the event could not be recorded, for the
// receipt: the store was checked ready before the write, so a failure here
// is the append itself, after the file is in force.
func (s *Server) journalOrderLimitsRevision(ctx context.Context, now time.Time, in rpc.CashPolicyApplyRequest, before, after *risk.Constitution, caps []cashPolicyEdit, receipt cashPolicyReceipt) string {
	changes := map[string]any{}
	for _, e := range caps {
		changes[e.key] = map[string]any{"from": e.from, "to": e.to}
	}
	entry := map[string]any{
		"version": 1, "at": now, "kind": orderLimitsRevisionEventKind,
		"request_id": in.RequestID, "desk_action_id": in.Confirmation.DeskActionID, "credential": in.Confirmation.Credential, "verified": receipt.Verified,
		"policy_id": after.PolicyID, "policy_version": after.PolicyVersion, "from_version": before.PolicyVersion,
		"fingerprint_version": rpc.RiskConstitutionFingerprintVersion, "policy_fingerprint": after.FingerprintKey(),
		"preset_before": receipt.PresetBefore, "preset_after": receipt.PresetAfter, "changes": changes,
	}
	var err error
	switch {
	case s.riskCapital != nil && s.riskCapital.core != nil:
		err = s.riskCapital.RecordDeskGovernanceEvent(entry)
	case s.coreStore != nil:
		err = s.appendRiskPolicyEvent(ctx, now, entry)
	default:
		err = errors.New("no state store")
	}
	if err != nil {
		s.warnf("order limits revision %d written from Desk, but its governance event could not be recorded: %v", after.PolicyVersion, err)
		return "not recorded: " + err.Error()
	}
	return "recorded"
}

// appendRiskPolicyEvent writes one governance event to the state store
// directly, for a server without a risk capital store.
func (s *Server) appendRiskPolicyEvent(ctx context.Context, at time.Time, entry map[string]any) error {
	raw, err := json.Marshal(entry)
	if err != nil {
		return err
	}
	key, err := coreStoreEventKey(ctx, s.coreStore, coreEventRiskPolicy, at, raw, 0)
	if err != nil {
		return err
	}
	v, _ := entry["policy_version"].(int)
	version := int64(v)
	projection := corestore.RiskPolicyEventProjection{Kind: fmt.Sprint(entry["kind"]), PolicyID: fmt.Sprint(entry["policy_id"]),
		PolicyFingerprint: fmt.Sprint(entry["policy_fingerprint"]), PolicyVersion: &version}
	_, err = s.coreStore.AppendEvents(ctx, []corestore.EventInput{{ScopeKey: daemonStateScope, EventKey: key, Type: coreEventRiskPolicy,
		Action: coreEventActionRecord, Origin: coreEventOriginDaemon, OccurredAt: at, PayloadJSON: raw,
		Projection: corestore.EventProjection{RiskPolicyEvent: &projection}}})
	return err
}

// cashPolicyRestore offers the owner's own values back (owner decision O5a,
// 2026-10-07 08:30 CEST). While the files read a preset, the most recent
// receipt whose stance before was custom carries the values that preset
// replaced; keys that save left alone are completed from the preset it
// applied. The offer is withdrawn when the files' revision is not the one
// Canary's latest save wrote: a hand edit, policy ensure or another writer
// changed them since. Nil while the files read custom.
func (s *Server) cashPolicyRestore(ctx context.Context, r cashPolicyRead, stance rpc.CashPolicyPreset) *rpc.CashPolicyRestore {
	if !cashPolicyPresetIsPreset(stance.ID) || s.coreStore == nil || !s.coreStore.Health().Ready {
		return nil
	}
	events, err := s.coreStore.LoadEvents(ctx, corestore.EventQuery{ScopeKey: daemonStateScope, Type: cashPolicyReceiptType, Limit: 10000})
	if err != nil || len(events) == 0 {
		return nil
	}
	type saved struct {
		seq     int64
		at      time.Time
		receipt cashPolicyReceipt
	}
	var receipts []saved
	for _, ev := range events {
		var receipt cashPolicyReceipt
		if decodeStrictPlatformSettingsJSON(ev.PayloadJSON, &receipt) != nil {
			continue
		}
		receipts = append(receipts, saved{seq: ev.EventSeq, at: ev.OccurredAt, receipt: receipt})
	}
	if len(receipts) == 0 {
		return nil
	}
	// Latest first, by the store's own order: two saves in one second keep
	// their order.
	slices.SortStableFunc(receipts, func(a, b saved) int { return int(b.seq - a.seq) })
	if receipts[0].receipt.WrittenRevision != r.revision {
		return nil
	}
	for _, sv := range receipts {
		rc := sv.receipt
		if rc.PresetBefore != rpc.CashPolicyPresetCustom || !cashPolicyPresetIsPreset(rc.PresetAfter) {
			continue
		}
		preset, _ := cashPolicyPresetByID(rc.PresetAfter)
		values := map[string]any{}
		for _, key := range cashPolicyCoveredKeys {
			if v, ok := rc.Before[key]; ok && v != nil {
				values[key] = v
				continue
			}
			values[key] = preset.values[key]
		}
		return &rpc.CashPolicyRestore{SavedAt: sv.at, Preset: rc.PresetAfter, Values: values}
	}
	return nil
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
