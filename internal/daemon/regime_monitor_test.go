package daemon

import (
	"bytes"
	"encoding/json"
	"reflect"
	"testing"

	"github.com/osauer/canary/v3/internal/rpc"
)

// regime.monitor answers what a reader built from regime.snapshot until now:
// the snapshot it would have decoded, compacted. Serving it leaves the
// snapshot the daemon keeps as it was.
func TestRegimeMonitorIsTheCompactedSnapshot(t *testing.T) {
	sim := regimeSimWithHYG(t)
	srv, stop := startOpenDServer(t, sim, opendTestAccount)
	defer stop()
	waitOpenDReady(t, srv)
	dispatchedRegime(t, srv)

	snapshot := func() json.RawMessage {
		t.Helper()
		res := dispatchRaw(t, srv, rpc.MethodRegimeSnapshot, `{}`)
		if !res.Ok {
			t.Fatalf("regime.snapshot: %+v", res.Error)
		}
		return res.Result
	}
	// A refresh between two reads changes the snapshot; ask until one
	// snapshot stands on both sides of the monitor read.
	for range 5 {
		before := snapshot()
		res := dispatchRaw(t, srv, rpc.MethodRegimeMonitor, `{}`)
		if !res.Ok {
			t.Fatalf("regime.monitor: %+v", res.Error)
		}
		after := snapshot()
		if !bytes.Equal(before, after) {
			continue
		}
		var regime rpc.RegimeSnapshotResult
		if err := json.Unmarshal(before, &regime); err != nil {
			t.Fatal(err)
		}
		want, err := json.Marshal(rpc.CompactRegimeMonitor(&regime))
		if err != nil {
			t.Fatal(err)
		}
		var got, compacted any
		if err := json.Unmarshal(res.Result, &got); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(want, &compacted); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got, compacted) {
			t.Fatalf("regime.monitor is not the compacted snapshot\ngot  %s\nwant %s", res.Result, want)
		}
		if len(regime.Composite.Verdict) == 0 {
			t.Fatal("the snapshot carried no verdict to compare")
		}
		return
	}
	t.Fatal("the regime snapshot never stood still across a monitor read")
}
