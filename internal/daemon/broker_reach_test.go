package daemon

import (
	"bufio"
	"os"
	"slices"
	"strings"
	"testing"
)

// dualPaths are the capabilities served on OpenD although their routes still
// reach a direct IBKR read: each handler reads the IBKR connector, finds none
// under OpenD and branches on s.opend to the broker's own answer, which a call
// graph cannot see. The named tests prove the OpenD branch. Each entry leaves
// when its reads move behind brokerReads.
var dualPaths = map[string]string{
	// collectDataHealth and statusHealthSnapshot read the IBKR connector, then
	// take s.opend.Health(); TestOpenDHealthCarriesItsOwnRows proves it.
	"status and data health": "TestOpenDHealthCarriesItsOwnRows",
}

// A route that still reaches a direct IBKR read (tools/brokerreach) is not
// served on OpenD, unless it is a dual path whose OpenD branch is tested.
func TestDirectIBKRReadsAreNotServedOnOpenD(t *testing.T) {
	t.Parallel()
	routes := directIBKRReadRoutes(t)
	if len(routes) == 0 {
		t.Fatal("testdata/direct-ibkr-reads.txt lists no routes")
	}
	s := &Server{}
	needed := map[string]bool{}
	for _, method := range routes {
		c, ok := s.capabilityOf(method)
		if !ok {
			t.Errorf("%s is listed but not routed: rewrite the list (go -C tools run ./brokerreach -write)", method)
			continue
		}
		if c.OpenD.Status != Served {
			continue
		}
		test, dual := dualPaths[c.Name]
		if !dual {
			t.Errorf("%s (%s) is served on OpenD but reaches a direct IBKR read: move the read behind brokerReads, or mark the capability for what OpenD does", method, c.Name)
			continue
		}
		if !slices.Contains(c.OpenD.Tests, test) {
			t.Errorf("dual path %s names %s, which its OpenD coverage does not list", c.Name, test)
		}
		needed[c.Name] = true
	}
	for name := range dualPaths {
		if !needed[name] {
			t.Errorf("dual path %s no longer reaches a direct IBKR read: drop it from dualPaths", name)
		}
	}
}

// directIBKRReadRoutes reads the [routes] section brokerreach writes.
func directIBKRReadRoutes(t *testing.T) []string {
	t.Helper()
	f, err := os.Open("testdata/direct-ibkr-reads.txt")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var routes []string
	inRoutes := false
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		switch {
		case line == "" || strings.HasPrefix(line, "#"):
		case strings.HasPrefix(line, "["):
			inRoutes = line == "[routes]"
		case inRoutes:
			routes = append(routes, line)
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	return routes
}
