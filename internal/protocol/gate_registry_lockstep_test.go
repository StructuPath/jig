package protocol

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"testing"
)

// TestGateRegistryStaysInLockstepWithEngineDispatch is the gate-registry
// analogue of store_test.go's
// TestActiveAttemptStatesStayInLockstepWithThePartialIndex: an invariant
// encoded in two places, checked mechanically so a divergence fails the
// build instead of failing soft at runtime.
//
// The two places here are builtinGates (this package, validation time —
// what DefinitionFromYAML accepts in a phase's gates list) and runGate's
// dispatch switch (internal/engine, execution time — what actually runs a
// gate by that name). A name added to one and forgotten in the other
// currently passes validation and then fails soft the first time the gate
// runs, landing on runGate's default case: "gate is not in the built-in
// registry".
//
// internal/engine cannot be imported from here: it imports this package
// (protocol.GateSpec, protocol.GateReport, ...), so the reverse import
// would cycle. runGate is also unexported, so even a third package that
// imports both could not call it directly. Short of editing
// internal/engine/gates.go to export a name list — out of scope for this
// change, and internal/engine is a package this change does not own —
// there is no compiled link between the two registries to assert on.
//
// Instead this test parses both sources as data: the map literal that
// declares builtinGates, and the case clauses of runGate's switch. It reads
// internal/engine/gates.go but never imports or otherwise depends on the
// package, so no cycle is introduced. Renaming either the builtinGates
// variable or the runGate switch shape (not just its cases) will fail this
// test's own sanity checks below, pointing here rather than surfacing as a
// silent false pass.
func TestGateRegistryStaysInLockstepWithEngineDispatch(t *testing.T) {
	repoRoot := findModuleRoot(t)

	validation := gateIdentsFromMapLiteral(t,
		filepath.Join(repoRoot, "internal", "protocol", "definition.go"), "builtinGates")
	dispatch := gateIdentsFromSwitchDispatch(t,
		filepath.Join(repoRoot, "internal", "engine", "gates.go"), "runGate", "protocol")

	// Sanity checks first: if either parse comes back empty, the source
	// shape moved out from under this test's assumptions and the real
	// comparison below would pass vacuously. Fail loudly instead.
	if len(validation) == 0 {
		t.Fatal("parsed zero gate names from protocol.builtinGates's map literal; " +
			"definition.go no longer has the expected shape, fix this test")
	}
	if len(dispatch) == 0 {
		t.Fatal("parsed zero gate names from runGate's dispatch switch; " +
			"internal/engine/gates.go no longer has the expected shape, fix this test")
	}
	// Cross-check the parsed validation set's size against the live map
	// this package actually uses, so an AST-walk bug that under-parses
	// builtinGates's composite literal can't make the comparison below
	// vacuously agree. (The set itself holds identifier names, e.g.
	// "GateFilesNonEmpty", not the string values those identifiers hold —
	// only the count is comparable here without re-deriving the same
	// name-to-value table the const block already is.)
	if len(validation) != len(builtinGates) {
		t.Fatalf("parsed %d gate name(s) from builtinGates's source, but the live map has %d entries; "+
			"this test's parser no longer matches the source shape", len(validation), len(builtinGates))
	}

	var onlyValidation, onlyDispatch []string
	for name := range validation {
		if !dispatch[name] {
			onlyValidation = append(onlyValidation, name)
		}
	}
	for name := range dispatch {
		if !validation[name] {
			onlyDispatch = append(onlyDispatch, name)
		}
	}
	sort.Strings(onlyValidation)
	sort.Strings(onlyDispatch)

	if len(onlyValidation) > 0 {
		t.Errorf("gate(s) %v are accepted by protocol.builtinGates (validation time) "+
			"but have no case in engine.runGate's dispatch switch (execution time): "+
			"a phase declaring one would pass validation and then fail soft at runtime", onlyValidation)
	}
	if len(onlyDispatch) > 0 {
		t.Errorf("gate(s) %v are dispatched by engine.runGate (execution time) "+
			"but are not in protocol.builtinGates (validation time): "+
			"the gate can never be configured on a phase, dead code", onlyDispatch)
	}
}

// findModuleRoot walks up from this test file's own directory to the
// directory containing go.mod.
func findModuleRoot(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller could not resolve this test file's path")
	}
	dir := filepath.Dir(thisFile)
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("walked up from %s to filesystem root without finding go.mod", thisFile)
		}
		dir = parent
	}
}

// gateIdentsFromMapLiteral parses path and returns the set of identifier
// names used as keys in the composite literal assigned to the package-level
// var named varName, e.g. `var builtinGates = map[string]bool{GateX: true}`
// yields {"GateX": true}.
func gateIdentsFromMapLiteral(t *testing.T, path, varName string) map[string]bool {
	t.Helper()
	file := parseGoFile(t, path)

	idents := map[string]bool{}
	found := false
	ast.Inspect(file, func(n ast.Node) bool {
		spec, ok := n.(*ast.ValueSpec)
		if !ok {
			return true
		}
		nameMatches := false
		for _, name := range spec.Names {
			if name.Name == varName {
				nameMatches = true
			}
		}
		if !nameMatches {
			return true
		}
		for _, value := range spec.Values {
			composite, ok := value.(*ast.CompositeLit)
			if !ok {
				continue
			}
			found = true
			for _, element := range composite.Elts {
				kv, ok := element.(*ast.KeyValueExpr)
				if !ok {
					continue
				}
				if ident, ok := kv.Key.(*ast.Ident); ok {
					idents[ident.Name] = true
				}
			}
		}
		return true
	})
	if !found {
		t.Fatalf("%s: no composite-literal initializer found for var %s", path, varName)
	}
	return idents
}

// gateIdentsFromSwitchDispatch parses path and returns the set of selector
// identifier names (e.g. "GateArtifactsExist" out of "protocol.GateArtifactsExist")
// used as case values in the first switch statement found in the body of
// the function named funcName, where the case expression is a selector
// on package qualifier pkgQualifier.
func gateIdentsFromSwitchDispatch(t *testing.T, path, funcName, pkgQualifier string) map[string]bool {
	t.Helper()
	file := parseGoFile(t, path)

	idents := map[string]bool{}
	foundFunc := false
	foundSwitch := false
	ast.Inspect(file, func(n ast.Node) bool {
		fn, ok := n.(*ast.FuncDecl)
		if !ok || fn.Name.Name != funcName {
			return true
		}
		foundFunc = true
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			sw, ok := n.(*ast.SwitchStmt)
			if !ok {
				return true
			}
			foundSwitch = true
			for _, stmt := range sw.Body.List {
				clause, ok := stmt.(*ast.CaseClause)
				if !ok {
					continue
				}
				for _, expr := range clause.List {
					sel, ok := expr.(*ast.SelectorExpr)
					if !ok {
						continue
					}
					pkg, ok := sel.X.(*ast.Ident)
					if !ok || pkg.Name != pkgQualifier {
						continue
					}
					idents[sel.Sel.Name] = true
				}
			}
			return false // only the outermost switch in the function body
		})
		return false
	})
	if !foundFunc {
		t.Fatalf("%s: no function named %s found", path, funcName)
	}
	if !foundSwitch {
		t.Fatalf("%s: function %s has no switch statement", path, funcName)
	}
	return idents
}

func parseGoFile(t *testing.T, path string) *ast.File {
	t.Helper()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	return file
}
