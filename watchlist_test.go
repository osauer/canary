package canary_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/osauer/canary/v2"
	"github.com/osauer/canary/v2/canarytest"
)

func TestWatchlistClientReadsCanonicalEmptyWithoutModelMutation(t *testing.T) {
	s := canarytest.Serve(t)
	s.Handle("watchlist.list", func(_ context.Context, _ json.RawMessage) (json.RawMessage, error) {
		return json.Marshal(canary.Watchlist{Version: 1, Revision: 1, Symbols: []canary.WatchlistContract{}})
	})
	c := canary.New(canary.Options{SocketPath: s.SocketPath()})
	out, err := c.Watchlist(t.Context())
	if err != nil || out.Revision != 1 || out.Symbols == nil || len(out.Symbols) != 0 {
		t.Fatal(out, err)
	}
	for _, tool := range canary.Tools() {
		for _, method := range tool.Methods {
			if method == "watchlist.replace" || method == "watchlist.add" || method == "watchlist.remove" {
				t.Fatal("model mutation exposed")
			}
		}
	}
}
