package devfiles

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// TestAllErrorsIsComplete parses the package's sources and checks AllErrors
// holds every exported Err* sentinel (matched by its errors.New message), so
// one added without updating AllErrors cannot slip past the web handler's
// completeness test.
func TestAllErrorsIsComplete(t *testing.T) {
	listed := map[string]bool{}
	for _, err := range AllErrors() {
		listed[err.Error()] = true
	}
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	found := 0
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, decl := range f.Decls {
			gd, ok := decl.(*ast.GenDecl)
			if !ok || gd.Tok != token.VAR {
				continue
			}
			for _, spec := range gd.Specs {
				vs := spec.(*ast.ValueSpec)
				for i, id := range vs.Names {
					if !id.IsExported() || !strings.HasPrefix(id.Name, "Err") {
						continue
					}
					found++
					msg, ok := errorsNewMessage(vs, i)
					if !ok {
						t.Errorf("%s (%s) is not an errors.New sentinel", id.Name, fset.Position(id.Pos()))
						continue
					}
					if !listed[msg] {
						t.Errorf("%s (%s) is missing from AllErrors", id.Name, fset.Position(id.Pos()))
					}
				}
			}
		}
	}
	if found != len(AllErrors()) {
		t.Errorf("found %d exported Err* sentinels, AllErrors lists %d", found, len(AllErrors()))
	}
}

// errorsNewMessage returns the string literal of vs's i-th value when it is
// errors.New("...").
func errorsNewMessage(vs *ast.ValueSpec, i int) (string, bool) {
	if i >= len(vs.Values) {
		return "", false
	}
	call, ok := vs.Values[i].(*ast.CallExpr)
	if !ok || len(call.Args) != 1 {
		return "", false
	}
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "New" {
		return "", false
	}
	if pkg, ok := sel.X.(*ast.Ident); !ok || pkg.Name != "errors" {
		return "", false
	}
	lit, ok := call.Args[0].(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return "", false
	}
	s, err := strconv.Unquote(lit.Value)
	return s, err == nil
}
