package chainrec

import (
	"testing"
	"time"

	"github.com/osauer/canary/v3/internal/marketcal"
)

func ny(t *testing.T, value string) time.Time {
	t.Helper()
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	at, err := time.ParseInLocation("2006-01-02 15:04", value, loc)
	if err != nil {
		t.Fatal(err)
	}
	return at
}

func TestDueSlotRegularSession(t *testing.T) {
	cal := marketcal.New()
	if _, due := DueSlot(cal, ny(t, "2026-10-14 15:29")); due {
		t.Fatal("due before close - 30 min")
	}
	slot, due := DueSlot(cal, ny(t, "2026-10-14 15:30"))
	if !due || slot.SessionDate != "2026-10-14" || !slot.ScheduledAt.Equal(ny(t, "2026-10-14 15:30")) ||
		!slot.Deadline.Equal(ny(t, "2026-10-14 16:00")) || slot.EarlyClose {
		t.Fatalf("slot = %+v due=%v, want 15:30 start and 16:00 deadline", slot, due)
	}
	if _, due := DueSlot(cal, ny(t, "2026-10-14 15:49")); !due {
		t.Fatal("a late start inside the window must still run")
	}
	if _, due := DueSlot(cal, ny(t, "2026-10-14 15:50")); due {
		t.Fatal("due with less than ten minutes to the close")
	}
}

func TestDueSlotEarlyCloseHolidayAndWeekend(t *testing.T) {
	cal := marketcal.New()
	slot, due := DueSlot(cal, ny(t, "2026-11-27 12:30"))
	if !due || !slot.EarlyClose || !slot.Deadline.Equal(ny(t, "2026-11-27 13:00")) {
		t.Fatalf("early close slot = %+v due=%v, want 12:30 start and 13:00 deadline", slot, due)
	}
	if _, due := DueSlot(cal, ny(t, "2026-11-27 15:30")); due {
		t.Fatal("due at the regular time on an early-close day")
	}
	for _, at := range []string{"2026-11-26 15:35", "2026-12-25 15:35", "2026-10-17 15:35", "2029-01-03 15:35"} {
		if _, due := DueSlot(cal, ny(t, at)); due {
			t.Fatalf("due at %s (holiday, weekend or outside calendar coverage)", at)
		}
	}
}

func TestNextSlotSkipsHoliday(t *testing.T) {
	slot, ok := NextSlot(marketcal.New(), ny(t, "2026-11-25 16:00"))
	if !ok || slot.SessionDate != "2026-11-27" || !slot.ScheduledAt.Equal(ny(t, "2026-11-27 12:30")) {
		t.Fatalf("next slot = %+v ok=%v, want the early-close Friday after Thanksgiving", slot, ok)
	}
	slot, ok = NextSlot(marketcal.New(), ny(t, "2026-10-14 09:00"))
	if !ok || slot.SessionDate != "2026-10-14" {
		t.Fatalf("next slot = %+v ok=%v, want today", slot, ok)
	}
}
