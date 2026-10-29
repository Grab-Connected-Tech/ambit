package checker

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/importer"
	"go/token"
	"go/types"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
)

const (
	amPkg      = "github.com/Grab-Connected-Tech/ambit/am"
	runtimePkg = "github.com/Grab-Connected-Tech/ambit/runtime"
)

// typeCheck resolves what every name in the app's packages refers to, so a rule can
// ask "is this runtime.View.Get" instead of "does the text contain .Get(". Imports
// come from the export data `go list -export` leaves in the build cache, which keeps
// the dependency set at the standard library (§2.10). Test files are not type-checked
// (their imports are not in the export set) and keep the syntactic rules.
func typeCheck(root string, fset *token.FileSet, parsed map[string]*ast.File) (*types.Info, map[*ast.File]bool, error) {
	cmd := exec.Command("go", "list", "-export", "-deps", "-json=ImportPath,Dir,Export,GoFiles,CgoFiles,Standard", "./...")
	cmd.Dir = root
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, nil, fmt.Errorf("go list: %v: %s", err, strings.TrimSpace(stderr.String()))
	}
	type pkg struct {
		ImportPath, Dir, Export string
		GoFiles, CgoFiles       []string
		Standard                bool
	}
	var pkgs []pkg
	exports := map[string]string{}
	for dec := json.NewDecoder(bytes.NewReader(out)); ; {
		var p pkg
		if err := dec.Decode(&p); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			return nil, nil, err
		}
		exports[p.ImportPath] = p.Export
		pkgs = append(pkgs, p)
	}
	imp := importer.ForCompiler(fset, "gc", func(path string) (io.ReadCloser, error) {
		if f := exports[path]; f != "" {
			return os.Open(f)
		}
		return nil, fmt.Errorf("no export data for %s", path)
	})
	info := &types.Info{
		Types:      map[ast.Expr]types.TypeAndValue{},
		Uses:       map[*ast.Ident]types.Object{},
		Defs:       map[*ast.Ident]types.Object{},
		Selections: map[*ast.SelectorExpr]*types.Selection{},
	}
	typed := map[*ast.File]bool{}
	absRoot, _ := filepath.Abs(root)
	var failed []string
	for _, p := range pkgs {
		if p.Standard || len(p.CgoFiles) > 0 || (p.Dir != absRoot && !strings.HasPrefix(p.Dir, absRoot+string(filepath.Separator))) {
			continue
		}
		var files []*ast.File
		for _, name := range p.GoFiles {
			if f := parsed[filepath.Join(p.Dir, name)]; f != nil {
				files = append(files, f)
			}
		}
		if len(files) == 0 || len(files) != len(p.GoFiles) {
			continue // a skipped folder (client/, migrations/) or a file that did not parse
		}
		conf := types.Config{Importer: imp, Error: func(error) {}}
		if _, err := conf.Check(p.ImportPath, fset, files, info); err != nil {
			failed = append(failed, p.ImportPath+": "+err.Error())
			continue
		}
		for _, f := range files {
			typed[f] = true
		}
	}
	if len(failed) > 0 {
		return info, typed, errors.New(strings.Join(failed, "; "))
	}
	return info, typed, nil
}

// callee is the function or method a call resolves to, or nil for a conversion, a
// builtin or a call through a func value.
func (v *visitor) callee(call *ast.CallExpr) *types.Func {
	fun := ast.Unparen(call.Fun)
	switch f := fun.(type) {
	case *ast.IndexExpr:
		fun = f.X
	case *ast.IndexListExpr:
		fun = f.X
	}
	var id *ast.Ident
	switch f := fun.(type) {
	case *ast.Ident:
		id = f
	case *ast.SelectorExpr:
		id = f.Sel
	default:
		return nil
	}
	fn, _ := v.info.Uses[id].(*types.Func)
	return fn
}

// named strips pointers and aliases down to the declared type, generic origin included.
func named(t types.Type) *types.TypeName {
	t = types.Unalias(t)
	if p, ok := t.(*types.Pointer); ok {
		t = types.Unalias(p.Elem())
	}
	if n, ok := t.(*types.Named); ok {
		return n.Origin().Obj()
	}
	return nil
}

func isType(obj *types.TypeName, pkgPath string, names ...string) bool {
	return obj != nil && obj.Pkg() != nil && obj.Pkg().Path() == pkgPath && slices.Contains(names, obj.Name())
}

// recvOf is the declared type a method is defined on, or nil for a plain function.
func recvOf(fn *types.Func) *types.TypeName {
	if r := fn.Signature().Recv(); r != nil {
		return named(r.Type())
	}
	return nil
}

func isFunc(fn *types.Func, pkgPath string, names ...string) bool {
	return fn != nil && fn.Pkg() != nil && fn.Pkg().Path() == pkgPath && recvOf(fn) == nil &&
		(len(names) == 0 || slices.Contains(names, fn.Name()))
}

// takesContext reports a first parameter of type context.Context: on the scoped DB
// types that is exactly the set of methods that run a statement, as opposed to the
// builders (Q, Where, OrderBy, With) that only assemble one.
func takesContext(fn *types.Func) bool {
	ps := fn.Signature().Params()
	return ps.Len() > 0 && isType(named(ps.At(0).Type()), "context", "Context")
}

// isDBCallTyped is a call that runs SQL through the action's scoped database.
func isDBCallTyped(fn *types.Func) bool {
	return fn != nil && isType(recvOf(fn), runtimePkg, "View", "Query", "DB") && takesContext(fn)
}

// isEffectCallTyped is a call that records or runs a declared effect.
func isEffectCallTyped(fn *types.Func) bool {
	return fn != nil && (isType(recvOf(fn), runtimePkg, "EffectHandle") ||
		(isType(recvOf(fn), runtimePkg, "DB") && fn.Name() == "Enqueue"))
}

// isLoggerTyped is a call whose arguments end up in a log line or an error message.
func isLoggerTyped(fn *types.Func) bool {
	if fn == nil {
		return false
	}
	if isType(recvOf(fn), "log/slog", "Logger") || isFunc(fn, "fmt") || isFunc(fn, "errors") || isFunc(fn, "log") {
		return true
	}
	// am.Invalid, am.Invariant, gen.ErrX(why...): every constructor of an *am.Error.
	res := fn.Signature().Results()
	return res.Len() > 0 && isType(named(res.At(0).Type()), amPkg, "Error")
}

// isGenPkg is a generated package: the only place resource and enum types live.
func isGenPkg(p *types.Package) bool {
	return p != nil && (p.Name() == "gen" || strings.HasSuffix(p.Path(), "/gen"))
}
