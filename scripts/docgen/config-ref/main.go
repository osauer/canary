// Command config-ref emits docs/docs/reference/config.md from two kinds of
//
//	for `// docgen:env NAME | description` comments. Name + the
//
// (writes to a tempfile, diffs against the checked-in copy, fails CI
package main

import (
	"bufio"
	"flag"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"

	"github.com/osauer/canary/v2/internal/rpc"
)

const (
	defaultOutput = "docs/docs/reference/config.md"
	envPrefix     = "// docgen:env "
)

// structSource is one generated table: a Go file, the root struct to
// walk, and the markdown framing around the emitted rows.
type structSource struct {
	Path    string // Go source file, repo-relative
	Root    string // root struct name inside that file
	Heading string // markdown H2 title
	Intro   string // paragraph rendered under the heading
}

// structSources drives the generated reference. Adding a settings
var structSources = []structSource{
	{
		Path:    "internal/config/config.go",
		Root:    "Config",
		Heading: "TOML config",
		Intro: "Config file is loaded from `$CANARY_CONFIG`, else `$XDG_CONFIG_HOME/ibkr/config.toml`, else `$HOME/.config/ibkr/config.toml`. " +
			"Every field is optional; absent fields take their documented default. Unknown keys fail the load with a targeted error.",
	},
	{
		Path:    "internal/daemon/protection_policy.go",
		Root:    "protectionPolicy",
		Heading: "Protection policy file",
		Intro: "Loaded from the path in `[auto_trade].policy_file` (default `~/.config/ibkr/policies/protection-policy.toml`). " +
			"No file is required or shipped: when absent, the daemon runs the embedded default — print it with `canary policy default protection`. " +
			"Edits apply only when `policy_version` is bumped (an edited file at an unchanged version reports drift), and unknown keys fail the load. " +
			"This policy shapes advisory protection proposals only; proposals never place broker orders by themselves.",
	},
	{
		Path:    "internal/daemon/opportunity_policy.go",
		Root:    "opportunityPolicy",
		Heading: "Opportunity policy file",
		Intro: "Loaded from the path in `[opportunities].policy_file` (default `~/.config/ibkr/policies/opportunity-policy.toml`). " +
			"Same envelope and reload discipline as the protection policy; print the embedded default with `canary policy default opportunity`. " +
			"Governs advisory option-exercise opportunity detection only.",
	},
}

// tomlField is one row in a generated struct table.
type tomlField struct {
	Path   string // dotted TOML path, e.g. "buckets.theta_hygiene.max_dte"
	GoType string // Go type as written, e.g. "*bool"
	Doc    string // first sentence of the Go doc comment
}

// Section is everything before the last path segment ("" for a
// top-level key); Field is the last segment.
func (f tomlField) Section() string {
	if i := strings.LastIndex(f.Path, "."); i >= 0 {
		return f.Path[:i]
	}
	return ""
}

func (f tomlField) Field() string {
	if i := strings.LastIndex(f.Path, "."); i >= 0 {
		return f.Path[i+1:]
	}
	return f.Path
}

// envVar is one row in the environment variables table.
type envVar struct {
	Name string // e.g. "CANARY_SPX_MEMBERS_AUTO_REFRESH"
	Desc string // free-text description
	File string // source file the docgen comment lives in (for debugging)
}

func main() {
	output := flag.String("o", defaultOutput, "output path (- for stdout)")
	root := flag.String("root", ".", "repo root to scan for // docgen:env comments")
	flag.Parse()

	tables := make([][]tomlField, len(structSources))
	for i, src := range structSources {
		rows, err := parseStructRows(filepath.Join(*root, src.Path), src.Root)
		if err != nil {
			fatal("parse %s: %v", src.Path, err)
		}
		tables[i] = rows
	}
	envs, err := scanEnvVars(*root)
	if err != nil {
		fatal("scan env vars: %v", err)
	}
	if err := validateDocumentedEnvReads(*root, envs); err != nil {
		fatal("validate env vars: %v", err)
	}

	body := render(tables, envs)

	if *output == "-" {
		os.Stdout.WriteString(body)
		return
	}
	if err := os.WriteFile(*output, []byte(body), 0o644); err != nil {
		fatal("write: %v", err)
	}
}

func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}

// parseStructRows walks the AST of path starting at the root struct
func parseStructRows(path, root string) ([]tomlField, error) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, nil, parser.ParseComments)
	if err != nil {
		return nil, err
	}

	structs := map[string]*ast.StructType{}
	forEachStruct(file, func(name string, st *ast.StructType, _ *ast.CommentGroup) {
		structs[name] = st
	})
	rootStruct, ok := structs[root]
	if !ok {
		return nil, fmt.Errorf("struct %s not found in %s", root, path)
	}

	var rows []tomlField
	var walk func(st *ast.StructType, prefix string)
	walk = func(st *ast.StructType, prefix string) {
		for _, f := range st.Fields.List {
			if len(f.Names) == 0 || f.Tag == nil {
				continue
			}
			tag := reflect.StructTag(strings.Trim(f.Tag.Value, "`"))
			name := stripOmit(tag.Get("toml"))
			if name == "" {
				continue
			}
			fieldPath := name
			if prefix != "" {
				fieldPath = prefix + "." + name
			}
			typ := f.Type
			if star, isPtr := typ.(*ast.StarExpr); isPtr {
				typ = star.X
			}
			if m, isMap := typ.(*ast.MapType); isMap {
				if valIdent, isIdent := m.Value.(*ast.Ident); isIdent {
					if child, isStruct := structs[valIdent.Name]; isStruct {
						walk(child, fieldPath+".<name>")
						continue
					}
				}
			}
			if ident, isIdent := typ.(*ast.Ident); isIdent {
				if child, isStruct := structs[ident.Name]; isStruct {
					walk(child, fieldPath)
					continue
				}
			}
			rows = append(rows, tomlField{
				Path:   fieldPath,
				GoType: goTypeName(f.Type),
				Doc:    firstSentence(commentText(f.Doc)),
			})
		}
	}
	walk(rootStruct, "")

	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Section() != rows[j].Section() {
			return rows[i].Section() < rows[j].Section()
		}
		return rows[i].Field() < rows[j].Field()
	})
	return rows, nil
}

// forEachStruct calls fn for every named struct type in file.
func forEachStruct(file *ast.File, fn func(name string, st *ast.StructType, doc *ast.CommentGroup)) {
	for _, decl := range file.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.TYPE {
			continue
		}
		for _, spec := range gen.Specs {
			ts, ok := spec.(*ast.TypeSpec)
			if !ok {
				continue
			}
			st, ok := ts.Type.(*ast.StructType)
			if !ok {
				continue
			}
			// Prefer the per-spec doc, fall back to the GenDecl doc
			// (Go puts the comment on GenDecl when there's exactly
			doc := ts.Doc
			if doc == nil {
				doc = gen.Doc
			}
			fn(ts.Name.Name, st, doc)
		}
	}
}

// stripOmit strips ",omitempty" and similar from a TOML tag.
func stripOmit(s string) string {
	if before, _, ok := strings.Cut(s, ","); ok {
		return before
	}
	return s
}

// goTypeName renders an ast.Expr type back to source-form text.
func goTypeName(e ast.Expr) string {
	switch t := e.(type) {
	case *ast.Ident:
		return t.Name
	case *ast.StarExpr:
		return "*" + goTypeName(t.X)
	case *ast.SelectorExpr:
		return goTypeName(t.X) + "." + t.Sel.Name
	case *ast.MapType:
		return "map[" + goTypeName(t.Key) + "]" + goTypeName(t.Value)
	case *ast.ArrayType:
		return "[]" + goTypeName(t.Elt)
	default:
		return "?"
	}
}

// commentText flattens a CommentGroup to a single line of prose,
// stripping leading "//" markers.
func commentText(cg *ast.CommentGroup) string {
	if cg == nil {
		return ""
	}
	var lines []string
	for _, c := range cg.List {
		line := strings.TrimPrefix(c.Text, "//")
		line = strings.TrimSpace(line)
		if line != "" {
			lines = append(lines, line)
		}
	}
	return strings.Join(lines, " ")
}

// firstSentence returns the first sentence-ish chunk of s. Heuristic:
func firstSentence(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.Index(s, ". "); i > 0 {
		return s[:i+1]
	}
	return s
}

// scanEnvVars walks root for *.go files and collects every
// `// docgen:env NAME | description` comment.
func scanEnvVars(root string) ([]envVar, error) {
	var out []envVar
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			// Skip vendor / hidden dirs / build artefacts.
			base := info.Name()
			if base == "vendor" || base == ".git" || base == "node_modules" || strings.HasPrefix(base, ".") {
				if path != root {
					return filepath.SkipDir
				}
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		defer f.Close()
		sc := bufio.NewScanner(f)
		// Allow long doc comments without bufio.Scanner's 64KiB cap.
		sc.Buffer(make([]byte, 0, 1<<20), 1<<20)
		for sc.Scan() {
			line := strings.TrimSpace(sc.Text())
			if !strings.HasPrefix(line, envPrefix) {
				continue
			}
			rest := strings.TrimPrefix(line, envPrefix)
			parts := strings.SplitN(rest, "|", 2)
			name := strings.TrimSpace(parts[0])
			desc := ""
			if len(parts) == 2 {
				desc = strings.TrimSpace(parts[1])
			}
			if name == "" {
				continue
			}
			out = append(out, envVar{Name: name, Desc: desc, File: path})
		}
		return sc.Err()
	})
	if err != nil {
		return nil, err
	}
	// Dedup by name (in case the same env var is referenced from
	// multiple files — take the first sighting).
	seen := map[string]bool{}
	uniq := make([]envVar, 0, len(out))
	for _, e := range out {
		if seen[e.Name] {
			continue
		}
		seen[e.Name] = true
		uniq = append(uniq, e)
	}
	sort.Slice(uniq, func(i, j int) bool { return uniq[i].Name < uniq[j].Name })
	return uniq, nil
}

// validateDocumentedEnvReads makes the docgen comment convention enforceable.
// CANARY_* or broker-specific IBKR_* string literal or package constant and requires a matching
// // docgen:env row. Dynamic/test-only environment reads are outside the public
func validateDocumentedEnvReads(root string, documented []envVar) error {
	type sourceFile struct {
		path string
		key  string
		file *ast.File
	}

	documentedNames := make(map[string]bool, len(documented))
	for _, env := range documented {
		documentedNames[env.Name] = true
	}
	// Retired inputs may still be read solely to fail closed or preserve an
	// anti-bypass restriction. They are deliberately absent from the canonical
	// configuration reference because they are not supported aliases.
	retiredInputs := map[string]bool{
		"IBKR_AGENT_CONTEXT": true,
		"IBKR_INSTALL_DIR":   true,
	}

	consts := map[string]map[string]string{}
	var files []sourceFile
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			base := info.Name()
			if base == "vendor" || base == ".git" || base == "node_modules" || strings.HasPrefix(base, ".") {
				if path != root {
					return filepath.SkipDir
				}
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return err
		}
		key := filepath.Dir(path) + "|" + file.Name.Name
		if consts[key] == nil {
			consts[key] = map[string]string{}
		}
		for _, decl := range file.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok || gen.Tok != token.CONST {
				continue
			}
			for _, spec := range gen.Specs {
				values, ok := spec.(*ast.ValueSpec)
				if !ok {
					continue
				}
				for i, name := range values.Names {
					if i >= len(values.Values) {
						continue
					}
					lit, ok := values.Values[i].(*ast.BasicLit)
					if !ok || lit.Kind != token.STRING {
						continue
					}
					value, err := strconv.Unquote(lit.Value)
					if err == nil {
						consts[key][name.Name] = value
					}
				}
			}
		}
		files = append(files, sourceFile{path: path, key: key, file: file})
		return nil
	})
	if err != nil {
		return err
	}

	missing := map[string]string{}
	for _, source := range files {
		ast.Inspect(source.file, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok || len(call.Args) != 1 {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			pkg, pkgOK := sel.X.(*ast.Ident)
			if !pkgOK || pkg.Name != "os" || (sel.Sel.Name != "Getenv" && sel.Sel.Name != "LookupEnv") {
				return true
			}
			name := ""
			switch arg := call.Args[0].(type) {
			case *ast.BasicLit:
				if arg.Kind == token.STRING {
					name, _ = strconv.Unquote(arg.Value)
				}
			case *ast.Ident:
				name = consts[source.key][arg.Name]
			}
			if (strings.HasPrefix(name, "CANARY_") || strings.HasPrefix(name, "IBKR_")) &&
				!documentedNames[name] && !retiredInputs[name] {
				missing[name] = source.path
			}
			return true
		})
	}
	if len(missing) == 0 {
		return nil
	}
	names := make([]string, 0, len(missing))
	for name := range missing {
		names = append(names, name)
	}
	sort.Strings(names)
	parts := make([]string, 0, len(names))
	for _, name := range names {
		parts = append(parts, fmt.Sprintf("%s (%s)", name, missing[name]))
	}
	return fmt.Errorf("undocumented production environment reads: %s", strings.Join(parts, ", "))
}

// render emits the final markdown body.
func render(tables [][]tomlField, envs []envVar) string {
	out := &strings.Builder{}
	out.WriteString("# Configuration reference\n\n")
	sourceFiles := make([]string, len(structSources))
	for i, src := range structSources {
		sourceFiles[i] = "`" + src.Path + "`"
	}
	fmt.Fprintf(out, "*Generated by `scripts/docgen/config-ref`. Do not edit by hand — run `make docs-regen` after changing %s or adding/removing a `// docgen:env` comment.*\n\n",
		strings.Join(sourceFiles, ", "))

	for i, src := range structSources {
		fmt.Fprintf(out, "## %s\n\n%s\n\n", src.Heading, src.Intro)
		rows := tables[i]
		if len(rows) == 0 {
			out.WriteString("*No documented fields.*\n\n")
			continue
		}
		out.WriteString("| Section | Field | Type | Description |\n")
		out.WriteString("|---------|-------|------|-------------|\n")
		for _, f := range rows {
			section := "*(top level)*"
			if s := f.Section(); s != "" {
				section = "`[" + s + "]`"
			}
			fmt.Fprintf(out, "| %s | `%s` | `%s` | %s |\n",
				section, f.Field(), f.GoType, escapeTable(f.Doc))
		}
		out.WriteString("\n")
	}

	out.WriteString("## Runtime platform settings\n\n")
	out.WriteString("Daemon-owned preferences persisted in `$XDG_STATE_HOME/ibkr/daemon.db` and changed at runtime without a restart. Feature preferences may be changed via `canary settings set <key>=<value>`, the SPA Settings tab, or `PATCH /api/settings`. ")
	out.WriteString("Setting a key to `null` clears the runtime override, and every response field carries access/source/reason metadata. ")
	out.WriteString("`trading.freeze` and trading-limit keys require `canary settings set` from an interactive human terminal; missing, agent, and paired-device origins are rejected in disabled, paper, and live modes. Trading-limit keys additionally require an experimental trading build with `[trading].mode` set. ")
	out.WriteString("Ownership and semantics: `internal-docs/design/platform-settings.md`.\n\n")
	out.WriteString("| Key | Value | Class | Description |\n")
	out.WriteString("|-----|-------|-------|-------------|\n")
	for _, spec := range rpc.SettingsKeys() {
		key, grammar := spec.Key, ""
		switch spec.Kind {
		case rpc.SettingsKindBool:
			grammar = "true/false/null"
		case rpc.SettingsKindFloat:
			grammar = "number/null"
		case rpc.SettingsKindInt:
			grammar = "integer/null"
		case rpc.SettingsKindDateMap:
			key += ".<SYMBOL>"
			grammar = "YYYY-MM-DD[Tamc/Tbmo]/null"
		case rpc.SettingsKindDateFormat:
			grammar = "us/eu/us_weekday/eu_weekday/null"
		}
		fmt.Fprintf(out, "| `%s` | `%s` | %s | %s |\n", key, grammar, spec.Class, escapeTable(spec.Doc))
	}
	out.WriteString("\n")

	out.WriteString("## Environment variables\n\n")
	out.WriteString("Read at process startup. Override TOML config where applicable; see the per-var description for precedence rules.\n\n")

	if len(envs) == 0 {
		out.WriteString("*No documented environment variables. To document a variable, add a `// docgen:env NAME | description` comment next to its `os.Getenv` site.*\n\n")
	} else {
		out.WriteString("| Variable | Description |\n")
		out.WriteString("|----------|-------------|\n")
		for _, e := range envs {
			fmt.Fprintf(out, "| `%s` | %s |\n", e.Name, escapeTable(e.Desc))
		}
		out.WriteString("\n")
	}
	return out.String()
}

// escapeTable escapes characters that would break a markdown table row.
func escapeTable(s string) string {
	s = strings.ReplaceAll(s, "|", "\\|")
	s = strings.ReplaceAll(s, "\n", " ")
	return s
}
