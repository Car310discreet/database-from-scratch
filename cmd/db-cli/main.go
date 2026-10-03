// Command byodb is an interactive SQL shell for the byodb database.
//
//	byodb file.db               interactive shell (or reads SQL from a pipe)
//	byodb -c "sql" file.db      run SQL and exit (exit status 1 on error)
package main

import (
	"bufio"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"unicode/utf8"

	"database_from_scratch/byodb"
)

const help = `SQL statements end with ';'. Examples:
  create table t (id int, name string, primary key (id), index (name));
  insert into t values (1, 'a'), (2, 'b');
  select * from t index by id >= 1 filter name != 'x' limit 10;
  update t set name = name + '!' index by id = 1;
  delete from t index by name = 'b';
  begin; ... commit;   (or rollback;)
  show tables;  describe t;  drop table t;
Meta-commands:
  .tables          list tables
  .schema TABLE    describe a table
  .verify          check the on-disk structure and print statistics
  .help            this text
  .quit            exit
`

func main() {
	command := flag.String("c", "", "run these SQL statements and exit")
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "usage: byodb [-c \"sql\"] file.db\n")
		flag.PrintDefaults()
	}
	flag.Parse()
	if flag.NArg() != 1 {
		flag.Usage()
		os.Exit(2)
	}

	db := &byodb.DB{Path: flag.Arg(0)}
	if err := db.Open(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
	sh := &shell{db: db, sess: byodb.NewSession(db), out: bufio.NewWriter(os.Stdout)}
	status := 0
	if *command != "" {
		if !sh.run(*command) {
			status = 1
		}
	} else {
		status = sh.repl(os.Stdin, isTerminal(os.Stdin))
	}
	sh.out.Flush()
	sh.sess.Close()
	db.Close()
	os.Exit(status)
}

func isTerminal(f *os.File) bool {
	st, err := f.Stat()
	return err == nil && st.Mode()&os.ModeCharDevice != 0
}

type shell struct {
	db   *byodb.DB
	sess *byodb.Session
	out  *bufio.Writer
}

// repl reads statements until EOF. Without a terminal, the first error
// stops execution, like a script.
func (sh *shell) repl(in io.Reader, interactive bool) int {
	if interactive {
		fmt.Fprintln(sh.out, "byodb shell. Type .help for help.")
	}
	r := bufio.NewReader(in)
	var buf strings.Builder
	for {
		if interactive {
			switch {
			case buf.Len() > 0:
				fmt.Fprint(sh.out, "   ...> ")
			case sh.sess.InTransaction():
				fmt.Fprint(sh.out, "byodb*> ")
			default:
				fmt.Fprint(sh.out, "byodb> ")
			}
			sh.out.Flush()
		}
		line, err := r.ReadString('\n')
		if line == "" && err != nil {
			break
		}
		if buf.Len() == 0 && strings.HasPrefix(strings.TrimSpace(line), ".") {
			quit, ok := sh.meta(strings.Fields(line))
			if quit {
				return 0
			}
			if !ok && !interactive {
				return 1
			}
			continue
		}
		buf.WriteString(line)
		if err != nil && !strings.HasSuffix(line, "\n") {
			buf.WriteString("\n")
		}
		if byodb.StatementComplete(buf.String()) {
			ok := sh.run(buf.String())
			buf.Reset()
			if !ok && !interactive {
				return 1
			}
		}
		if err != nil {
			break
		}
	}
	if strings.TrimSpace(buf.String()) != "" {
		fmt.Fprintln(os.Stderr, "error: incomplete statement at end of input (missing ';'?)")
		return 1
	}
	if interactive {
		fmt.Fprintln(sh.out)
	}
	return 0
}

// meta runs a dot-command, reporting whether to quit and whether it succeeded.
func (sh *shell) meta(args []string) (quit, ok bool) {
	switch args[0] {
	case ".quit", ".exit", ".q":
		return true, true
	case ".help":
		fmt.Fprint(sh.out, help)
	case ".tables":
		return false, sh.run("show tables;")
	case ".schema":
		if len(args) != 2 {
			sh.fail(fmt.Errorf("usage: .schema TABLE"))
			return false, false
		}
		return false, sh.run("describe `" + strings.ReplaceAll(args[1], "`", "``") + "`;")
	case ".verify", ".stats":
		st, err := sh.db.Verify()
		if err != nil {
			sh.fail(err)
			return false, false
		}
		fmt.Fprintf(sh.out, "ok: %d commits, %d pages (%d tree, height %d; %d free; %d free-list), %d keys\n",
			st.Version, st.Pages, st.TreePages, st.TreeHeight, st.FreePages, st.ListPages, st.Keys)
	default:
		sh.fail(fmt.Errorf("unknown command %s (try .help)", args[0]))
		return false, false
	}
	sh.out.Flush()
	return false, true
}

func (sh *shell) run(src string) bool {
	results, err := sh.sess.Exec(src)
	for _, res := range results {
		sh.print(res)
	}
	if err != nil {
		sh.fail(err)
		return false
	}
	sh.out.Flush()
	return true
}

func (sh *shell) fail(err error) {
	sh.out.Flush()
	fmt.Fprintln(os.Stderr, "error:", err)
}

func (sh *shell) print(res byodb.QLResult) {
	switch {
	case res.Cols != nil:
		printTable(sh.out, res)
	case res.Message != "":
		fmt.Fprintln(sh.out, res.Message)
	default:
		fmt.Fprintf(sh.out, "%d row(s) affected\n", res.Affected)
	}
}

func printTable(w io.Writer, res byodb.QLResult) {
	cells := make([][]string, len(res.Rows))
	width := make([]int, len(res.Cols))
	numeric := make([]bool, len(res.Cols))
	for j, c := range res.Cols {
		width[j] = utf8.RuneCountInString(c)
	}
	for i, row := range res.Rows {
		cells[i] = make([]string, len(row))
		for j, v := range row {
			s := byodb.FormatValue(v)
			cells[i][j] = s
			numeric[j] = v.Type == byodb.TYPE_INT64
			if n := utf8.RuneCountInString(s); n > width[j] {
				width[j] = n
			}
		}
	}
	sep := "+"
	for _, n := range width {
		sep += strings.Repeat("-", n+2) + "+"
	}
	line := func(vals []string) {
		var b strings.Builder
		b.WriteString("|")
		for j, s := range vals {
			pad := strings.Repeat(" ", width[j]-utf8.RuneCountInString(s))
			if numeric[j] {
				b.WriteString(" " + pad + s + " |")
			} else {
				b.WriteString(" " + s + pad + " |")
			}
		}
		fmt.Fprintln(w, b.String())
	}
	fmt.Fprintln(w, sep)
	line(res.Cols)
	fmt.Fprintln(w, sep)
	for _, row := range cells {
		line(row)
	}
	fmt.Fprintln(w, sep)
	fmt.Fprintf(w, "(%d row(s))\n", len(res.Rows))
}
