package daemon

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/rpc"
)

func profileTestRequest(kind string, seconds int) *rpc.Request {
	raw, _ := json.Marshal(rpc.ProfileParams{Kind: kind, Seconds: seconds})
	return &rpc.Request{Method: rpc.MethodProfileCapture, Params: raw}
}

func TestProfileBoundsAndExclusiveCapture(t *testing.T) {
	s := &Server{}
	for _, p := range []rpc.ProfileParams{{Kind: "cpu"}, {Kind: "cpu", Seconds: 121}, {Kind: "heap", Seconds: 1}, {Kind: "allocs", Seconds: -1}} {
		if _, err := s.handleProfile(t.Context(), profileTestRequest(p.Kind, p.Seconds)); err == nil {
			t.Fatal("invalid capture admitted", p)
		}
	}
	processProfileGate.Lock()
	_, err := s.handleProfile(t.Context(), profileTestRequest("cpu", 1))
	processProfileGate.Unlock()
	if err == nil {
		t.Fatal("overlapping capture admitted")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := s.handleProfile(ctx, profileTestRequest("cpu", 1)); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func TestProfileCaptureFilesAndCancellation(t *testing.T) {
	root := t.TempDir()
	t.Setenv("TMPDIR", root)
	t.Setenv("TMP", root)
	t.Setenv("TEMP", root)
	s := &Server{}
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	if _, err := s.handleProfile(ctx, profileTestRequest("cpu", 120)); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 0 {
		t.Fatalf("cancelled capture retained files: %v %v", entries, err)
	}
	for _, kind := range []string{"cpu", "allocs"} {
		start := time.Now()
		result, err := s.handleProfile(t.Context(), profileTestRequest(kind, 1))
		if err != nil {
			t.Fatal(err)
		}
		if time.Since(start) < time.Second || result.PID != os.Getpid() || !result.FinishedAt.After(result.StartedAt) {
			t.Fatal("incorrect process or capture window")
		}
		info, err := os.Stat(result.Directory)
		if err != nil || info.Mode().Perm() != 0o700 {
			t.Fatalf("directory permissions: %v %v", info, err)
		}
		wantFiles := 1
		if kind == "allocs" {
			wantFiles = 2
		}
		if len(result.Files) != wantFiles {
			t.Fatal(result.Files)
		}
		for _, name := range result.Files {
			path := filepath.Join(result.Directory, name)
			info, err := os.Stat(path)
			if err != nil || info.Mode().Perm() != 0o600 {
				t.Fatalf("file permissions: %v %v", info, err)
			}
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			reader, err := gzip.NewReader(bytes.NewReader(raw))
			if err != nil {
				t.Fatal(err)
			}
			body, err := io.ReadAll(reader)
			_ = reader.Close()
			if err != nil || len(body) == 0 {
				t.Fatalf("invalid compressed profile: %v", err)
			}
		}
	}
}

func TestProfileWriterBoundsOutput(t *testing.T) {
	var out bytes.Buffer
	w := &profileWriter{w: &out, remaining: 3}
	if n, err := w.Write([]byte("abc")); n != 3 || err != nil {
		t.Fatal(n, err)
	}
	if _, err := w.Write([]byte("d")); err == nil {
		t.Fatal("unbounded output")
	}
	if _, err := w.Write(nil); err == nil || out.String() != "abc" {
		t.Fatal("write error not retained")
	}
}
