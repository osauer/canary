package spx

import (
	"os"
	"strings"
	"testing"
	"time"
)

func TestParseHTMLCannotLoseSectorsOrMembersWhenTheColumnMoves(t *testing.T) {
	html, err := os.ReadFile("testdata/wikipedia-snippet.html")
	if err != nil {
		t.Fatal(err)
	}
	members, sectors, err := ParseHTMLWithSectors(html)
	if err != nil {
		t.Fatal(err)
	}
	if len(members) != 6 || sectors["BRK.B"] != "Financials" || sectors["GOOGL"] != "Communication Services" || sectors["MMM"] != "Industrials" {
		t.Fatalf("members=%v sectors=%v", members, sectors)
	}
	// A page without the sector column still yields the membership.
	stripped := []byte(strings.ReplaceAll(string(html), "<th>GICS Sector</th>", "<th>Something Else</th>"))
	members2, sectors2, err := ParseHTMLWithSectors(stripped)
	if err != nil || len(members2) != 6 || len(sectors2) != 0 {
		t.Fatalf("membership must not depend on the sector column: %v %v %v", members2, sectors2, err)
	}
}

func TestSectorRegistryCannotBeBlankedByAShortMap(t *testing.T) {
	before, _ := SectorList()
	if SetSectors(map[string]string{"AAPL": "Information Technology"}, time.Now()) {
		t.Fatal("short map replaced the registry")
	}
	after, _ := SectorList()
	if len(after) != len(before) {
		t.Fatal("registry changed")
	}
	if s, ok := SectorOf("brk-b"); !ok || s != "Financials" {
		t.Fatalf("class-share lookup: %q %v", s, ok)
	}
}
