// Command doccheck reports Go doc comments that break Go's conventions: a
// package without a package comment, a doc comment that does not start with
// the name it documents, and an exported name without a doc comment. Test
// files are skipped. It exits 1 when it reports anything.
//
//	go run ./tools/doccheck [dir ...]
//
// A directory ending in /... is walked; the default is ./... .
package main

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

func main() {
	args := os.Args[1:]
	if len(args) == 0 {
		args = []string{"./..."}
	}
	var dirs []string
	for _, a := range args {
		root, walk := strings.CutSuffix(a, "/...")
		if !walk {
			dirs = append(dirs, a)
			continue
		}
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			name := d.Name()
			if d.IsDir() && path != root && (strings.HasPrefix(name, ".") || name == "testdata" || name == "vendor") {
				return filepath.SkipDir
			}
			if d.IsDir() {
				dirs = append(dirs, path)
			}
			return nil
		})
		if err != nil {
			fmt.Fprintln(os.Stderr, "doccheck:", err)
			os.Exit(2)
		}
	}
	var problems []string
	for _, dir := range dirs {
		p, err := checkDir(dir)
		if err != nil {
			fmt.Fprintln(os.Stderr, "doccheck:", err)
			os.Exit(2)
		}
		problems = append(problems, p...)
	}
	sort.Strings(problems)
	for _, p := range problems {
		fmt.Println(p)
	}
	if len(problems) > 0 {
		os.Exit(1)
	}
}

// checkDir checks the non-test Go files of one directory.
func checkDir(dir string) ([]string, error) {
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, dir, func(fi fs.FileInfo) bool {
		return !strings.HasSuffix(fi.Name(), "_test.go")
	}, parser.ParseComments)
	if err != nil {
		return nil, err
	}
	var out []string
	report := func(pos token.Pos, format string, args ...any) {
		out = append(out, fmt.Sprintf("%s: %s", fset.Position(pos), fmt.Sprintf(format, args...)))
	}
	for name, pkg := range pkgs {
		files := make([]string, 0, len(pkg.Files))
		for f := range pkg.Files {
			files = append(files, f)
		}
		sort.Strings(files)
		hasDoc := false
		for _, f := range files {
			if pkg.Files[f].Doc != nil {
				hasDoc = true
			}
		}
		if !hasDoc && len(files) > 0 {
			report(pkg.Files[files[0]].Package, "package %s has no package comment", name)
		}
		for _, f := range files {
			checkFile(pkg.Files[f], report)
		}
	}
	return out, nil
}

// checkFile checks the doc comments of one file's top-level declarations.
func checkFile(f *ast.File, report func(token.Pos, string, ...any)) {
	for _, decl := range f.Decls {
		switch d := decl.(type) {
		case *ast.FuncDecl:
			exported := d.Name.IsExported() && (d.Recv == nil || exportedRecv(d.Recv))
			checkDoc(d.Doc, d.Name, exported, report)
		case *ast.GenDecl:
			// A comment on a group documents the group, so its specs need no
			// comment of their own and it need not start with one name.
			grouped := d.Lparen.IsValid()
			for _, spec := range d.Specs {
				switch s := spec.(type) {
				case *ast.TypeSpec:
					doc := s.Doc
					if doc == nil && !grouped {
						doc = d.Doc
					}
					if doc == nil && grouped && d.Doc != nil {
						continue
					}
					checkDoc(doc, s.Name, s.Name.IsExported(), report)
				case *ast.ValueSpec:
					if len(s.Names) != 1 {
						continue
					}
					doc := s.Doc
					if doc == nil && !grouped {
						doc = d.Doc
					}
					if doc == nil && grouped {
						if d.Doc != nil || !s.Names[0].IsExported() {
							continue
						}
					}
					checkDoc(doc, s.Names[0], s.Names[0].IsExported(), report)
				}
			}
		}
	}
}

// checkDoc reports a doc comment that does not start with the name, and a
// missing doc comment on an exported name.
func checkDoc(doc *ast.CommentGroup, name *ast.Ident, exported bool, report func(token.Pos, string, ...any)) {
	if name.Name == "_" || name.Name == "main" || name.Name == "init" {
		return
	}
	if doc == nil {
		if exported {
			report(name.Pos(), "%s is exported and has no doc comment", name.Name)
		}
		return
	}
	text := strings.TrimSpace(doc.Text())
	for _, article := range []string{"A ", "An ", "The "} {
		text = strings.TrimPrefix(text, article)
	}
	first, _, _ := strings.Cut(text, " ")
	first = strings.TrimRight(first, ".,:;")
	if first != name.Name {
		report(doc.Pos(), "the doc comment of %s starts with %q, not the name", name.Name, first)
	}
}

// exportedRecv reports whether a method's receiver type is exported, so that
// the method shows in the package's documentation.
func exportedRecv(recv *ast.FieldList) bool {
	if recv == nil || len(recv.List) == 0 {
		return false
	}
	t := recv.List[0].Type
	if s, ok := t.(*ast.StarExpr); ok {
		t = s.X
	}
	if g, ok := t.(*ast.IndexExpr); ok {
		t = g.X
	}
	if g, ok := t.(*ast.IndexListExpr); ok {
		t = g.X
	}
	id, ok := t.(*ast.Ident)
	return ok && id.IsExported()
}
