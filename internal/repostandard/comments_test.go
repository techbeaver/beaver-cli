package repostandard

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ADR 0009 forbids verbose comments here. Without a check it is a preference,
// and the ported MCP code arrived carrying the private repository's house
// style, which is exactly the drift the ADR was written to stop.
//
// The rule, stated mechanically: a comment block inside a function body is at
// most one line. Doc comments on declarations and struct fields are untouched,
// because those are the form ADR 0009 keeps.
func TestNoMultiLineCommentsInsideFunctionBodies(t *testing.T) {
	root, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	root = filepath.Dir(root)

	var offences []string
	err = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			if name := info.Name(); name == ".git" || name == "dist" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}

		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, path, nil, parser.ParseComments)
		if err != nil {
			return err
		}

		type span struct{ from, to token.Pos }
		var bodies []span
		ast.Inspect(file, func(n ast.Node) bool {
			switch fn := n.(type) {
			case *ast.FuncDecl:
				if fn.Body != nil {
					bodies = append(bodies, span{fn.Body.Lbrace, fn.Body.Rbrace})
				}
			case *ast.FuncLit:
				if fn.Body != nil {
					bodies = append(bodies, span{fn.Body.Lbrace, fn.Body.Rbrace})
				}
			}
			return true
		})

		for _, group := range file.Comments {
			if len(group.List) < 2 {
				continue
			}
			if strings.HasPrefix(group.List[0].Text, "//go:") {
				continue
			}
			inBody := false
			for _, b := range bodies {
				if group.Pos() > b.from && group.End() < b.to {
					inBody = true
					break
				}
			}
			if !inBody {
				continue
			}
			rel, _ := filepath.Rel(root, path)
			offences = append(offences, strings.TrimSpace(
				rel+":"+itoa(fset.Position(group.Pos()).Line)+
					" ("+itoa(len(group.List))+" lines) "+firstLine(group.List[0].Text)))
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	if len(offences) > 0 {
		t.Errorf("%d comment blocks inside function bodies run to more than one line. "+
			"ADR 0009: say it in one line, move it to the function's doc comment, or write an ADR.\n  %s",
			len(offences), strings.Join(offences, "\n  "))
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

func firstLine(s string) string {
	s = strings.TrimPrefix(s, "//")
	s = strings.TrimSpace(s)
	if len(s) > 60 {
		return s[:60]
	}
	return s
}
