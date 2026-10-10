package netx

import (
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestDialAndListenOnlyHere fails when code outside this package opens a socket
// any other way: through package net directly, through quic-go or http3 helpers
// that open their own, by ListenAndServe, or by an HTTP transport left to dial
// for itself. Each would bypass the keepalive rule.
func TestDialAndListenOnlyHere(t *testing.T) {
	forbidden := map[string]map[string]bool{
		"net": {
			"Dial": true, "DialTimeout": true, "DialTCP": true, "Dialer": true,
			"Listen": true, "ListenTCP": true, "ListenConfig": true,
			"ListenPacket": true, "ListenUDP": true, "DialUDP": true, "ListenMulticastUDP": true,
			"ListenIP": true, "DialIP": true, "ListenUnix": true, "ListenUnixgram": true, "DialUnix": true,
		},
		"github.com/quic-go/quic-go":       {"DialAddr": true, "DialAddrEarly": true, "ListenAddr": true, "ListenAddrEarly": true},
		"github.com/quic-go/quic-go/http3": {"ListenAndServeQUIC": true, "ListenAndServeTLS": true},
	}
	// A transport literal must name its dial function, or it dials on its own.
	mustDial := map[string]map[string]string{
		"net/http":                         {"Transport": "DialContext"},
		"github.com/quic-go/quic-go/http3": {"Transport": "Dial"},
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
		imported := map[string]string{} // local name -> import path
		for _, imp := range f.Imports {
			p := strings.Trim(imp.Path.Value, `"`)
			name := p[strings.LastIndex(p, "/")+1:]
			if p == "github.com/quic-go/quic-go" {
				name = "quic"
			}
			if imp.Name != nil {
				name = imp.Name.Name
			}
			imported[name] = p
		}
		pkgOf := func(e ast.Expr) (string, string, bool) {
			sel, ok := e.(*ast.SelectorExpr)
			if !ok {
				return "", "", false
			}
			id, ok := sel.X.(*ast.Ident)
			if !ok {
				return "", "", false
			}
			p, ok := imported[id.Name]
			return p, sel.Sel.Name, ok
		}
		ast.Inspect(f, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.SelectorExpr:
				if p, name, ok := pkgOf(n); ok && forbidden[p][name] {
					t.Errorf("%s: %s.%s used outside netx", fset.Position(n.Pos()), p, name)
				}
				if n.Sel.Name == "ListenAndServe" || n.Sel.Name == "ListenAndServeTLS" {
					t.Errorf("%s: %s opens its own listener", fset.Position(n.Pos()), n.Sel.Name)
				}
			case *ast.CompositeLit:
				p, name, ok := pkgOf(n.Type)
				field := mustDial[p][name]
				if !ok || field == "" {
					return true
				}
				for _, el := range n.Elts {
					if kv, ok := el.(*ast.KeyValueExpr); ok {
						if k, ok := kv.Key.(*ast.Ident); ok && k.Name == field {
							return true
						}
					}
				}
				t.Errorf("%s: %s.%s without %s dials on its own", fset.Position(n.Pos()), p, name, field)
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

func TestIsClientResource(t *testing.T) {
	for _, errno := range resourceErrnos {
		err := &net.OpError{Op: "dial", Net: "tcp", Err: os.NewSyscallError("connect", errno)}
		if !IsClientResource(err) {
			t.Errorf("%v not classified as a client resource error", err)
		}
	}
	for _, err := range []error{nil, errors.New("connection refused"), &net.OpError{Op: "dial", Err: os.ErrDeadlineExceeded}} {
		if IsClientResource(err) {
			t.Errorf("%v classified as a client resource error", err)
		}
	}
}
