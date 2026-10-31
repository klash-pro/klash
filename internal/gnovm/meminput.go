// Copyright (C) 2026 Jae Kwon
//
// This file is part of gnosh's GnoVM machine subsystem. It is licensed under
// the GNO Network General Public License, version 6, as published by
// NewTendermint, LLC or its successor, or any later version of that License
// that the Licensor accepts as the proxy under its section 14, by a statement
// published in the canonical copy of internal/gnovm/LICENSE.md; together with
// the Additional Terms (Strong Attribution) in that file.

package gnovm

import (
	"go/ast"
	"go/parser"
	"go/token"
	"regexp"
	"strconv"
	"strings"
)

// memSource is a mem input made ready to run as a program's main: its
// statements in order, the declarations that must stand at the top level,
// and the imports it names or uses.
type memSource struct {
	main    string
	decls   []string
	imports []string // specs: `"strings"`, `gnomem "klash/gnomem/v2"`
}

// shapeMemInput makes a mem input runnable, taking what models write:
// statements, a whole program (its package clause dropped, its main called
// last), imports or none (a package used without one is imported; mem's
// own, import "mem", is dropped), and declarations. A declaration of a func stands at the top level; others run
// where they are, in order with the statements. A read of mem standing as
// a statement of its own (mem.Show(), mem.Find(…), mem.JSON(…),
// mem.JSON(…), or mem alone, trailing) prints what it says.
func shapeMemInput(code string) (memSource, error) {
	prog, err := normalize(code, 1, nil, nil)
	if err != nil {
		return memSource{}, err
	}
	var src memSource
	var main strings.Builder
	for _, o := range prog.ops {
		switch {
		case o.Stmts != "":
			main.WriteString(memShowReads(o.Stmts) + "\n")
		case o.File != "" && declaresFunc(o.File):
			src.decls = append(src.decls, o.File)
		case o.File != "":
			main.WriteString(o.File + "\n")
		case o.Eval != "":
			main.WriteString(memShowExpr(o.Eval) + "\n")
		case o.Call != "":
			main.WriteString(o.Call + "()\n")
		}
	}
	src.main = main.String()
	for _, imp := range append(prog.imports, autoImports(prog, nil)...) {
		if imp.Path == "mem" && (imp.Name == "" || imp.Name == "mem") {
			continue // mem is there already; models import it
		}
		spec := strconv.Quote(imp.Path)
		if imp.Name != "" {
			spec = imp.Name + " " + spec
		}
		src.imports = append(src.imports, spec)
	}
	return src, nil
}

// memCommaJoined reports whether code joins calls with commas, as a list
// would: a line ending in "),", then one that calls mem.
func memCommaJoined(code string) bool {
	lines := strings.Split(code, "\n")
	for i := 0; i+1 < len(lines); i++ {
		if strings.HasSuffix(strings.TrimSpace(lines[i]), "),") && strings.HasPrefix(strings.TrimSpace(lines[i+1]), "mem.") {
			return true
		}
	}
	return false
}

// declaresFunc reports whether declarations hold a func, which cannot be
// declared inside main.
func declaresFunc(decls string) bool {
	f, err := parser.ParseFile(token.NewFileSet(), "", "package p\n"+decls, parser.SkipObjectResolution)
	if err != nil {
		return true // let gno say what is wrong, at the top level
	}
	for _, d := range f.Decls {
		if _, ok := d.(*ast.FuncDecl); ok {
			return true
		}
	}
	return false
}

// memReads are mem's calls that return text to show.
var memReads = map[string]bool{"Show": true, "Find": true, "JSON": true, "Check": true, "String": true}

// memShown are mem's calls that return a value with a String of its own to
// show: a Frame, a Thought; and Recall, whose Namables are shown one a
// line.
var memShown = map[string]string{"Frame": ".String()", "Thought": ".String()"}

// memShowExpr is the statement a mem input's trailing expression becomes:
// a read of mem prints its text; any other call runs as it is, since it may
// return nothing (mem.Done), or what it printed already (mem.Todo); any
// other expression prints its value.
func memShowExpr(expr string) string {
	x, err := parser.ParseExpr(expr)
	if err != nil {
		return expr
	}
	switch x := x.(type) {
	case *ast.Ident:
		if x.Name == "mem" {
			return "showMem_(mem.Show())"
		}
	case *ast.CallExpr:
		if sel, ok := x.Fun.(*ast.SelectorExpr); ok {
			if id, ok := sel.X.(*ast.Ident); ok && id.Name == "mem" {
				switch {
				case memReads[sel.Sel.Name]:
					return "showMem_(" + expr + ")"
				case sel.Sel.Name == "Recall":
					return "showRecall_(" + expr + ")"
				case memShown[sel.Sel.Name] != "":
					return "showMem_(" + expr + memShown[sel.Sel.Name] + ")"
				}
			}
		}
		return expr
	}
	return "println(" + expr + ")"
}

// memShowReads makes each read of mem standing as a statement of its own in
// stmts print what it says, as a trailing one does (memShowExpr). A model
// that asked for three goals in one call was shown only the last, the
// others dropped without a word, and asked again (QA, 2026-10-30).
func memShowReads(stmts string) string {
	const head = "package p\nfunc _() {\n"
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "", head+stmts+"\n}\n", parser.SkipObjectResolution)
	if err != nil || len(f.Decls) != 1 {
		return stmts
	}
	fn, ok := f.Decls[0].(*ast.FuncDecl)
	if !ok {
		return stmts
	}
	var b strings.Builder
	last := 0
	for _, s := range fn.Body.List {
		e, ok := s.(*ast.ExprStmt)
		if !ok {
			continue
		}
		start, end := fset.Position(e.Pos()).Offset-len(head), fset.Position(e.End()).Offset-len(head)
		shown := memShowExpr(stmts[start:end])
		if !strings.HasPrefix(shown, "showMem_(") && !strings.HasPrefix(shown, "showRecall_(") {
			continue
		}
		b.WriteString(stmts[last:start] + shown)
		last = end
	}
	if last == 0 {
		return stmts
	}
	return b.String() + stmts[last:]
}

// memHint is what to tell the model about a mem input that failed with
// msg, for the habits of older mems; "" for none.
func memHint(code, msg string) string {
	if m := memMissingRe.FindStringSubmatch(msg); m != nil {
		return memMissing(m[1], m[2] == "Frame")
	}
	if strings.Contains(msg, "untyped Bigint as StringKind") {
		return `items are named, not numbered: type the name mem printed in backticks, as mem.Done("release-v2/tag", "…")`
	}
	if m := memUndeclaredRe.FindStringSubmatch(msg); m != nil {
		if regexp.MustCompile(`mem\.\w+\(\s*` + regexp.QuoteMeta(m[1]) + `[-/]`).MatchString(code) {
			return `a name is a string: quote it, as mem.Done("release-v2/bump", "…")`
		}
	}
	if strings.Contains(msg, "missing field PushFrame") || strings.Contains(msg, "missing field PopFrame") || strings.Contains(msg, "missing field PushThought") {
		return "frames are entered with mem.Next() (the next todo) or .Push() on a Frame or a Thought, and left with mem.Pop(result)"
	}
	if strings.Contains(msg, "cannot use *klash/gnomem/v2.Frame as string") {
		return "mem.Todo(…) gives a Frame, not a name: pass its Id, or the name mem printed in backticks"
	}
	if memAssignRe.MatchString(code) {
		return "mem is your memory, kept by gnosh: change it with its calls (mem.Todo, mem.Done, mem.Fact, …), never by setting it"
	}
	return ""
}

var (
	memMissingRe    = regexp.MustCompile(`missing field (\w+) in \*?klash/gnomem/v2\.(Mem|Frame)\b`)
	memUndeclaredRe = regexp.MustCompile(`name (\w+) not declared`)
	memAssignRe     = regexp.MustCompile(`(^|[^.\w])mem\s*:?=[^=]`)
)

// memMoved says where an older mem's call or field went.
var memMoved = map[string]string{
	"Add":      `mem.Todo("what to get done") adds a todo under the one you are in, a top-level one at the root, and names it for you`,
	"Goal":     `mem.Todo("what to get done") adds a todo under the one you are in, a top-level one at the root, and names it for you; .Push() enters it`,
	"Push":     `mem.Next() steps into the next todo under the frame you are in (at the root, of the newest open task); a particular frame: mem.Frame("name").Push(), mem.Todo("…").Push()`,
	"Step":     `mem.Todo("a todo") adds one under the todo you are in; mem.Frame("name").Todo("…") adds under another`,
	"Steps":    `mem.Todo("a todo") adds one under the todo you are in, one call each, in order; mem.Frame("name").Todo("…") adds under another`,
	"NewFrame": `mem.Todo("what to get done") adds a todo under the one you are in, and names it for you; .Push() enters it`,
	"Remind":   `a reminder for the user is a question for them, which every session raises: mem.Todo("what to do").Ask("…"); one for a day waits for it: mem.Todo("what to do").Wait("2026-12-01")`,
	"Remember": `mem.Fact("name", "text") keeps a fact, and mem.Show("name") reads it back`,
	"Facts":    `mem.Fact("name", "text") keeps a fact, and mem.Show("name") reads it back`,
	"Items":    `mem.Show("name") shows an item, and mem.JSON("name") gives it as JSON`,
	"List":     `mem.Show() lists what mem holds, and mem.Show("name") shows a todo with the todos under it`,
	"Erase":    `mem.Forget("name") erases a closed item and its steps, or a fact; mem.Redact("text") takes a secret out everywhere`,
}
