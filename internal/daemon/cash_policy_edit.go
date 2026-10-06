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

// cashPolicyDraft is a draft checked against one read of the file: the
// changes that change something, the errors per key ("" for one that names
// no key), and, when there are none, the policy as Canary would read it.
type cashPolicyDraft struct {
	edits  []cashPolicyEdit
	errors map[string]string
	policy protectionPolicy
	data   []byte
}

// planCashPolicyDraft checks a draft against the file: each key must be in
// scope and of its type, a change that changes nothing is dropped, owner
// question 3's rule applies, and the loader's own validation runs on each
// change alone and then on all of them together. Nothing is written.
func planCashPolicyDraft(r cashPolicyRead, changes map[string]json.RawMessage) cashPolicyDraft {
	d := cashPolicyDraft{errors: map[string]string{}, policy: r.file, data: r.data}
	for _, key := range slices.Sorted(maps.Keys(changes)) {
		sp, ccy, ok := cashPolicySpecFor(key)
		if !ok {
			d.errors[key] = "Desk cannot change this setting; change it in the file."
			continue
		}
		to, msg := sp.decode(changes[key])
		if msg != "" {
			d.errors[key] = msg
			continue
		}
		defined := r.defined[key]
		from := sp.effective(r.file, ccy)
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
	out, err := editCashPolicyFile(r.data, d.edits, r.file.PolicyVersion, nil)
	if err != nil {
		d.errors[""] = cashPolicyMessage(err.Error(), "")
		return d
	}
	after, _, err := parseProtectionPolicy(out)
	if err != nil {
		key := ""
		for _, e := range d.edits {
			if strings.HasPrefix(err.Error(), e.key) {
				key = e.key
			}
		}
		d.errors[key] = cashPolicyMessage(err.Error(), key)
		return d
	}
	if err := verifyCashPolicyEdit(r.file, after, d.edits); err != nil {
		d.errors[""] = "Canary cannot write these changes alone: " + err.Error() + "."
		return d
	}
	d.policy, d.data = after, out
	return d
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
			return nil, "Must be a whole number of days."
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
}

func (n *cashPolicyNote) set(was string, present bool) string {
	if n == nil {
		return ""
	}
	text := "set in Desk " + n.at + ", " + n.confirmed
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
		got, set := e.spec.get(after, e.ccy)
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
		ccys := []string{""}
		if sp.perCurrency {
			ccys = slices.Sorted(maps.Keys(currencies))
		}
		for _, ccy := range ccys {
			if key := sp.key(ccy); !edited[key] && !cashPolicySame(sp.effective(before, ccy), sp.effective(after, ccy)) {
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
// orders itself: the file pre-authorises the sweep, and it is on and active.
func cashPolicyDaemonSends(p protectionPolicy) bool {
	return p.preAuthorised(preAuthorisedBucketCashSweep) && p.Cash.Sweep.enabled() && p.Cash.Sweep.effectiveMode() == rpc.CashSweepModeActive
}

// cashPolicyDaemonSendsSentence says that the daemon will send the sweep's
// orders itself and how large each may be: the sweep's cap in force at
// today's NLV, held to the order cap in force unless bills are exempt.
func cashPolicyDaemonSendsSentence(p protectionPolicy, b cashPolicyBook) string {
	s := p.Cash.Sweep
	text := "The daemon will send sweep orders itself, " + cashPolicyDuration(p.Authority.vetoWindow()) + " after announcing them"
	if s.MaxOrderNotional <= 0 {
		return text + ", once the sweep's numbers are in the file."
	}
	top, pct := s.MaxOrderNotional, policyCheckDeref(s.MaxOrderPctNLV)
	if pct > 0 && b.nlv <= 0 {
		return text + ", up to " + cashPolicyMoney(top, b.base) + " each, more as NLV grows."
	}
	top = max(top, pct/100*b.nlv)
	if !s.billsExempt() && b.orderCapReason == "" && b.orderCap > 0 {
		top = min(top, b.orderCap)
	}
	if pct > 0 {
		return text + ", up to " + cashPolicyMoney(top, b.base) + " each at today's NLV."
	}
	return text + ", up to " + cashPolicyMoney(top, b.base) + " each."
}

// cashPolicyConsequences states, for each change that lets more reach the
// broker, what changes there: a feature switched on, shadow changed to
// active, a rule loosened, a size, reserve or band made larger. A number
// written where none was is not a direction. When the draft makes the daemon
// send the sweep's orders itself (the file pre-authorises the sweep), that
// comes first (owner decision 2026-10-06 15:31 CEST); while it already does,
// each sweep change says so.
func cashPolicyConsequences(before, after protectionPolicy, edits []cashPolicyEdit, b cashPolicyBook) []string {
	out := []string{}
	money := func(v any) string { f, _ := cashPolicyNumber(v); return cashPolicyMoney(f, b.base) }
	num := func(v any) string { f, _ := cashPolicyNumber(v); return policyCheckNumber(f) }
	startsSending := !cashPolicyDaemonSends(before) && cashPolicyDaemonSends(after)
	daemonSends := cashPolicyDaemonSends(before) && cashPolicyDaemonSends(after)
	if startsSending {
		out = append(out, cashPolicyDaemonSendsSentence(after, b))
	}
	for _, e := range edits {
		from, to := e.from, e.to
		if e.spec.section == rpc.CashPolicySectionSweep && e.spec.perCurrency {
			from, to = cashPolicyKeepCash(before, e.ccy), cashPolicyKeepCash(after, e.ccy)
		}
		if e.spec.more == nil || !e.spec.more(from, to) {
			continue
		}
		var text string
		switch e.key {
		case "cash.leveling.enabled":
			text = "Currency leveling starts proposing conversions that repay borrowed currencies. Each repayment waits for your approval."
			if t := after.Cash.Leveling.TriggerBase; t != nil {
				text = "Currency leveling starts proposing conversions for loans beyond " + cashPolicyMoney(*t, b.base) + ". Each repayment waits for your approval."
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
			switch {
			case startsSending:
				continue
			case after.Cash.Sweep.effectiveMode() == rpc.CashSweepModeActive:
				text = "The cash sweep starts proposing bill orders for you to approve."
			}
		case "cash.sweep.mode":
			text = "Sweep rows become proposals you can approve."
			switch {
			case startsSending:
				continue
			case !after.Cash.Sweep.enabled():
				text = "Once the sweep is on, its rows become proposals you can approve."
			}
		case "cash.sweep.reserve_floor_base":
			text = "The sweep keeps at least " + money(to) + " as cash instead of " + money(from) + "."
		case "cash.sweep.reserve_pct_nlv":
			text = "The sweep keeps " + num(to) + "% of NLV as cash instead of " + num(from) + "%."
		case "cash.sweep.min_order_notional":
			text = "The smallest bill buy falls from " + money(from) + " to " + money(to) + "."
		case "cash.sweep.max_order_notional":
			text = "The largest sweep order rises from " + money(from) + " to " + money(to) + "."
		case "cash.sweep.max_order_pct_nlv":
			text = "The largest sweep order rises from " + num(from) + "% to " + num(to) + "% of NLV."
		case "cash.sweep.no_buy_while_borrowed":
			text = "Bill buys may go ahead while a currency is borrowed."
		case "cash.sweep.bills_exempt_from_trading_max_notional":
			text = "Bill orders may pass the order cap in force, up to the sweep's largest order."
		case "cash.sweep.keep_cash":
			text = "The settlement float falls from " + num(from) + " to " + num(to) + " in each currency's own unit."
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
		if daemonSends && e.spec.section == rpc.CashPolicySectionSweep {
			text += " The daemon sends sweep orders itself, " + cashPolicyDuration(after.Authority.vetoWindow()) + " after announcing them."
		}
		out = append(out, text)
	}
	return out
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
