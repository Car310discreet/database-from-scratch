package byodb

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// crashChild is run in a subprocess: it commits forever, printing the
// number of each commit after it returns, until it is killed.
func crashChild(path string) {
	db := &DB{Path: path}
	if err := db.Open(); err != nil {
		fmt.Println("ERR", err)
		os.Exit(1)
	}
	s := NewSession(db)
	// these fail harmlessly when the tables already exist
	s.Exec("create table rows (k int, v string, primary key (k));")
	s.Exec("create table counter (id int, n int, primary key (id));")
	s.Exec("insert into counter values (1, 0);")
	for {
		res, err := s.Exec("select n from counter;")
		if err != nil {
			fmt.Println("ERR", err)
			os.Exit(1)
		}
		n := res[0].Rows[0][0].I64 + 1
		pad := strings.Repeat("x", int(n%500))
		sql := fmt.Sprintf("begin; insert into rows values (%d, '%s'); update counter set n = %d; commit;", n, pad, n)
		if _, err := s.Exec(sql); err != nil {
			fmt.Println("ERR", err)
			os.Exit(1)
		}
		fmt.Println(n)
	}
}

func TestMain(m *testing.M) {
	if p := os.Getenv("BYODB_CRASH_CHILD"); p != "" {
		crashChild(p)
		return
	}
	os.Exit(m.Run())
}

// TestCrashRecovery kills a writer process at random points and checks that
// every acknowledged commit survives and that the file is always consistent.
func TestCrashRecovery(t *testing.T) {
	if testing.Short() {
		t.Skip("slow")
	}
	path := filepath.Join(t.TempDir(), "crash.db")
	acked := int64(0)
	for round := 0; round < 12; round++ {
		cmd := exec.Command(os.Args[0], "-test.run=^$")
		cmd.Env = append(os.Environ(), "BYODB_CRASH_CHILD="+path)
		out, err := cmd.StdoutPipe()
		if err != nil {
			t.Fatal(err)
		}
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		done := make(chan struct{})
		go func() {
			sc := bufio.NewScanner(out)
			for sc.Scan() {
				line := sc.Text()
				if n, err := strconv.ParseInt(line, 10, 64); err == nil {
					acked = n
				} else {
					t.Errorf("child: %s", line)
				}
			}
			close(done)
		}()
		time.Sleep(time.Duration(50+round*37%200) * time.Millisecond)
		cmd.Process.Kill()
		<-done
		cmd.Wait()

		db := &DB{Path: path}
		if err := db.Open(); err != nil {
			t.Fatalf("round %d: reopen: %v", round, err)
		}
		if _, err := db.Verify(); err != nil {
			t.Fatalf("round %d: verify: %v", round, err)
		}
		s := NewSession(db)
		res, err := s.Exec("select n from counter;")
		if err != nil {
			t.Fatal(err)
		}
		n := res[0].Rows[0][0].I64
		rows, err := s.Exec("select k from rows;")
		if err != nil {
			t.Fatal(err)
		}
		if int64(len(rows[0].Rows)) != n {
			t.Fatalf("round %d: counter %d but %d rows", round, n, len(rows[0].Rows))
		}
		if n < acked || n > acked+1 {
			t.Fatalf("round %d: acked %d, found %d", round, acked, n)
		}
		s.Close()
		db.Close()
	}
	t.Logf("survived 12 kills, %d commits", acked)
}
