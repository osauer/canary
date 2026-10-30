package daemon

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/rpc"
)

// Owner, 2026-10-30 06:56 CET: the Add bands live in the policy file, the
// daemon writes today's values where the file has none, and Add uses them
// unless the owner overrides them.
func TestAddBandsAreWrittenToThePolicyFileAndReadBack(t *testing.T) {
	before := string(OpportunityPolicyTemplate("v0"))
	cut := strings.Index(before, "\n[add]")
	if cut < 0 {
		t.Fatal("a new opportunity policy file carries no [add] table")
	}
	legacy := []byte(before[:cut+1])
	migrated, changes, _, err := migrateOpportunityPolicyFile(legacy, "v1")
	if err != nil || len(changes) == 0 || !strings.Contains(string(migrated), "\n[add]\n") {
		t.Fatalf("migration without [add]: err=%v changes=%q", err, changes)
	}
	p, err := parseOpportunityPolicy(migrated)
	if err != nil {
		t.Fatal(err)
	}
	want := defaultOpportunityAddPolicy()
	if len(p.Add.Calls) != 4 || len(p.Add.Puts) != 4 || p.Add.Calls[2].Delta[1] != want.Calls[2].Delta[1] || p.Add.Puts[0].Target != .35 || len(p.diagnostics) != 0 {
		t.Fatalf("written bands read back as %+v (diagnostics %v)", p.Add, p.diagnostics)
	}
	if again, changes, _, err := migrateOpportunityPolicyFile(migrated, "v2"); err != nil || len(changes) != 0 || string(again) != string(migrated) {
		t.Fatalf("a second start rewrote the file: %q", changes)
	}

	owned := strings.Replace(string(migrated), "{ days = 90, dte = [76, 105], delta = [0.20, 0.30], target = 0.25 }", "{ days = 90, dte = [76, 105], delta = [0.15, 0.25], target = 0.20 }", 1)
	if again, changes, _, _ := migrateOpportunityPolicyFile([]byte(owned), "v2"); len(changes) != 0 || string(again) != owned {
		t.Fatalf("the owner's band was rewritten: %q", changes)
	}
	path := filepath.Join(privateTestDir(t), "opportunity-policy.toml")
	if err := os.WriteFile(path, []byte(owned), 0o600); err != nil {
		t.Fatal(err)
	}
	m := newOpportunityPolicyManager(path, false, 0, nil)
	m.reload()
	s := &Server{opportunityPolicies: m}
	if b := s.addSide("P").bandForDTE(90); b.Min != .15 || b.Max != .25 || b.Target != .2 {
		t.Fatalf("the owner's put band was not used: %+v", b)
	}
	if b := s.addSide("C").bandForDTE(30); b.Min != .35 || b.Max != .5 || b.Target != .5 {
		t.Fatalf("call band = %+v, want the default", b)
	}
}

// A band the file lists but Add cannot use is reported, and Add keeps
// Canary's bands; option exercise is not paused for it.
func TestAnUnusableAddBandKeepsTheDefaultsAndExercise(t *testing.T) {
	raw := string(OpportunityPolicyTemplate("v0"))
	bad := strings.Replace(raw, "delta = [0.30, 0.40], target = 0.35", "delta = [0.40, 0.30], target = 0.35", 1)
	p, err := parseOpportunityPolicy([]byte(bad))
	if err != nil {
		t.Fatalf("an unusable band refused the whole policy: %v", err)
	}
	if len(p.diagnostics) != 1 || p.diagnostics[0].Key != "add" {
		t.Fatalf("diagnostics = %+v, want one for add", p.diagnostics)
	}
	if effectiveOpportunityPolicy(p) != effectiveOpportunityPolicy(defaultOpportunityPolicyFromTemplate(t)) {
		t.Fatal("the Add bands changed the exercise policy's identity")
	}
	path := filepath.Join(privateTestDir(t), "opportunity-policy.toml")
	if err := os.WriteFile(path, []byte(bad), 0o600); err != nil {
		t.Fatal(err)
	}
	m := newOpportunityPolicyManager(path, false, 0, nil)
	m.reload()
	if b := (&Server{opportunityPolicies: m}).addSide("P").bandForDTE(30); b.Min != .3 || b.Max != .4 {
		t.Fatalf("unusable band served: %+v", b)
	}
}

func defaultOpportunityPolicyFromTemplate(t *testing.T) opportunityPolicy {
	t.Helper()
	p, err := parseOpportunityPolicy(OpportunityPolicyTemplate("v0"))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// The bands judge a put's delta without its sign, and a contract between or
// past horizons takes the nearest earlier horizon's band.
func TestAddSideBands(t *testing.T) {
	puts, calls := defaultAddSide("P"), defaultAddSide("C")
	for _, tc := range []struct {
		side    addSide
		dte     int
		min, mx float64
	}{
		{calls, 3, .5, .7}, {calls, 21, .5, .7}, {calls, 22, .35, .5}, {calls, 57, .25, .4}, {calls, 200, .25, .4},
		{puts, 10, .3, .4}, {puts, 60, .25, .35}, {puts, 90, .2, .3}, {puts, 300, .2, .3},
	} {
		if b := tc.side.bandForDTE(tc.dte); b.Min != tc.min || b.Max != tc.mx {
			t.Errorf("%s %d days: band %+v, want %v–%v", tc.side.noun(), tc.dte, b, tc.min, tc.mx)
		}
	}
	c := testAddContract("P", 35)
	if !puts.holds(c, -.35) || puts.holds(c, .35) || puts.holds(c, -.5) {
		t.Fatal("put delta is not judged by its size")
	}
}

// testAddContract is a standard option expiring dte days from today in New
// York, as addHorizonExpiry counts them.
func testAddContract(right string, dte int) *rpc.ContractParams {
	loc, _ := time.LoadLocation("America/New_York")
	today := time.Now().In(loc)
	expiry := time.Date(today.Year(), today.Month(), today.Day()+dte, 0, 0, 0, 0, time.UTC).Format("20060102")
	return &rpc.ContractParams{ConID: 1, Symbol: "ABC", SecType: "OPT", Exchange: "SMART", Currency: "USD", Expiry: expiry, Strike: 100, Right: right, Multiplier: 100, TradingClass: "ABC"}
}

// testCallSide is the call picker with Canary's default bands.
var testCallSide = defaultAddSide("C")
