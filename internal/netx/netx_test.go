package netx

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"
)

// TestDialAndListenOnlyHere fails when code outside this package dials or listens
// through package net directly, which would bypass the keepalive rule.
func TestDialAndListenOnlyHere(t *testing.T) {
	forbidden := map[string]bool{
		"Dial": true, "DialTimeout": true, "DialTCP": true, "Dialer": true,
		"Listen": true, "ListenTCP": true, "ListenConfig": true,
	}
	self, err := filepath.Abs(".")
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join("..", "..")
	fset := token.NewFileSet()
	scanned := 0

	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			abs, err := filepath.Abs(path)
			if err != nil {
				return err
			}
			name := d.Name()
			if abs == self || (path != root && (strings.HasPrefix(name, ".") || name == "testdata" || name == "bin")) {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return err
		}
		scanned++
		netName := ""
		for _, imp := range f.Imports {
			if imp.Path.Value == `"net"` {
				netName = "net"
				if imp.Name != nil {
					netName = imp.Name.Name
				}
			}
		}
		if netName == "" {
			return nil
		}
		ast.Inspect(f, func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			if id, ok := sel.X.(*ast.Ident); ok && id.Name == netName && forbidden[sel.Sel.Name] {
				t.Errorf("%s: net.%s used outside netx", fset.Position(sel.Pos()), sel.Sel.Name)
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if scanned == 0 {
		t.Fatal("no Go files scanned; module root not found")
	}
}
