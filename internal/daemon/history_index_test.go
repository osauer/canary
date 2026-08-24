package daemon

import (
	"context"
	"errors"
	"io"
	"path/filepath"
	"testing"

	"github.com/osauer/canary/v2/internal/daemon/corestore"
	"github.com/osauer/canary/v2/internal/rpc"
)

func TestHistoryHandlersHonorRequestCancellation(t *testing.T) {
	store, err := corestore.Open(t.Context(), corestore.Options{
		Path: filepath.Join(privateTestDir(t), "daemon.db"),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	server := &Server{
		coreStore: store,
		logger:    NewLogger(io.Discard, "error"),
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	tests := []struct {
		name string
		call func() error
	}{
		{
			name: "rules history",
			call: func() error {
				_, err := server.handleRulesHistory(ctx, &rpc.Request{})
				return err
			},
		},
		{
			name: "reconciliation equity",
			call: func() error {
				_, err := server.handleReconEquity(ctx, &rpc.Request{})
				return err
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := test.call(); !errors.Is(err, errHistoryIndexUnavailable) {
				t.Fatalf("error = %v, want %v", err, errHistoryIndexUnavailable)
			}
		})
	}
}
