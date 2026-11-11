package daemon

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// Every method belongs to a capability the table lists, once.
func TestEveryMethodNamesACapability(t *testing.T) {
	t.Parallel()
	seen := map[string]bool{}
	for _, c := range capabilities {
		if seen[c.Name] {
			t.Errorf("capability %q is listed twice", c.Name)
		}
		seen[c.Name] = true
		for _, side := range []Coverage{c.IBKR, c.OpenD} {
			if !slices.Contains([]CoverageStatus{Served, Partial, Unverified, Refused, NotServed}, side.Status) {
				t.Errorf("%s: status %q", c.Name, side.Status)
			}
			if side.Status != Served && side.Status != Unverified && side.Note == "" {
				t.Errorf("%s: a %s capability says why", c.Name, side.Status)
			}
		}
	}
	s := &Server{}
	for method := range dispatchMethodValues(t, nil) {
		if _, ok := s.capabilityOf(method); !ok {
			t.Errorf("%s names no capability in the coverage table", method)
		}
	}
}

// A proof the table cites exists: each test function in the tree, each smoke
// probe in scripts/wire-smoke.sh.
func TestCoverageProofsExist(t *testing.T) {
	t.Parallel()
	var tests strings.Builder
	for _, glob := range []string{"*_test.go", filepath.Join("..", "..", "test", "integration", "*_test.go")} {
		files, err := filepath.Glob(glob)
		if err != nil {
			t.Fatal(err)
		}
		for _, f := range files {
			body, err := os.ReadFile(f)
			if err != nil {
				t.Fatal(err)
			}
			tests.Write(body)
		}
	}
	smoke, err := os.ReadFile(filepath.Join("..", "..", "scripts", "wire-smoke.sh"))
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range capabilities {
		for _, side := range []Coverage{c.IBKR, c.OpenD} {
			for _, name := range side.Tests {
				if !regexp.MustCompile(`func ` + regexp.QuoteMeta(name) + `\(`).MatchString(tests.String()) {
					t.Errorf("%s cites %s, which is not a test", c.Name, name)
				}
			}
			for _, probe := range side.Smoke {
				if probe == "status" {
					if !strings.Contains(string(smoke), `status --json`) {
						t.Errorf("%s cites the smoke's status check, which is gone", c.Name)
					}
					continue
				}
				if !regexp.MustCompile(`(?m)^run_probe ` + regexp.QuoteMeta(probe) + ` `).Match(smoke) {
					t.Errorf("%s cites smoke probe %s, which the smoke does not run", c.Name, probe)
				}
			}
		}
	}
}

// listedThoughPartial are the words the status row keeps although their
// capability is Partial on OpenD, because "not on OpenD" is still the truer
// line for the reader. Each entry leaves when its reason does.
var listedThoughPartial = map[string]bool{
	// Answers only from breadth stored before; OpenD never refreshes it, and
	// the decision on it is open.
	"breadth": true,
}

// The status row and the brief say what OpenD does not serve in a short list.
// It may say less than the table, never that something OpenD serves, even in
// part, is missing.
func TestStatusRowListsOnlyWhatOpenDDoesNotServe(t *testing.T) {
	t.Parallel()
	status := func(word string) (CoverageStatus, bool) {
		i := slices.IndexFunc(capabilities, func(c Capability) bool { return c.Name == word })
		if i < 0 {
			return "", false
		}
		return capabilities[i].OpenD.Status, true
	}
	for _, word := range opendNotServed {
		st, ok := status(word)
		switch {
		case !ok:
			t.Errorf("the status row lists %q, which is no capability", word)
		case st == Partial && listedThoughPartial[word]:
		case st == Served || st == Partial || st == Unverified:
			t.Errorf("the status row lists %q as not on OpenD, but the table says %s", word, st)
		}
	}
	for word := range listedThoughPartial {
		if !slices.Contains(opendNotServed, word) {
			t.Errorf("%q is no longer in the status row: drop it from listedThoughPartial", word)
		} else if st, _ := status(word); st != Partial {
			t.Errorf("%q is %s on OpenD, no longer partial: drop it from listedThoughPartial", word, st)
		}
	}
}

// What a capability needs is an input the table lists, and its OpenD status
// fits what OpenD supplies: nothing that answers needs data OpenD does not
// have, and nothing served or partial rests on a question still open.
func TestCoverageFitsWhatEachBrokerSupplies(t *testing.T) {
	t.Parallel()
	for _, c := range capabilities {
		for _, need := range c.Needs {
			supply := inputSupply(need)
			switch {
			case supply == "":
				t.Errorf("%s needs %q, which is no input", c.Name, need)
			case supply == NotSupplied && (c.OpenD.Status == Served || c.OpenD.Status == Partial || c.OpenD.Status == Unverified):
				t.Errorf("%s is %s on OpenD but needs %s, which OpenD does not supply", c.Name, c.OpenD.Status, need)
			case supply == ToVerify && (c.OpenD.Status == Served || c.OpenD.Status == Partial):
				t.Errorf("%s is %s on OpenD but needs %s, which is not yet known to be supplied", c.Name, c.OpenD.Status, need)
			}
		}
	}
}
