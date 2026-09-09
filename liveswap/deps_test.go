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
// there are a few deliberate ones — but allowedUpward lists them by the
// names allowed to cross, with the reason, so a new backwards edge has
// to be argued for in review instead of arriving unnoticed.

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

// upward is one deliberate backwards edge: the names that may cross,
// and why that is right. Naming them matters — a pair-level exemption
// would let the *next* upward name from the same file cross unnoticed,
// which is the whole failure this test exists to stop.
type upward struct {
	names  []string
	reason string
}

// allowedUpward lists every edge that runs against the layering on
// purpose. It is checked in both directions: an unlisted name fails,
// and so does a listed name — or a whole entry — that is no longer an
// upward edge. A stale exception is how a list like this rots into a
// rubber stamp.
var allowedUpward = map[[2]string]upward{
	{"app.go", "liveswap.go"}: {
		names: []string{"caddyExiting", "liveStartedApps"},
		reason: "process-lifecycle facts live with the module that owns the " +
			"pool; Destruct reads them to tell a shutdown from an app being " +
			"removed (#65)",
	},

	{"sweep.go", "liveswap.go"}: {
		names: []string{"appPool", "caddyExiting", "poolKey"},
		reason: "same: the pool is the ledger the sweep asks what is still " +
			"configured",
	},

	{"download.go", "app.go"}: {
		names: []string{"appSpec", "deployRequest", "validationError"},
		reason: "app.go holds the fetcher interface and download.go implements " +
			"it, so the call points down; only the value types it is handed " +
			"point back",
	},

	{"sandbox.go", "app.go"}: {
		names: []string{"appSpec"},
		reason: "sandboxSpecFor is a method on *appSpec — methods live with the " +
			"concern, not with the receiver (#65)",
	},

	{"sandbox.go", "liveswap.go"}: {
		names: []string{"App"},
		reason: "validateEnvFileIsolation is a whole-config Validate rule: " +
			"checking one app's env_file against every other app's dirs needs " +
			"the whole config (#65)",
	},

	{"systemd_dbus.go", "liveswap.go"}: {
		names: []string{"managerClient"},
		reason: "the interface is declared where it is consumed and asserted " +
			"where it is implemented (`var _ managerClient`) — Go's normal " +
			"direction for an interface",
	},
}

func TestFileLayering(t *testing.T) {
	fset := token.NewFileSet()
	files := parsePackageFiles(t, fset)

	// name -> the files declaring it. A build-tagged pair (linkSocket in
	// socket_linux.go and socket_other.go) declares one name twice; both
	// sit in the same layer, so an edge to either reads the same.
	structTypes := structTypeNames(files)

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
		for _, use := range freeIdents(f, structTypes) {
			for _, decl := range declaredIn[use] {
				if decl == name {
					continue
				}
				e := [2]string{name, decl}
				edges[e] = append(edges[e], use)
			}
		}
	}

	seen := map[[2]string]map[string]bool{}
	for e, names := range edges {
		from, to := fileLayer[e[0]], fileLayer[e[1]]
		if from >= to {
			continue
		}
		names = dedupe(names)
		sort.Strings(names)
		ex, ok := allowedUpward[e]
		if !ok {
			t.Errorf("%s (layer %d) uses %s (layer %d): %s\n"+
				"\tthis edge points up. Move the names down into a lower file, or — if the edge\n"+
				"\tis right — add it to allowedUpward in deps_test.go with the reason.",
				e[0], from, e[1], to, strings.Join(names, ", "))
			continue
		}
		seen[e] = map[string]bool{}
		allowed := map[string]bool{}
		for _, n := range ex.names {
			allowed[n] = true
		}
		for _, n := range names {
			seen[e][n] = true
			if !allowed[n] {
				t.Errorf("%s (layer %d) uses %s (layer %d): %s\n"+
					"\tthis name is not one of the deliberate upward edges between these two\n"+
					"\tfiles (%s). Move it down, or add it to that allowedUpward entry.",
					e[0], from, e[1], to, n, strings.Join(ex.names, ", "))
			}
		}
	}

	for e, ex := range allowedUpward {
		if strings.TrimSpace(ex.reason) == "" {
			t.Errorf("allowedUpward excuses %s -> %s with no reason: say why the edge is right",
				e[0], e[1])
		}
		if len(ex.names) == 0 {
			t.Errorf("allowedUpward excuses %s -> %s without naming any name", e[0], e[1])
		}
		crossed, ok := seen[e]
		if !ok {
			t.Errorf("allowedUpward excuses %s -> %s, which is no longer an upward edge: remove it",
				e[0], e[1])
			continue
		}
		for _, n := range ex.names {
			if !crossed[n] {
				t.Errorf("allowedUpward excuses %q crossing %s -> %s, which it no longer does: remove it",
					n, e[0], e[1])
			}
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
		// Only files the compiler would compile into this package: a
		// `//go:build ignore` helper in `package main` (the usual shape
		// for a codegen or tool file) declares names that are not this
		// package's, and merging them would invent edges.
		if f.Name.Name != "liveswap" {
			continue
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

// structTypeNames returns the package's type names whose underlying
// type is a struct, so a composite literal of one can be recognised
// without type information.
func structTypeNames(files map[string]*ast.File) map[string]bool {
	out := map[string]bool{}
	for _, f := range files {
		for _, d := range f.Decls {
			g, ok := d.(*ast.GenDecl)
			if !ok || g.Tok != token.TYPE {
				continue
			}
			for _, sp := range g.Specs {
				ts, ok := sp.(*ast.TypeSpec)
				if !ok {
					continue
				}
				if _, ok := ts.Type.(*ast.StructType); ok {
					out[ts.Name.Name] = true
				}
			}
		}
	}
	return out
}

// structLit reports whether a composite literal of this type has field
// names for keys. A nil type is an elided literal nested inside
// another (`[]bindMount{{Source: ...}}`); assuming struct there can
// hide an edge written as a key of a nested *map* literal, which is
// the safe direction — it never invents one.
func structLit(t ast.Expr, structTypes map[string]bool) bool {
	switch t := t.(type) {
	case nil, *ast.StructType:
		return true
	case *ast.Ident:
		return structTypes[t.Name]
	case *ast.SelectorExpr:
		return true // a struct from another package
	case *ast.StarExpr:
		return structLit(t.X, structTypes)
	default:
		return false // map, array, slice: keys are expressions
	}
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
func freeIdents(f *ast.File, structTypes map[string]bool) []string {
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
			// Only a struct literal's keys are field names. A map or
			// array literal's keys are ordinary expressions, and
			// skipping those would hide any edge written as a map key.
			if !structLit(n.Type, structTypes) {
				return true
			}
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
