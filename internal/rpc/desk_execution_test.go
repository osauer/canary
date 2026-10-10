package rpc

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"
)

func deskItem(class string) DeskExecutionItem {
	it := DeskExecutionItem{RequestID: "req-" + class, EpisodeID: "ep-" + class, Intent: DeskExecutionIntent{Class: class}}
	if class == DeskClassAdd {
		it.Intent.Add = &DeskExecutionAdd{ConID: 101, Symbol: "SYNA", Currency: "USD", RuleID: "rule-1", RuleRevision: 2, EvidenceID: "ev-1"}
	} else {
		it.Intent.Proposal = &DeskExecutionProposal{Key: "proposal-" + class, Revision: "r1"}
	}
	return it
}

func TestDeskExecutionItemValidate(t *testing.T) {
	for _, class := range []string{DeskClassProtect, DeskClassReduce, DeskClassAdd} {
		if err := deskItem(class).Validate(); err != nil {
			t.Fatalf("%s: %v", class, err)
		}
	}
	bad := map[string]func(*DeskExecutionItem){
		"no request":      func(it *DeskExecutionItem) { it.RequestID = "" },
		"padded request":  func(it *DeskExecutionItem) { it.RequestID = " x" },
		"long request":    func(it *DeskExecutionItem) { it.RequestID = strings.Repeat("x", 129) },
		"no episode":      func(it *DeskExecutionItem) { it.EpisodeID = "" },
		"two sources":     func(it *DeskExecutionItem) { it.Intent.Add = deskItem(DeskClassAdd).Intent.Add },
		"no source":       func(it *DeskExecutionItem) { it.Intent.Proposal = nil },
		"proposal as add": func(it *DeskExecutionItem) { it.Intent.Class = DeskClassAdd },
		"unknown class":   func(it *DeskExecutionItem) { it.Intent.Class = "hedge" },
		"proposal no rev": func(it *DeskExecutionItem) { it.Intent.Proposal.Revision = "" },
	}
	for name, mutate := range bad {
		it := deskItem(DeskClassReduce)
		mutate(&it)
		if it.Validate() == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	add := deskItem(DeskClassAdd)
	add.Intent.Class = DeskClassProtect
	if add.Validate() == nil {
		t.Error("an addition labelled protection was accepted")
	}
	add = deskItem(DeskClassAdd)
	add.Intent.Add.EvidenceID = ""
	if add.Validate() == nil {
		t.Error("an addition without evidence was accepted")
	}
}

func TestDeskExecutionReceiptOutcomes(t *testing.T) {
	item := deskItem(DeskClassReduce)
	base := DeskExecutionReceipt{RequestID: item.RequestID, Digest: DeskIntentDigest(item), Class: DeskClassReduce}
	why := []TradingBlocker{{Code: DeskBlockerDrawdownBrake, Message: "brake"}}
	cases := []struct {
		name   string
		r      DeskExecutionReceipt
		lookup bool
		ok     bool
	}{
		{"accepted with order", DeskExecutionReceipt{Outcome: DeskOutcomeAccepted, OrderRef: "o-1"}, false, true},
		{"accepted without order", DeskExecutionReceipt{Outcome: DeskOutcomeAccepted}, false, false},
		{"refused with blocker", DeskExecutionReceipt{Outcome: DeskOutcomeRefused, Blockers: why}, false, true},
		{"refused silently", DeskExecutionReceipt{Outcome: DeskOutcomeRefused}, false, false},
		{"refused with order", DeskExecutionReceipt{Outcome: DeskOutcomeRefused, OrderRef: "o-1", Blockers: why}, false, false},
		{"unknown with message", DeskExecutionReceipt{Outcome: DeskOutcomeUnknown, Message: "transport closed"}, false, true},
		{"absent on submit", DeskExecutionReceipt{Outcome: DeskOutcomeAbsent}, false, false},
		{"absent on lookup", DeskExecutionReceipt{Outcome: DeskOutcomeAbsent}, true, true},
		{"empty outcome", DeskExecutionReceipt{}, true, false},
	}
	for _, c := range cases {
		r := c.r
		r.RequestID, r.Digest, r.Class = base.RequestID, base.Digest, base.Class
		if err := r.Validate(item, c.lookup); (err == nil) != c.ok {
			t.Errorf("%s: err=%v", c.name, err)
		}
	}
	other := deskItem(DeskClassReduce)
	other.Intent.Proposal.Revision = "r2"
	r := base
	r.Outcome, r.OrderRef = DeskOutcomeAccepted, "o-1"
	if r.Validate(other, false) == nil {
		t.Error("a receipt answered a request with a different intent")
	}
}

func TestDeskIntentDigestIsCompactJSON(t *testing.T) {
	item := deskItem(DeskClassAdd)
	raw, _ := json.Marshal(item)
	sum := sha256.Sum256(raw)
	if DeskIntentDigest(item) != "sha256:"+hex.EncodeToString(sum[:]) {
		t.Fatal("digest is not the sha256 of the compact wire form")
	}
	if string(raw) != `{"request_id":"req-add","episode_id":"ep-add","intent":{"class":"add","add":{"con_id":101,"symbol":"SYNA","currency":"USD","rule_id":"rule-1","rule_revision":2,"evidence_id":"ev-1"}}}` {
		t.Fatalf("wire form changed: %s", raw)
	}
}

func TestValidateAutomaticOrderKeepsAdditionsLast(t *testing.T) {
	p, r, a := deskItem(DeskClassProtect), deskItem(DeskClassReduce), deskItem(DeskClassAdd)
	for _, ok := range [][]DeskExecutionItem{{p, r, a}, {r, p, a}, {a, a}, {p}, nil} {
		if err := ValidateAutomaticOrder(ok); err != nil {
			t.Errorf("refused %v", err)
		}
	}
	if ValidateAutomaticOrder([]DeskExecutionItem{p, a, r}) == nil {
		t.Error("an addition before a reduction was accepted")
	}
}
