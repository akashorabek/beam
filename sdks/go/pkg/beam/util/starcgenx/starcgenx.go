// Licensed to the Apache Software Foundation (ASF) under one or more
// contributor license agreements.  See the NOTICE file distributed with
// this work for additional information regarding copyright ownership.
// The ASF licenses this file to You under the Apache License, Version 2.0
// (the "License"); you may not use this file except in compliance with
// the License.  You may obtain a copy of the License at
//
//    http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Package starcgenx is a Static Analysis Type Assertion shim and Registration Code Generator
// which provides an extractor to extract types from a package, in order to generate
// appropriate shims for a package so code can be generated for it.
//
// It's written for use by the starcgen tool, but separate to permit
// alternative "go/importer" Importers for accessing types from imported packages.
package starcgenx

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/token"
	"go/types"
	"strconv"
	"strings"

	"github.com/apache/beam/sdks/v2/go/pkg/beam/core/graph"
	"github.com/apache/beam/sdks/v2/go/pkg/beam/internal/errors"
	"github.com/apache/beam/sdks/v2/go/pkg/beam/util/shimx"
)

// NewExtractor returns an extractor for the given package.
func NewExtractor(pkg string) *Extractor {
	return &Extractor{
		Package:     pkg,
		functions:   make(map[string]struct{}),
		types:       make(map[string]struct{}),
		wraps:       make(map[string]map[string]*types.Signature),
		funcs:       make(map[string]*types.Signature),
		emits:       make(map[string]shimx.Emitter),
		iters:       make(map[string]shimx.Input),
		imports:     make(map[string]struct{}),
		allExported: true,
	}
}

// Extractor contains and uniquifies the cache of types and things that need to be generated.
type Extractor struct {
	w       bytes.Buffer
	Package string

	// Debug enables printing out the analysis information to the output.
	Debug bool

	// LegacyIdentifiers disables parts of the code generator analysis
	// requiring a list of identifiers to be passed in. Notably this
	// disables RegisterDoFn support.
	LegacyIdentifiers bool

	// Ids is an optional slice of package local identifiers
	Ids []string

	// Register and uniquify the needed shims for each kind.
	// Functions to Register
	functions map[string]struct{}
	// Types to Register (structs, essentially)
	types map[string]struct{}
	// StructuralDoFn wraps needed (receiver type, then method names)
	wraps map[string]map[string]*types.Signature
	// FuncShims needed
	funcs map[string]*types.Signature
	// Emitter Shims needed
	emits map[string]shimx.Emitter
	// Iterator Shims needed
	iters map[string]shimx.Input

	// list of packages we need to import.
	imports map[string]struct{}

	allExported bool // Marks if all ptransforms are exported and available in main.
}

// Summary prints out a summary of the shims and registrations to
// be generated to the buffer.
func (e *Extractor) Summary() {
	e.Print("\n")
	e.Print("Summary\n")
	e.Printf("All exported?: %v\n", e.allExported)
	e.Printf("%d\t Functions\n", len(e.functions))
	e.Printf("%d\t Types\n", len(e.types))
	e.Printf("%d\t Wraps\n", len(e.wraps))
	e.Printf("%d\t Shims\n", len(e.funcs))
	e.Printf("%d\t Emits\n", len(e.emits))
	e.Printf("%d\t Inputs\n", len(e.iters))
}

// Bytes forwards to fmt.Fprint to the extractor buffer.
func (e *Extractor) Bytes() []byte {
	return e.w.Bytes()
}

// Print forwards to fmt.Fprint to the extractor buffer.
func (e *Extractor) Print(s string) {
	if e.Debug {
		fmt.Fprint(&e.w, s)
	}
}

// Printf forwards to fmt.Printf to the extractor buffer.
func (e *Extractor) Printf(f string, args ...any) {
	if e.Debug {
		fmt.Fprintf(&e.w, f, args...)
	}
}

// FromAsts analyses the contents of a package
func (e *Extractor) FromAsts(imp types.Importer, fset *token.FileSet, files []*ast.File) error {
	conf := types.Config{
		Importer:                 imp,
		IgnoreFuncBodies:         false,
		DisableUnusedImportCheck: true,
	}
	info := &types.Info{
		Uses: make(map[*ast.Ident]types.Object),
		Defs: make(map[*ast.Ident]types.Object),
	}
	if e.LegacyIdentifiers {
		info.Uses = nil
		conf.IgnoreFuncBodies = true
		if len(e.Ids) != 0 {
			// TODO(lostluck): This becomes unnnecessary iff we can figure out
			// which ParDos are being passed to beam.ParDo or beam.Combine.
			// If there are ids, we need to also look at function bodies, and uses.
			var checkFuncBodies bool
			for _, v := range e.Ids {
				if strings.Contains(v, ".") {
					checkFuncBodies = true
					break
				}
			}
			conf.IgnoreFuncBodies = !checkFuncBodies
			info.Uses = make(map[*ast.Ident]types.Object)
		}
	}

	if _, err := conf.Check(e.Package, fset, files, info); err != nil {
		return errors.Wrapf(err, "failed to type check package %s", e.Package)
	}

	e.Print("/*\n")
	e.Print("CHECKING for RegisterDoFn EXPRs\n")
	visitor := findRegisterDoFnCalls{info, e, make(map[string]bool)}
	for _, file := range files {
		ast.Walk(visitor, file)
	}
	for id := range visitor.idsToFind {
		e.Ids = append(e.Ids, id)
	}

	var idsRequired, idsFound map[string]bool
	if len(e.Ids) > 0 {
		e.Printf("Filtering by %d identifiers: %q\n\n", len(e.Ids), strings.Join(e.Ids, ", "))
		idsRequired = make(map[string]bool)
		idsFound = make(map[string]bool)
		for _, id := range e.Ids {
			idsRequired[id] = true
		}
	}

	e.Print("CHECKING DEFS\n")
	for id, obj := range info.Defs {
		e.fromObj(fset, id, obj, idsRequired, idsFound)
	}
	e.Print("CHECKING USES\n")
	for id, obj := range info.Uses {
		e.fromObj(fset, id, obj, idsRequired, idsFound)
	}
	var notFound []string
	for _, k := range e.Ids {
		if !idsFound[k] {
			notFound = append(notFound, k)
		}
	}
	if len(notFound) > 0 {
		return errors.Errorf("couldn't find the following identifiers; please check for typos, or remove them: %v", strings.Join(notFound, ", "))
	}
	e.Print("*/\n")

	return nil
}

type findRegisterDoFnCalls struct {
	info      *types.Info
	e         *Extractor
	idsToFind map[string]bool
}

func (v findRegisterDoFnCalls) Visit(node ast.Node) (w ast.Visitor) {
	switch node := node.(type) {
	case *ast.CallExpr:
		if !v.isRegisterDoFnCall(node) {
			return v
		}
		// We have the RegisterDoFn call, now we need to extract the parameter's local identifier
		// for code gen.
		param := node.Args[0]
		v.e.Printf("\tparam - %v\n", types.ExprString(param))

		// Strip the reflect.TypeOf call if present.
		if node, ok := param.(*ast.CallExpr); ok {
			if !v.isReflectTypeOf(node) {
				break
			}
			param = node.Args[0]
		}

		switch node := param.(type) {
		case *ast.CallExpr: // Strip a (*DoFn)(nil) structure.
			param = node.Fun.(*ast.ParenExpr).X.(*ast.StarExpr).X
		case *ast.UnaryExpr: // Handle &DoFn{} if present.
			param = node.X
			v.e.Printf("\t\tpost unary - %v %T\n", types.ExprString(param), param)
		}
		switch node := param.(type) {
		case *ast.CompositeLit: // extract the DoFn from `DoFn{}`
			param = node.Type
			v.e.Printf("\t\tpost composite - %v %T\n", types.ExprString(param), param)
		case *ast.SelectorExpr: // Handle function primitives
			str := node.X.(*ast.Ident).String() + "." + node.Sel.Name
			if pkgName := v.findPackageRename(node.X.(*ast.Ident)); pkgName != nil {
				str = pkgName.Imported().Name() + "." + node.Sel.Name
			}
			v.e.Printf("\t\thave function - %v %s\n", types.ExprString(param), str)
			v.idsToFind[str] = true
			// Need to look up package identifiers for renamed imports.
			return v
		case *ast.Ident: // Already have DoFn.
		default:
			v.e.Printf("\t\t\t can't handle - %v %T\n", types.ExprString(param), param)
			return v
		}
		iden := param.(*ast.Ident)
		v.e.Printf("\t\thave type - %v\n", iden.Name)
		v.idsToFind[iden.Name] = true
	}
	return v
}

func (v findRegisterDoFnCalls) isReflectTypeOf(node *ast.CallExpr) bool {
	switch inner := node.Fun.(type) {
	case *ast.SelectorExpr:
		if inner.Sel.Name != "TypeOf" {
			return false
		}
		iden, ok := inner.X.(*ast.Ident)
		if !ok {
			return false
		}
		if iden.Name == "reflect" {
			return true
		}
	}
	return false
}

func (v findRegisterDoFnCalls) isRegisterDoFnCall(node *ast.CallExpr) bool {
	switch inner := node.Fun.(type) {
	case *ast.SelectorExpr:
		if inner.Sel.Name != "RegisterDoFn" {
			return false
		}
		// While it's unlikely that there will be other uses of "RegisterDoFn"
		// outside of beam related code, from other packages, we should at least
		// do some diligence to check that it's from one of the two packages we
		// expect that call, either the user facing beam package or the internal
		// genx package, and handle if those packages are renamed.

		// We can't simply check the fully qualified path due to vendoring.
		v.e.Printf("%v\n", types.ExprString(node))
		iden, ok := inner.X.(*ast.Ident)
		if !ok {
			v.e.Printf("\tfail %v!\n", types.ExprString(iden))
			return false
		}
		switch iden.Name {
		case "beam", "genx":
			v.e.Printf("\t success!\n")
			return true
		default:
			v.e.Printf("\tpackage renamed %v!\n", iden)
			// We have a *use* of the identifier, but not the original definition.
			// Look up the definition and trust that typechecked name resolution
			// is correct.
			pkgName := v.findPackageRename(iden)
			if pkgName == nil {
				return false
			}
			switch pkgName.Imported().Name() {
			case "beam", "genx":
				return true
			default:
				v.e.Printf("\tfail - not likely the beam package? %v\n", types.ObjectString(pkgName, nil))
			}
		}
	}
	return false
}

func (v findRegisterDoFnCalls) findPackageRename(iden *ast.Ident) *types.PkgName {
	for k, imp := range v.info.Defs {
		if k.Name == iden.Name {
			if pkgName, ok := imp.(*types.PkgName); ok {
				v.e.Printf("\tfound package rename %#v  -  %v\n", k, types.ObjectString(imp, nil))
				return pkgName
			}
		}
	}
	v.e.Printf("\tfail - %v not a package\n", types.ExprString(iden))
	return nil
}

func (e *Extractor) isRequired(ident string, obj types.Object, idsRequired, idsFound map[string]bool) bool {
	if idsRequired == nil {
		return true
	}
	if idsFound == nil {
		panic("broken invariant: idsFound map is nil, but idsRequired map exists")
	}
	// If we're filtering IDs, then it needs to be in the filtered identifiers,
	// or it's receiver type identifier needs to be in the filtered identifiers.
	if idsRequired[ident] {
		idsFound[ident] = true
		e.Printf("isRequired found: %s\n", ident)
		return true
	}
	// Check if this is a function.
	sig, ok := obj.Type().(*types.Signature)
	if !ok {
		return false
	}
	// If this is a function, and it has a receiver, it's a method.
	if recv := sig.Recv(); recv != nil && graph.IsLifecycleMethod(ident) {
		// We don't want to care about pointers, so dereference to value type.
		t := recv.Type()
		p, ok := types.Unalias(t).(*types.Pointer)
		for ok {
			t = p.Elem()
			p, ok = types.Unalias(t).(*types.Pointer)
		}
		ts := types.TypeString(t, e.qualifier)
		e.Printf("recv %v has %v, ts: %s %s--- ", recv, sig, ts, ident)
		if !idsRequired[ts] {
			e.Print("IGNORE\n")
			return false
		}
		e.Print("KEEP\n")
		idsFound[ts] = true
		return true
	}
	return false
}

func (e *Extractor) fromObj(fset *token.FileSet, id *ast.Ident, obj types.Object, idsRequired, idsFound map[string]bool) {
	if obj == nil { // Omit the package declaration.
		e.Printf("%s: %q has no object, probably a package\n",
			fset.Position(id.Pos()), id.Name)
		return
	}

	pkg := obj.Pkg()
	if pkg == nil {
		e.Printf("%s: %q has no package \n",
			fset.Position(id.Pos()), id.Name)
		// No meaningful identifier.
		return
	}
	ident := fmt.Sprintf("%s.%s", pkg.Name(), obj.Name())
	if pkg.Name() == e.Package {
		ident = obj.Name()
	}
	if !e.isRequired(ident, obj, idsRequired, idsFound) {
		e.Printf("%s: %q with package %q is not required \n",
			fset.Position(id.Pos()), id.Name, pkg.Name())
		return
	}

	switch ot := obj.(type) {
	case *types.Var:
		// Vars are tricky since they could be anything, and anywhere (package scope, parameters, etc)
		// eg. Flags, or Field Tags, among others.
		// I'm increasingly convinced that we should simply ignore vars.
		// Do nothing for vars.
	case *types.Func:
		sig := obj.Type().(*types.Signature)
		if recv := sig.Recv(); recv != nil {
			// Methods don't need registering, but they do need shim generation.
			e.Printf("%s: %q is a method of %v -> %v--- %T %v %v %v\n",
				fset.Position(id.Pos()), id.Name, recv.Type(), obj, obj, id, obj.Pkg(), obj.Type())
			if !graph.IsLifecycleMethod(id.Name) {
				// If this is not a lifecycle method, we should ignore it.
				return
			}
			// This must be a structural DoFn! We should generate a closure wrapper for it.
			t := recv.Type()
			p, ok := types.Unalias(t).(*types.Pointer)
			for ok {
				t = p.Elem()
				p, ok = types.Unalias(t).(*types.Pointer)
			}
			ts := types.TypeString(t, e.qualifier)
			mthdMap := e.wraps[ts]
			if mthdMap == nil {
				mthdMap = make(map[string]*types.Signature)
				e.wraps[ts] = mthdMap
			}
			mthdMap[id.Name] = sig
		} else if id.Name != "init" {
			// init functions are special and should be ignored.
			// Functions need registering, as well as shim generation.
			e.Printf("%s: %q is a top level func %v --- %T %v %v %v\n",
				fset.Position(id.Pos()), ident, obj, obj, id, obj.Pkg(), obj.Type())
			e.functions[ident] = struct{}{}
		}
		// For functions from other packages.
		if pkg.Name() != e.Package {
			e.imports[pkg.Path()] = struct{}{}
		}

		e.funcs[e.sigKey(sig)] = sig
		e.extractFromSignature(sig)
		e.Printf("\t%v\n", sig)
	case *types.TypeName:
		e.Printf("%s: %q is a type %v --- %T %v %v %v %v\n",
			fset.Position(id.Pos()), id.Name, obj, obj, id, obj.Pkg(), obj.Type(), obj.Name())
		// Probably need to sanity check that this type actually is/has a ProcessElement
		// or MergeAccumulators defined for this type so unnecessary registrations don't happen,
		// and can explicitly produce an error if an explicitly named type *isn't* a DoFn or CombineFn.
		e.extractType(ot)
	default:
		e.Printf("%s: %q defines %v --- %T %v %v %v\n",
			fset.Position(id.Pos()), types.ObjectString(obj, e.qualifier), obj, obj, id, obj.Pkg(), obj.Type())
	}
}

func (e *Extractor) extractType(ot *types.TypeName) {
	name := types.TypeString(ot.Type(), e.qualifier)
	// Unwrap an alias by one level.
	// Attempting to deference a full chain of aliases runs the risk of crossing
	// a visibility boundary such as internal packages.
	// A single level is safe since the code we're analysing imports it,
	// so we can assume the generated code can access it too.
	if ot.IsAlias() {
		if t, ok := ot.Type().(*types.Alias); ok {
			ot = t.Obj()
			name = types.TypeString(t, e.qualifier)
		}
		if t, ok := ot.Type().(*types.Named); ok {
			ot = t.Obj()
			name = types.TypeString(t, e.qualifier)
		}
	}
	// Only register non-universe types (eg. avoid `error` and similar)
	if pkg := ot.Pkg(); pkg != nil {
		path := pkg.Path()
		e.imports[path] = struct{}{}

		// Do not add universal types to be registered.
		if path == shimx.TypexImport {
			return
		}
		e.types[name] = struct{}{}
	}
}

// Examines the signature and extracts types of parameters and results for
// generating necessary imports and emitter and iterator code.
func (e *Extractor) extractFromSignature(sig *types.Signature) {
	e.extractFromTuple(sig.Params())
	e.extractFromTuple(sig.Results())
}

// extractFromContainer recurses through nested non-map container types to a non-derived
// element type.
func (e *Extractor) extractFromContainer(t types.Type) types.Type {
	// Container types need to be iteratively unwrapped until we're at the base type,
	// so we can get the import if necessary.
	for {
		if s, ok := types.Unalias(t).(*types.Slice); ok {
			t = s.Elem()
			continue
		}

		if p, ok := types.Unalias(t).(*types.Pointer); ok {
			t = p.Elem()
			continue
		}

		if a, ok := types.Unalias(t).(*types.Array); ok {
			t = a.Elem()
			continue
		}

		return t
	}
}

func (e *Extractor) extractFromTuple(tuple *types.Tuple) {
	for i := 0; i < tuple.Len(); i++ {
		s := tuple.At(i) // *types.Var

		t := e.extractFromContainer(s.Type())

		// Here's where we ensure we register new imports.
		if at, ok := t.(*types.Alias); ok {
			if pkg := at.Obj().Pkg(); pkg != nil {
				e.imports[pkg.Path()] = struct{}{}
			}
			e.extractType(at.Obj())
		}
		if t, ok := t.(*types.Named); ok {
			if pkg := t.Obj().Pkg(); pkg != nil {
				e.Printf("extractType: adding import path %q for %v\n", pkg.Path(), t)
				e.imports[pkg.Path()] = struct{}{}
			} else {
				e.Printf("extractType: %v has no package to import\n", t)
			}
			e.extractType(t.Obj())
		}

		if a, ok := s.Type().(*types.Signature); ok {
			// Check if the type is an emitter or iterator for the specialized
			// shim generation for those types.
			if emt, ok := e.makeEmitter(a); ok {
				e.emits[emt.Name] = emt
			}
			if ipt, ok := e.makeInput(a); ok {
				e.iters[ipt.Name] = ipt
			}
			// Tail recurse on functional signature.
			e.extractFromSignature(a)
		}
	}
}

func (e *Extractor) qualifier(pkg *types.Package) string {
	n := tail(pkg.Name())
	if n == e.Package {
		return ""
	}
	return n
}

func tail(path string) string {
	if i := strings.LastIndex("/", path); i >= 0 {
		path = path[i:]
	}
	return path
}

func (e *Extractor) tupleStrings(t *types.Tuple) []string {
	var vs []string
	for i := 0; i < t.Len(); i++ {
		v := t.At(i)
		vs = append(vs, types.TypeString(v.Type(), e.qualifier))
	}
	return vs
}

// sigKey produces a variable name agnostic key for the function signature.
func (e *Extractor) sigKey(sig *types.Signature) string {
	ps, rs := e.tupleStrings(sig.Params()), e.tupleStrings(sig.Results())
	return fmt.Sprintf("func(%v) (%v)", strings.Join(ps, ","), strings.Join(rs, ","))
}

// Generate produces an additional file for the Go package that was extracted,
// to be included in a subsequent compilation.
func (e *Extractor) Generate(filename string) []byte {
	var functions []string
	for fn := range e.functions {
		// No extra processing necessary, since these should all be package local.
		functions = append(functions, fn)
	}
	var typs []string
	for t := range e.types {
		typs = append(typs, t)
	}
	var wraps []shimx.Wrap
	for typ, mthdMap := range e.wraps {
		wrap := shimx.Wrap{Type: typ, Name: shimx.Name(typ)}
		for mName, mthd := range mthdMap {
			shim := e.makeFunc(mthd)
			shim.Name = mName
			wrap.Methods = append(wrap.Methods, shim)
		}
		wraps = append(wraps, wrap)
	}
	var shims []shimx.Func
	for sig, t := range e.funcs {
		shim := e.makeFunc(t)
		shim.Type = sig
		shims = append(shims, shim)
	}
	var emits []shimx.Emitter
	for _, t := range e.emits {
		emits = append(emits, t)
	}
	var inputs []shimx.Input
	for _, t := range e.iters {
		inputs = append(inputs, t)
	}

	var imports []string
	for k := range e.imports {
		if k == "" || k == e.Package {
			continue
		}
		imports = append(imports, k)
	}

	top := shimx.Top{
		FileName:  filename,
		ToolName:  "starcgen",
		Package:   e.Package,
		Imports:   imports,
		Functions: functions,
		Types:     typs,
		Wraps:     wraps,
		Shims:     shims,
		Emitters:  emits,
		Inputs:    inputs,
	}
	e.Print("\n")
	shimx.File(&e.w, &top)
	return e.w.Bytes()
}

func (e *Extractor) makeFunc(t *types.Signature) shimx.Func {
	shim := shimx.Func{}
	var inNames []string
	in := t.Params() // *types.Tuple
	for i := 0; i < in.Len(); i++ {
		s := in.At(i) // *types.Var
		shim.In = append(shim.In, types.TypeString(s.Type(), e.qualifier))
		inNames = append(inNames, e.NameType(s.Type()))
	}
	var outNames []string
	out := t.Results() // *types.Tuple
	for i := 0; i < out.Len(); i++ {
		s := out.At(i)
		shim.Out = append(shim.Out, types.TypeString(s.Type(), e.qualifier))
		outNames = append(outNames, e.NameType(s.Type()))
	}
	shim.Name = shimx.FuncName(inNames, outNames)
	return shim
}

func (e *Extractor) makeEmitter(sig *types.Signature) (shimx.Emitter, bool) {
	// Emitters must have no return values.
	if sig.Results().Len() != 0 {
		return shimx.Emitter{}, false
	}
	p := sig.Params()
	emt := shimx.Emitter{Type: e.sigKey(sig)}
	switch p.Len() {
	case 1:
		emt.Time = false
		emt.Val = e.varString(p.At(0))
	case 2:
		// TODO(rebo): Fix this when imports are resolved.
		// This is the tricky one... Need to verify what happens with aliases.
		// And get a candle to compare this against somehwere. isEventTime(p.At(0)) maybe.
		// if p.At(0) == typex.EventTimeType {
		// 	emt.Time = true
		// } else {
		emt.Key = e.varString(p.At(0))
		//}
		emt.Val = e.varString(p.At(1))
	case 3:
		// If there's 3, the first one must be typex.EventTime.
		emt.Time = true
		emt.Key = e.varString(p.At(1))
		emt.Val = e.varString(p.At(2))
	default:
		return shimx.Emitter{}, false
	}
	if emt.Time {
		emt.Name = fmt.Sprintf("ET%s%s", shimx.Name(emt.Key), shimx.Name(emt.Val))
	} else {
		emt.Name = fmt.Sprintf("%s%s", shimx.Name(emt.Key), shimx.Name(emt.Val))
	}
	return emt, true
}

// makeInput checks if the given signature is an iterator or not, and if so,
// returns a shimx.Input struct for the signature for use by the code
// generator. The canonical check for an iterator signature is in the
// funcx.UnfoldIter function which uses the reflect library,
// and this logic is replicated here.
func (e *Extractor) makeInput(sig *types.Signature) (shimx.Input, bool) {
	r := sig.Results()
	if r.Len() != 1 {
		return shimx.Input{}, false
	}
	// Iterators must return a bool.
	if b, ok := types.Unalias(r.At(0).Type()).(*types.Basic); !ok || b.Kind() != types.Bool {
		return shimx.Input{}, false
	}
	p := sig.Params()
	for i := 0; i < p.Len(); i++ {
		// All params for iterators must be pointers.
		if _, ok := types.Unalias(p.At(i).Type()).(*types.Pointer); !ok {
			return shimx.Input{}, false
		}
	}
	itr := shimx.Input{Type: e.sigKey(sig)}
	switch p.Len() {
	case 1:
		itr.Time = false
		itr.Val = e.deref(p.At(0))
	case 2:
		// TODO(rebo): Fix this when imports are resolved.
		// This is the tricky one... Need to verify what happens with aliases.
		// And get a candle to compare this against somehwere. isEventTime(p.At(0)) maybe.
		// if p.At(0) == typex.EventTimeType {
		// 	itr.Time = true
		// } else {
		itr.Key = e.deref(p.At(0))
		//}
		itr.Val = e.deref(p.At(1))
	case 3:
		// If there's 3, the first one must be typex.EventTime.
		itr.Time = true
		itr.Key = e.deref(p.At(1))
		itr.Val = e.deref(p.At(2))
	default:
		return shimx.Input{}, false
	}
	if itr.Time {
		itr.Name = fmt.Sprintf("ET%s%s", shimx.Name(itr.Key), shimx.Name(itr.Val))
	} else {
		itr.Name = fmt.Sprintf("%s%s", shimx.Name(itr.Key), shimx.Name(itr.Val))
	}
	return itr, true
}

// deref returns the string identifier for the element type of a pointer var.
// deref panics if the var type is not a pointer.
func (e *Extractor) deref(v *types.Var) string {
	p := v.Type().(*types.Pointer)
	return types.TypeString(p.Elem(), e.qualifier)
}

// varString provides the correct type for a variable within the
// package for which we're generated code.
func (e *Extractor) varString(v *types.Var) string {
	return types.TypeString(v.Type(), e.qualifier)
}

// NameType turns a reflect.Type into a string based on it's name.
// It prefixes Emit or Iter if the function satisfies the constrains of those types.
func (e *Extractor) NameType(t types.Type) string {
	switch a := t.(type) {
	case *types.Signature:
		if emt, ok := e.makeEmitter(a); ok {
			return "Emit" + emt.Name
		}
		if ipt, ok := e.makeInput(a); ok {
			return "Iter" + ipt.Name
		}
		return shimx.Name(e.sigKey(a))
	case *types.Slice:
		return "SliceOf" + e.NameType(a.Elem())
	case *types.Map:
		return "MapOf" + e.NameType(a.Key()) + "_" + e.NameType(a.Elem())
	case *types.Array:
		return "ArrayOf" + strconv.Itoa(int(a.Len())) + e.NameType(a.Elem())
	default:
		return shimx.Name(types.TypeString(t, e.qualifier))
	}
}
