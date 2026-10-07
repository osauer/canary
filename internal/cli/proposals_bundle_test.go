package cli

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/osauer/canary/v2/internal/rpc"
)

// The bundle reference and the owner's confirmation travel on standard
// input only; the send keeps the caller's origin and the fast path.
func TestSubmitBundleReadsItsObjectFromStdinAndKeepsTheOrigin(t *testing.T) {
	var out bytes.Buffer
	c := &preparedProposalCLIConn{}
	env := &Env{Conn: c, Stdin: strings.NewReader(goldenBundleSubmitInput), Stdout: &out, Stderr: &out, Origin: rpc.OrderOriginAgent}
	if exit := Run(t.Context(), env, "proposals", []string{"submit-bundle", "--stdin", "--json"}); exit != 0 || c.calls != 1 || c.method != rpc.MethodTradeProposalsSubmitBundle {
		t.Fatalf("submit-bundle exit=%d calls=%d method=%s output=%s", exit, c.calls, c.method, &out)
	}
	p, ok := c.params.(rpc.TradeProposalSubmitBundleParams)
	if !ok || p.BundleRef != goldenBundleRef || p.BundleID != goldenBundleID || p.Revision != goldenBundleRevision || p.TermsDigest != "sha256:"+strings.Repeat("ab", 32) ||
		p.Confirmation == nil || p.Confirmation.DeskActionID != "desk-action-synthetic" || p.Confirmation.Envelope != "envelope-synthetic" ||
		p.Origin != rpc.OrderOriginAgent || !p.FastPath {
		t.Fatalf("submit-bundle params = %+v", c.params)
	}
	if strings.Contains(out.String(), goldenBundleRef) {
		t.Fatal("the private reference escaped into the output")
	}
}

func TestBundleStatusReadsTheReferenceFromStdin(t *testing.T) {
	var out bytes.Buffer
	c := &preparedProposalCLIConn{}
	env := &Env{Conn: c, Stdin: strings.NewReader(goldenBundleRef + "\n"), Stdout: &out, Stderr: &out}
	if exit := Run(t.Context(), env, "proposals", []string{"bundle-status", "--bundle-ref-stdin", "--json"}); exit != 0 || c.method != rpc.MethodTradeProposalsPreparedBundleStatus {
		t.Fatalf("bundle-status exit=%d method=%s output=%s", exit, c.method, &out)
	}
	if p, ok := c.params.(rpc.TradeProposalPreparedBundleStatusParams); !ok || p.BundleRef != goldenBundleRef {
		t.Fatalf("bundle-status params = %+v", c.params)
	}
}

func TestBundleCLIRefusesAmbiguousInputsBeforeRPC(t *testing.T) {
	valid := goldenBundleSubmitInput
	for _, tc := range []struct {
		args  []string
		stdin string
	}{
		{[]string{"prepare-bundle", "only-one-argument"}, ""},
		{[]string{"submit-bundle", "--json"}, valid},
		{[]string{"submit-bundle", "--stdin", goldenBundleRef}, valid},
		{[]string{"submit-bundle", "--stdin"}, `{"bundle_ref":"r","bundle_id":"b","revision":"v"}`},
		{[]string{"submit-bundle", "--stdin"}, `{"bundle_ref":"r","bundle_id":"b","revision":"v","terms_digest":"d","extra":1}`},
		{[]string{"submit-bundle", "--stdin"}, `{"bundle_ref":"r","bundle_id":"b","revision":"v","terms_digest":"d","confirmation":{"desk_action_id":"a","signature":"x"}}`},
		{[]string{"submit-bundle", "--stdin"}, strings.Repeat(" ", bundleSubmitInputMaxBytes+1)},
		{[]string{"bundle-status", "--json"}, goldenBundleRef},
		{[]string{"bundle-status", goldenBundleRef, "--json"}, goldenBundleRef},
		{[]string{"bundle-status", "--bundle-ref-stdin"}, "first\nsecond"},
	} {
		var out bytes.Buffer
		c := &preparedProposalCLIConn{}
		env := &Env{Conn: c, Stdin: strings.NewReader(tc.stdin), Stdout: &out, Stderr: &out}
		if Run(t.Context(), env, "proposals", tc.args) == 0 || c.calls != 0 {
			t.Fatalf("%v reached RPC", tc.args)
		}
	}
}

// The human review never prints the private reference; JSON is the
// backend's handoff and carries it with the exact terms bytes.
func TestPrepareBundlePrintsTheReferenceOnlyAsJSON(t *testing.T) {
	prepared := goldenBundlePrepared()
	conn := goldenConn{rpc.MethodTradeProposalsPrepareBundle: prepared}
	var text, js bytes.Buffer
	if exit := Run(t.Context(), &Env{Conn: conn, Stdout: &text, Stderr: &text}, "proposals", []string{"prepare-bundle", goldenBundleID, goldenBundleRevision}); exit != 0 {
		t.Fatalf("prepare-bundle exit %d: %s", exit, &text)
	}
	if strings.Contains(text.String(), goldenBundleRef) || strings.Contains(text.String(), "canarypb1.") {
		t.Fatal("the human review printed the private reference")
	}
	if exit := Run(t.Context(), &Env{Conn: conn, Stdout: &js, Stderr: &js}, "proposals", []string{"prepare-bundle", "--json", goldenBundleID, goldenBundleRevision}); exit != 0 {
		t.Fatalf("prepare-bundle --json exit %d: %s", exit, &js)
	}
	var got rpc.TradeProposalPrepareBundleResult
	dec := json.NewDecoder(&js)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&got); err != nil || got.BundleRef != goldenBundleRef || got.Terms != prepared.Terms {
		t.Fatalf("JSON handoff = %+v, %v", got, err)
	}
}
