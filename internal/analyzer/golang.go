package analyzer

import (
	"bufio"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"gitlab-doc-platform/internal/evidence"
	"golang.org/x/mod/module"
)

func analyzeGoSource(path, rel, sha string) ([]evidence.Item, error) {
	files := token.NewFileSet()
	f, err := parser.ParseFile(files, path, nil, 0)
	if err != nil {
		return nil, err
	}
	pkg := filepath.ToSlash(filepath.Dir(rel))
	var out []evidence.Item
	add := func(category, typ, value string, n ast.Node) {
		e := item(category, typ, value, rel, files.Position(n.Pos()).Line, files.Position(n.End()).Line, sha)
		e.RelatedComponents = []string{pkg}
		out = append(out, e)
	}
	add("application", "go_package", pkg, f.Name)
	imports := map[string]string{}
	for _, imp := range f.Imports {
		name, _ := strconv.Unquote(imp.Path.Value)
		alias := filepath.Base(name)
		if imp.Name != nil {
			alias = imp.Name.Name
		}
		imports[alias] = name
		add("dependency", "go_import", name, imp)
	}
	for _, decl := range f.Decls {
		if gen, ok := decl.(*ast.GenDecl); ok {
			for _, spec := range gen.Specs {
				if declaration, ok := spec.(*ast.ValueSpec); ok && (gen.Tok == token.CONST || gen.Tok == token.VAR) && evidence.GoApplicationVersionSource(rel) {
					command := strings.HasPrefix(rel, "cmd/")
					if !command || f.Name.Name == "main" {
						for i, name := range declaration.Names {
							switch name.Name {
							case "Version", "AppVersion", "ApplicationVersion", "version", "appVersion", "applicationVersion":
							default:
								continue
							}
							if i >= len(declaration.Values) {
								continue
							}
							literal, ok := declaration.Values[i].(*ast.BasicLit)
							if !ok || literal.Kind != token.STRING {
								continue
							}
							value, err := strconv.Unquote(literal.Value)
							if err == nil && evidence.ValidApplicationVersion(value) {
								add("application", "application_version", value, declaration)
							}
						}
					}
				}
				typ, ok := spec.(*ast.TypeSpec)
				if !ok {
					continue
				}
				structure, ok := typ.Type.(*ast.StructType)
				if !ok {
					continue
				}
				category, kind := "application", "struct"
				for _, field := range structure.Fields.List {
					if selector, ok := field.Type.(*ast.SelectorExpr); ok && selector.Sel.Name == "Model" && imports[goName(selector.X)] == "gorm.io/gorm" {
						category, kind = "data", "model"
					}
					if field.Tag != nil {
						tag, _ := strconv.Unquote(field.Tag.Value)
						if strings.Contains(tag, "gorm:") {
							category, kind = "data", "model"
						}
					}
				}
				add(category, kind, typ.Name.Name, typ)
				if kind == "model" {
					for _, field := range structure.Fields.List {
						for _, name := range field.Names {
							add("data", "model_field", typ.Name.Name+"."+name.Name, field)
						}
					}
				}
			}
		}
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Body == nil {
			continue
		}
		routers := map[string]string{}
		if fn.Type.Params != nil {
			for _, field := range fn.Type.Params.List {
				typ := field.Type
				if pointer, ok := typ.(*ast.StarExpr); ok {
					typ = pointer.X
				}
				if sel, ok := typ.(*ast.SelectorExpr); ok && imports[goName(sel.X)] == "github.com/gin-gonic/gin" {
					if sel.Sel.Name == "Context" {
						add("api", "handler", fn.Name.Name, fn)
					}
					if sel.Sel.Name == "Engine" {
						for _, name := range field.Names {
							routers[name.Name] = ""
						}
					}
				}
			}
		}
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			if assign, ok := n.(*ast.AssignStmt); ok {
				for i, rhs := range assign.Rhs {
					if i >= len(assign.Lhs) {
						break
					}
					call, ok := rhs.(*ast.CallExpr)
					if !ok {
						continue
					}
					sel, ok := call.Fun.(*ast.SelectorExpr)
					if !ok {
						continue
					}
					name := goName(assign.Lhs[i])
					receiver := goName(sel.X)
					if imports[receiver] == "github.com/gin-gonic/gin" && (sel.Sel.Name == "New" || sel.Sel.Name == "Default") {
						routers[name] = ""
					}
					if prefix, ok := routers[receiver]; ok && sel.Sel.Name == "Group" && len(call.Args) > 0 {
						if suffix, ok := goString(call.Args[0]); ok {
							routers[name] = strings.TrimRight(prefix, "/") + suffix
						}
					}
				}
			}
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			receiver := goName(sel.X)
			if imports[receiver] == "os" && (sel.Sel.Name == "Getenv" || sel.Sel.Name == "LookupEnv") && len(call.Args) > 0 {
				if key, ok := goString(call.Args[0]); ok {
					add("configuration", "environment_variable", key, call)
				}
			}
			prefix, known := routers[receiver]
			if !known {
				return true
			}
			if sel.Sel.Name == "Use" {
				for _, arg := range call.Args {
					if name := goName(arg); name != "" {
						add("security", "middleware", name, call)
					}
				}
				return true
			}
			switch sel.Sel.Name {
			case "GET", "POST", "PUT", "PATCH", "DELETE", "HEAD", "OPTIONS", "Any":
				if len(call.Args) > 0 {
					if route, ok := goString(call.Args[0]); ok {
						add("api", "route", sel.Sel.Name+" "+strings.TrimRight(prefix, "/")+route, call)
					} else {
						add("api", "route", "dynamic Gin route path unavailable", call)
						out[len(out)-1].Confidence = evidence.Unavailable
					}
				}
			}
			return true
		})
	}
	return out, nil
}

func analyzeGoSum(path, rel, sha string) ([]evidence.Item, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	var out []evidence.Item
	line := 0
	for scanner.Scan() {
		line++
		fields := strings.Fields(scanner.Text())
		if len(fields) != 3 {
			continue
		}
		version := strings.TrimSuffix(fields[1], "/go.mod")
		if module.Check(fields[0], version) != nil {
			continue
		}
		out = append(out, item("dependency", "go_checksum_module", fields[0]+"@"+version, rel, line, line, sha))
	}
	return out, scanner.Err()
}

func goString(expr ast.Expr) (string, bool) {
	if lit, ok := expr.(*ast.BasicLit); ok && lit.Kind == token.STRING {
		value, err := strconv.Unquote(lit.Value)
		return value, err == nil
	}
	return "", false
}

func goName(expr ast.Expr) string {
	switch n := expr.(type) {
	case *ast.Ident:
		return n.Name
	case *ast.SelectorExpr:
		if parent := goName(n.X); parent != "" {
			return parent + "." + n.Sel.Name
		}
	case *ast.CallExpr:
		return goName(n.Fun)
	}
	return ""
}
