package liveswap

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// liveswap is one flat package, so the compiler can never report a
// dependency pointing the wrong way: an import cycle is unreachable by
// construction and every file can see every other. The layering is
// real all the same — #60 found eight edges running backwards, and it
// took a hand-built map to see them. This test is that map, kept.
//
// The rule: a file may use names declared in a lower layer, or in its
// own, and nothing above. Sideways is allowed because a layer is a
// concern, not a file, and a concern is sometimes split (appdirs.go
// and socket.go are one path vocabulary; runner.go and sandbox.go pass
// each other value types). Upward edges are not forbidden outright —
// there are a few deliberate ones — but each must be named in
// allowedUpward with the reason it is right, so a new one has to be
// argued for in review instead of arriving unnoticed.

// fileLayer places every non-test file in the package. Low is leaf.
var fileLayer = map[string]int{
	// 0 — vocabulary with no dependencies of its own.
	"clock.go": 0,
	"names.go": 0,

	// 1 — self-contained mechanisms over that vocabulary.
	"appdirs.go":      1,
	"socket.go":       1,
	"socket_linux.go": 1,
	"socket_other.go": 1,
	"extract.go":      1,
	"allowlist.go":    1,
	"authlimit.go":    1,
	"deploytrust.go":  1,
	"health.go":       1,

	// 2 — the launch contract, and what is written down about it.
	"runner.go":   2,
	"sandbox.go":  2,
	"download.go": 2,
	"state.go":    2,

	// 3 — the one runner implementation and its transport.
	"runner_systemd.go": 3,
	"systemd_dbus.go":   3,

	// 4 — per-app orchestration.
	"app.go":      4,
	"watchdog.go": 4,
	"sweep.go":    4,

	// 5 — the Caddy module surface: config, lifecycle, HTTP.
	"liveswap.go":        5,
	"handler.go":         5,
	"upstreams.go":       5,
	"caddyfile.go":       5,
	"deploytoken_cmd.go": 5,
}

// allowedUpward lists every edge that runs against the layering on
// purpose. An entry that stops being an upward edge is a failure too:
// a stale exception is how a list like this rots into a rubber stamp.
var allowedUpward = map[[2]string]string{
	{"app.go", "liveswap.go"}: "process-lifecycle facts (caddyExiting, " +
		"liveStartedApps) live with the module that owns the pool; Destruct " +
		"reads them to tell a shutdown from an app being removed (#65)",

	{"sweep.go", "liveswap.go"}: "same: the pool is the ledger the sweep asks " +
		"what is still configured (appPool, poolKey, caddyExiting)",

	{"download.go", "app.go"}: "app.go holds the fetcher interface and " +
		"download.go implements it, so the call points down; only the value " +
		"types it is handed (appSpec, deployRequest, validationError) point back",

	{"sandbox.go", "app.go"}: "sandboxSpecFor is a method on *appSpec — " +
		"methods live with the concern, not with the receiver (#65)",

	{"sandbox.go", "liveswap.go"}: "validateEnvFileIsolation is a whole-config " +
		"Validate rule: checking one app's env_file against every other app's " +
		"dirs needs the whole config (#65)",

	{"systemd_dbus.go", "liveswap.go"}: "managerClient is declared where it is " +
		"consumed and asserted where it is implemented (`var _ managerClient`) " +
		"— Go's normal direction for an interface",
}

func TestFileLayering(t *testing.T) {
	fset := token.NewFileSet()
	files := parsePackageFiles(t, fset)

	// name -> the files declaring it. A build-tagged pair (linkSocket in
	// socket_linux.go and socket_other.go) declares one name twice; both
	// sit in the same layer, so an edge to either reads the same.
	declaredIn := map[string][]string{}
	for name, f := range files {
		for _, d := range packageScopeDecls(f) {
			declaredIn[d] = append(declaredIn[d], name)
		}
	}

	for name := range files {
		if _, ok := fileLayer[name]; !ok {
			t.Errorf("%s has no layer: add it to fileLayer in deps_test.go", name)
		}
	}
	for name := range fileLayer {
		if _, ok := files[name]; !ok {
			t.Errorf("fileLayer names %s, which is not a file in the package", name)
		}
	}
	if t.Failed() {
		return
	}

	edges := map[[2]string][]string{}
	for name, f := range files {
		for _, use := range freeIdents(f) {
			for _, decl := range declaredIn[use] {
				if decl == name {
					continue
				}
				e := [2]string{name, decl}
				edges[e] = append(edges[e], use)
			}
		}
	}

	used := map[[2]string]bool{}
	for e, names := range edges {
		from, to := fileLayer[e[0]], fileLayer[e[1]]
		if from >= to {
			continue
		}
		if _, ok := allowedUpward[e]; ok {
			used[e] = true
			continue
		}
		sort.Strings(names)
		t.Errorf("%s (layer %d) uses %s (layer %d): %s\n"+
			"\tthis edge points up. Move the names down into a lower file, or — if the edge\n"+
			"\tis right — add it to allowedUpward in deps_test.go with the reason.",
			e[0], from, e[1], to, strings.Join(dedupe(names), ", "))
	}
	for e, reason := range allowedUpward {
		if strings.TrimSpace(reason) == "" {
			t.Errorf("allowedUpward excuses %s -> %s with no reason: say why the edge is right",
				e[0], e[1])
		}
		if !used[e] {
			t.Errorf("allowedUpward excuses %s -> %s, which is no longer an upward edge: remove it",
				e[0], e[1])
		}
	}
}

// parsePackageFiles reads every non-test .go file in the package,
// including both halves of a build-tagged pair: the layering is a
// property of the source, not of one platform's build.
func parsePackageFiles(t *testing.T, fset *token.FileSet) map[string]*ast.File {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}
	files := map[string]*ast.File{}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(".", name), nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		files[name] = f
	}
	if len(files) == 0 {
		t.Fatal("no package files found")
	}
	return files
}

// packageScopeDecls returns the names a file declares at package scope.
// Methods are excluded: a method call resolves through its receiver,
// and the receiver's type is itself a package-scope name that this
// already counts. Counting method names would make every `.status()`
// on any type an edge to whoever declared a `status`.
func packageScopeDecls(f *ast.File) []string {
	var out []string
	add := func(n string) {
		// `_` is declared at package scope by every interface guard
		// (`var _ runner = ...`) and binds nothing.
		if n != "_" {
			out = append(out, n)
		}
	}
	for _, d := range f.Decls {
		switch d := d.(type) {
		case *ast.FuncDecl:
			if d.Recv == nil {
				add(d.Name.Name)
			}
		case *ast.GenDecl:
			if d.Tok == token.IMPORT {
				continue
			}
			for _, s := range d.Specs {
				switch s := s.(type) {
				case *ast.TypeSpec:
					add(s.Name.Name)
				case *ast.ValueSpec:
					for _, n := range s.Names {
						add(n.Name)
					}
				}
			}
		}
	}
	return out
}

// freeIdents returns identifiers a file uses that it does not itself
// bind: not selector right-hand sides (x.Sel), struct field names,
// composite-literal keys, labels, or import names.
//
// Local shadowing is handled by over-approximation — a name bound
// anywhere in the file as a local is ignored everywhere in it. That
// can hide a real edge (a false negative) but can never invent one, so
// the check does not cry wolf. It reports nothing on this package
// today; see TestFileLayeringShadowing.
func freeIdents(f *ast.File) []string {
	bound := boundNames(f)
	skip := map[*ast.Ident]bool{}
	ast.Inspect(f, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.SelectorExpr:
			skip[n.Sel] = true
		case *ast.Field:
			for _, id := range n.Names {
				skip[id] = true
			}
		case *ast.CompositeLit:
			for _, el := range n.Elts {
				if kv, ok := el.(*ast.KeyValueExpr); ok {
					if id, ok := kv.Key.(*ast.Ident); ok {
						skip[id] = true
					}
				}
			}
		case *ast.LabeledStmt:
			skip[n.Label] = true
		case *ast.BranchStmt:
			if n.Label != nil {
				skip[n.Label] = true
			}
		}
		return true
	})

	var out []string
	for _, d := range f.Decls {
		if g, ok := d.(*ast.GenDecl); ok && g.Tok == token.IMPORT {
			continue
		}
		// The declared name itself is a binding, not a use.
		if fd, ok := d.(*ast.FuncDecl); ok {
			skip[fd.Name] = true
		}
		if g, ok := d.(*ast.GenDecl); ok {
			for _, s := range g.Specs {
				switch s := s.(type) {
				case *ast.TypeSpec:
					skip[s.Name] = true
				case *ast.ValueSpec:
					for _, n := range s.Names {
						skip[n] = true
					}
				}
			}
		}
		ast.Inspect(d, func(n ast.Node) bool {
			id, ok := n.(*ast.Ident)
			if !ok || skip[id] || bound[id.Name] {
				return true
			}
			out = append(out, id.Name)
			return true
		})
	}
	return out
}

// boundNames collects every name a file binds locally: receivers,
// parameters, results, :=, function-scope var/const/type, range and
// type-switch variables.
func boundNames(f *ast.File) map[string]bool {
	bound := map[string]bool{}
	add := func(e ast.Expr) {
		if id, ok := e.(*ast.Ident); ok && id.Name != "_" {
			bound[id.Name] = true
		}
	}
	addFields := func(fl *ast.FieldList) {
		if fl == nil {
			return
		}
		for _, fld := range fl.List {
			for _, id := range fld.Names {
				if id.Name != "_" {
					bound[id.Name] = true
				}
			}
		}
	}
	ast.Inspect(f, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.FuncDecl:
			addFields(n.Recv)
			addFields(n.Type.Params)
			addFields(n.Type.Results)
		case *ast.FuncLit:
			addFields(n.Type.Params)
			addFields(n.Type.Results)
		case *ast.AssignStmt:
			if n.Tok == token.DEFINE {
				for _, l := range n.Lhs {
					add(l)
				}
			}
		case *ast.RangeStmt:
			if n.Tok == token.DEFINE {
				add(n.Key)
				add(n.Value)
			}
		case *ast.TypeSwitchStmt:
			if a, ok := n.Assign.(*ast.AssignStmt); ok {
				for _, l := range a.Lhs {
					add(l)
				}
			}
		case *ast.DeclStmt:
			if g, ok := n.Decl.(*ast.GenDecl); ok {
				for _, s := range g.Specs {
					switch s := s.(type) {
					case *ast.TypeSpec:
						add(s.Name)
					case *ast.ValueSpec:
						for _, id := range s.Names {
							add(id)
						}
					}
				}
			}
		}
		return true
	})
	return bound
}

func dedupe(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

// TestFileLayeringShadowing pins the approximation in freeIdents: no
// file binds a local with the same name as a package-scope declaration
// from another file, so ignoring shadowed names hides no edge today.
// If this fails, the name it reports is one the layering check has
// gone blind to — rename the local, or teach freeIdents real scopes.
func TestFileLayeringShadowing(t *testing.T) {
	fset := token.NewFileSet()
	files := parsePackageFiles(t, fset)

	declaredIn := map[string][]string{}
	for name, f := range files {
		for _, d := range packageScopeDecls(f) {
			declaredIn[d] = append(declaredIn[d], name)
		}
	}

	var blind []string
	for name, f := range files {
		for local := range boundNames(f) {
			for _, decl := range declaredIn[local] {
				if decl != name {
					blind = append(blind, fmt.Sprintf("%s binds %q, declared at package scope in %s", name, local, decl))
				}
			}
		}
	}
	sort.Strings(blind)
	for _, b := range blind {
		t.Error(b)
	}
}
