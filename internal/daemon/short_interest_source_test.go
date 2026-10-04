package daemon

import (
	"strings"
	"testing"
	"time"
)

const shortInterestTestHeader = "symbolCode|issueName|marketClassCode|currentShortPositionQuantity|previousShortPositionQuantity|averageDailyVolumeQuantity|daysToCoverQuantity|stockSplitFlag|revisionFlag|settlementDate\n"

func TestShortInterestParser(t *testing.T) {
	body := shortInterestTestHeader + "AAA|Example Alpha|NYSE|500|250|100|5||R|2026-09-15\nBBB|Example Beta|OTC|200|100|0|0|S||2026-09-15\nCCC|Example Gamma|ARCA|40|0|100|1|||2026-09-15\n"
	rows, skipped, err := parseShortInterest(body, "2026-09-15")
	if err != nil || skipped != 0 || len(rows) != 3 {
		t.Fatalf("parse=%v %d %d", err, skipped, len(rows))
	}
	if *rows[0].ChangePct != 100 || !rows[0].Revised || *rows[0].DaysToCover != 5 {
		t.Fatal(rows[0])
	}
	if !rows[1].Split || rows[1].ChangePct != nil || rows[1].DaysToCover != nil {
		t.Fatal(rows[1])
	}
	if rows[2].ChangePct != nil || *rows[2].DaysToCover != 1 || rows[2].ShortInterestPctFloat != nil {
		t.Fatal(rows[2])
	}
	for _, bad := range []string{strings.Replace(body, "2026-09-15", "2026-09-14", 1), body + "AAA|Again|NYSE|1|1|1|1|||2026-09-15\n", strings.Replace(body, "symbolCode", "bad", 1)} {
		if _, _, err := parseShortInterest(bad, "2026-09-15"); err == nil {
			t.Fatal("accepted invalid publication")
		}
	}
}

func TestShortInterestSourceLink(t *testing.T) {
	now := time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)
	good := "https://cdn.finra.org/equity/otcmarket/biweekly/shrt20260915.csv"
	url, date, err := shortInterestLatestURL([]byte(good+" https://cdn.finra.org/equity/otcmarket/biweekly/shrt20261231.csv"), now)
	if err != nil || url != good || date != "2026-09-15" {
		t.Fatal(url, date, err)
	}
	for _, bad := range []string{"https://evil.example/shrt20260915.csv", strings.Replace(good, "20260915", "20260115", 1), strings.Replace(good, "20260915", "20269999", 1)} {
		if _, _, err := shortInterestLatestURL([]byte(bad), now); err == nil {
			t.Fatal("bad link accepted")
		}
	}
}
