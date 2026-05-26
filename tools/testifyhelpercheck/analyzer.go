package testifyhelpercheck

import (
	"go/ast"
	"go/types"
	"strings"

	"golang.org/x/tools/go/analysis"
)

var Analyzer = &analysis.Analyzer{
	Name: "testifyhelpercheck",
	Doc:  "checks testify helper usage in tests",
	Run:  run,
}

const assertDiagnosticMessage = "test has %d direct testify package calls; create a local assert helper with assert := Assert.New(t) and use it for repeated checks"
const requireDiagnosticMessage = "test has %d direct testify package calls; create a local require helper with require := require.New(t) and use it for repeated checks"

func run(pass *analysis.Pass) (any, error) {
	for _, file := range pass.Files {
		if !strings.HasSuffix(pass.Fset.Position(file.Pos()).Filename, "_test.go") {
			continue
		}

		imports := importNames(file)
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || !isTestFuncDecl(fn, imports.testing) {
				continue
			}
			analyzeBody(pass, fn.Body, fn.Type.Params.List[0].Names[0].Name, imports)
		}

		ast.Inspect(file, func(n ast.Node) bool {
			lit, ok := n.(*ast.FuncLit)
			if !ok || !isTestLikeFuncType(lit.Type, imports.testing) {
				return true
			}
			paramName := lit.Type.Params.List[0].Names[0].Name
			analyzeBody(pass, lit.Body, paramName, imports)
			return false
		})
	}

	return nil, nil
}

type importSet struct {
	testing map[string]struct{}
}

func importNames(file *ast.File) importSet {
	set := importSet{
		testing: make(map[string]struct{}),
	}

	for _, spec := range file.Imports {
		path := strings.Trim(spec.Path.Value, "\"")
		name := ""
		if spec.Name != nil {
			name = spec.Name.Name
		}
		switch path {
		case "testing":
			if name == "" {
				name = "testing"
			}
			set.testing[name] = struct{}{}
		}
	}

	return set
}

func isTestFuncDecl(fn *ast.FuncDecl, testingImports map[string]struct{}) bool {
	if fn.Body == nil || fn.Name == nil || !strings.HasPrefix(fn.Name.Name, "Test") {
		return false
	}
	return isTestLikeFuncType(fn.Type, testingImports)
}

func isTestLikeFuncType(fnType *ast.FuncType, testingImports map[string]struct{}) bool {
	if fnType == nil || fnType.Params == nil || len(fnType.Params.List) != 1 {
		return false
	}
	field := fnType.Params.List[0]
	if len(field.Names) != 1 {
		return false
	}
	return isTestingT(field.Type, testingImports)
}

func isTestingT(expr ast.Expr, testingImports map[string]struct{}) bool {
	star, ok := expr.(*ast.StarExpr)
	if !ok {
		return false
	}
	sel, ok := star.X.(*ast.SelectorExpr)
	if !ok || sel.Sel == nil || sel.Sel.Name != "T" {
		return false
	}
	pkg, ok := sel.X.(*ast.Ident)
	if !ok {
		return false
	}
	_, ok = testingImports[pkg.Name]
	return ok
}

func analyzeBody(pass *analysis.Pass, body *ast.BlockStmt, tName string, imports importSet) {
	if body == nil {
		return
	}

	var assertHelper, requireHelper helperState
	var assertCallPositions []ast.Node
	var requireCallPositions []ast.Node

	var visit func(ast.Node)
	visit = func(n ast.Node) {
		switch node := n.(type) {
		case *ast.FuncLit:
			return
		case *ast.AssignStmt:
			for i, rhs := range node.Rhs {
				if i >= len(node.Lhs) {
					continue
				}
				if ident, ok := node.Lhs[i].(*ast.Ident); ok {
					if ident.Name == "assert" && isAssertNewCall(pass, rhs, tName) {
						assertHelper.add(identObject(pass, ident))
					}
					if ident.Name == "require" && isRequireNewCall(pass, rhs, tName) {
						requireHelper.add(identObject(pass, ident))
					}
				}
			}
		case *ast.ValueSpec:
			for i, value := range node.Values {
				if i >= len(node.Names) {
					continue
				}
				if node.Names[i].Name == "assert" && isAssertNewCall(pass, value, tName) {
					assertHelper.add(identObject(pass, node.Names[i]))
				}
				if node.Names[i].Name == "require" && isRequireNewCall(pass, value, tName) {
					requireHelper.add(identObject(pass, node.Names[i]))
				}
			}
		case *ast.CallExpr:
			switch callKind(pass, node, assertHelper, requireHelper) {
			case "assert":
				assertCallPositions = append(assertCallPositions, node)
			case "require":
				requireCallPositions = append(requireCallPositions, node)
			case "assert-helper":
				assertHelper.used = true
			case "require-helper":
				requireHelper.used = true
			}
		}

		ast.Inspect(n, func(child ast.Node) bool {
			if child == nil || child == n {
				return true
			}
			if _, ok := child.(*ast.FuncLit); ok {
				return false
			}
			visit(child)
			return false
		})
	}

	for _, stmt := range body.List {
		visit(stmt)
	}

	total := len(assertCallPositions) + len(requireCallPositions)
	if total <= 3 {
		return
	}

	hasAssertHelper := len(assertHelper.objs) > 0 && assertHelper.used
	hasRequireHelper := len(requireHelper.objs) > 0 && requireHelper.used

	if len(assertCallPositions) >= 2 && !hasAssertHelper {
		pass.Reportf(assertCallPositions[len(assertCallPositions)-1].Pos(), assertDiagnosticMessage, total)
	}

	if len(requireCallPositions) >= 2 && !hasRequireHelper {
		pass.Reportf(requireCallPositions[len(requireCallPositions)-1].Pos(), requireDiagnosticMessage, total)
	}
}

type helperState struct {
	objs map[types.Object]struct{}
	used bool
}

func (s *helperState) add(obj types.Object) {
	if obj == nil {
		return
	}
	if s.objs == nil {
		s.objs = make(map[types.Object]struct{})
	}
	s.objs[obj] = struct{}{}
}

func (s helperState) has(obj types.Object) bool {
	if obj == nil {
		return false
	}
	_, ok := s.objs[obj]
	return ok
}

func identObject(pass *analysis.Pass, ident *ast.Ident) types.Object {
	if ident == nil {
		return nil
	}
	if obj := pass.TypesInfo.Defs[ident]; obj != nil {
		return obj
	}
	return pass.TypesInfo.Uses[ident]
}

func isAssertNewCall(pass *analysis.Pass, expr ast.Expr, tName string) bool {
	return isHelperNewCall(pass, expr, tName, "github.com/stretchr/testify/assert")
}

func isRequireNewCall(pass *analysis.Pass, expr ast.Expr, tName string) bool {
	return isHelperNewCall(pass, expr, tName, "github.com/stretchr/testify/require")
}

func isHelperNewCall(pass *analysis.Pass, expr ast.Expr, tName string, importPath string) bool {
	call, ok := expr.(*ast.CallExpr)
	if !ok || len(call.Args) != 1 {
		return false
	}
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || sel.Sel == nil || sel.Sel.Name != "New" {
		return false
	}
	pkg, ok := sel.X.(*ast.Ident)
	if !ok {
		return false
	}
	pkgObj, ok := pass.TypesInfo.Uses[pkg].(*types.PkgName)
	if !ok || pkgObj.Imported().Path() != importPath {
		return false
	}
	arg, ok := call.Args[0].(*ast.Ident)
	return ok && arg.Name == tName
}

func callKind(pass *analysis.Pass, call *ast.CallExpr, assertHelper, requireHelper helperState) string {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return ""
	}
	pkg, ok := sel.X.(*ast.Ident)
	if !ok {
		return ""
	}
	obj := pass.TypesInfo.Uses[pkg]
	switch {
	case assertHelper.has(obj):
		return "assert-helper"
	case requireHelper.has(obj):
		return "require-helper"
	}

	pkgObj, ok := obj.(*types.PkgName)
	if !ok || sel.Sel.Name == "New" {
		return ""
	}

	switch pkgObj.Imported().Path() {
	case "github.com/stretchr/testify/assert":
		return "assert"
	case "github.com/stretchr/testify/require":
		return "require"
	default:
		return ""
	}
}
