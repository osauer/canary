package daemon

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/osauer/canary/v2/internal/rpc"
)

func TestPlatformSettingsV2UpgradeAddsDisplayPreference(t *testing.T) {
	raw := []byte(`{"version":2,"trading_control_generation":7,"features":{},"trading":{},"regime":{},"stress":{},"history":{}}`)
	data, err := decodePlatformSettings(raw)
	if err != nil {
		t.Fatalf("decode v2 settings: %v", err)
	}
	if data.Version != platformSettingsDocVersion || data.TradingControlGeneration != 7 {
		t.Fatalf("upgraded settings = %+v", data)
	}
	if data.Display.DateFormat != nil || displayDateFormatFrom(data) != rpc.DisplayDateFormatUS {
		t.Fatalf("display default = %+v", data.Display)
	}

	withFutureField := strings.Replace(string(raw), `"features":{}`, `"display":{"date_format":"eu"},"features":{}`, 1)
	if _, err := decodePlatformSettings([]byte(withFutureField)); err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("v2 document accepted v3 display field: %v", err)
	}
}

func TestDisplayDateFormatPatchIsClosedAndDoesNotAdvanceTradingGeneration(t *testing.T) {
	current := platformSettingsData{Version: platformSettingsDocVersion, TradingControlGeneration: 11}
	for _, value := range []string{
		rpc.DisplayDateFormatUS,
		rpc.DisplayDateFormatEU,
		rpc.DisplayDateFormatUSWeekday,
		rpc.DisplayDateFormatEUWeekday,
	} {
		next := current
		raw, _ := json.Marshal(value)
		if err := applySettingsKey(&next, "display.date_format", raw); err != nil {
			t.Fatalf("apply %q: %v", value, err)
		}
		if next.Display.DateFormat == nil || *next.Display.DateFormat != value {
			t.Fatalf("stored %q = %+v", value, next.Display.DateFormat)
		}
		if err := deriveTradingControlGeneration(current, &next); err != nil {
			t.Fatalf("derive generation for %q: %v", value, err)
		}
		if next.TradingControlGeneration != current.TradingControlGeneration {
			t.Fatalf("display update advanced trading generation: %d -> %d", current.TradingControlGeneration, next.TradingControlGeneration)
		}
	}

	for _, raw := range []json.RawMessage{json.RawMessage(`"iso"`), json.RawMessage(`true`), json.RawMessage(`42`)} {
		next := current
		if err := applySettingsKey(&next, "display.date_format", raw); err == nil {
			t.Fatalf("invalid display value accepted: %s", raw)
		}
	}

	next := current
	value := rpc.DisplayDateFormatEU
	next.Display.DateFormat = &value
	if err := applySettingsKey(&next, "display.date_format", json.RawMessage(`null`)); err != nil {
		t.Fatalf("clear display preference: %v", err)
	}
	if next.Display.DateFormat != nil || displayDateFormatFrom(next) != rpc.DisplayDateFormatUS {
		t.Fatalf("cleared display preference = %+v", next.Display)
	}
}
