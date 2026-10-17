package web

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// TestRegisteredRoutesMatchesRoutes keeps registeredRoutes honest. That list is
// hand-maintained (http.ServeMux enumerates nothing) and drives
// TestNoCORSHeadersOnAnyRoute, so a route added to routes() but not to the list
// would silently drop out of a load-bearing invariant's coverage — a test driven
// from a list that has quietly stopped covering everything is worse than no test.
//
// It reads routes() from source with go/ast rather than exercising the mux,
// because the patterns are only ever strings handed to HandleFunc/Handle: there
// is no runtime observer that can list them back.
func TestRegisteredRoutesMatchesRoutes(t *testing.T) {
	inSource := routePatternsFromSource(t)
	if len(inSource) == 0 {
		t.Fatal("no route patterns parsed out of routes(); the parser stopped matching the code")
	}

	listed := map[string]bool{}
	for _, r := range registeredRoutes {
		if listed[r] {
			t.Errorf("registeredRoutes lists %q twice", r)
		}
		listed[r] = true
	}

	for _, r := range inSource {
		if !listed[r] {
			t.Errorf("routes() registers %q but registeredRoutes does not list it — add it, "+
				"or TestNoCORSHeadersOnAnyRoute stops covering that route", r)
		}
		delete(listed, r)
	}
	for r := range listed {
		t.Errorf("registeredRoutes lists %q but routes() no longer registers it", r)
	}
}

// routePatternsFromSource returns every pattern routes() hands to the mux,
// normalized the way registeredRoutes writes them: a method-less pattern (the
// two catch-alls) is listed under GET, because a driver has to issue something.
func routePatternsFromSource(t *testing.T) []string {
	t.Helper()

	// The whole package is parsed, not server.go alone, so moving routes() to
	// another file does not silently turn this into a no-op.
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package directory: %v", err)
	}
	fset := token.NewFileSet()
	var body *ast.BlockStmt
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if ok && fn.Name.Name == "routes" && fn.Recv != nil && fn.Body != nil {
				body = fn.Body
			}
		}
	}
	if body == nil {
		t.Fatal("could not find func (*Server) routes() in the package source")
	}

	var patterns []string
	ast.Inspect(body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || len(call.Args) == 0 {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || (sel.Sel.Name != "HandleFunc" && sel.Sel.Name != "Handle") {
			return true
		}
		lit, ok := call.Args[0].(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			t.Errorf("%s: route pattern is not a string literal; registeredRoutes cannot be checked "+
				"against it", fset.Position(call.Pos()))
			return true
		}
		pattern, err := strconv.Unquote(lit.Value)
		if err != nil {
			t.Errorf("%s: unquote route pattern: %v", fset.Position(call.Pos()), err)
			return true
		}
		if !strings.Contains(pattern, " ") {
			pattern = "GET " + pattern
		}
		patterns = append(patterns, pattern)
		return true
	})
	sort.Strings(patterns)
	return patterns
}
