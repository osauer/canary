// Command brokerreach finds every daemon route whose handler can reach a
// direct read of the IBKR connector outside the broker seam, and holds that
// set to a recorded list that may only shrink.
//
// A direct read is a call to (*Server).gatewayConnector or a read of the
// Server.connector field. Under OpenD such a read finds no connector, so a
// route that reaches one either answers "IBKR only" or works through a branch
// on s.opend that a call graph cannot see. The recorded list,
// internal/daemon/testdata/direct-ibkr-reads.txt, is the list of routes still
// to move behind brokerReads (internal/broker); the daemon's
// TestDirectIBKRReadsAreNotServedOnOpenD fails when one of them is marked
// served on OpenD in the coverage table.
//
// Excluded by design, so only reads reachable from route handlers count: the
// seam and the connection's own connect and reconnect (seamFuncs), and the
// order-write path, which internal/broker/doc.go keeps off the read seam
// (writeCapabilities).
//
// The call graph is VTA over CHA (golang.org/x/tools/go/callgraph/vta), built
// for both product builds, default and -tags trading; a route counts when it
// reaches a direct read in either. Run from tools/:
//
//	go run ./brokerreach          check the code against the recorded list
//	go run ./brokerreach -write   rewrite the list from the code
//	go run ./brokerreach -v       also print the reads each route reaches
package main

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"go/ast"
	"go/constant"
	"go/token"
	"go/types"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"golang.org/x/tools/go/callgraph"
	"golang.org/x/tools/go/callgraph/cha"
	"golang.org/x/tools/go/callgraph/vta"
	"golang.org/x/tools/go/packages"
	"golang.org/x/tools/go/ssa"
	"golang.org/x/tools/go/ssa/ssautil"
)

const (
	apiPath  = "github.com/osauer/canary/v2/internal/rpc/api"
	listPath = "internal/daemon/testdata/direct-ibkr-reads.txt"
)

// seamFuncs are the Server methods that own the connector: the read seam and
// the reconnect it triggers. Reads inside them are the seam's own, and the
// search does not follow their calls.
var seamFuncs = []string{
	"(*Server).gatewayConnector",
	"(*Server).brokerReads",
	"(*Server).adapterFor",
	"(*Server).triggerReconnect",
	"(ibkrScope).Scope", // scopeSource's IBKR side; OpenD answers its own scope
}

// writeCapabilities are the coverage-table capabilities whose routes preview
// or send broker writes. Writes stay on a concrete IBKR connector by design
// (internal/broker/doc.go), so their connector reads are not bypasses.
var writeCapabilities = []string{"orders"}

func main() {
	root := flag.String("C", "..", "Canary repository root")
	write := flag.Bool("write", false, "rewrite the recorded list from the code")
	verbose := flag.Bool("v", false, "print the reading functions each route reaches")
	flag.Parse()
	if err := run(*root, *write, *verbose); err != nil {
		fmt.Fprintln(os.Stderr, "brokerreach:", err)
		os.Exit(1)
	}
}

// found is what one or both builds show.
type found struct {
	routes map[string][]string // route -> the functions with direct reads it reaches
	sites  map[string]bool     // "file:line function" of every direct read outside the seam; true for a gatewayConnector call
	calls  int                 // gatewayConnector calls anywhere in the daemon, the seam's own included
}

func run(root string, write, verbose bool) error {
	all := found{routes: map[string][]string{}, sites: map[string]bool{}}
	for _, tags := range []string{"", "trading"} {
		b, err := analyse(root, tags)
		if err != nil {
			return fmt.Errorf("build %q: %w", tags, err)
		}
		maps.Copy(all.sites, b.sites)
		all.calls = max(all.calls, b.calls)
		for route, fns := range b.routes {
			for _, fn := range fns {
				if !slices.Contains(all.routes[route], fn) {
					all.routes[route] = append(all.routes[route], fn)
				}
			}
		}
	}
	routes := slices.Sorted(maps.Keys(all.routes))
	var funcs []string
	for _, fns := range all.routes {
		for _, fn := range fns {
			if !slices.Contains(funcs, fn) {
				funcs = append(funcs, fn)
			}
		}
	}
	slices.Sort(funcs)
	calls := 0
	for _, isCall := range all.sites {
		if isCall {
			calls++
		}
	}
	fmt.Printf("brokerreach: %d read routes reach direct IBKR reads in %d functions; outside the seam the daemon has %d direct reads (%d gatewayConnector calls, %d connector field reads); %d gatewayConnector calls in all\n",
		len(routes), len(funcs), len(all.sites), calls, len(all.sites)-calls, all.calls)
	if verbose {
		for _, r := range routes {
			slices.Sort(all.routes[r])
			fmt.Printf("%s\n\t%s\n", r, strings.Join(all.routes[r], "\n\t"))
		}
	}
	path := filepath.Join(root, listPath)
	if write {
		return writeList(path, routes, funcs)
	}
	recordedRoutes, recordedFuncs, err := readList(path)
	if err != nil {
		return err
	}
	problems := ratchet("route", routes, recordedRoutes)
	problems = append(problems, ratchet("function", funcs, recordedFuncs)...)
	if len(problems) > 0 {
		return errors.New(strings.Join(problems, "\n"))
	}
	return nil
}

// ratchet fails what the code reaches that the list lacks, and what the list
// holds that the code no longer reaches, so the list only shrinks.
func ratchet(kind string, now, recorded []string) []string {
	var problems []string
	for _, x := range now {
		if !slices.Contains(recorded, x) {
			problems = append(problems, fmt.Sprintf("%s %s newly reaches a direct IBKR read: read through brokerReads (internal/broker) instead", kind, x))
		}
	}
	for _, x := range recorded {
		if !slices.Contains(now, x) {
			problems = append(problems, fmt.Sprintf("%s %s no longer reaches a direct IBKR read: shrink %s with go -C tools run ./brokerreach -write", kind, x, listPath))
		}
	}
	return problems
}

func analyse(root, tags string) (*found, error) {
	cfg := &packages.Config{Mode: packages.LoadAllSyntax, Dir: root}
	if tags != "" {
		cfg.BuildFlags = []string{"-tags=" + tags}
	}
	pkgs, err := packages.Load(cfg, "./internal/daemon")
	if err != nil {
		return nil, err
	}
	if packages.PrintErrors(pkgs) > 0 {
		return nil, errors.New("package errors")
	}
	names, err := methodNames(pkgs[0])
	if err != nil {
		return nil, err
	}
	prog, spkgs := ssautil.AllPackages(pkgs, ssa.InstantiateGenerics)
	prog.Build()
	daemon := spkgs[0]
	all := ssautil.AllFunctions(prog)
	graph := vta.CallGraph(all, cha.CallGraph(prog))

	server := daemon.Type("Server")
	if server == nil {
		return nil, errors.New("no daemon.Server")
	}
	seam := map[*ssa.Function]bool{}
	for fn := range all {
		if fn.Pkg == daemon && slices.Contains(seamFuncs, fn.RelString(daemon.Pkg)) {
			seam[fn] = true
		}
	}
	if len(seam) != len(seamFuncs) {
		return nil, fmt.Errorf("found %d of the %d seamFuncs: update the list", len(seam), len(seamFuncs))
	}
	gateway := prog.LookupMethod(types.NewPointer(server.Type()), daemon.Pkg, "gatewayConnector")

	res := &found{routes: map[string][]string{}, sites: map[string]bool{}}
	reads := map[*ssa.Function]bool{} // functions outside the seam with a direct read
	for fn := range all {
		if fn.Pkg != daemon {
			continue
		}
		for _, blk := range fn.Blocks {
			for _, instr := range blk.Instrs {
				isCall, ok := directRead(instr, gateway, server.Type())
				if !ok {
					continue
				}
				if isCall {
					res.calls++
				}
				if seam[fn] {
					continue
				}
				pos := prog.Fset.Position(instr.Pos())
				res.sites[fmt.Sprintf("%s:%d %s", filepath.Base(pos.Filename), pos.Line, fn.RelString(daemon.Pkg))] = isCall
				reads[fn] = true
			}
		}
	}

	routes, err := routeRoots(daemon, names)
	if err != nil {
		return nil, err
	}
	for method, r := range routes {
		if slices.Contains(writeCapabilities, r.capability) {
			continue
		}
		for _, fn := range reachable(graph, r.roots, seam) {
			if reads[fn] {
				res.routes[method] = append(res.routes[method], fn.RelString(daemon.Pkg))
			}
		}
	}
	return res, nil
}

// directRead reports a call to gatewayConnector or a read of Server.connector,
// and which of the two it is.
func directRead(instr ssa.Instruction, gateway *ssa.Function, server types.Type) (isCall, ok bool) {
	switch v := instr.(type) {
	case ssa.CallInstruction:
		return true, v.Common().StaticCallee() == gateway
	case *ssa.FieldAddr:
		ptr, isPtr := v.X.Type().Underlying().(*types.Pointer)
		if !isPtr || !types.Identical(ptr.Elem(), server) {
			return false, false
		}
		if server.Underlying().(*types.Struct).Field(v.Field).Name() != "connector" {
			return false, false
		}
		// Setting the connector is the connection's own work; count reads only.
		if refs := v.Referrers(); refs != nil {
			for _, ref := range *refs {
				if store, isStore := ref.(*ssa.Store); isStore && store.Addr == v {
					return false, false
				}
			}
		}
		return false, true
	}
	return false, false
}

// reachable lists the functions the roots can reach, not following calls out
// of the seam.
func reachable(graph *callgraph.Graph, roots []*ssa.Function, seam map[*ssa.Function]bool) []*ssa.Function {
	seen := map[*ssa.Function]bool{}
	queue := slices.Clone(roots)
	var out []*ssa.Function
	for len(queue) > 0 {
		fn := queue[0]
		queue = queue[1:]
		if seen[fn] {
			continue
		}
		seen[fn] = true
		out = append(out, fn)
		if seam[fn] {
			continue
		}
		// A closure made here may run wherever it is passed.
		queue = append(queue, fn.AnonFuncs...)
		queue = append(queue, funcValues(fn)...)
		if node := graph.Nodes[fn]; node != nil {
			for _, e := range node.Out {
				if !inModule(e.Callee.Func) {
					continue
				}
				queue = append(queue, e.Callee.Func)
			}
		}
	}
	return out
}

const modulePath = "github.com/osauer/canary/v2"

// inModule reports whether fn is Canary's own code, or a wrapper the compiler
// made for it. The walk does not enter the standard library or other modules:
// their shared helpers (sync.Once, encoding/json) would join every caller to
// every callback, since the call graph does not tell callers apart. A callback
// Canary hands them is followed through funcValues instead.
func inModule(fn *ssa.Function) bool {
	for fn.Origin() != nil {
		fn = fn.Origin()
	}
	if fn.Pkg == nil {
		return fn.Synthetic != ""
	}
	path := fn.Pkg.Pkg.Path()
	return path == modulePath || strings.HasPrefix(path, modulePath+"/")
}

// funcValues are the Canary functions fn makes into values (closures, method
// values, function references): any of them may run wherever it is passed.
func funcValues(fn *ssa.Function) []*ssa.Function {
	var out []*ssa.Function
	var ops []*ssa.Value
	for _, blk := range fn.Blocks {
		for _, instr := range blk.Instrs {
			if mc, ok := instr.(*ssa.MakeClosure); ok {
				out = append(out, mc.Fn.(*ssa.Function))
			}
			if c, ok := instr.(ssa.CallInstruction); ok && c.Common().StaticCallee() != nil {
				// A direct call's callee is an edge, not a value; its arguments still count.
				for _, arg := range c.Common().Args {
					if f, ok := arg.(*ssa.Function); ok && inModule(f) {
						out = append(out, f)
					}
				}
				continue
			}
			for _, op := range instr.Operands(ops[:0]) {
				if f, ok := (*op).(*ssa.Function); ok && inModule(f) {
					out = append(out, f)
				}
			}
		}
	}
	return out
}

type routeRoot struct {
	capability string
	roots      []*ssa.Function
}

// routeRoots reads (*Server).routeTable: for each serve call, the method its
// declaration names, the capability and the handler functions it wraps.
func routeRoots(daemon *ssa.Package, names map[string]string) (map[string]routeRoot, error) {
	fn := daemon.Prog.LookupMethod(types.NewPointer(daemon.Type("Server").Type()), daemon.Pkg, "routeTable")
	if fn == nil {
		return nil, errors.New("no (*Server).routeTable")
	}
	routes := map[string]routeRoot{}
	for _, blk := range fn.Blocks {
		for _, instr := range blk.Instrs {
			call, ok := instr.(*ssa.Call)
			if !ok {
				continue
			}
			callee := call.Call.StaticCallee()
			if callee == nil || callee.Origin() == nil || !strings.HasPrefix(callee.Origin().Name(), "serve") || len(call.Call.Args) != 3 {
				continue
			}
			decl := globalName(call.Call.Args[0])
			method, ok := names[decl]
			if !ok {
				return nil, fmt.Errorf("route at %s: no method name for api.%s", daemon.Prog.Fset.Position(call.Pos()), decl)
			}
			capability, _ := call.Call.Args[1].(*ssa.Const)
			if capability == nil {
				return nil, fmt.Errorf("route %s: capability is not a constant", method)
			}
			roots := handlerFuncs(call.Call.Args[2])
			if len(roots) == 0 {
				return nil, fmt.Errorf("route %s: no handler function found", method)
			}
			routes[method] = routeRoot{constant.StringVal(capability.Value), roots}
		}
	}
	if len(routes) == 0 {
		return nil, errors.New("routeTable has no serve calls")
	}
	return routes, nil
}

// globalName is the api variable a serve call's first argument loads.
func globalName(v ssa.Value) string {
	if load, ok := v.(*ssa.UnOp); ok && load.Op == token.MUL {
		if g, ok := load.X.(*ssa.Global); ok && g.Pkg.Pkg.Path() == apiPath {
			return g.Name()
		}
	}
	return ""
}

// handlerFuncs are the functions a handler value is made from: a method
// value, a function literal, or an adapter call around one.
func handlerFuncs(v ssa.Value) []*ssa.Function {
	switch h := v.(type) {
	case *ssa.Function:
		return []*ssa.Function{h}
	case *ssa.MakeClosure:
		return []*ssa.Function{h.Fn.(*ssa.Function)}
	case *ssa.Call:
		var out []*ssa.Function
		for _, arg := range h.Call.Args {
			out = append(out, handlerFuncs(arg)...)
		}
		return out
	case *ssa.ChangeType:
		return handlerFuncs(h.X)
	}
	return nil
}

// methodNames maps each api declaration to the method name it defines:
// AccountSummary = rpc.Define[...](rpc.MethodAccountSummary) maps
// AccountSummary to that constant's value.
func methodNames(daemon *packages.Package) (map[string]string, error) {
	api := daemon.Imports[apiPath]
	if api == nil {
		return nil, errors.New("daemon does not import package api")
	}
	names := map[string]string{}
	for _, file := range api.Syntax {
		for _, decl := range file.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok || gen.Tok != token.VAR {
				continue
			}
			for _, spec := range gen.Specs {
				vs := spec.(*ast.ValueSpec)
				for i, name := range vs.Names {
					if i >= len(vs.Values) {
						continue
					}
					call, ok := vs.Values[i].(*ast.CallExpr)
					if !ok || len(call.Args) != 1 {
						continue
					}
					if tv, ok := api.TypesInfo.Types[call.Args[0]]; ok && tv.Value != nil && tv.Value.Kind() == constant.String {
						names[name.Name] = constant.StringVal(tv.Value)
					}
				}
			}
		}
	}
	if len(names) == 0 {
		return nil, errors.New("no method declarations found in package api")
	}
	return names, nil
}

const listHeader = `# Read routes whose handler can reach a direct IBKR connector read outside
# the broker seam, and the functions that hold those reads, found from the
# code by tools/brokerreach. Each still reads IBKR without going through
# brokerReads (internal/broker). Move a function's read behind the seam, then
# shrink this list with: go -C tools run ./brokerreach -write
# The list only shrinks: make broker-reach-check fails on a route or function
# that newly reaches a direct read, and on one listed here that no longer does.
# A route listed here is not served on OpenD in internal/daemon/coverage.go
# unless TestDirectIBKRReadsAreNotServedOnOpenD names it as a dual path.
`

const routesMark, funcsMark = "[routes]", "[functions]"

func writeList(path string, routes, funcs []string) error {
	var b strings.Builder
	b.WriteString(listHeader)
	b.WriteString("\n" + routesMark + "\n")
	for _, r := range routes {
		b.WriteString(r + "\n")
	}
	b.WriteString("\n" + funcsMark + "\n")
	for _, f := range funcs {
		b.WriteString(f + "\n")
	}
	return os.WriteFile(path, []byte(b.String()), 0o644)
}

func readList(path string) (routes, funcs []string, err error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, fmt.Errorf("recorded list: %w (write it with -write)", err)
	}
	defer f.Close()
	var section *[]string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		switch line := strings.TrimSpace(sc.Text()); {
		case line == "" || strings.HasPrefix(line, "#"):
		case line == routesMark:
			section = &routes
		case line == funcsMark:
			section = &funcs
		case section == nil:
			return nil, nil, fmt.Errorf("%s: %q before %s", path, line, routesMark)
		default:
			*section = append(*section, line)
		}
	}
	return routes, funcs, sc.Err()
}
