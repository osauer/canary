package daemon

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"math"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/BurntSushi/toml"

	"github.com/osauer/canary/v2/internal/risk"
	"github.com/osauer/canary/v2/internal/rpc"
)

// cashPolicyEdit is one change a draft makes: the key in scope, the value
// that applies before it (with where that comes from) and the new value; a
// nil To removes the key. Raw is To as canonical JSON.
type cashPolicyEdit struct {
	key        string
	spec       cashPolicySpec
	ccy        string
	from       any
	fromSource string
	to         any
	raw        json.RawMessage
}

// cashPolicyDraft is a draft checked against one read of the files: the
// changes that change something, the errors per key ("" for one that names
// no key), and, when there are none, the policies as Canary would read them:
// the protection policy and its bytes, and the constitution and its bytes
// (the read ones when no order-limit key changes).
type cashPolicyDraft struct {
	edits        []cashPolicyEdit
	errors       map[string]string
	policy       protectionPolicy
	data         []byte
	constitution *risk.Constitution
	conData      []byte
}

// state is the draft as the settings read it.
func (d cashPolicyDraft) state() cashPolicyState {
	return cashPolicyState{p: d.policy, c: d.constitution}
}

// cashEdits and capEdits split the edits by file.
func (d cashPolicyDraft) cashEdits() []cashPolicyEdit {
	return slices.DeleteFunc(slices.Clone(d.edits), func(e cashPolicyEdit) bool { return e.spec.section == rpc.CashPolicySectionOrderLimits })
}

func (d cashPolicyDraft) capEdits() []cashPolicyEdit {
	return slices.DeleteFunc(slices.Clone(d.edits), func(e cashPolicyEdit) bool { return e.spec.section != rpc.CashPolicySectionOrderLimits })
}

// planCashPolicyDraft checks a draft against the file: each key must be in
// scope and of its type, a change that changes nothing is dropped, owner
// question 3's rule applies, and the loader's own validation runs on each
// change alone and then on all of them together. Nothing is written.
func planCashPolicyDraft(r cashPolicyRead, changes map[string]json.RawMessage) cashPolicyDraft {
	st := r.view()
	// Without a cap edit the draft's constitution is the one in view (the
	// policy in force while the file is not writable), so the stance after a
	// cash-only change reads as the screen does.
	d := cashPolicyDraft{errors: map[string]string{}, policy: r.file, data: r.data, constitution: st.c, conData: r.con.data}
	for _, key := range slices.Sorted(maps.Keys(changes)) {
		sp, ccy, ok := cashPolicySpecFor(key)
		if !ok || (sp.section == rpc.CashPolicySectionOrderLimits && (!r.con.present || r.con.file == nil)) {
			d.errors[key] = "Desk cannot change this setting; change it in the file."
			continue
		}
		to, msg := sp.decode(changes[key])
		if msg != "" {
			d.errors[key] = msg
			continue
		}
		defined := r.defined[key]
		if sp.section == rpc.CashPolicySectionOrderLimits {
			defined = r.con.defined[key]
		}
		from := sp.effective(st, ccy)
		if !defined {
			from = sp.builtin
		}
		switch {
		case defined && cashPolicySame(from, to):
			continue
		case !defined && (to == nil || (sp.absent == rpc.CashPolicySourceCanaryDefault && cashPolicySame(sp.builtin, to))):
			continue
		}
		source := sp.absent
		if defined {
			source = rpc.CashPolicySourceFile
		}
		raw, _ := json.Marshal(to)
		e := cashPolicyEdit{key: key, spec: sp, ccy: ccy, from: from, fromSource: source, to: to, raw: raw}
		if sp.section == rpc.CashPolicySectionOrderLimits {
			if out, err := editConstitutionFile(r.con.data, []cashPolicyEdit{e}, r.con.file.PolicyVersion, nil); err != nil {
				d.errors[key] = cashPolicyMessage(err.Error(), key)
				continue
			} else if _, err := parseConstitutionFile(out); err != nil {
				d.errors[key] = cashPolicyMessage(err.Error(), key)
				continue
			}
			d.edits = append(d.edits, e)
			continue
		}
		if out, err := editCashPolicyFile(r.data, []cashPolicyEdit{e}, r.file.PolicyVersion, nil); err != nil {
			d.errors[key] = cashPolicyMessage(err.Error(), key)
			continue
		} else if _, _, err := parseProtectionPolicy(out); err != nil {
			d.errors[key] = cashPolicyMessage(err.Error(), key)
			continue
		}
		d.edits = append(d.edits, e)
	}
	if len(d.errors) > 0 || len(d.edits) == 0 {
		return d
	}
	// Screen order: a feature's switch first, then its settings.
	order := func(e cashPolicyEdit) int {
		return slices.IndexFunc(cashPolicySpecs, func(sp cashPolicySpec) bool {
			return sp.section == e.spec.section && sp.leaf == e.spec.leaf && sp.perCurrency == e.spec.perCurrency
		})
	}
	slices.SortStableFunc(d.edits, func(a, b cashPolicyEdit) int {
		if oa, ob := order(a), order(b); oa != ob {
			return oa - ob
		}
		return strings.Compare(a.ccy, b.ccy)
	})
	if caps := d.capEdits(); len(caps) > 0 {
		out, err := editConstitutionFile(r.con.data, caps, r.con.file.PolicyVersion, nil)
		if err != nil {
			d.errors[""] = cashPolicyMessage(err.Error(), "")
			return d
		}
		after, err := parseConstitutionFile(out)
		if err != nil {
			key := ""
			for _, e := range caps {
				if strings.HasPrefix(err.Error(), e.key) {
					key = e.key
				}
			}
			d.errors[key] = cashPolicyMessage(err.Error(), key)
			return d
		}
		if err := verifyConstitutionEdit(r.con.file, after, caps); err != nil {
			d.errors[""] = "Canary cannot write these changes alone: " + err.Error() + "."
			return d
		}
		d.constitution, d.conData = after, out
	}
	cash := d.cashEdits()
	if len(cash) == 0 {
		return d
	}
	out, err := editCashPolicyFile(r.data, cash, r.file.PolicyVersion, nil)
	if err != nil {
		d.errors[""] = cashPolicyMessage(err.Error(), "")
		return d
	}
	after, _, err := parseProtectionPolicy(out)
	if err != nil {
		key := ""
		for _, e := range cash {
			if strings.HasPrefix(err.Error(), e.key) {
				key = e.key
			}
		}
		d.errors[key] = cashPolicyMessage(err.Error(), key)
		return d
	}
	if err := verifyCashPolicyEdit(r.file, after, cash); err != nil {
		d.errors[""] = "Canary cannot write these changes alone: " + err.Error() + "."
		return d
	}
	d.policy, d.data = after, out
	return d
}

// parseConstitutionFile reads a constitution the way the risk policy
// manager does: strictly, then validated.
func parseConstitutionFile(data []byte) (*risk.Constitution, error) {
	var c risk.Constitution
	md, err := toml.Decode(string(data), &c)
	if err != nil {
		return nil, fmt.Errorf("parse: %w", err)
	}
	if undecoded := md.Undecoded(); len(undecoded) > 0 {
		keys := make([]string, len(undecoded))
		for i, k := range undecoded {
			keys[i] = k.String()
		}
		return nil, fmt.Errorf("unknown risk policy key(s): %s", strings.Join(keys, ", "))
	}
	if err := c.Validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

// editConstitutionFile writes order-limit edits into the constitution's
// bytes and raises policy_version to version+1, changing only the lines it
// names, as editCashPolicyFile does. The table must be in the file: Canary
// writes it at its start, and Desk never creates it.
func editConstitutionFile(data []byte, edits []cashPolicyEdit, version int, note *cashPolicyNote) ([]byte, error) {
	if _, err := toml.Decode(string(data), &map[string]any{}); err != nil {
		return nil, err
	}
	doc := parseTOMLDoc(data)
	if doc.headerLine(risk.OrderLimitsTable) < 0 {
		return nil, fmt.Errorf("%w: risk-policy.toml has no [%s] table yet; Canary writes it at its next start", errCashPolicyUnwritable, risk.OrderLimitsTable)
	}
	for _, e := range edits {
		if e.to == nil {
			return nil, fmt.Errorf("%s cannot be left empty", e.key)
		}
		doc.setNoted(risk.OrderLimitsTable, e.spec.leaf, e.literal(), note.set)
	}
	doc.setNoted("", "policy_version", strconv.Itoa(version+1), note.raised)
	return doc.bytes(), nil
}

// verifyConstitutionEdit proves a written constitution says what the edits
// meant and nothing else: each edited key reads its new value, every other
// [order_limits] key reads as before, everything outside the table is
// unchanged, and policy_version is one higher.
func verifyConstitutionEdit(before, after *risk.Constitution, edits []cashPolicyEdit) error {
	if before == nil || after == nil {
		return errors.New("the constitution could not be read")
	}
	if after.PolicyVersion != before.PolicyVersion+1 {
		return fmt.Errorf("policy_version reads %d, not %d", after.PolicyVersion, before.PolicyVersion+1)
	}
	if before.OrderLimits == nil || after.OrderLimits == nil {
		return errors.New("[order_limits] is missing")
	}
	edited := map[string]bool{}
	for _, e := range edits {
		edited[e.key] = true
		got, set := e.spec.get(cashPolicyState{c: after}, "")
		if !set || !cashPolicySame(got, e.to) {
			return fmt.Errorf("%s reads %v, not %v", e.key, got, e.to)
		}
	}
	rest := *after.OrderLimits
	for _, key := range []string{risk.OrderLimitMaxOrderFloorBase, risk.OrderLimitMaxOrderPctNLV, risk.OrderLimitMaxOptionContracts} {
		if !edited[risk.OrderLimitsTable+"."+key] {
			continue
		}
		switch key {
		case risk.OrderLimitMaxOrderFloorBase:
			rest.MaxOrderFloorBase = before.OrderLimits.MaxOrderFloorBase
		case risk.OrderLimitMaxOrderPctNLV:
			rest.MaxOrderPctNLV = before.OrderLimits.MaxOrderPctNLV
		case risk.OrderLimitMaxOptionContracts:
			rest.MaxOptionContracts = before.OrderLimits.MaxOptionContracts
		}
	}
	if !reflect.DeepEqual(rest, *before.OrderLimits) {
		return errors.New("an [order_limits] key changed, and no change named it")
	}
	a, b := *after, *before
	a.OrderLimits, b.OrderLimits = nil, nil
	a.PolicyVersion = b.PolicyVersion
	if a.FingerprintKey() != b.FingerprintKey() {
		return errors.New("a setting outside [order_limits] changed")
	}
	return nil
}

// decode reads one draft value as the key's type. Bounds are the loader's;
// decode adds what the loader cannot see: a whole number of days, and a
// largest order of 0, which the loader reads as not written.
func (sp cashPolicySpec) decode(raw json.RawMessage) (any, string) {
	raw = bytes.TrimSpace(raw)
	if bytes.Equal(raw, []byte("null")) {
		if sp.removable {
			return nil, ""
		}
		return nil, "Cannot be left empty."
	}
	switch sp.typ {
	case rpc.CashPolicyTypeBool:
		var v bool
		if json.Unmarshal(raw, &v) != nil {
			return nil, "Must be on or off."
		}
		return v, ""
	case rpc.CashPolicyTypeChoice:
		var v string
		if json.Unmarshal(raw, &v) != nil || !slices.Contains(sp.choices, v) {
			return nil, "Must be " + strings.Join(sp.choiceNames, " or ") + "."
		}
		return v, ""
	}
	var v float64
	if json.Unmarshal(raw, &v) != nil || math.IsNaN(v) || math.IsInf(v, 0) {
		return nil, "Must be a number."
	}
	if sp.typ == rpc.CashPolicyTypeInteger {
		if v != math.Trunc(v) || math.Abs(v) > 1e9 {
			if sp.unit == rpc.CashPolicyUnitDays {
				return nil, "Must be a whole number of days."
			}
			return nil, "Must be a whole number."
		}
		return int(v), ""
	}
	if sp.leaf == "max_order_notional" && v == 0 {
		return nil, "Must be more than 0: Canary reads 0 as not written."
	}
	return v, ""
}

// cashPolicySame compares two values of one key; numbers compare by value.
func cashPolicySame(a, b any) bool {
	fa, aNum := cashPolicyNumber(a)
	fb, bNum := cashPolicyNumber(b)
	if aNum || bNum {
		return aNum && bNum && fa == fb
	}
	return a == b
}

// cashPolicyMessage is a loader or check message in the screen's words: the
// key path it starts with dropped, capitalised, ending in a full stop.
func cashPolicyMessage(msg, key string) string {
	msg = strings.TrimSpace(msg)
	if key != "" {
		for _, sep := range []string{" ", ": "} {
			if rest, ok := strings.CutPrefix(msg, key+sep); ok {
				msg = rest
				break
			}
		}
	}
	if r, size := utf8.DecodeRuneInString(msg); size > 0 {
		msg = string(unicode.ToUpper(r)) + msg[size:]
	}
	if !strings.HasSuffix(msg, ".") {
		msg += "."
	}
	return msg
}

// cashPolicyNote is the provenance a save writes beside each line it
// changes: when, in Europe/Berlin time, and how the owner confirmed it.
type cashPolicyNote struct {
	at        string
	confirmed string
	// preset names the preset the save lands on (" from the Cautious
	// preset"), empty for a custom save.
	preset string
}

func (n *cashPolicyNote) set(was string, present bool) string {
	if n == nil {
		return ""
	}
	text := "set in Desk " + n.at + n.preset + ", " + n.confirmed
	if !present {
		return text + "; was not in the file"
	}
	return text + "; was " + was
}

func (n *cashPolicyNote) raised(string, bool) string {
	if n == nil {
		return ""
	}
	return "raised in Desk " + n.at
}

// table names the table an edit writes, at the place the file keeps it: a
// file that still has the sweep under [buckets.cash_sweep] is edited there,
// so the sweep is never written twice.
func (e cashPolicyEdit) table(legacy bool) string {
	table := "cash." + e.spec.section
	if legacy && e.spec.section == rpc.CashPolicySectionSweep {
		table = "buckets.cash_sweep"
	}
	if e.spec.perCurrency {
		table += ".currency." + e.ccy
	}
	return table
}

// literal is the edit's value as TOML.
func (e cashPolicyEdit) literal() string {
	switch v := e.to.(type) {
	case float64:
		return tomlFloat(v)
	case int:
		return strconv.Itoa(v)
	case bool:
		return strconv.FormatBool(v)
	case string:
		return strconv.Quote(v)
	}
	return fmt.Sprint(e.to)
}

// errCashPolicyUnwritable marks a file Canary cannot edit in place.
var errCashPolicyUnwritable = errors.New("policy unwritable")

// editCashPolicyFile writes edits into the file's bytes and raises
// policy_version to version+1. It changes only the lines it names: a new
// table goes after its family, a removed key leaves a comment in its place,
// and every other line, value and comment stays byte for byte. A nil note
// writes no provenance (a check); a save passes the note.
func editCashPolicyFile(data []byte, edits []cashPolicyEdit, version int, note *cashPolicyNote) ([]byte, error) {
	var raw map[string]any
	md, err := toml.Decode(string(data), &raw)
	if err != nil {
		return nil, err
	}
	legacy := md.IsDefined("buckets", "cash_sweep")
	doc := parseTOMLDoc(data)
	for _, e := range edits {
		table := e.table(legacy)
		if doc.headerLine(table) < 0 {
			if md.IsDefined(strings.Split(table, ".")...) {
				return nil, fmt.Errorf("%w: [%s] is not a plain section in the file, so Canary does not edit it; change it in the file", errCashPolicyUnwritable, table)
			}
			doc.addTable(table)
		}
		if e.to == nil {
			if note == nil {
				doc.remove(table, e.spec.leaf)
				continue
			}
			doc.removeNoted(table, e.spec.leaf, func(was string) string {
				return "removed in Desk " + note.at + ", " + note.confirmed + "; was " + was
			})
			continue
		}
		doc.setNoted(table, e.spec.leaf, e.literal(), note.set)
	}
	doc.setNoted("", "policy_version", strconv.Itoa(version+1), note.raised)
	return doc.bytes(), nil
}

// verifyCashPolicyEdit proves a written file says what the edits meant and
// nothing else: each edited key reads its new value, every other key in scope
// reads as before, everything outside cash management is unchanged, and
// policy_version is one higher.
func verifyCashPolicyEdit(before, after protectionPolicy, edits []cashPolicyEdit) error {
	if after.PolicyVersion != before.PolicyVersion+1 {
		return fmt.Errorf("policy_version reads %d, not %d", after.PolicyVersion, before.PolicyVersion+1)
	}
	edited := map[string]bool{}
	for _, e := range edits {
		edited[e.key] = true
		got, set := e.spec.get(cashPolicyState{p: after}, e.ccy)
		switch {
		case e.to == nil && set:
			return fmt.Errorf("%s still reads %v", e.key, got)
		case e.to != nil && (!set || !cashPolicySame(got, e.to)):
			return fmt.Errorf("%s reads %v, not %v", e.key, got, e.to)
		}
	}
	currencies := map[string]bool{}
	for _, p := range []protectionPolicy{before, after} {
		if l := p.Cash.Leveling; l != nil {
			for ccy := range l.Currency {
				currencies[ccy] = true
			}
		}
		if s := p.Cash.Sweep; s != nil {
			for ccy := range s.Currency {
				currencies[ccy] = true
			}
		}
	}
	for _, sp := range cashPolicySpecs {
		if sp.section == rpc.CashPolicySectionOrderLimits {
			continue
		}
		ccys := []string{""}
		if sp.perCurrency {
			ccys = slices.Sorted(maps.Keys(currencies))
		}
		for _, ccy := range ccys {
			if key := sp.key(ccy); !edited[key] && !cashPolicySame(sp.effective(cashPolicyState{p: before}, ccy), sp.effective(cashPolicyState{p: after}, ccy)) {
				return fmt.Errorf("%s changed, and no change named it", key)
			}
		}
	}
	if cashPolicyOutOfScope(before) != cashPolicyOutOfScope(after) {
		return errors.New("a setting outside cash management changed")
	}
	return nil
}

// cashPolicyOutOfScope fingerprints a policy with every key in scope cleared:
// two policies that differ only by settings the screen edits get the same
// key. Every leveling key is in scope; a sweep currency table left with only
// its compiled declaration reads as no table.
func cashPolicyOutOfScope(p protectionPolicy) string {
	p.PolicyVersion = 0
	p.Cash.Leveling = nil
	if s := p.Cash.Sweep; s != nil {
		c := *s
		c.Enabled, c.Mode, c.MaxOrderNotional = false, "", 0
		c.MaxOrderPctNLV, c.MinOrderNotional, c.ReserveFloorBase, c.ReservePctNLV, c.OrderStepBase, c.KeepCash = nil, nil, nil, nil, nil, nil
		c.BillsExemptFromTradingMaxNotional, c.NoBuyWhileBorrowed = nil, nil
		currencies := map[string]protectionCashSweepCurrency{}
		for ccy, v := range c.Currency {
			v.KeepCash = nil
			if !reflect.DeepEqual(v, defaultCashSweepCurrency(ccy)) {
				currencies[ccy] = v
			}
		}
		c.Currency = nil
		if len(currencies) > 0 {
			c.Currency = currencies
		}
		p.Cash.Sweep = &c
		if reflect.DeepEqual(c, protectionCashSweepPolicy{}) {
			p.Cash.Sweep = nil
		}
	}
	return fingerprintProtectionPolicy(p).Key
}

// cashPolicyKeepCash is the settlement float that applies to ccy: its own,
// else the common one; nil when neither is written.
func cashPolicyKeepCash(p protectionPolicy, ccy string) any {
	if v, ok := p.Cash.Sweep.keepCash(ccy); ok {
		return v
	}
	return nil
}

// cashPolicyDaemonSends reports whether the daemon places the sweep's
// orders itself: the file pre-authorises the sweep, it is on and active, and
// it has every number it reads from the file (until then it holds).
func cashPolicyDaemonSends(p protectionPolicy) bool {
	return p.preAuthorised(preAuthorisedBucketCashSweep) && p.Cash.Sweep.enabled() && p.Cash.Sweep.effectiveMode() == rpc.CashSweepModeActive &&
		len(p.Cash.Sweep.missingNumbers()) == 0
}

// cashPolicyDaemonSendsSentence says that the daemon will send the sweep's
// orders itself and how large each may be: the sweep's cap in force at
// today's NLV, held to the order cap in force unless bills are exempt.
func cashPolicyDaemonSendsSentence(p protectionPolicy, b cashPolicyBook) string {
	s := p.Cash.Sweep
	text := "The daemon will send sweep orders itself, " + cashPolicyDuration(p.Authority.vetoWindow()) + " after announcing them"
	top, _, ok := cashPolicyLargestOrder(s, b)
	switch {
	case s.MaxOrderNotional <= 0:
		return text + ", once the sweep's numbers are in the file."
	case !ok:
		return text + ", up to " + cashPolicyMoney(s.MaxOrderNotional, b.base) + " each, more as NLV grows."
	case policyCheckDeref(s.MaxOrderPctNLV) > 0:
		return text + ", up to " + cashPolicyMoney(top, b.base) + " each at today's NLV."
	}
	return text + ", up to " + cashPolicyMoney(top, b.base) + " each."
}

// cashPolicyLargestOrder is the sweep's largest order at today's NLV under
// the order cap in force: the larger of the fixed amount and the share of
// NLV, held to the order cap unless bills are exempt from it. Bound names
// what sets it (fixed, share or cap); ok is false without the fixed amount,
// or when a share applies and NLV cannot be read.
func cashPolicyLargestOrder(s *protectionCashSweepPolicy, b cashPolicyBook) (top float64, bound string, ok bool) {
	if s == nil || s.MaxOrderNotional <= 0 {
		return 0, "", false
	}
	top, bound = s.MaxOrderNotional, "fixed"
	if pct := policyCheckDeref(s.MaxOrderPctNLV); pct > 0 {
		if b.nlv <= 0 {
			return 0, "", false
		}
		if share := pct / 100 * b.nlv; share > top {
			top, bound = share, "share"
		}
	}
	if !s.billsExempt() && b.orderCapReason == "" && b.orderCap > 0 && b.orderCap < top {
		top, bound = b.orderCap, "cap"
	}
	return top, bound, true
}

// cashPolicyReserve is the sweep's reserve at today's NLV: the larger of the
// amount and the share of NLV. Bound names what sets it (amount or share);
// ok is false when a share applies and NLV cannot be read.
func cashPolicyReserve(s *protectionCashSweepPolicy, b cashPolicyBook) (reserve float64, bound string, ok bool) {
	if s == nil || (s.ReserveFloorBase == nil && s.ReservePctNLV == nil) {
		return 0, "", false
	}
	reserve, bound = policyCheckDeref(s.ReserveFloorBase), "amount"
	if pct := policyCheckDeref(s.ReservePctNLV); pct > 0 {
		if b.nlv <= 0 {
			return 0, "", false
		}
		if share := pct / 100 * b.nlv; share > reserve {
			reserve, bound = share, "share"
		}
	}
	return reserve, bound, true
}

// cashPolicyNouns names each number a feature reads from the file only, for
// a sentence about the file not having it.
var cashPolicyNouns = map[string]string{
	"trigger_base": "band", "cushion_base": "cushion", "max_slippage_bp": "limit from the mid", "payback_days": "payback window",
	"reserve_floor_base": "reserve amount", "reserve_pct_nlv": "reserve share of NLV", "min_order_notional": "smallest buy",
	"max_order_notional": "largest order", "max_order_pct_nlv": "largest order's share of NLV", "no_buy_while_borrowed": "rule on borrowed currencies",
	"order_step_base": "order step", "keep_cash": "common settlement float",
}

func cashPolicyNounList(keys []string) string {
	var names []string
	for _, k := range keys {
		if n, ok := cashPolicyNouns[k]; ok {
			names = append(names, n)
		} else {
			names = append(names, k)
		}
	}
	return cashPolicyList(names)
}

// cashPolicyMissing lists what a feature still needs from the file before it
// does anything, as if it were on: the numbers it reads from the file only.
// The sweep's common settlement float counts: a currency without its own
// waits for it.
func cashPolicyMissing(p protectionPolicy, section string) []string {
	if section == rpc.CashPolicySectionLeveling {
		l := protectionCurrencyLevelingPolicy{}
		if p.Cash.Leveling != nil {
			l = *p.Cash.Leveling
		}
		l.Enabled = true
		return l.missingNumbers()
	}
	s := protectionCashSweepPolicy{}
	if p.Cash.Sweep != nil {
		s = *p.Cash.Sweep
	}
	s.Enabled = true
	missing := s.missingNumbers()
	if s.KeepCash == nil {
		missing = append(missing, "keep_cash")
	}
	return missing
}

// cashPolicyConsequences states what changes at the broker, one sentence
// for each change that lets more reach it: a feature switched on, shadow
// changed to active, a rule loosened, a band, reserve or float made smaller,
// a limit, cushion, payback window or order size made larger, and any number
// written where the file had none (a feature that waited for it can then
// act). Sizes that depend on NLV or the order cap in force say what they come
// to at today's NLV, and when that does not change today, why. When the
// draft makes the daemon send the sweep's orders itself (the file
// pre-authorises the sweep), that comes first (owner decision 2026-10-06
// 15:31 CEST); while it already does, each sweep change says so. Whether a
// change has a sentence never depends on the book: only the wording does.
func cashPolicyConsequences(beforeState, afterState cashPolicyState, edits []cashPolicyEdit, b cashPolicyBook) []string {
	before, after := beforeState.p, afterState.p
	out := []string{}
	// One sentence per quantity (D5): the reserve pair and the order cap's
	// floor and share each describe one quantity, so the second key of a
	// pair adds no second sentence.
	quantity := map[string]string{"cash.sweep.reserve_floor_base": "reserve", "cash.sweep.reserve_pct_nlv": "reserve",
		"order_limits.max_order_floor_base": "order cap", "order_limits.max_order_pct_nlv": "order cap"}
	saidQuantity := map[string]bool{}
	money := func(v any) string { f, _ := cashPolicyNumber(v); return cashPolicyMoney(f, b.base) }
	num := func(v any) string { f, _ := cashPolicyNumber(v); return policyCheckNumber(f) }
	startsSending := !cashPolicyDaemonSends(before) && cashPolicyDaemonSends(after)
	daemonSends := cashPolicyDaemonSends(before) && cashPolicyDaemonSends(after)
	if startsSending {
		out = append(out, cashPolicyDaemonSendsSentence(after, b))
	}
	switchedOn := map[string]bool{}
	written := map[string][]string{}
	for _, e := range edits {
		if !e.spec.perCurrency && e.spec.leaf == "enabled" && cashPolicyTurnsOn(e.from, e.to) {
			switchedOn[e.spec.section] = true
		}
	}
	for _, e := range edits {
		from, to := e.from, e.to
		if e.spec.section == rpc.CashPolicySectionSweep && e.spec.perCurrency {
			from, to = cashPolicyKeepCash(before, e.ccy), cashPolicyKeepCash(after, e.ccy)
		}
		if from == nil && to != nil {
			key := e.spec.leaf
			if e.spec.perCurrency {
				key = e.ccy + " settlement float"
			}
			written[e.spec.section] = append(written[e.spec.section], key)
		}
	}
	said := map[string]bool{}
	for _, e := range edits {
		from, to := e.from, e.to
		if e.spec.section == rpc.CashPolicySectionSweep && e.spec.perCurrency {
			from, to = cashPolicyKeepCash(before, e.ccy), cashPolicyKeepCash(after, e.ccy)
		}
		section := e.spec.section
		if from == nil && to != nil && !said[section] {
			said[section] = true
			covered := switchedOn[section] || (section == rpc.CashPolicySectionSweep && startsSending)
			if text := cashPolicyWrittenSentence(before, after, section, written[section], covered); text != "" {
				out = append(out, text)
			}
		}
		if e.spec.more == nil || !e.spec.more(from, to) {
			continue
		}
		if q := quantity[e.key]; q != "" {
			if saidQuantity[q] {
				continue
			}
			saidQuantity[q] = true
		}
		var text string
		switch e.key {
		case "cash.leveling.enabled":
			text = "Currency leveling starts proposing conversions that repay borrowed currencies. Each repayment waits for your approval."
			if t := after.Cash.Leveling.TriggerBase; t != nil {
				text = "Currency leveling starts proposing conversions for loans beyond " + cashPolicyMoney(*t, b.base) + ". Each repayment waits for your approval."
			}
			if missing := cashPolicyMissing(after, section); len(missing) > 0 {
				text = "Currency leveling is switched on and waits until the file has its " + cashPolicyNounList(missing) + "."
			}
		case "cash.leveling.trigger_base":
			text = "Leveling repays loans from " + money(to) + " instead of " + money(from) + "."
		case "cash.leveling.cushion_base":
			text = "A repaid currency may end up to " + money(to) + " above zero instead of " + money(from) + "."
		case "cash.leveling.max_slippage_bp":
			text = "A conversion's limit may sit " + num(to) + " bp from the mid instead of " + num(from) + " bp."
		case "cash.leveling.payback_days":
			text = "A conversion may take " + num(to) + " days to pay back instead of " + num(from) + "."
		case "cash.sweep.enabled":
			text = "The cash sweep starts in Observe only: it lists the bills it would buy and sends no order."
			switch missing := cashPolicyMissing(after, section); {
			case startsSending:
				continue
			case len(missing) > 0:
				text = "The cash sweep is switched on and waits until the file has its " + cashPolicyNounList(missing) + "."
			case after.Cash.Sweep.effectiveMode() == rpc.CashSweepModeActive:
				text = "The cash sweep starts proposing bill orders for you to approve."
			}
		case "cash.sweep.mode":
			text = "Sweep rows become proposals you can approve."
			switch missing := cashPolicyMissing(after, section); {
			case startsSending:
				continue
			case !after.Cash.Sweep.enabled():
				text = "Once the sweep is on, its rows become proposals you can approve."
			case after.preAuthorised(preAuthorisedBucketCashSweep) && len(after.Cash.Sweep.missingNumbers()) > 0:
				text = "Once the file has the sweep's " + cashPolicyNounList(after.Cash.Sweep.missingNumbers()) + ", the daemon will send sweep orders itself, " +
					cashPolicyDuration(after.Authority.vetoWindow()) + " after announcing them."
			case len(missing) > 0:
				text = "Once the file has the sweep's " + cashPolicyNounList(missing) + ", its rows become proposals you can approve."
			}
		case "cash.sweep.reserve_floor_base", "cash.sweep.reserve_pct_nlv":
			text = cashPolicyReserveSentence(before.Cash.Sweep, after.Cash.Sweep, e.key, from, to, b)
		case "cash.sweep.min_order_notional":
			text = "The smallest bill buy falls from " + money(from) + " to " + money(to) + "."
		case "cash.sweep.max_order_notional", "cash.sweep.max_order_pct_nlv":
			text = cashPolicyLargestOrderSentence(before.Cash.Sweep, after.Cash.Sweep, e.key, from, to, b)
		case "cash.sweep.no_buy_while_borrowed":
			text = "Bill buys may go ahead while a currency is borrowed."
		case "cash.sweep.bills_exempt_from_trading_max_notional":
			text = cashPolicyExemptSentence(before.Cash.Sweep, after.Cash.Sweep, b)
		case "cash.sweep.keep_cash":
			text = "The settlement float falls from " + num(from) + " to " + num(to) + " in each currency's own unit."
		case "order_limits.max_order_floor_base", "order_limits.max_order_pct_nlv":
			text = cashPolicyOrderCapSentence(beforeState.c, afterState.c, e.key, from, to, b)
		case "order_limits.max_option_contracts":
			f, _ := cashPolicyNumber(from)
			t, _ := cashPolicyNumber(to)
			text = "An opening option order may hold up to " + cashPolicyPresetContracts(int(t)) + " instead of " + cashPolicyPresetContracts(int(f)) + "; a delta-reducing exit is not held to it."
		default:
			switch {
			case e.spec.leaf == "deliberate_carry":
				text = "Leveling may repay " + e.ccy + " and spend from it."
			case e.spec.leaf == "keep_cash":
				f, _ := cashPolicyNumber(from)
				t, _ := cashPolicyNumber(to)
				text = e.ccy + " keeps " + cashPolicyMoney(t, e.ccy) + " as settlement float instead of " + cashPolicyMoney(f, e.ccy) + "."
			}
		}
		if text == "" {
			continue
		}
		if daemonSends && section == rpc.CashPolicySectionSweep {
			text += " The daemon sends sweep orders itself, " + cashPolicyDuration(after.Authority.vetoWindow()) + " after announcing them."
		}
		out = append(out, text)
	}
	return out
}

// cashPolicyWrittenSentence says what writing numbers the file had none of
// means for a feature: it can then act, or it still waits for others. A
// feature switched on in the same save, or a sweep the daemon starts to send
// for, already has its sentence; the numbers are in the change list.
func cashPolicyWrittenSentence(before, after protectionPolicy, section string, written []string, covered bool) string {
	if covered {
		return ""
	}
	missingBefore, missingAfter := cashPolicyMissing(before, section), cashPolicyMissing(after, section)
	feature, act := "The sweep", "work once you switch it on"
	if section == rpc.CashPolicySectionLeveling {
		feature = "Leveling"
		if after.Cash.Leveling.enabled() {
			act = "propose conversions for you to approve"
		}
	} else if s := after.Cash.Sweep; s.enabled() {
		act = "list the bills it would buy; it sends no order"
		if s.effectiveMode() == rpc.CashSweepModeActive {
			act = "propose bill orders for you to approve"
		}
	}
	whose := "the sweep's"
	if section == rpc.CashPolicySectionLeveling {
		whose = "leveling's"
	}
	switch {
	case len(missingAfter) > 0:
		return "The file gets " + whose + " " + cashPolicyNounList(written) + "; " + strings.ToLower(feature[:1]) + feature[1:] + " still waits for its " + cashPolicyNounList(missingAfter) + "."
	case len(missingBefore) > 0:
		return feature + " can now " + act + ": the file had no " + cashPolicyNounList(written) + "."
	}
	return feature + " now uses the " + cashPolicyNounList(written) + " written here; the file had none."
}

// cashPolicyLargestOrderSentence says what a larger fixed amount or share
// does to the sweep's largest order at today's NLV under the cap in force,
// and why when nothing changes today.
func cashPolicyLargestOrderSentence(before, after *protectionCashSweepPolicy, key string, from, to any, b cashPolicyBook) string {
	was, _, okBefore := cashPolicyLargestOrder(before, b)
	now, bound, okAfter := cashPolicyLargestOrder(after, b)
	if !okBefore || !okAfter {
		if key == "cash.sweep.max_order_pct_nlv" {
			f, _ := cashPolicyNumber(from)
			t, _ := cashPolicyNumber(to)
			return "The largest sweep order's share rises from " + policyCheckNumber(f) + "% to " + policyCheckNumber(t) + "% of NLV; what that comes to today is unknown, because NLV cannot be read now."
		}
		f, _ := cashPolicyNumber(from)
		t, _ := cashPolicyNumber(to)
		return "The largest sweep order's fixed amount rises from " + cashPolicyMoney(f, b.base) + " to " + cashPolicyMoney(t, b.base) + "; what that comes to today is unknown, because NLV cannot be read now."
	}
	if now > was {
		return "At today's NLV the largest sweep order rises from " + cashPolicyMoney(was, b.base) + " to " + cashPolicyMoney(now, b.base) + "."
	}
	why := "the fixed amount sets it, so the change matters only if NLV grows"
	switch bound {
	case "share":
		why = policyCheckNumber(policyCheckDeref(after.MaxOrderPctNLV)) + "% of NLV sets it, so the change matters only if NLV falls"
	case "cap":
		why = "the order cap in force holds it, so the change matters only if that cap rises or bills may pass it"
	}
	return "At today's NLV the largest sweep order stays " + cashPolicyMoney(now, b.base) + ": " + why + "."
}

// cashPolicyOrderCapSentence says what a larger floor or share does to the
// order cap on new orders at today's NLV under the file's ceiling, and why
// when nothing changes today. Delta-reducing exits are not held to the cap,
// so the sentence speaks of new orders.
func cashPolicyOrderCapSentence(before, after *risk.Constitution, key string, from, to any, b cashPolicyBook) string {
	f, _ := cashPolicyNumber(from)
	t, _ := cashPolicyNumber(to)
	var was, now float64
	okBefore, okAfter := false, false
	if before != nil {
		was, okBefore = cashPolicyOrderCapAt(before.OrderLimits, b.nlv)
	}
	if after != nil {
		now, okAfter = cashPolicyOrderCapAt(after.OrderLimits, b.nlv)
	}
	if b.nlv <= 0 || !okBefore || !okAfter {
		if key == "order_limits.max_order_pct_nlv" {
			return "The order cap's share rises from " + policyCheckNumber(f) + "% to " + policyCheckNumber(t) + "% of NLV; what that comes to today is unknown, because NLV cannot be read now, so the floor applies meanwhile."
		}
		return "The order cap's floor rises from " + cashPolicyMoney(f, b.base) + " to " + cashPolicyMoney(t, b.base) + ": the cap on new orders while NLV cannot be read."
	}
	if now > was {
		return "At today's NLV the order cap on new orders rises from " + cashPolicyMoney(was, b.base) + " to " + cashPolicyMoney(now, b.base) + "."
	}
	why := "the floor sets it, so the change matters only if NLV grows"
	o := after.OrderLimits
	switch {
	case o.MaxOrderCeilingBase != nil && now >= *o.MaxOrderCeilingBase:
		why = "the " + cashPolicyMoney(*o.MaxOrderCeilingBase, b.base) + " ceiling holds it"
	case o.MaxOrderPctNLV != nil && o.MaxOrderFloorBase != nil && *o.MaxOrderPctNLV/100*b.nlv > *o.MaxOrderFloorBase:
		why = policyCheckNumber(*o.MaxOrderPctNLV) + "% of NLV sets it, so the change matters only if NLV falls"
	}
	return "At today's NLV the order cap on new orders stays " + cashPolicyMoney(now, b.base) + ": " + why + "."
}

// cashPolicyReserveSentence says what a smaller amount or share does to the
// reserve at today's NLV, and why when nothing changes today.
func cashPolicyReserveSentence(before, after *protectionCashSweepPolicy, key string, from, to any, b cashPolicyBook) string {
	was, _, okBefore := cashPolicyReserve(before, b)
	now, bound, okAfter := cashPolicyReserve(after, b)
	f, _ := cashPolicyNumber(from)
	t, _ := cashPolicyNumber(to)
	if !okBefore || !okAfter {
		if key == "cash.sweep.reserve_pct_nlv" {
			return "The reserve's share falls from " + policyCheckNumber(f) + "% to " + policyCheckNumber(t) + "% of NLV; what that comes to today is unknown, because NLV cannot be read now."
		}
		return "The reserve's amount falls from " + cashPolicyMoney(f, b.base) + " to " + cashPolicyMoney(t, b.base) + "; what that comes to today is unknown, because NLV cannot be read now."
	}
	if now < was {
		return "At today's NLV the reserve kept as cash falls from " + cashPolicyMoney(was, b.base) + " to " + cashPolicyMoney(now, b.base) + "."
	}
	why := "the amount sets it, so the change matters only if NLV grows"
	if bound == "share" {
		why = policyCheckNumber(policyCheckDeref(after.ReservePctNLV)) + "% of NLV sets it, so the change matters only if NLV falls"
	}
	return "At today's NLV the reserve kept as cash stays " + cashPolicyMoney(now, b.base) + ": " + why + "."
}

// cashPolicyExemptSentence says what letting bill orders pass the order cap
// in force does to the largest order at today's NLV.
func cashPolicyExemptSentence(before, after *protectionCashSweepPolicy, b cashPolicyBook) string {
	text := "Bill orders may pass the order cap in force, up to the sweep's largest order."
	if b.orderCapReason != "" || b.orderCap <= 0 {
		return text
	}
	was, _, okBefore := cashPolicyLargestOrder(before, b)
	now, _, okAfter := cashPolicyLargestOrder(after, b)
	switch {
	case !okBefore || !okAfter:
		return text
	case now > was:
		return "Bill orders may pass the order cap in force of " + cashPolicyMoney(b.orderCap, b.base) + ": at today's NLV the largest sweep order rises from " +
			cashPolicyMoney(was, b.base) + " to " + cashPolicyMoney(now, b.base) + "."
	}
	return "Bill orders may pass the order cap in force of " + cashPolicyMoney(b.orderCap, b.base) + "; at today's NLV the largest sweep order, " +
		cashPolicyMoney(now, b.base) + ", is within it already, so nothing changes today."
}

// cashPolicyTermsKind names the terms a check returns and apply takes.
const cashPolicyTermsKind = "canary.cash_policy_change"

// cashPolicyTerms are the exact terms of one save. Its fields are in
// alphabetical order and Changes is a map, so json.Marshal writes the
// canonical form, keys sorted; Digest is the hash of those bytes.
type cashPolicyTerms struct {
	Changes          map[string]json.RawMessage `json:"changes"`
	ExpectedRevision string                     `json:"expected_revision"`
	Kind             string                     `json:"kind"`
	Version          int                        `json:"version"`
}

// cashPolicyTermsFor returns the canonical terms of a draft and their digest.
func cashPolicyTermsFor(revision string, edits []cashPolicyEdit) (string, string) {
	changes := map[string]json.RawMessage{}
	for _, e := range edits {
		changes[e.key] = e.raw
	}
	raw, _ := json.Marshal(cashPolicyTerms{Changes: changes, ExpectedRevision: revision, Kind: cashPolicyTermsKind, Version: 1})
	return string(raw), cashPolicyDigest(raw)
}

func cashPolicyDigest(raw []byte) string {
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// decodeCashPolicyTerms reads terms apply received; they must be exactly the
// canonical bytes a check returns.
func decodeCashPolicyTerms(terms string) (cashPolicyTerms, error) {
	var t cashPolicyTerms
	if err := decodeStrictPlatformSettingsJSON([]byte(terms), &t); err != nil {
		return t, errBadRequest("cash policy terms do not decode")
	}
	if t.Kind != cashPolicyTermsKind || t.Version != 1 || t.ExpectedRevision == "" || len(t.Changes) == 0 {
		return t, errBadRequest("cash policy terms are not a cash policy change")
	}
	if canonical, _ := json.Marshal(t); string(canonical) != terms {
		return t, errBadRequest("cash policy terms are not in Canary's canonical form")
	}
	return t, nil
}
