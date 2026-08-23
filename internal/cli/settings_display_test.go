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
