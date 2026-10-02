package cli

import (
	"encoding/json"
	"testing"
)

func TestSettingsPatchAcceptsClosedDateFormatStrings(t *testing.T) {
	for _, assignment := range []string{
		"display.date_format=us",
		"display.date_format=EU",
		"display.date_format=us_weekday",
		"display.date_format=eu_weekday",
		"display.date_format=null",
	} {
		raw, err := settingsPatchFromAssignment(assignment)
		if err != nil {
			t.Fatalf("%s: %v", assignment, err)
		}
		var patch struct {
			Display struct {
				DateFormat any `json:"date_format"`
			} `json:"display"`
		}
		if err := json.Unmarshal(raw, &patch); err != nil {
			t.Fatalf("decode %s: %v", assignment, err)
		}
		if assignment == "display.date_format=EU" && patch.Display.DateFormat != "eu" {
			t.Fatalf("normalized patch = %#v", patch.Display.DateFormat)
		}
	}
}

func TestSettingsPatchAcceptsCashSweepPriorityAndClear(t *testing.T) {
	for _, v := range []string{"usd_first", "balanced", "eur_first", "null"} {
		raw, err := settingsPatchFromAssignment("cash_sweep.currency_priority=" + v)
		if err != nil {
			t.Fatal(err)
		}
		var patch struct {
			CashSweep struct {
				CurrencyPriority *string `json:"currency_priority"`
			} `json:"cash_sweep"`
		}
		if err = json.Unmarshal(raw, &patch); err != nil {
			t.Fatal(err)
		}
		if v == "null" {
			if patch.CashSweep.CurrencyPriority != nil {
				t.Fatal("clear lost")
			}
		} else if patch.CashSweep.CurrencyPriority == nil || *patch.CashSweep.CurrencyPriority != v {
			t.Fatal("priority lost")
		}
	}
}
