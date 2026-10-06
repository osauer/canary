//go:build trading

package daemon_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/osauer/canary/v2/internal/cli"
	"github.com/osauer/canary/v2/internal/daemon"
	"github.com/osauer/canary/v2/internal/rpc"
)

// updateBundleContract rewrites the Desk contract goldens:
//
//	go test -tags trading ./internal/daemon -run TestLevelingBundleDeskContract -update-bundle-contract
var updateBundleContract = flag.Bool("update-bundle-contract", false, "rewrite internal/daemon/testdata/bundle-contract from the current answers")

const bundleContractDir = "testdata/bundle-contract"

// Desk reaches a leveling repayment only through the canary CLI, without a
// shell, with the private reference on standard input and the agent origin
// (Desk's execution_canary.go). This runs the three commands in process
// against the trading rig, the way Desk runs them, and pins their JSON
// answers as goldens Desk copies into its own contract tests. Every random
// identity in them is replaced by a stable synthetic one, and the terms
// digest is recomputed over the normalised terms, so the goldens stay
// self-consistent.
func TestLevelingBundleDeskContract(t *testing.T) {
	c := daemon.NewLevelingBundleContract(t)
	run := func(stdin string, args ...string) []byte {
		t.Helper()
		var out, stderr bytes.Buffer
		env := &cli.Env{Stdout: &out, Stderr: &stderr, Conn: c, Origin: rpc.OrderOriginAgent}
		if stdin != "" {
			env.Stdin = strings.NewReader(stdin)
		}
		if code := cli.Run(t.Context(), env, "proposals", args); code != 0 {
			t.Fatalf("canary proposals %s: exit %d: %s", strings.Join(args, " "), code, &stderr)
		}
		return out.Bytes()
	}
	prepareOut := run("", "prepare-bundle", "--json", c.BundleID, c.Revision)
	var prepared rpc.TradeProposalPrepareBundleResult
	if err := json.Unmarshal(prepareOut, &prepared); err != nil || !prepared.Accepted || prepared.TermsDigest == "" {
		t.Fatalf("prepare-bundle = %s (%v)", prepareOut, err)
	}
	submitIn, err := json.Marshal(map[string]any{"bundle_ref": prepared.BundleRef, "bundle_id": c.BundleID, "revision": c.Revision, "terms_digest": prepared.TermsDigest,
		"confirmation": map[string]string{"desk_action_id": "desk-action-synthetic", "credential": "credential-synthetic", "envelope": "envelope-synthetic"}})
	if err != nil {
		t.Fatal(err)
	}
	submitOut := run(string(submitIn), "submit-bundle", "--stdin", "--json")
	statusOut := run(prepared.BundleRef+"\n", "bundle-status", "--bundle-ref-stdin", "--json")

	var submitted rpc.TradeProposalSubmitBundleResult
	var status rpc.TradeProposalPreparedBundleStatusResult
	if err := json.Unmarshal(submitOut, &submitted); err != nil || !submitted.Accepted || submitted.Outcome != rpc.BundleOutcomeSent || submitted.Sent != 2 || c.Orders() != 2 {
		t.Fatalf("submit-bundle = %s (orders %d, %v)", submitOut, c.Orders(), err)
	}
	if err := json.Unmarshal(statusOut, &status); err != nil || status.Outcome != rpc.BundleOutcomeSent || status.Sent != 2 || len(status.Legs) != 2 {
		t.Fatalf("bundle-status = %s (%v)", statusOut, err)
	}
	for name, out := range map[string][]byte{"prepare-bundle": prepareOut, "submit-bundle": submitOut, "bundle-status": statusOut} {
		if bytes.Contains(out, []byte("canarypp1.")) {
			t.Fatalf("%s carries a conversion's private reference", name)
		}
		if name != "prepare-bundle" && bytes.Contains(out, []byte("canarypb1.")) {
			t.Fatalf("%s carries the bundle's private reference", name)
		}
	}
	answers := normaliseBundleContract(t, prepared, map[string][]byte{"prepare-bundle.json": prepareOut, "submit-bundle.json": submitOut, "bundle-status.json": statusOut})
	for name, got := range answers {
		path := filepath.Join(bundleContractDir, name)
		if *updateBundleContract {
			if err := os.MkdirAll(bundleContractDir, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, got, 0o644); err != nil {
				t.Fatal(err)
			}
			continue
		}
		want, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("%v; create it with: go test -tags trading ./internal/daemon -run TestLevelingBundleDeskContract -update-bundle-contract", err)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("%s changed; Desk copies it, so review the diff and update both:\n%s", path, got)
		}
	}
}

// normaliseBundleContract replaces every random identity (the bundle's
// reference and preparation, each conversion's preparation, preview token,
// order reference and draft fingerprint) with a stable synthetic one, then
// recomputes the terms digest over the normalised terms.
func normaliseBundleContract(t *testing.T, prepared rpc.TradeProposalPrepareBundleResult, answers map[string][]byte) map[string][]byte {
	t.Helper()
	var terms rpc.LevelingBundleTerms
	if err := json.Unmarshal([]byte(prepared.Terms), &terms); err != nil {
		t.Fatal(err)
	}
	id := func(n byte) string { return base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{n}, 16)) }
	// The reference names its record, as Canary's does: its first part is
	// the bundle's preparation id.
	replace := []string{prepared.BundleRef, "canarypb1." + id(0xb0) + "." + id(0xb2), terms.PreparationID, id(0xb0)}
	for i, leg := range terms.Legs {
		n := byte(i + 1)
		replace = append(replace, leg.PreparationID, id(0xa0+n), leg.PreviewTokenID, id(0xc0+n),
			leg.OrderRef, fmt.Sprintf("canary-synthetic-order-%d", n), leg.DraftFingerprint, strings.Repeat(fmt.Sprintf("%x", 0xd0+n), 32))
	}
	r := strings.NewReplacer(replace...)
	out := map[string][]byte{}
	for name, raw := range answers {
		out[name] = []byte(r.Replace(string(raw)))
	}
	var normalised rpc.TradeProposalPrepareBundleResult
	if err := json.Unmarshal(out["prepare-bundle.json"], &normalised); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(normalised.Terms))
	digest := strings.NewReplacer(prepared.TermsDigest, "sha256:"+hex.EncodeToString(sum[:]))
	for name, raw := range out {
		out[name] = []byte(digest.Replace(string(raw)))
	}
	return out
}
