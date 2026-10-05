package stress

import (
	"context"
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/osauer/canary/v2/internal/rpc"
)

type fakeComposeReader struct {
	results map[string]any
	calls   []string
}

func (f *fakeComposeReader) Call(_ context.Context, method string, _, out any) error {
	f.calls = append(f.calls, method)
	value, ok := f.results[method]
	if !ok {
		return &rpc.Error{Code: rpc.CodeUnknownMethod, Message: "unexpected method"}
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, out)
}

func TestComposeReturnsFullAssessmentFromDaemonReads(t *testing.T) {
	reader := &fakeComposeReader{results: map[string]any{
		rpc.MethodAccountSummary:       rpc.AccountResult{},
		rpc.MethodPositionsList:        rpc.PositionsResult{Stocks: []rpc.PositionView{{Symbol: "TEST", SecType: "STK", Quantity: 1}}},
		rpc.MethodMarketEventsSnapshot: rpc.MarketEventsResult{},
		rpc.MethodRegimeSnapshot:       rpc.RegimeSnapshotResult{},
	}}
	got, _, err := Compose(t.Context(), reader)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Rows) != 11 || len(got.MarketIndicators) != 8 || got.InputHealth == "ok" || got.NotExecution == "" {
		t.Fatalf("incomplete or falsely healthy assessment: rows=%d indicators=%d health=%q", len(got.Rows), len(got.MarketIndicators), got.InputHealth)
	}
	if !reflect.DeepEqual(reader.calls, ComposeMethods()) {
		t.Fatalf("unexpected stress read sequence: %v", reader.calls)
	}
}

func TestComposeFailsWithoutAccount(t *testing.T) {
	reader := &fakeComposeReader{results: map[string]any{}}
	if _, _, err := Compose(t.Context(), reader); err == nil || !strings.HasPrefix(err.Error(), "account:") {
		t.Fatalf("missing account was accepted: %v", err)
	}
	if !reflect.DeepEqual(reader.calls, []string{rpc.MethodAccountSummary}) {
		t.Fatalf("composition continued after a missing account: %v", reader.calls)
	}
}

func TestStressSnapshotDeadlineCoversComposeReads(t *testing.T) {
	var sum int64
	for _, method := range ComposeMethods() {
		timing, ok := rpc.LookupMethodTiming(method)
		if !ok {
			t.Fatalf("missing method timing: %s", method)
		}
		sum += int64(timing.DaemonTimeout)
	}
	timing, ok := rpc.LookupMethodTiming(rpc.MethodStressSnapshot)
	if !ok || int64(timing.DaemonTimeout) < sum {
		t.Fatalf("stress.snapshot deadline %v does not cover its reads", timing.DaemonTimeout)
	}
}

// TestOnlyTheDaemonEvaluates keeps every verdict in one process. A reader
// that composes or computes one runs the logic it was compiled with, and a
// pinned reader (Desk compiles the exported client) then disagrees with the
// installed daemon after every judgement fix (2026-10-05). Readers fetch the
// daemon's result; only the daemon, the packages that define the
// evaluations, and the offline regime lab call them.
func TestOnlyTheDaemonEvaluates(t *testing.T) {
	root := filepath.Join("..", "..")
	evaluators := map[string]map[string]bool{
		"stress": {"Compose": true, "ComputeStress": true},
		"rpc": {"BuildRegimeClusterBands": true, "BuildRegimePosture": true, "BuildRegimeLifecycle": true,
			"RegimeHeadline": true, "CompactPositionsRisk": true},
	}
	allowed := map[string]bool{
		filepath.Join("internal", "daemon"): true,
		filepath.Join("internal", "stress"): true,
		filepath.Join("internal", "rpc"):    true,
		"regimelab":                         true,
		"tmp":                               true,
	}
	checked := 0
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		if d.IsDir() {
			if path != root && (allowed[rel] || strings.HasPrefix(d.Name(), ".") || d.Name() == "node_modules") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		checked++
		file, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if err != nil {
			return err
		}
		ast.Inspect(file, func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			if pkg, ok := sel.X.(*ast.Ident); ok && evaluators[pkg.Name][sel.Sel.Name] {
				t.Errorf("%s calls %s.%s; readers fetch the daemon's result, only the daemon evaluates", rel, pkg.Name, sel.Sel.Name)
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if checked < 100 {
		t.Fatalf("checked only %d Go files; the walk is not covering the readers", checked)
	}
}
