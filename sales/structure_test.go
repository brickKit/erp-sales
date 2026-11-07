package sales

import (
	"encoding/json"
	"go/ast"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The guards of the order's lifecycle and of the billable ledger, as checks (internal/AGENTS.md, "规则只在一处"):
// the order's state is decided in internal/orders/lifecycle.go and the ledger is written in internal/ledger, and nothing else
// quietly decides or writes them. The checks read the code by its types, not by its variables' names.

const module = "github.com/brickKit/erp-sales/v3"

// orderStateReaders are the places outside internal/orders/lifecycle.go that read the order's state (in Go or in SQL), by file and
// function, each with what it reads it for. A new state or phase means deciding, for each of them, what it does with
// an order in that state.
var orderStateReaders = map[string]reader{
	"internal/orders/orders.go:orderFromAll":   {SQL, "订单列表的进度筛选 to_deliver：已确认、还有实物没交完的订单"},
	"internal/orders/orders.go:orderFrom":      {SQL, "订单列表的默认范围（协议 11 的 TR10）：未结束（completed、cancelled 以外）或最近 N 天创建"},
	"internal/orders/archive.go:OrdersClass":   {SQL, "冷热分层：结束的订单（completed、cancelled）才封存"},
	"internal/orders/archive.go:Reader":        {SQL, "历史列表（history=true）按用户给的状态筛选，状态原样比较"},
	"internal/billingsource/billing.go:Ledger": {Go, "出账来源的台账把订单状态原样交给 erp/billing（契约 LedgerResponse.order_state）"},
	"internal/recoverycheck/recoverycheck.go:ReservationsCheck": {SQL, "协议 04 的检查：最近一个过程已确认的已确认订单，" +
		"分支在参与方应已提交或已消耗"},
}

// reader is a registered reader of the order's state: whether it reads it in Go or in SQL, and what for.
type reader struct {
	in  string
	why string
}

const (
	Go  = "go"
	SQL = "sql"
)

type pkg struct {
	path  string
	dir   string
	files []*ast.File
	info  *types.Info
}

// load type-checks the packages under internal/ against the compiled export data of their dependencies.
func load(t *testing.T) (*token.FileSet, []pkg) {
	t.Helper()
	cmd := exec.Command("go", "list", "-export", "-deps", "-json=ImportPath,Dir,GoFiles,Export", "./internal/...")
	cmd.Dir = ".."
	out, err := cmd.Output()
	require.NoError(t, err)
	type listed struct {
		ImportPath, Dir, Export string
		GoFiles                 []string
	}
	exports := map[string]string{}
	var mine []listed
	dec := json.NewDecoder(strings.NewReader(string(out)))
	for dec.More() {
		var l listed
		require.NoError(t, dec.Decode(&l))
		exports[l.ImportPath] = l.Export
		if strings.HasPrefix(l.ImportPath, module+"/internal/") {
			mine = append(mine, l)
		}
	}
	require.NotEmpty(t, mine)
	fset := token.NewFileSet()
	imp := importer.ForCompiler(fset, "gc", func(path string) (io.ReadCloser, error) { return os.Open(exports[path]) })
	var pkgs []pkg
	for _, l := range mine {
		p := pkg{path: l.ImportPath, dir: l.Dir, info: &types.Info{Selections: map[*ast.SelectorExpr]*types.Selection{}}}
		for _, f := range l.GoFiles {
			file, err := parser.ParseFile(fset, filepath.Join(l.Dir, f), nil, 0)
			require.NoError(t, err)
			p.files = append(p.files, file)
		}
		_, err := (&types.Config{Importer: imp}).Check(l.ImportPath, fset, p.files, p.info)
		require.NoError(t, err, l.ImportPath)
		pkgs = append(pkgs, p)
	}
	return fset, pkgs
}

func rel(fset *token.FileSet, pos token.Pos) string {
	p := fset.Position(pos)
	wd, _ := os.Getwd()
	r, _ := filepath.Rel(filepath.Dir(wd), p.Filename)
	return r + ":" + strconv.Itoa(p.Line)
}

func in(p pkg, name string) bool { return p.path == module+"/internal/"+name }

// theLifecycle is the one file that decides on the order's state without registering.
const theLifecycle = "internal/orders/lifecycle.go"

func lifecycleFile(fset *token.FileSet, pos token.Pos) bool {
	return strings.HasPrefix(rel(fset, pos), theLifecycle+":")
}

// where is "file:function" of pos: the function declared around it, or the top-level declaration's first name.
func where(fset *token.FileSet, f *ast.File, pos token.Pos) string {
	file := strings.SplitN(rel(fset, pos), ":", 2)[0]
	for _, d := range f.Decls {
		if pos < d.Pos() || pos > d.End() {
			continue
		}
		switch d := d.(type) {
		case *ast.FuncDecl:
			return file + ":" + d.Name.Name
		case *ast.GenDecl:
			for _, s := range d.Specs {
				if v, ok := s.(*ast.ValueSpec); ok && pos >= v.Pos() && pos <= v.End() {
					return file + ":" + v.Names[0].Name
				}
			}
		}
	}
	return file + ":?"
}

// fileOf is the parsed file holding pos.
func fileOf(p pkg, pos token.Pos) *ast.File {
	for _, f := range p.files {
		if pos >= f.Pos() && pos <= f.End() {
			return f
		}
	}
	return nil
}

// TestOnlyTheLifecycleReadsTheOrderState: outside internal/orders/lifecycle.go (and internal/core, which reads the
// rows), nothing reads core.Order's State but the registered readers: every decision on an order asks orders.Decide.
func TestOnlyTheLifecycleReadsTheOrderState(t *testing.T) {
	fset, pkgs := load(t)
	found := 0
	readers := map[string]bool{}
	for _, p := range pkgs {
		for sel, s := range p.info.Selections {
			if s.Kind() != types.FieldVal || s.Obj().Name() != "State" {
				continue
			}
			recv := s.Recv()
			if ptr, ok := recv.(*types.Pointer); ok {
				recv = ptr.Elem()
			}
			named, ok := recv.(*types.Named)
			if !ok || named.Obj().Pkg().Path() != module+"/internal/core" || named.Obj().Name() != "Order" {
				continue
			}
			found++
			if in(p, "core") || lifecycleFile(fset, sel.Pos()) {
				continue
			}
			w := where(fset, fileOf(p, sel.Pos()), sel.Pos())
			readers[w] = true
			if orderStateReaders[w].in != Go {
				t.Errorf("%s (%s) reads the order's state outside "+theLifecycle+": ask orders.Decide, or register "+
					"it in orderStateReaders with what it reads it for", rel(fset, sel.Pos()), w)
			}
		}
	}
	assert.NotZero(t, found, "the check finds the reads in internal/orders (else it checks nothing)")
	for w, r := range orderStateReaders {
		if r.in == Go {
			assert.True(t, readers[w], "%s is registered but no longer reads the order's state: unregister it", w)
		}
	}
}

var (
	writesOrderState = regexp.MustCompile(`(?is)UPDATE\s+sales_order\b.*\bSET\b.*\bstate\s*=`)
	writesLedger     = regexp.MustCompile(`(?is)UPDATE\s+order_line\b.*\b(delivered|billable_qty|billable_amt|claimed_qty|claimed_amt|billed_qty|billed_amt)\s*=`)
	stateCondition   = regexp.MustCompile(`(?is)\b(\w+\.)?state\s*(=|<>|!=|IN\b|NOT\s+IN\b)`)
	ordersTable      = regexp.MustCompile(`\bsales_order\b`)
	fromClause       = regexp.MustCompile(`(?i)\bFROM\b`)
)

// TestTheOrderTablesHaveTheirOwners: the SQL that writes the order's state is in internal/orders/lifecycle.go
// (Transition), the SQL that writes the billable ledger in internal/ledger, and the SQL elsewhere whose conditions
// read the order's state is registered in orderStateReaders.
func TestTheOrderTablesHaveTheirOwners(t *testing.T) {
	fset, pkgs := load(t)
	hits := map[string]int{}
	readers := map[string]bool{}
	for _, p := range pkgs {
		for _, f := range p.files {
			f := f
			ast.Inspect(f, func(n ast.Node) bool {
				lit, ok := n.(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					return true
				}
				sql, err := strconv.Unquote(lit.Value)
				if err != nil {
					return true
				}
				at := rel(fset, lit.Pos())
				w := where(fset, f, lit.Pos())
				if writesOrderState.MatchString(sql) {
					hits["state"]++
					if !lifecycleFile(fset, lit.Pos()) {
						t.Errorf("%s writes the order's state outside %s: use orders.Transition", at, theLifecycle)
					}
				}
				if writesLedger.MatchString(sql) {
					hits["ledger"]++
					if !in(p, "ledger") {
						t.Errorf("%s writes the billable ledger outside internal/ledger: use its functions", at)
					}
				}
				// a state condition in SQL on sales_order; in internal/orders also a fragment without FROM (a list's or the
				// archive's condition on the order row), the other tables' states being in SQL with their own FROM
				reads := stateCondition.MatchString(sql) && (ordersTable.MatchString(sql) ||
					in(p, "orders") && !fromClause.MatchString(sql))
				if reads && !lifecycleFile(fset, lit.Pos()) && !writesOrderState.MatchString(sql) {
					readers[w] = true
					if orderStateReaders[w].in != SQL {
						t.Errorf("%s (%s) reads the order's state in SQL outside "+theLifecycle+": register it in "+
							"orderStateReaders with what it reads it for", at, w)
					}
				}
				return true
			})
		}
	}
	assert.NotZero(t, hits["state"], "the check finds the write in internal/orders")
	assert.NotZero(t, hits["ledger"], "the check finds the writes in internal/ledger")
	for w, r := range orderStateReaders {
		if r.in == SQL {
			assert.True(t, readers[w], "%s is registered but no longer reads the order's state: unregister it", w)
		}
	}
}
