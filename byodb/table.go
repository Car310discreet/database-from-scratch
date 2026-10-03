package byodb

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
)

// Record is a row, or part of one: column names with values.
type Record struct {
	Cols []string
	Vals []Value
}

func (rec *Record) AddStr(col string, val []byte) *Record {
	rec.Cols = append(rec.Cols, col)
	rec.Vals = append(rec.Vals, Value{Type: TYPE_BYTES, Str: val})
	return rec
}

func (rec *Record) AddInt64(col string, val int64) *Record {
	rec.Cols = append(rec.Cols, col)
	rec.Vals = append(rec.Vals, Value{Type: TYPE_INT64, I64: val})
	return rec
}

func (rec *Record) Get(col string) *Value {
	for i, c := range rec.Cols {
		if c == col {
			return &rec.Vals[i]
		}
	}
	return nil
}

// TableDef is a table schema. Rows are stored as
//
//	key: prefix[0] + primary key columns    value: the other columns
//
// and each secondary index i as
//
//	key: prefix[i] + index columns + remaining primary key columns    value: empty
//
// Appending the primary key makes index keys unique and lets a scan on the
// index find the row.
type TableDef struct {
	Name     string
	Types    []uint32
	Cols     []string
	Indexes  [][]string // Indexes[0] is the primary key
	Prefixes []uint32   // auto-assigned key prefixes, one per index
}

// Internal tables. Table schemas are stored as JSON in @table; @meta holds
// the table prefix counter.
var TDEF_META = &TableDef{
	Name:     "@meta",
	Types:    []uint32{TYPE_BYTES, TYPE_BYTES},
	Cols:     []string{"key", "val"},
	Indexes:  [][]string{{"key"}},
	Prefixes: []uint32{1},
}

var TDEF_TABLE = &TableDef{
	Name:     "@table",
	Types:    []uint32{TYPE_BYTES, TYPE_BYTES},
	Cols:     []string{"name", "def"},
	Indexes:  [][]string{{"name"}},
	Prefixes: []uint32{2},
}

const TABLE_PREFIX_MIN = 100

func (tdef *TableDef) colIndex(col string) int { return slices.Index(tdef.Cols, col) }

func (tdef *TableDef) isPKey(col string) bool { return slices.Contains(tdef.Indexes[0], col) }

// pick returns the values of the given columns, from a full row.
func (tdef *TableDef) pick(row []Value, cols []string) []Value {
	out := make([]Value, len(cols))
	for i, c := range cols {
		out[i] = row[tdef.colIndex(c)]
	}
	return out
}

func (tdef *TableDef) nonPKeyCols() []string {
	var cols []string
	for _, c := range tdef.Cols {
		if !tdef.isPKey(c) {
			cols = append(cols, c)
		}
	}
	return cols
}

// ---- the database and its transactions ----

// DB is a relational database on top of KV.
type DB struct {
	Path string
	kv   KV
}

func (db *DB) Open() error {
	db.kv.Path = db.Path
	return db.kv.Open()
}

func (db *DB) Close() { db.kv.Close() }

// Verify checks the underlying storage structure.
func (db *DB) Verify() (Stats, error) { return db.kv.Verify() }

// DBTX is a relational transaction.
type DBTX struct {
	kv    KVTX
	db    *DB
	tdefs map[string]*TableDef // schemas read by this transaction
}

func (db *DB) Begin(tx *DBTX) {
	tx.db = db
	tx.tdefs = map[string]*TableDef{}
	db.kv.Begin(&tx.kv)
}

func (db *DB) Commit(tx *DBTX) error { return db.kv.Commit(&tx.kv) }
func (db *DB) Abort(tx *DBTX)        { db.kv.Abort(&tx.kv) }

// getTableDef reads a schema through the transaction, so concurrent schema
// changes are detected as conflicts like any other data.
func getTableDef(tx *DBTX, name string) (*TableDef, error) {
	switch name {
	case TDEF_META.Name:
		return TDEF_META, nil
	case TDEF_TABLE.Name:
		return TDEF_TABLE, nil
	}
	if tdef, ok := tx.tdefs[name]; ok {
		return tdef, nil
	}
	rec := (&Record{}).AddStr("name", []byte(name))
	ok, err := dbGet(tx, TDEF_TABLE, rec)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("table not found: %s", name)
	}
	tdef := &TableDef{}
	if err := json.Unmarshal(rec.Get("def").Str, tdef); err != nil {
		return nil, fmt.Errorf("corrupted schema for %s: %w", name, err)
	}
	tx.tdefs[name] = tdef
	return tdef, nil
}

// checkRecord reorders a record's values to the schema's column order.
// With full=true every column is required; otherwise exactly the primary key.
func checkRecord(tdef *TableDef, rec Record, full bool) ([]Value, error) {
	if len(rec.Cols) != len(rec.Vals) {
		return nil, errors.New("record has mismatched columns and values")
	}
	row := make([]Value, len(tdef.Cols))
	for i, col := range rec.Cols {
		j := tdef.colIndex(col)
		switch {
		case j < 0:
			return nil, fmt.Errorf("table %s has no column %s", tdef.Name, col)
		case row[j].Type != TYPE_ERROR:
			return nil, fmt.Errorf("duplicate column: %s", col)
		case !full && !tdef.isPKey(col):
			return nil, fmt.Errorf("column %s is not part of the primary key", col)
		case rec.Vals[i].Type != tdef.Types[j]:
			return nil, fmt.Errorf("column %s expects %s, got %s",
				col, typeName(tdef.Types[j]), typeName(rec.Vals[i].Type))
		}
		row[j] = rec.Vals[i]
	}
	for j, col := range tdef.Cols {
		if row[j].Type == TYPE_ERROR && (full || tdef.isPKey(col)) {
			return nil, fmt.Errorf("missing column: %s", col)
		}
	}
	return row, nil
}

func encodeRowKey(tdef *TableDef, row []Value) []byte {
	return encodeKey(nil, tdef.Prefixes[0], tdef.pick(row, tdef.Indexes[0]))
}

func encodeIndexKey(tdef *TableDef, i int, row []Value) []byte {
	return encodeKey(nil, tdef.Prefixes[i], tdef.pick(row, tdef.Indexes[i]))
}

// decodeRowValue fills the non-key columns of `row` from a stored value.
func decodeRowValue(tdef *TableDef, row []Value, val []byte) error {
	cols := tdef.nonPKeyCols()
	vals := make([]Value, len(cols))
	for i, c := range cols {
		vals[i].Type = tdef.Types[tdef.colIndex(c)]
	}
	if _, err := decodeValues(val, vals); err != nil {
		return err
	}
	for i, c := range cols {
		row[tdef.colIndex(c)] = vals[i]
	}
	return nil
}

func dbGet(tx *DBTX, tdef *TableDef, rec *Record) (bool, error) {
	row, err := checkRecord(tdef, *rec, false)
	if err != nil {
		return false, err
	}
	val, ok := tx.kv.Get(encodeRowKey(tdef, row))
	if !ok {
		return false, nil
	}
	if err := decodeRowValue(tdef, row, val); err != nil {
		return false, err
	}
	rec.Cols, rec.Vals = slices.Clone(tdef.Cols), row
	return true, nil
}

func dbSet(tx *DBTX, tdef *TableDef, rec Record, mode int) (bool, error) {
	row, err := checkRecord(tdef, rec, true)
	if err != nil {
		return false, err
	}
	key := encodeRowKey(tdef, row)
	val := encodeValues(nil, tdef.pick(row, tdef.nonPKeyCols()))
	// validate every key before changing anything
	if err := checkKey(key); err != nil {
		return false, err
	}
	if len(val) > BTREE_MAX_VAL_SIZE {
		return false, fmt.Errorf("row too large (%d > %d bytes)", len(val), BTREE_MAX_VAL_SIZE)
	}
	newKeys := make([][]byte, len(tdef.Indexes))
	for i := 1; i < len(tdef.Indexes); i++ {
		newKeys[i] = encodeIndexKey(tdef, i, row)
		if err := checkKey(newKeys[i]); err != nil {
			return false, fmt.Errorf("index key: %w", err)
		}
	}

	req := UpdateReq{Key: key, Val: val, Mode: mode}
	if _, err := tx.kv.Update(&req); err != nil || !req.Updated {
		return false, err
	}
	oldKeys := make([][]byte, len(tdef.Indexes))
	if !req.Added {
		old := slices.Clone(row)
		if err := decodeRowValue(tdef, old, req.Old); err != nil {
			return false, err
		}
		for i := 1; i < len(tdef.Indexes); i++ {
			oldKeys[i] = encodeIndexKey(tdef, i, old)
		}
	}
	for i := 1; i < len(tdef.Indexes); i++ {
		if bytes.Equal(oldKeys[i], newKeys[i]) {
			continue // this index is unaffected
		}
		if oldKeys[i] != nil {
			if ok, err := tx.kv.Del(&DeleteReq{Key: oldKeys[i]}); err != nil || !ok {
				return false, indexErr(err)
			}
		}
		ireq := UpdateReq{Key: newKeys[i], Mode: MODE_INSERT_ONLY}
		if ok, err := tx.kv.Update(&ireq); err != nil || !ok {
			return false, indexErr(err)
		}
	}
	return true, nil
}

func indexErr(err error) error {
	if err != nil {
		return err
	}
	return errors.New("byodb: secondary index is inconsistent with the table")
}

func dbDelete(tx *DBTX, tdef *TableDef, rec Record) (bool, error) {
	row, err := checkRecord(tdef, rec, false)
	if err != nil {
		return false, err
	}
	req := DeleteReq{Key: encodeRowKey(tdef, row)}
	if ok, err := tx.kv.Del(&req); err != nil || !ok {
		return false, err
	}
	if err := decodeRowValue(tdef, row, req.Old); err != nil {
		return false, err
	}
	for i := 1; i < len(tdef.Indexes); i++ {
		if ok, err := tx.kv.Del(&DeleteReq{Key: encodeIndexKey(tdef, i, row)}); err != nil || !ok {
			return false, indexErr(err)
		}
	}
	return true, nil
}

// ---- public row operations ----

// Get looks up a row by primary key; rec holds the key on input and the
// full row on output.
func (tx *DBTX) Get(table string, rec *Record) (bool, error) {
	tdef, err := getTableDef(tx, table)
	if err != nil {
		return false, err
	}
	return dbGet(tx, tdef, rec)
}

// Set writes a full row. It reports whether anything changed.
func (tx *DBTX) Set(table string, rec Record, mode int) (bool, error) {
	tdef, err := getTableDef(tx, table)
	if err != nil {
		return false, err
	}
	return dbSet(tx, tdef, rec, mode)
}

func (tx *DBTX) Insert(table string, rec Record) (bool, error) {
	return tx.Set(table, rec, MODE_INSERT_ONLY)
}
func (tx *DBTX) Update(table string, rec Record) (bool, error) {
	return tx.Set(table, rec, MODE_UPDATE_ONLY)
}
func (tx *DBTX) Upsert(table string, rec Record) (bool, error) {
	return tx.Set(table, rec, MODE_UPSERT)
}

// Delete removes a row by primary key.
func (tx *DBTX) Delete(table string, rec Record) (bool, error) {
	tdef, err := getTableDef(tx, table)
	if err != nil {
		return false, err
	}
	return dbDelete(tx, tdef, rec)
}

// ---- schema operations ----

func validateTableDef(tdef *TableDef) error {
	if tdef.Name == "" || strings.HasPrefix(tdef.Name, "@") {
		return fmt.Errorf("invalid table name: %q", tdef.Name)
	}
	if len(tdef.Cols) == 0 || len(tdef.Cols) != len(tdef.Types) {
		return errors.New("a table needs columns, each with a type")
	}
	for i, col := range tdef.Cols {
		if col == "" || slices.Index(tdef.Cols, col) != i {
			return fmt.Errorf("invalid or duplicate column: %q", col)
		}
		if tdef.Types[i] != TYPE_BYTES && tdef.Types[i] != TYPE_INT64 {
			return fmt.Errorf("column %s has an invalid type", col)
		}
	}
	if len(tdef.Indexes) == 0 || len(tdef.Indexes[0]) == 0 {
		return errors.New("a table needs a primary key")
	}
	for _, index := range tdef.Indexes {
		for i, col := range index {
			if tdef.colIndex(col) < 0 {
				return fmt.Errorf("index uses unknown column: %s", col)
			}
			if slices.Index(index, col) != i {
				return fmt.Errorf("index repeats column: %s", col)
			}
		}
	}
	return nil
}

// TableNew creates a table. Secondary indexes are extended with the primary
// key columns they lack.
func (tx *DBTX) TableNew(def *TableDef) error {
	tdef := &TableDef{
		Name:  def.Name,
		Types: slices.Clone(def.Types),
		Cols:  slices.Clone(def.Cols),
	}
	for _, index := range def.Indexes {
		tdef.Indexes = append(tdef.Indexes, slices.Clone(index))
	}
	if err := validateTableDef(tdef); err != nil {
		return err
	}
	pkey := tdef.Indexes[0]
	for i := 1; i < len(tdef.Indexes); i++ {
		for _, col := range pkey {
			if !slices.Contains(tdef.Indexes[i], col) {
				tdef.Indexes[i] = append(tdef.Indexes[i], col)
			}
		}
		for j := 0; j < i; j++ {
			if slices.Equal(tdef.Indexes[i], tdef.Indexes[j]) {
				return fmt.Errorf("duplicate index: (%s)", strings.Join(tdef.Indexes[i], ", "))
			}
		}
	}

	if _, err := getTableDef(tx, tdef.Name); err == nil {
		return fmt.Errorf("table exists: %s", tdef.Name)
	}
	// allocate key prefixes from the counter in @meta
	meta := (&Record{}).AddStr("key", []byte("next_prefix"))
	ok, err := dbGet(tx, TDEF_META, meta)
	if err != nil {
		return err
	}
	next := uint32(TABLE_PREFIX_MIN)
	if ok {
		next = binary.LittleEndian.Uint32(meta.Get("val").Str)
	}
	for range tdef.Indexes {
		tdef.Prefixes = append(tdef.Prefixes, next)
		next++
	}
	meta = (&Record{}).AddStr("key", []byte("next_prefix")).
		AddStr("val", binary.LittleEndian.AppendUint32(nil, next))
	if _, err := dbSet(tx, TDEF_META, *meta, MODE_UPSERT); err != nil {
		return err
	}
	def2, _ := json.Marshal(tdef)
	rec := (&Record{}).AddStr("name", []byte(tdef.Name)).AddStr("def", def2)
	if _, err := dbSet(tx, TDEF_TABLE, *rec, MODE_INSERT_ONLY); err != nil {
		return err
	}
	tx.tdefs[tdef.Name] = tdef
	return nil
}

// TableDrop deletes a table with all its rows and index entries.
func (tx *DBTX) TableDrop(name string) error {
	tdef, err := getTableDef(tx, name)
	if err != nil {
		return err
	}
	if strings.HasPrefix(name, "@") {
		return fmt.Errorf("cannot drop internal table %s", name)
	}
	for _, prefix := range tdef.Prefixes {
		start := binary.BigEndian.AppendUint32(nil, prefix)
		stop := append(bytes.Clone(start), 0xff)
		var keys [][]byte
		for it := tx.kv.Range(start, CMP_GE, stop, CMP_LE); it.Valid(); it.Next() {
			k, _ := it.Deref()
			keys = append(keys, bytes.Clone(k))
		}
		for _, k := range keys {
			if _, err := tx.kv.Del(&DeleteReq{Key: k}); err != nil {
				return err
			}
		}
	}
	rec := (&Record{}).AddStr("name", []byte(name))
	if _, err := dbDelete(tx, TDEF_TABLE, *rec); err != nil {
		return err
	}
	delete(tx.tdefs, name)
	return nil
}

// TableDef returns a table's schema.
func (tx *DBTX) TableDef(name string) (*TableDef, error) { return getTableDef(tx, name) }

// TableList returns the names of all user tables, sorted.
func (tx *DBTX) TableList() ([]string, error) {
	sc := Scanner{Cmp1: CMP_GE, Cmp2: CMP_LE}
	if err := dbScan(tx, TDEF_TABLE, &sc); err != nil {
		return nil, err
	}
	var names []string
	for ; sc.Valid(); sc.Next() {
		var rec Record
		if err := sc.Deref(&rec); err != nil {
			return nil, err
		}
		names = append(names, string(rec.Get("name").Str))
	}
	return names, nil
}

// ---- range queries ----

// Scanner iterates rows in a key range of some index: rows whose index key
// satisfies `key <Cmp1> Key1` and `key <Cmp2> Key2`. Key1 and Key2 may name a
// prefix of the index columns. The index is chosen by matching column names.
// The direction is ascending when Cmp1 is > or >=, descending otherwise.
type Scanner struct {
	Cmp1, Cmp2 int
	Key1, Key2 Record

	tx    *DBTX
	tdef  *TableDef
	index int // which index is being scanned
	iter  *KVIter
}

func (sc *Scanner) Valid() bool { return sc.iter != nil && sc.iter.Valid() }
func (sc *Scanner) Next()       { sc.iter.Next() }

// Deref decodes the current row, following a secondary index to the row.
func (sc *Scanner) Deref(rec *Record) error {
	tdef := sc.tdef
	key, val := sc.iter.Deref()
	cols := tdef.Indexes[sc.index]
	vals := make([]Value, len(cols))
	for i, c := range cols {
		vals[i].Type = tdef.Types[tdef.colIndex(c)]
	}
	if _, err := decodeValues(key[4:], vals); err != nil {
		return err
	}
	if sc.index == 0 {
		row := make([]Value, len(tdef.Cols))
		for i, c := range cols {
			row[tdef.colIndex(c)] = vals[i]
		}
		if err := decodeRowValue(tdef, row, val); err != nil {
			return err
		}
		rec.Cols, rec.Vals = slices.Clone(tdef.Cols), row
		return nil
	}
	pk := Record{}
	for i, c := range cols {
		if tdef.isPKey(c) {
			pk.Cols = append(pk.Cols, c)
			pk.Vals = append(pk.Vals, vals[i])
		}
	}
	ok, err := dbGet(sc.tx, tdef, &pk)
	if err == nil && !ok {
		err = indexErr(nil)
	}
	*rec = pk
	return err
}

func (tx *DBTX) Scan(table string, sc *Scanner) error {
	tdef, err := getTableDef(tx, table)
	if err != nil {
		return err
	}
	return dbScan(tx, tdef, sc)
}

func isPrefixOf(cols, index []string) bool {
	return len(cols) <= len(index) && slices.Equal(index[:len(cols)], cols)
}

func dbScan(tx *DBTX, tdef *TableDef, sc *Scanner) error {
	if !(sc.Cmp1 > 0 && sc.Cmp2 < 0) && !(sc.Cmp1 < 0 && sc.Cmp2 > 0) {
		return errors.New("bad range: the two bounds must face each other")
	}
	sc.index = slices.IndexFunc(tdef.Indexes, func(index []string) bool {
		return isPrefixOf(sc.Key1.Cols, index) && isPrefixOf(sc.Key2.Cols, index)
	})
	if sc.index < 0 {
		cols := sc.Key1.Cols
		if len(sc.Key2.Cols) > len(cols) {
			cols = sc.Key2.Cols
		}
		return fmt.Errorf("no index on (%s) in table %s", strings.Join(cols, ", "), tdef.Name)
	}
	for _, key := range []Record{sc.Key1, sc.Key2} {
		for i, col := range key.Cols {
			if want := tdef.Types[tdef.colIndex(col)]; key.Vals[i].Type != want {
				return fmt.Errorf("column %s expects %s, got %s", col, typeName(want), typeName(key.Vals[i].Type))
			}
		}
	}
	prefix := tdef.Prefixes[sc.index]
	start := encodeKeyPartial(nil, prefix, sc.Key1.Vals, sc.Cmp1)
	stop := encodeKeyPartial(nil, prefix, sc.Key2.Vals, sc.Cmp2)
	sc.tx, sc.tdef = tx, tdef
	sc.iter = tx.kv.Range(start, sc.Cmp1, stop, sc.Cmp2)
	return nil
}
