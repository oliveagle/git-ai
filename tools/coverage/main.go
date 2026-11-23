// Command coverage reports statement coverage, block coverage and an
// AST-based branch coverage from a Go coverprofile.
//
// Go's cover tool instruments basic blocks, not branches, so there is no
// native branch coverage. This tool approximates it: it walks the AST for
// branch constructs (if/else arms, case clauses, loop bodies, select
// clauses, && and || right-hand sides) and counts an arm as covered when the
// coverprofile block starting at the arm's first statement or expression was
// hit at least once.
package main

import (
	"bufio"
	"flag"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

type block struct {
	startLine, startCol int
	endLine, endCol     int
	stmts               int
	count               int64
}

type fileCov struct {
	rel  string
	blocks []*block
}

type arm struct {
	pos  token.Position
	desc string
}

type pkgStat struct {
	name          string
	totalStmts    int
	covStmts      int
	totalBlocks   int
	covBlocks     int
	totalArms     int
	covArms       int
	uncovArms     []arm
}

func main() {
	profile := flag.String("profile", "", "path to the coverprofile")
	root := flag.String("root", ".", "module root the profile paths are relative to")
	prefix := flag.String("prefix", "", "only include files under this path prefix (e.g. internal/graft)")
	jsonOut := flag.Bool("json", false, "print per-package JSON-ish lines")
	flag.Parse()

	if *profile == "" {
		fmt.Fprintln(os.Stderr, "usage: coverage -profile cover.out [-root .] [-prefix internal/graft]")
		os.Exit(2)
	}

	files, err := parseProfile(*profile)
	if err != nil {
		fatal(err)
	}

	fset := token.NewFileSet()
	stats := map[string]*pkgStat{}
	for fileName, cov := range files {
		rel := strings.TrimPrefix(fileName, "github.com/feder-cr/git-ai/")
		if *prefix != "" && !hasAnyPrefix(rel, strings.Split(*prefix, ",")) {
			continue
		}
		srcPath := filepath.Join(*root, filepath.FromSlash(rel))
		src, err := os.ReadFile(srcPath)
		if err != nil {
			// Generated or vendored file without sources: fall back to
			// block/statement stats only.
			st := pkgStatFor(cov, rel)
			merge(stats, st)
			continue
		}

		f, err := parser.ParseFile(fset, srcPath, src, 0)
		if err != nil {
			fatal(err)
		}

		st := pkgStatFor(cov, rel)
		st.totalArms, st.covArms, st.uncovArms = countArms(fset, f, cov)
		merge(stats, st)
	}

	names := make([]string, 0, len(stats))
	for n := range stats {
		names = append(names, n)
	}
	sort.Strings(names)

	var (
		tS, cS, tB, cB, tA, cA int
	)
	for _, n := range names {
		s := stats[n]
		tS += s.totalStmts
		cS += s.covStmts
		tB += s.totalBlocks
		cB += s.covBlocks
		tA += s.totalArms
		cA += s.covArms
		if *jsonOut {
			fmt.Printf("%s stmts %d/%d (%.2f%%) blocks %d/%d (%.2f%%) branches %d/%d (%.2f%%)\n",
				n, s.covStmts, s.totalStmts, pct(s.covStmts, s.totalStmts),
				s.covBlocks, s.totalBlocks, pct(s.covBlocks, s.totalBlocks),
				s.covArms, s.totalArms, pct(s.covArms, s.totalArms))
		}
	}
	fmt.Printf("TOTAL stmts %d/%d (%.2f%%) blocks %d/%d (%.2f%%) branches %d/%d (%.2f%%)\n",
		cS, tS, pct(cS, tS), cB, tB, pct(cB, tB), cA, tA, pct(cA, tA))

	if !*jsonOut {
		// List the packages with the most uncovered branches.
		type row struct {
			name string
			n    int
		}
		rows := []row{}
		for _, n := range names {
			if u := len(stats[n].uncovArms); u > 0 {
				rows = append(rows, row{n, u})
			}
		}
		sort.Slice(rows, func(i, j int) bool { return rows[i].n > rows[j].n })
		for _, r := range rows {
			fmt.Printf("  %4d uncovered branches in %s\n", r.n, r.name)
			for _, a := range stats[r.name].uncovArms {
				if a.pos.IsValid() {
					fmt.Printf("         %s:%d: %s\n", relShort(*root, a.pos.Filename), a.pos.Line, a.desc)
				}
			}
		}
	}
}

// hasAnyPrefix reports whether rel starts with one of the comma-separated
// prefixes.
func hasAnyPrefix(rel string, prefixes []string) bool {
	for _, p := range prefixes {
		if p != "" && strings.HasPrefix(rel, p) {
			return true
		}
	}
	return false
}

func relShort(root, name string) string {
	if r, err := filepath.Rel(root, name); err == nil {
		return r
	}
	return name
}

func pct(c, t int) float64 {
	if t == 0 {
		return 100.0
	}
	return 100.0 * float64(c) / float64(t)
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, err)
	os.Exit(1)
}

func merge(stats map[string]*pkgStat, st pkgStat) {
	if old, ok := stats[st.name]; ok {
		old.totalStmts += st.totalStmts
		old.covStmts += st.covStmts
		old.totalBlocks += st.totalBlocks
		old.covBlocks += st.covBlocks
		old.totalArms += st.totalArms
		old.covArms += st.covArms
		old.uncovArms = append(old.uncovArms, st.uncovArms...)
		return
	}
	stats[st.name] = &st
}

func pkgName(rel string) string {
	idx := strings.LastIndex(rel, "/")
	if idx < 0 {
		return rel
	}
	return rel[:idx]
}

func pkgStatFor(cov *fileCov, rel string) pkgStat {
	st := pkgStat{name: pkgName(rel)}
	for _, b := range cov.blocks {
		st.totalStmts += b.stmts
		st.totalBlocks++
		if b.count > 0 {
			st.covStmts += b.stmts
			st.covBlocks++
		}
	}
	return st
}

func parseProfile(path string) (map[string]*fileCov, error) {
	fh, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer fh.Close()

	files := map[string]*fileCov{}
	sc := bufio.NewScanner(fh)
	lineNo := 0
	for sc.Scan() {
		lineNo++
		line := strings.TrimSpace(sc.Text())
		if line == "" || lineNo == 1 && strings.HasPrefix(line, "mode:") {
			continue
		}
		// name.go:startLine.startCol,endLine.endCol numStmt count
		colon := strings.LastIndex(line, ":")
		if colon < 0 {
			continue
		}
		name := line[:colon]
		rest := line[colon+1:]
		fields := strings.Fields(rest)
		if len(fields) != 3 {
			continue
		}
		var b block
		if n, err := fmt.Sscanf(fields[0], "%d.%d,%d.%d", &b.startLine, &b.startCol, &b.endLine, &b.endCol); err != nil || n != 4 {
			continue
		}
		b.stmts, err = strconv.Atoi(fields[1])
		if err != nil {
			continue
		}
		b.count, err = strconv.ParseInt(fields[2], 10, 64)
		if err != nil {
			continue
		}
		fc, ok := files[name]
		if !ok {
			fc = &fileCov{rel: name}
			files[name] = fc
		}
		fc.blocks = append(fc.blocks, &b)
	}
	return files, sc.Err()
}

// countArms walks the AST for branch constructs and reports how many arms the
// coverprofile shows executed at least once.
func countArms(fset *token.FileSet, f *ast.File, cov *fileCov) (total, covered int, uncov []arm) {
	var walk func(n ast.Node)
	walk = func(n ast.Node) {
		if n == nil {
			return
		}
		switch n := n.(type) {
		case *ast.IfStmt:
			countArm(n.Body, "if body", fset, cov, &total, &covered, &uncov)
			switch e := n.Else.(type) {
			case *ast.BlockStmt:
				countArm(e, "else body", fset, cov, &total, &covered, &uncov)
			case *ast.IfStmt:
				// else-if: the inner if is visited as its own branch.
			}
		case *ast.ForStmt:
			countArm(n.Body, "for body", fset, cov, &total, &covered, &uncov)
		case *ast.RangeStmt:
			countArm(n.Body, "range body", fset, cov, &total, &covered, &uncov)
		case *ast.SwitchStmt:
			for _, c := range n.Body.List {
				if cc, ok := c.(*ast.CaseClause); ok {
					if len(cc.Body) > 0 {
						countArm(&ast.BlockStmt{List: cc.Body}, "case body", fset, cov, &total, &covered, &uncov)
					}
				}
			}
		case *ast.TypeSwitchStmt:
			for _, c := range n.Body.List {
				if cc, ok := c.(*ast.CaseClause); ok {
					if len(cc.Body) > 0 {
						countArm(&ast.BlockStmt{List: cc.Body}, "type case body", fset, cov, &total, &covered, &uncov)
					}
				}
			}
		case *ast.SelectStmt:
			for _, c := range n.Body.List {
				if cc, ok := c.(*ast.CommClause); ok {
					if len(cc.Body) > 0 {
						countArm(&ast.BlockStmt{List: cc.Body}, "select clause body", fset, cov, &total, &covered, &uncov)
					}
				}
			}
		case *ast.BinaryExpr:
			if n.Op == token.LAND || n.Op == token.LOR {
				total++
				if hit(fset, n.Y.Pos(), cov) {
					covered++
				} else {
					uncov = append(uncov, arm{pos: fset.Position(n.Y.Pos()), desc: "boolean " + n.Op.String() + " right-hand side"})
				}
			}
		}
		ast.Inspect(n, func(c ast.Node) bool {
			if c == nil || c == n {
				return true
			}
			walk(c)
			return false
		})
	}
	walk(f)
	return total, covered, uncov
}

func countArm(body *ast.BlockStmt, desc string, fset *token.FileSet, cov *fileCov, total, covered *int, uncov *[]arm) {
	*total++
	if hit(fset, body.List[0].Pos(), cov) {
		*covered++
	} else {
		*uncov = append(*uncov, arm{pos: fset.Position(body.List[0].Pos()), desc: desc})
	}
}

// hit reports whether the coverprofile has a block starting at pos that was
// executed, or a containing block that was executed.
func hit(fset *token.FileSet, pos token.Pos, cov *fileCov) bool {
	p := fset.Position(pos)
	for _, b := range cov.blocks {
		if b.startLine == p.Line && b.startCol == p.Column {
			return b.count > 0
		}
	}
	for _, b := range cov.blocks {
		if p.Line < b.startLine || p.Line > b.endLine {
			continue
		}
		if (p.Line > b.startLine || p.Column >= b.startCol) &&
			(p.Line < b.endLine || p.Column <= b.endCol) {
			return b.count > 0
		}
	}
	return false
}
