package byodb

import (
	"bytes"
	"fmt"
	"math"
	"math/rand"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
)

func TestEncodingOrder(t *testing.T) {
	rng := rand.New(rand.NewSource(3))
	ints := []int64{math.MinInt64, -1 << 40, -2, -1, 0, 1, 2, 255, 256, 1 << 40, math.MaxInt64}
	strs := [][]byte{{}, {0}, {0, 0}, {0, 1}, {1}, {1, 0}, {2}, []byte("a"), []byte("a\x00"), []byte("ab"), []byte("b"), {0xff}}
	type tuple struct {
		s []byte
		i int64
	}
	var tuples []tuple
	for i := 0; i < 400; i++ {
		tuples = append(tuples, tuple{strs[rng.Intn(len(strs))], ints[rng.Intn(len(ints))]})
	}
	enc := func(tp tuple) []byte {
		return encodeValues(nil, []Value{{Type: TYPE_BYTES, Str: tp.s}, {Type: TYPE_INT64, I64: tp.i}})
	}
	for _, a := range tuples {
		for _, b := range tuples[:50] {
			want := bytes.Compare(a.s, b.s)
			if want == 0 {
				want = map[bool]int{true: -1, false: 1}[a.i < b.i]
				if a.i == b.i {
					want = 0
				}
			}
			if got := bytes.Compare(enc(a), enc(b)); got != want {
				t.Fatalf("order of %v vs %v: got %d want %d", a, b, got, want)
			}
		}
		out := []Value{{Type: TYPE_BYTES}, {Type: TYPE_INT64}}
		if rest, err := decodeValues(enc(a), out); err != nil || len(rest) != 0 ||
			!bytes.Equal(out[0].Str, a.s) || out[1].I64 != a.i {
			t.Fatalf("round trip of %v failed", a)
		}
	}
}

func openDB(t *testing.T) *DB {
	t.Helper()
	db := &DB{Path: filepath.Join(t.TempDir(), "test.db")}
	if err := db.Open(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(db.Close)
	return db
}

func mustExec(t *testing.T, s *Session, sql string) []QLResult {
	t.Helper()
	res, err := s.Exec(sql)
	if err != nil {
		t.Fatalf("%s\n  -> %v", sql, err)
	}
	return res
}

// rowsString renders SELECT results compactly: "a,b|c,d".
func rowsString(res QLResult) string {
	var rows []string
	for _, row := range res.Rows {
		var cells []string
		for _, v := range row {
			cells = append(cells, FormatValue(v))
		}
		rows = append(rows, strings.Join(cells, ","))
	}
	return strings.Join(rows, "|")
}

func query(t *testing.T, s *Session, sql string) string {
	t.Helper()
	res := mustExec(t, s, sql)
	return rowsString(res[len(res)-1])
}

func TestSQLBasics(t *testing.T) {
	db := openDB(t)
	s := NewSession(db)
	mustExec(t, s, `
		create table people (
			id int, name string, age int, city string,
			primary key (id),
			index (city, age),
		);
		insert into people values
			(1, 'ada', 36, 'london'), (2, 'alan', 41, 'wilmslow'),
			(3, 'grace', 85, 'new york'), (4, 'edsger', 72, 'austin'),
			(5, 'barbara', 30, 'london');`)

	checks := []struct{ sql, want string }{
		{"select name from people", "ada|alan|grace|edsger|barbara"},
		{"select name from people index by id > 3", "edsger|barbara"},
		{"select name from people index by id < 3", "alan|ada"}, // descending
		{"select name from people index by id >= 2 and id <= 4", "alan|grace|edsger"},
		{"select name from people index by id <= 4 and id > 2", "edsger|grace"},
		{"select name, age from people index by city = 'london'", "barbara,30|ada,36"},
		{"select name from people index by (city, age) > ('london', 30) and city <= 'london'", "ada"},
		{"select name from people index by city > 'london'", "grace|alan"},
		{"select name from people filter age > 40 and city != 'austin'", "alan|grace"},
		{"select name from people limit 2", "ada|alan"},
		{"select name from people limit 1, 2", "alan|grace"},
		{"select name from people filter age > 35 limit 1, 1", "alan"},
		{"select id * 10 + 1 as x, name + '!' from people index by id = 3", "31,grace!"},
		{"select -age, age % 7, not (age > 50) from people index by id = 4", "-72,2,0"},
		{"select name from people filter (city, age) >= ('london', 36) and (city, age) < ('wilmslow', 0)", "ada|grace"},
		{"select count from people filter 1 = 0", ""},
	}
	for _, c := range checks {
		if got := query(t, s, c.sql); got != c.want {
			t.Errorf("%s\n  got  %q\n  want %q", c.sql, got, c.want)
		}
	}

	res := mustExec(t, s, "update people set age = age + 1, city = 'cambridge' index by id = 2")
	if res[0].Affected != 1 {
		t.Fatalf("update affected %d rows", res[0].Affected)
	}
	if got := query(t, s, "select name, age from people index by city = 'cambridge'"); got != "alan,42" {
		t.Fatalf("after update: %q", got)
	}
	if got := query(t, s, "select name from people index by city = 'wilmslow'"); got != "" {
		t.Fatalf("stale index entry: %q", got)
	}
	// moving a row by changing its primary key
	mustExec(t, s, "update people set id = id + 100 filter city = 'london'")
	if got := query(t, s, "select id, name from people index by city = 'london'"); got != "105,barbara|101,ada" {
		t.Fatalf("after pk update: %q", got)
	}
	res = mustExec(t, s, "delete from people index by id > 100")
	if res[0].Affected != 2 {
		t.Fatalf("delete affected %d", res[0].Affected)
	}
	if got := query(t, s, "select name from people index by city >= ''"); got != "edsger|alan|grace" {
		t.Fatalf("after delete: %q", got)
	}
	mustExec(t, s, "upsert into people values (4, 'dijkstra', 72, 'austin'), (9, 'new', 1, 'x')")
	if got := query(t, s, "select name from people"); got != "alan|grace|dijkstra|new" {
		t.Fatalf("after upsert: %q", got)
	}
}

func TestSQLErrors(t *testing.T) {
	db := openDB(t)
	s := NewSession(db)
	mustExec(t, s, "create table t (a int, b string, primary key (a), index (b));")
	mustExec(t, s, "insert into t values (1, 'x')")
	bad := map[string]string{
		"insert into t values (1, 'y')":             "duplicate primary key",
		"insert into t values ('1', 'y')":           "expects int64",
		"insert into t (a) values (2)":              "missing column",
		"select zzz from t":                         "unknown column",
		"select a from t index by zzz = 1":          "no index",
		"select a from t index by a > 1 and a > 2":  "lower and one upper",
		"select a from t filter b + 1":              "type error",
		"select a / 0 from t":                       "division by zero",
		"select a from nope":                        "table not found",
		"create table t (a int, primary key (a))":   "table exists",
		"create table u (a int)":                    "missing PRIMARY KEY",
		"create table u (a float, primary key (a))": "unknown type",
		"selec a from t":                            "expected a statement",
		"select a from t where a = 1":               "expected `;`",
		"select 'unterminated from t":               "unterminated quote",
		"commit":                                    "no transaction",
	}
	for sql, want := range bad {
		_, err := s.Exec(sql)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s\n  got error %v, want %q", sql, err, want)
		}
	}
	if got := query(t, s, "select a, b from t"); got != "1,x" {
		t.Fatalf("failed statements changed data: %q", got)
	}
}

func TestSQLTransactions(t *testing.T) {
	db := openDB(t)
	s := NewSession(db)
	mustExec(t, s, "create table kv (k string, v int, primary key (k))")
	mustExec(t, s, "begin; insert into kv values ('a', 1), ('b', 2);")

	other := NewSession(db)
	if got := query(t, other, "select k from kv"); got != "" {
		t.Fatalf("uncommitted rows visible: %q", got)
	}
	// a failing statement is undone, the transaction stays open
	if _, err := s.Exec("insert into kv values ('c', 3), ('a', 9)"); err == nil {
		t.Fatal("expected a duplicate key error")
	}
	if !s.InTransaction() {
		t.Fatal("transaction closed by a statement error")
	}
	if got := query(t, s, "select k, v from kv"); got != "a,1|b,2" {
		t.Fatalf("statement not rolled back: %q", got)
	}
	mustExec(t, s, "commit")
	if got := query(t, other, "select k from kv"); got != "a|b" {
		t.Fatalf("after commit: %q", got)
	}
	mustExec(t, s, "begin; delete from kv; rollback;")
	if got := query(t, s, "select k from kv"); got != "a|b" {
		t.Fatalf("after rollback: %q", got)
	}

	// DDL is transactional too
	mustExec(t, s, "begin; create table tmp (x int, primary key (x)); insert into tmp values (1); rollback;")
	if _, err := s.Exec("select x from tmp"); err == nil {
		t.Fatal("rolled-back table exists")
	}
}

func TestIndexScansAgainstReference(t *testing.T) {
	db := openDB(t)
	s := NewSession(db)
	mustExec(t, s, "create table t (id int, grp int, name string, primary key (id), index (grp, name))")
	type row struct {
		id, grp int64
		name    string
	}
	rng := rand.New(rand.NewSource(11))
	ref := map[int64]row{}
	for round := 0; round < 40; round++ {
		var sql strings.Builder
		sql.WriteString("begin;")
		for i := 0; i < 25; i++ {
			r := row{rng.Int63n(400) - 200, rng.Int63n(8) - 4, fmt.Sprintf("n%02d", rng.Intn(30))}
			if rng.Intn(4) == 0 {
				fmt.Fprintf(&sql, "delete from t index by id = %d;", r.id)
				delete(ref, r.id)
			} else {
				fmt.Fprintf(&sql, "upsert into t values (%d, %d, '%s');", r.id, r.grp, r.name)
				ref[r.id] = r
			}
		}
		sql.WriteString("commit;")
		mustExec(t, s, sql.String())
	}
	var all []row
	for _, r := range ref {
		all = append(all, r)
	}
	byPK := func(a, b row) int { return int(a.id - b.id) }
	byIdx := func(a, b row) int {
		if a.grp != b.grp {
			return int(a.grp - b.grp)
		}
		if c := strings.Compare(a.name, b.name); c != 0 {
			return c
		}
		return int(a.id - b.id)
	}
	expect := func(order func(a, b row) int, desc bool, keep func(row) bool) string {
		rows := slices.Clone(all)
		slices.SortFunc(rows, order)
		if desc {
			slices.Reverse(rows)
		}
		var ids []string
		for _, r := range rows {
			if keep(r) {
				ids = append(ids, fmt.Sprint(r.id))
			}
		}
		return strings.Join(ids, "|")
	}
	for trial := 0; trial < 150; trial++ {
		a, b := rng.Int63n(400)-200, rng.Int63n(400)-200
		g, n := rng.Int63n(8)-4, fmt.Sprintf("n%02d", rng.Intn(30))
		cases := []struct {
			sql  string
			want string
		}{
			{fmt.Sprintf("select id from t index by id >= %d and id < %d", a, b),
				expect(byPK, false, func(r row) bool { return r.id >= a && r.id < b })},
			{fmt.Sprintf("select id from t index by id <= %d and id > %d", a, b),
				expect(byPK, true, func(r row) bool { return r.id <= a && r.id > b })},
			{fmt.Sprintf("select id from t index by grp = %d", g),
				expect(byIdx, false, func(r row) bool { return r.grp == g })},
			{fmt.Sprintf("select id from t index by grp > %d", g),
				expect(byIdx, false, func(r row) bool { return r.grp > g })},
			{fmt.Sprintf("select id from t index by grp <= %d", g),
				expect(byIdx, true, func(r row) bool { return r.grp <= g })},
			{fmt.Sprintf("select id from t index by (grp, name) > (%d, '%s') and grp < %d", g, n, g+2),
				expect(byIdx, false, func(r row) bool {
					return (r.grp > g || (r.grp == g && r.name > n)) && r.grp < g+2
				})},
			{fmt.Sprintf("select id from t index by (grp, name) < (%d, '%s') and (grp, name) >= (%d, '')", g, n, g),
				expect(byIdx, true, func(r row) bool { return r.grp == g && r.name < n })},
		}
		for _, c := range cases {
			if got := query(t, s, c.sql); got != c.want {
				t.Fatalf("%s\n  got  %s\n  want %s", c.sql, got, c.want)
			}
		}
	}
	// the index must hold exactly one entry per row
	st, err := db.Verify()
	if err != nil {
		t.Fatal(err)
	}
	if got := query(t, s, "select id from t index by grp >= -100"); strings.Count(got, "|")+1 != len(ref) {
		t.Fatalf("index has %d entries for %d rows", strings.Count(got, "|")+1, len(ref))
	}
	if st.Keys != uint64(2*len(ref)+2) { // rows + index entries + 2 internal rows
		t.Fatalf("store has %d keys for %d rows", st.Keys, len(ref))
	}
}

func TestDropTableFreesSpace(t *testing.T) {
	db := openDB(t)
	s := NewSession(db)
	mustExec(t, s, "create table big (id int, pad string, primary key (id), index (pad))")
	var sql strings.Builder
	sql.WriteString("insert into big values ")
	for i := 0; i < 3000; i++ {
		if i > 0 {
			sql.WriteString(",")
		}
		fmt.Fprintf(&sql, "(%d, '%s')", i, strings.Repeat("x", 100)+fmt.Sprint(i))
	}
	mustExec(t, s, sql.String())
	mustExec(t, s, "drop table big")
	st, err := db.Verify()
	if err != nil {
		t.Fatal(err)
	}
	if st.Keys != 1 { // only the prefix counter remains
		t.Fatalf("%d keys left after drop", st.Keys)
	}
	if got := query(t, s, "show tables"); got != "" {
		t.Fatalf("tables: %q", got)
	}
}

// Concurrent transfers between accounts must never create or destroy money.
func TestConcurrentTransfers(t *testing.T) {
	db := openDB(t)
	setup := NewSession(db)
	mustExec(t, setup, "create table acct (id int, balance int, primary key (id))")
	const accounts, workers, transfers = 10, 8, 40
	for i := 0; i < accounts; i++ {
		mustExec(t, setup, fmt.Sprintf("insert into acct values (%d, 1000)", i))
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	conflicts := 0
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			s := NewSession(db)
			rng := rand.New(rand.NewSource(int64(w)))
			for i := 0; i < transfers; i++ {
				from, to := rng.Intn(accounts), rng.Intn(accounts)
				amount := rng.Intn(50)
				for {
					_, err := s.Exec(fmt.Sprintf(`begin;
						update acct set balance = balance - %d index by id = %d;
						update acct set balance = balance + %d index by id = %d;
						commit;`, amount, from, amount, to))
					if err == nil {
						break
					}
					s.Close()
					if !strings.Contains(err.Error(), "conflict") {
						t.Error(err)
						return
					}
					mu.Lock()
					conflicts++
					mu.Unlock()
				}
			}
		}(w)
	}
	// a concurrent reader always sees a consistent total
	stop := make(chan bool)
	var readerErr error
	go func() {
		s := NewSession(db)
		for {
			select {
			case <-stop:
				stop <- true
				return
			default:
			}
			res, err := s.Exec("select balance from acct")
			if err != nil {
				readerErr = err
				continue
			}
			total := int64(0)
			for _, row := range res[0].Rows {
				total += row[0].I64
			}
			if total != accounts*1000 {
				readerErr = fmt.Errorf("reader saw total %d", total)
			}
		}
	}()
	wg.Wait()
	stop <- true
	<-stop
	if readerErr != nil {
		t.Fatal(readerErr)
	}
	res := mustExec(t, setup, "select balance from acct")
	total := int64(0)
	for _, row := range res[0].Rows {
		total += row[0].I64
	}
	if total != accounts*1000 {
		t.Fatalf("money was created or destroyed: total %d", total)
	}
	t.Logf("%d conflicts were detected and retried", conflicts)
	if _, err := db.Verify(); err != nil {
		t.Fatal(err)
	}
}

func TestParserPrecedence(t *testing.T) {
	db := openDB(t)
	s := NewSession(db)
	mustExec(t, s, "create table one (x int, primary key (x)); insert into one values (0);")
	cases := map[string]string{
		"1 + 2 * 3 - 4":                "3",
		"(1 + 2) * 3":                  "9",
		"10 - 3 - 2":                   "5",
		"100 / 10 / 5":                 "2",
		"-2 * -3":                      "6",
		"1 < 2 and 2 < 3 or 0":         "1",
		"not 1 = 2":                    "1",
		"1 = 1 and not 0 or 1 / 0 = 1": "1", // short-circuit skips the division
		"'ab' + 'cd' = 'abcd'":         "1",
		"(1, 'b') > (1, 'a')":          "1",
		"-9223372036854775808":         "-9223372036854775808",
		"'it''s' + \"\\x41\"":          "it'sA",
	}
	keys := make([]string, 0, len(cases))
	for k := range cases {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, expr := range keys {
		if got := query(t, s, "select "+expr+" from one"); got != cases[expr] {
			t.Errorf("%s = %s, want %s", expr, got, cases[expr])
		}
	}
}
