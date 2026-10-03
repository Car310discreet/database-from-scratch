package byodb

import (
	"bytes"
	"errors"
	"fmt"
	"slices"
	"strings"
)

// ---- expression evaluation ----

func qlErrType(op string, a, b Value) error {
	return fmt.Errorf("type error: %s %s %s", typeName(a.Type), op, typeName(b.Type))
}

func truthy(v Value) (bool, error) {
	if v.Type != TYPE_INT64 {
		return false, fmt.Errorf("type error: expected a boolean (int), got %s", typeName(v.Type))
	}
	return v.I64 != 0, nil
}

func boolValue(b bool) Value {
	if b {
		return Value{Type: TYPE_INT64, I64: 1}
	}
	return Value{Type: TYPE_INT64}
}

func compareValues(a, b Value) (int, error) {
	if a.Type != b.Type {
		return 0, qlErrType("compared with", a, b)
	}
	if a.Type == TYPE_INT64 {
		switch {
		case a.I64 < b.I64:
			return -1, nil
		case a.I64 > b.I64:
			return 1, nil
		}
		return 0, nil
	}
	return bytes.Compare(a.Str, b.Str), nil
}

// qlEval evaluates an expression against a row.
func qlEval(env *Record, node QLNode) (Value, error) {
	switch node.Type {
	case TYPE_INT64, TYPE_BYTES:
		return node.Value, nil
	case QL_SYM:
		if v := env.Get(string(node.Str)); v != nil {
			return *v, nil
		}
		return Value{}, fmt.Errorf("unknown column: %s", node.Str)
	case QL_TUP:
		return Value{}, errors.New("a tuple can only be compared with another tuple")
	case QL_STAR:
		return Value{}, errors.New("`*` is only allowed as a SELECT output")
	case QL_NEG:
		v, err := qlEval(env, node.Kids[0])
		if err == nil && v.Type != TYPE_INT64 {
			err = fmt.Errorf("type error: -%s", typeName(v.Type))
		}
		v.I64 = -v.I64
		return v, err
	case QL_NOT:
		v, err := qlEval(env, node.Kids[0])
		if err != nil {
			return Value{}, err
		}
		b, err := truthy(v)
		return boolValue(!b), err
	case QL_AND, QL_OR: // short-circuit
		v, err := qlEval(env, node.Kids[0])
		if err != nil {
			return Value{}, err
		}
		b, err := truthy(v)
		if err != nil || b == (node.Type == QL_OR) {
			return boolValue(b), err
		}
		if v, err = qlEval(env, node.Kids[1]); err != nil {
			return Value{}, err
		}
		b, err = truthy(v)
		return boolValue(b), err
	case QL_CMP_EQ, QL_CMP_NE, QL_CMP_LT, QL_CMP_LE, QL_CMP_GT, QL_CMP_GE:
		c, err := qlCompare(env, node.Kids[0], node.Kids[1])
		if err != nil {
			return Value{}, err
		}
		ok := map[uint32]bool{
			QL_CMP_EQ: c == 0, QL_CMP_NE: c != 0, QL_CMP_LT: c < 0,
			QL_CMP_LE: c <= 0, QL_CMP_GT: c > 0, QL_CMP_GE: c >= 0,
		}[node.Type]
		return boolValue(ok), nil
	case QL_ADD, QL_SUB, QL_MUL, QL_DIV, QL_MOD:
		a, err := qlEval(env, node.Kids[0])
		if err != nil {
			return Value{}, err
		}
		b, err := qlEval(env, node.Kids[1])
		if err != nil {
			return Value{}, err
		}
		return qlArith(node.Type, a, b)
	}
	return Value{}, fmt.Errorf("cannot evaluate node type %d", node.Type)
}

func qlArith(op uint32, a, b Value) (Value, error) {
	sym := map[uint32]string{QL_ADD: "+", QL_SUB: "-", QL_MUL: "*", QL_DIV: "/", QL_MOD: "%"}[op]
	if op == QL_ADD && a.Type == TYPE_BYTES && b.Type == TYPE_BYTES {
		return Value{Type: TYPE_BYTES, Str: slices.Concat(a.Str, b.Str)}, nil
	}
	if a.Type != TYPE_INT64 || b.Type != TYPE_INT64 {
		return Value{}, qlErrType(sym, a, b)
	}
	if (op == QL_DIV || op == QL_MOD) && b.I64 == 0 {
		return Value{}, errors.New("division by zero")
	}
	r := Value{Type: TYPE_INT64}
	switch op {
	case QL_ADD:
		r.I64 = a.I64 + b.I64
	case QL_SUB:
		r.I64 = a.I64 - b.I64
	case QL_MUL:
		r.I64 = a.I64 * b.I64
	case QL_DIV:
		r.I64 = a.I64 / b.I64
	case QL_MOD:
		r.I64 = a.I64 % b.I64
	}
	return r, nil
}

// qlCompare compares scalars, or tuples column by column.
func qlCompare(env *Record, left, right QLNode) (int, error) {
	a, err := qlEvalTuple(env, left)
	if err != nil {
		return 0, err
	}
	b, err := qlEvalTuple(env, right)
	if err != nil {
		return 0, err
	}
	if len(a) != len(b) {
		return 0, fmt.Errorf("cannot compare tuples of %d and %d items", len(a), len(b))
	}
	for i := range a {
		if c, err := compareValues(a[i], b[i]); err != nil || c != 0 {
			return c, err
		}
	}
	return 0, nil
}

func qlEvalTuple(env *Record, node QLNode) ([]Value, error) {
	kids := []QLNode{node}
	if node.Type == QL_TUP {
		kids = node.Kids
	}
	vals := make([]Value, len(kids))
	for i, kid := range kids {
		var err error
		if vals[i], err = qlEval(env, kid); err != nil {
			return nil, err
		}
	}
	return vals, nil
}

// ---- INDEX BY -> Scanner range ----

var qlToCmp = map[uint32]int{
	QL_CMP_GE: CMP_GE, QL_CMP_GT: CMP_GT, QL_CMP_LT: CMP_LT, QL_CMP_LE: CMP_LE, QL_CMP_EQ: CMP_GE,
}

// qlEvalScanKey turns `(a, b) > (1, 2)` into a record and a comparison.
func qlEvalScanKey(node QLNode) (Record, int, error) {
	cmp, ok := qlToCmp[node.Type]
	if !ok {
		return Record{}, 0, errors.New("INDEX BY supports =, <, <=, >, >= only")
	}
	left := []QLNode{node.Kids[0]}
	if node.Kids[0].Type == QL_TUP {
		left = node.Kids[0].Kids
	}
	var rec Record
	for _, kid := range left {
		if kid.Type != QL_SYM {
			return Record{}, 0, errors.New("INDEX BY: the left side must be column names")
		}
		rec.Cols = append(rec.Cols, string(kid.Str))
	}
	vals, err := qlEvalTuple(&Record{}, node.Kids[1])
	if err != nil {
		return Record{}, 0, fmt.Errorf("INDEX BY: the right side must be constants: %w", err)
	}
	if len(vals) != len(rec.Cols) {
		return Record{}, 0, errors.New("INDEX BY: column and value counts differ")
	}
	rec.Vals = vals
	return rec, cmp, nil
}

// qlScanInit supports three forms of INDEX BY:
//
//	a = v                a prefix of the index, as the range [v, v]
//	a > v                open-ended; the other bound is the empty tuple
//	a > v AND a < w      an interval; reversed operands scan descending
func qlScanInit(req *QLScan, sc *Scanner) (err error) {
	switch {
	case req.Key1.Type == QL_UNINIT: // no INDEX BY: full scan of the primary key
		sc.Cmp1, sc.Cmp2 = CMP_GE, CMP_LE
		return nil
	case req.Key1.Type == QL_CMP_EQ && req.Key2.Type == QL_UNINIT:
		if sc.Key1, sc.Cmp1, err = qlEvalScanKey(req.Key1); err != nil {
			return err
		}
		sc.Key2, sc.Cmp2 = sc.Key1, CMP_LE
		return nil
	case req.Key1.Type == QL_CMP_EQ || req.Key2.Type == QL_CMP_EQ:
		return errors.New("INDEX BY: `=` cannot be combined with another bound")
	}
	if sc.Key1, sc.Cmp1, err = qlEvalScanKey(req.Key1); err != nil {
		return err
	}
	if req.Key2.Type == QL_UNINIT {
		sc.Key2 = Record{} // the empty tuple: -inf or +inf of the index
		if sc.Cmp1 > 0 {
			sc.Cmp2 = CMP_LE
		} else {
			sc.Cmp2 = CMP_GE
		}
		return nil
	}
	if sc.Key2, sc.Cmp2, err = qlEvalScanKey(req.Key2); err != nil {
		return err
	}
	if (sc.Cmp1 > 0) == (sc.Cmp2 > 0) {
		return errors.New("INDEX BY: an interval needs one lower and one upper bound, like `a > 1 AND a < 5`")
	}
	return nil
}

// ---- the iterator chain ----
//
//	BIter -> KVIter -> Scanner -> qlScanIter -> qlSelectIter
//	(tree)   (+own writes) (rows)  (filter, limit) (expressions)

// RecordIter streams rows without materializing them.
type RecordIter interface {
	Valid() bool
	Next()
	Deref(*Record) error
}

type qlScanIter struct {
	req     *QLScan
	sc      Scanner
	skipped int64
	emitted int64
	rec     Record
	err     error
	end     bool
}

func (it *qlScanIter) Valid() bool { return !it.end }

func (it *qlScanIter) Next() {
	it.sc.Next()
	it.load()
}

func (it *qlScanIter) Deref(rec *Record) error {
	*rec = it.rec
	return it.err
}

// load advances to the next row that passes FILTER and OFFSET.
func (it *qlScanIter) load() {
	for {
		if it.err != nil || !it.sc.Valid() || it.emitted >= it.req.Limit {
			it.end = true
			return
		}
		if it.err = it.sc.Deref(&it.rec); it.err != nil {
			return // stay valid so Deref reports the error
		}
		pass := true
		if it.req.Filter.Type != QL_UNINIT {
			v, err := qlEval(&it.rec, it.req.Filter)
			if err == nil {
				pass, err = truthy(v)
			}
			if err != nil {
				it.err = err
				return
			}
		}
		if pass {
			if it.skipped < it.req.Offset {
				it.skipped++
			} else {
				it.emitted++
				return
			}
		}
		it.sc.Next()
	}
}

func qlScan(tx *DBTX, req *QLScan) (*qlScanIter, error) {
	it := &qlScanIter{req: req}
	if err := qlScanInit(req, &it.sc); err != nil {
		return nil, err
	}
	if err := tx.Scan(req.Table, &it.sc); err != nil {
		return nil, err
	}
	it.load()
	return it, nil
}

type qlSelectIter struct {
	iter  RecordIter
	names []string
	exprs []QLNode
}

func (it *qlSelectIter) Valid() bool { return it.iter.Valid() }
func (it *qlSelectIter) Next()       { it.iter.Next() }

func (it *qlSelectIter) Deref(rec *Record) error {
	if err := it.iter.Deref(rec); err != nil {
		return err
	}
	vals := make([]Value, len(it.exprs))
	for i, expr := range it.exprs {
		var err error
		if vals[i], err = qlEval(rec, expr); err != nil {
			return err
		}
	}
	*rec = Record{Cols: it.names, Vals: vals}
	return nil
}

// Query runs a SELECT and returns a streaming iterator over its rows.
func (tx *DBTX) Query(req *QLSelect) (RecordIter, error) {
	tdef, err := getTableDef(tx, req.Table)
	if err != nil {
		return nil, err
	}
	var names []string
	var exprs []QLNode
	for i, node := range req.Output { // expand `*`
		if node.Type == QL_STAR {
			for _, col := range tdef.Cols {
				names = append(names, col)
				exprs = append(exprs, QLNode{Value: Value{Type: QL_SYM, Str: []byte(col)}})
			}
		} else {
			names = append(names, req.Names[i])
			exprs = append(exprs, node)
		}
	}
	scan, err := qlScan(tx, &req.QLScan)
	if err != nil {
		return nil, err
	}
	return &qlSelectIter{iter: scan, names: names, exprs: exprs}, nil
}

// ---- statement execution ----

// QLResult is the outcome of one statement.
type QLResult struct {
	Cols     []string // SELECT output
	Rows     [][]Value
	Affected int64  // rows changed by INSERT/UPDATE/DELETE
	Message  string // for other statements
}

// collect materializes the matching rows. UPDATE and DELETE finish reading
// before writing, so a change can never make a row be visited twice.
func collect(tx *DBTX, req *QLScan) ([]Record, error) {
	it, err := qlScan(tx, req)
	if err != nil {
		return nil, err
	}
	var rows []Record
	for ; it.Valid(); it.Next() {
		var rec Record
		if err := it.Deref(&rec); err != nil {
			return nil, err
		}
		rows = append(rows, rec)
	}
	return rows, nil
}

func pkeyOf(tdef *TableDef, rec Record) Record {
	var pk Record
	for _, col := range tdef.Indexes[0] {
		pk.Cols = append(pk.Cols, col)
		pk.Vals = append(pk.Vals, *rec.Get(col))
	}
	return pk
}

func describeKey(rec Record) string {
	var parts []string
	for i, c := range rec.Cols {
		v := rec.Vals[i]
		s := v.String()
		if v.Type == TYPE_BYTES {
			s = fmt.Sprintf("%q", v.Str)
		}
		parts = append(parts, c+"="+s)
	}
	return strings.Join(parts, ", ")
}

// Exec executes one parsed statement (not a transaction-control statement).
func (tx *DBTX) Exec(stmt interface{}) (QLResult, error) {
	switch req := stmt.(type) {
	case *QLSelect:
		it, err := tx.Query(req)
		if err != nil {
			return QLResult{}, err
		}
		res := QLResult{}
		for ; it.Valid(); it.Next() {
			var rec Record
			if err := it.Deref(&rec); err != nil {
				return QLResult{}, err
			}
			res.Cols = rec.Cols
			res.Rows = append(res.Rows, rec.Vals)
		}
		if res.Cols == nil { // no rows: still report the column names
			res.Cols = it.(*qlSelectIter).names
		}
		return res, nil

	case *QLInsert:
		tdef, err := getTableDef(tx, req.Table)
		if err != nil {
			return QLResult{}, err
		}
		names := req.Names
		if names == nil {
			names = tdef.Cols
		}
		res := QLResult{}
		for _, row := range req.Values {
			if len(row) != len(names) {
				return QLResult{}, fmt.Errorf("expected %d values, got %d", len(names), len(row))
			}
			rec := Record{Cols: names}
			for _, expr := range row {
				v, err := qlEval(&Record{}, expr)
				if err != nil {
					return QLResult{}, err
				}
				rec.Vals = append(rec.Vals, v)
			}
			ok, err := dbSet(tx, tdef, rec, req.Mode)
			if err != nil {
				return QLResult{}, err
			}
			if !ok && req.Mode == MODE_INSERT_ONLY {
				return QLResult{}, fmt.Errorf("duplicate primary key: %s", describeKey(pkeyOf(tdef, rec)))
			}
			if ok {
				res.Affected++
			}
		}
		return res, nil

	case *QLUpdate:
		tdef, err := getTableDef(tx, req.Table)
		if err != nil {
			return QLResult{}, err
		}
		for i, name := range req.Names {
			if tdef.colIndex(name) < 0 {
				return QLResult{}, fmt.Errorf("table %s has no column %s", tdef.Name, name)
			}
			if slices.Index(req.Names, name) != i {
				return QLResult{}, fmt.Errorf("column %s is assigned twice", name)
			}
		}
		rows, err := collect(tx, &req.QLScan)
		if err != nil {
			return QLResult{}, err
		}
		res := QLResult{}
		for _, old := range rows {
			rec := Record{Cols: old.Cols, Vals: slices.Clone(old.Vals)}
			for i, name := range req.Names {
				v, err := qlEval(&old, req.Values[i]) // evaluated on the old row
				if err != nil {
					return QLResult{}, err
				}
				*rec.Get(name) = v
			}
			oldPK, newPK := pkeyOf(tdef, old), pkeyOf(tdef, rec)
			var ok bool
			if slices.EqualFunc(oldPK.Vals, newPK.Vals, func(a, b Value) bool {
				c, err := compareValues(a, b)
				return err == nil && c == 0
			}) {
				ok, err = dbSet(tx, tdef, rec, MODE_UPDATE_ONLY)
			} else { // the primary key changed: move the row
				if _, err = dbDelete(tx, tdef, oldPK); err == nil {
					ok, err = dbSet(tx, tdef, rec, MODE_INSERT_ONLY)
					if err == nil && !ok {
						err = fmt.Errorf("duplicate primary key: %s", describeKey(newPK))
					}
				}
			}
			if err != nil {
				return QLResult{}, err
			}
			if ok {
				res.Affected++
			}
		}
		return res, nil

	case *QLDelete:
		tdef, err := getTableDef(tx, req.Table)
		if err != nil {
			return QLResult{}, err
		}
		rows, err := collect(tx, &req.QLScan)
		if err != nil {
			return QLResult{}, err
		}
		res := QLResult{}
		for _, row := range rows {
			ok, err := dbDelete(tx, tdef, pkeyOf(tdef, row))
			if err != nil {
				return QLResult{}, err
			}
			if ok {
				res.Affected++
			}
		}
		return res, nil

	case *QLCreateTable:
		if err := tx.TableNew(&req.Def); err != nil {
			return QLResult{}, err
		}
		return QLResult{Message: "table created: " + req.Def.Name}, nil

	case *QLDropTable:
		if err := tx.TableDrop(req.Name); err != nil {
			return QLResult{}, err
		}
		return QLResult{Message: "table dropped: " + req.Name}, nil

	case *QLShowTables:
		names, err := tx.TableList()
		if err != nil {
			return QLResult{}, err
		}
		res := QLResult{Cols: []string{"table"}}
		for _, n := range names {
			res.Rows = append(res.Rows, []Value{{Type: TYPE_BYTES, Str: []byte(n)}})
		}
		return res, nil

	case *QLDescribe:
		tdef, err := getTableDef(tx, req.Table)
		if err != nil {
			return QLResult{}, err
		}
		str := func(s string) Value { return Value{Type: TYPE_BYTES, Str: []byte(s)} }
		res := QLResult{Cols: []string{"column", "type", "key"}}
		for i, col := range tdef.Cols {
			key := ""
			if tdef.isPKey(col) {
				key = "primary"
			}
			res.Rows = append(res.Rows, []Value{str(col), str(typeName(tdef.Types[i])), str(key)})
		}
		for i, index := range tdef.Indexes[1:] {
			res.Rows = append(res.Rows, []Value{
				str(fmt.Sprintf("index #%d", i+1)), str("(" + strings.Join(index, ", ") + ")"), str(""),
			})
		}
		return res, nil
	}
	return QLResult{}, fmt.Errorf("statement %T must be handled by a Session", stmt)
}

// ---- sessions ----

// Session runs SQL text with transaction control. Outside BEGIN/COMMIT each
// statement runs in its own transaction and is retried on conflicts. Inside
// one, a failing statement is undone but the transaction stays open.
// A Session is not safe for concurrent use; use one per goroutine.
type Session struct {
	DB *DB
	tx *DBTX

	// MaxRetries bounds automatic retries of conflicting auto-commit statements.
	MaxRetries int
}

func NewSession(db *DB) *Session { return &Session{DB: db, MaxRetries: 100} }

// InTransaction reports whether an explicit transaction is open.
func (s *Session) InTransaction() bool { return s.tx != nil }

// Close aborts any open transaction.
func (s *Session) Close() {
	if s.tx != nil {
		s.DB.Abort(s.tx)
		s.tx = nil
	}
}

// Exec parses and runs SQL. It returns the results of the statements that
// ran before the first error.
func (s *Session) Exec(src string) ([]QLResult, error) {
	stmts, err := ParseSQL(src)
	if err != nil {
		return nil, err
	}
	var results []QLResult
	for _, stmt := range stmts {
		res, err := s.execOne(stmt)
		if err != nil {
			return results, err
		}
		results = append(results, res)
	}
	return results, nil
}

func (s *Session) execOne(stmt interface{}) (QLResult, error) {
	switch stmt.(type) {
	case *QLBegin:
		if s.tx != nil {
			return QLResult{}, errors.New("a transaction is already open")
		}
		s.tx = &DBTX{}
		s.DB.Begin(s.tx)
		return QLResult{Message: "BEGIN"}, nil
	case *QLCommit:
		if s.tx == nil {
			return QLResult{}, errors.New("no transaction is open")
		}
		tx := s.tx
		s.tx = nil
		if err := s.DB.Commit(tx); err != nil {
			return QLResult{}, fmt.Errorf("commit failed, transaction rolled back: %w", err)
		}
		return QLResult{Message: "COMMIT"}, nil
	case *QLRollback:
		if s.tx == nil {
			return QLResult{}, errors.New("no transaction is open")
		}
		s.Close()
		return QLResult{Message: "ROLLBACK"}, nil
	}

	if s.tx != nil {
		s.tx.kv.savepoint()
		res, err := s.tx.Exec(stmt)
		if err != nil {
			s.tx.kv.rollbackToSavepoint()
			return QLResult{}, err
		}
		s.tx.kv.release()
		return res, nil
	}

	for attempt := 0; ; attempt++ {
		var tx DBTX
		s.DB.Begin(&tx)
		res, err := tx.Exec(stmt)
		if err != nil {
			s.DB.Abort(&tx)
			return QLResult{}, err
		}
		err = s.DB.Commit(&tx)
		if err == nil {
			return res, nil
		}
		if err != ErrConflict || attempt >= s.MaxRetries {
			return QLResult{}, err
		}
	}
}

// FormatValue renders a value for display.
func FormatValue(v Value) string {
	if v.Type == TYPE_BYTES {
		s := string(v.Str)
		if strings.ToValidUTF8(s, "\uFFFD") != s || strings.ContainsAny(s, "\x00\t\n\r") {
			return fmt.Sprintf("%q", v.Str)
		}
		return s
	}
	if v.Type == TYPE_INT64 {
		return fmt.Sprint(v.I64)
	}
	return "NULL"
}
