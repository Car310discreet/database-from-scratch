package byodb

import (
	"errors"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"testing"
)

func openKV(t *testing.T, path string) *KV {
	t.Helper()
	kv := &KV{Path: path}
	if err := kv.Open(); err != nil {
		t.Fatal(err)
	}
	return kv
}

func mustVerify(t *testing.T, kv *KV) Stats {
	t.Helper()
	st, err := kv.Verify()
	if err != nil {
		t.Fatal(err)
	}
	return st
}

func TestKVPersistence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "kv.db")
	kv := openKV(t, path)
	ref := map[string]string{}
	rng := rand.New(rand.NewSource(7))
	for i := 0; i < 300; i++ {
		var tx KVTX
		kv.Begin(&tx)
		for j := 0; j < 20; j++ {
			k := fmt.Sprintf("k%05d", rng.Intn(3000))
			if rng.Intn(4) == 0 {
				tx.Del(&DeleteReq{Key: []byte(k)})
				delete(ref, k)
			} else {
				v := fmt.Sprintf("v%d-%d", i, j)
				tx.Set([]byte(k), []byte(v))
				ref[k] = v
			}
		}
		if err := kv.Commit(&tx); err != nil {
			t.Fatal(err)
		}
	}
	st := mustVerify(t, kv)
	if st.Keys != uint64(len(ref)) {
		t.Fatalf("keys = %d, want %d", st.Keys, len(ref))
	}
	kv.Close()

	kv = openKV(t, path) // reopen and compare
	defer kv.Close()
	mustVerify(t, kv)
	for k, v := range ref {
		got, ok := kv.Get([]byte(k))
		if !ok || string(got) != v {
			t.Fatalf("after reopen %s = %q, want %q", k, got, v)
		}
	}
	if _, ok := kv.Get([]byte("missing")); ok {
		t.Fatal("found a missing key")
	}
}

func TestKVSpaceReuse(t *testing.T) {
	path := filepath.Join(t.TempDir(), "kv.db")
	kv := openKV(t, path)
	defer kv.Close()
	val := make([]byte, 500)
	for i := 0; i < 2000; i++ {
		kv.Set([]byte(fmt.Sprintf("key%d", i%100)), val)
	}
	st := mustVerify(t, kv)
	// 100 keys * 500B fits in ~15 pages; without reuse this would be ~6000.
	if st.Pages > 64 {
		t.Fatalf("file grew to %d pages; free pages are not being reused", st.Pages)
	}
	for i := 0; i < 100; i++ {
		kv.Del([]byte(fmt.Sprintf("key%d", i)))
	}
	st = mustVerify(t, kv)
	if st.Keys != 0 || st.TreePages != 1 {
		t.Fatalf("empty tree has %d keys in %d pages", st.Keys, st.TreePages)
	}
}

func TestKVBigTransaction(t *testing.T) {
	kv := openKV(t, filepath.Join(t.TempDir(), "kv.db"))
	defer kv.Close()
	var tx KVTX
	kv.Begin(&tx)
	for i := 0; i < 50000; i++ {
		tx.Set([]byte(fmt.Sprintf("key%08d", i)), []byte("value"))
	}
	if len(tx.pages) > 2000 {
		t.Fatalf("pending tree holds %d pages; freed pages are not released", len(tx.pages))
	}
	if err := kv.Commit(&tx); err != nil {
		t.Fatal(err)
	}
	st := mustVerify(t, kv)
	if st.Pages > 2*st.TreePages+10 {
		t.Fatalf("one commit used %d pages for a %d-page tree", st.Pages, st.TreePages)
	}
}

func TestKVWriteFailure(t *testing.T) {
	path := filepath.Join(t.TempDir(), "kv.db")
	kv := openKV(t, path)
	kv.Set([]byte("a"), []byte("1"))

	for _, stage := range []string{"write", "fsync", "meta"} {
		injected := errors.New("injected " + stage + " failure")
		kv.faults = func(s string) error {
			if s == stage {
				return injected
			}
			return nil
		}
		if err := kv.Set([]byte("a"), []byte("2")); !errors.Is(err, injected) {
			t.Fatalf("%s: expected the injected error, got %v", stage, err)
		}
		// reads still see the last committed state
		if v, _ := kv.Get([]byte("a")); string(v) != "1" {
			t.Fatalf("%s: after a failed commit a = %q", stage, v)
		}
		kv.faults = nil
		mustVerify(t, kv)
	}
	// the error was temporary: the DB recovers and keeps working
	if err := kv.Set([]byte("a"), []byte("3")); err != nil {
		t.Fatal(err)
	}
	kv.Close()
	kv = openKV(t, path)
	defer kv.Close()
	if v, _ := kv.Get([]byte("a")); string(v) != "3" {
		t.Fatalf("after recovery a = %q", v)
	}
	mustVerify(t, kv)
}

func TestKVCorruption(t *testing.T) {
	path := filepath.Join(t.TempDir(), "kv.db")
	kv := openKV(t, path)
	kv.Set([]byte("a"), []byte("1"))
	kv.Close()
	f, _ := os.OpenFile(path, os.O_RDWR, 0)
	f.WriteAt([]byte{0xff}, 20) // flip a byte in the meta page
	f.Close()
	kv = &KV{Path: path}
	if err := kv.Open(); !errors.Is(err, ErrCorrupted) {
		t.Fatalf("expected ErrCorrupted, got %v", err)
	}
}

func TestTxIsolationAndReadYourWrites(t *testing.T) {
	kv := openKV(t, filepath.Join(t.TempDir(), "kv.db"))
	defer kv.Close()
	for i := 0; i < 10; i++ {
		kv.Set([]byte(fmt.Sprintf("k%d", i)), []byte("old"))
	}
	var reader, writer KVTX
	kv.Begin(&reader)
	kv.Begin(&writer)
	writer.Set([]byte("k3"), []byte("new"))
	writer.Del(&DeleteReq{Key: []byte("k5")})
	writer.Set([]byte("k55"), []byte("added"))

	// the writer sees its own changes, merged in order
	var got []string
	for it := writer.Range([]byte("k"), CMP_GE, []byte("k9"), CMP_LE); it.Valid(); it.Next() {
		k, v := it.Deref()
		got = append(got, string(k)+"="+string(v))
	}
	want := "[k0=old k1=old k2=old k3=new k4=old k55=added k6=old k7=old k8=old k9=old]"
	if fmt.Sprint(got) != want {
		t.Fatalf("writer view:\n got %v\nwant %s", got, want)
	}
	if err := kv.Commit(&writer); err != nil {
		t.Fatal(err)
	}
	// the reader still sees its snapshot, even though pages were freed
	for i := 0; i < 300; i++ { // churn to tempt page reuse
		kv.Set([]byte(fmt.Sprintf("churn%d", i%7)), make([]byte, 2000))
	}
	if v, _ := reader.Get([]byte("k3")); string(v) != "old" {
		t.Fatalf("reader saw k3 = %q", v)
	}
	if _, ok := reader.Get([]byte("k5")); !ok {
		t.Fatal("reader lost k5")
	}
	kv.Abort(&reader)
	mustVerify(t, kv)
}

func TestTxIterateWhileWriting(t *testing.T) {
	kv := openKV(t, filepath.Join(t.TempDir(), "kv.db"))
	defer kv.Close()
	var tx KVTX
	kv.Begin(&tx)
	for i := 0; i < 1000; i++ {
		tx.Set([]byte(fmt.Sprintf("k%04d", i)), []byte("x"))
	}
	// delete every key while iterating, and add keys ahead of the cursor
	n := 0
	for it := tx.Range([]byte("k"), CMP_GE, []byte("l"), CMP_LT); it.Valid(); it.Next() {
		k, _ := it.Deref()
		tx.Del(&DeleteReq{Key: k})
		if n == 0 {
			tx.Set([]byte("k9999"), []byte("late"))
		}
		n++
	}
	if n != 1001 {
		t.Fatalf("visited %d keys, want 1001", n)
	}
	kv.Abort(&tx)
}

func TestTxConflicts(t *testing.T) {
	kv := openKV(t, filepath.Join(t.TempDir(), "kv.db"))
	defer kv.Close()
	kv.Set([]byte("a"), []byte("0"))
	kv.Set([]byte("b"), []byte("0"))

	// tx1 reads a and writes b; tx2 writes a and commits first -> conflict
	var tx1, tx2 KVTX
	kv.Begin(&tx1)
	kv.Begin(&tx2)
	tx1.Get([]byte("a"))
	tx1.Set([]byte("b"), []byte("1"))
	tx2.Set([]byte("a"), []byte("1"))
	if err := kv.Commit(&tx2); err != nil {
		t.Fatal(err)
	}
	if err := kv.Commit(&tx1); err != ErrConflict {
		t.Fatalf("expected a conflict, got %v", err)
	}

	// disjoint keys do not conflict
	kv.Begin(&tx1)
	kv.Begin(&tx2)
	tx1.Set([]byte("x"), []byte("1"))
	tx2.Set([]byte("y"), []byte("1"))
	if kv.Commit(&tx1) != nil || kv.Commit(&tx2) != nil {
		t.Fatal("disjoint transactions conflicted")
	}

	// a range read conflicts with an insert into that range (phantom)
	kv.Begin(&tx1)
	kv.Begin(&tx2)
	for it := tx1.Range([]byte("m"), CMP_GE, []byte("n"), CMP_LE); it.Valid(); it.Next() {
	}
	tx1.Set([]byte("summary"), []byte("0 rows"))
	tx2.Set([]byte("mango"), []byte("1"))
	kv.Commit(&tx2)
	if err := kv.Commit(&tx1); err != ErrConflict {
		t.Fatalf("expected a phantom conflict, got %v", err)
	}
	if len(kv.history) != 0 {
		t.Fatalf("history not trimmed: %d entries", len(kv.history))
	}
}

func TestTxConcurrentCounters(t *testing.T) {
	kv := openKV(t, filepath.Join(t.TempDir(), "kv.db"))
	defer kv.Close()
	const workers, rounds = 8, 50
	var wg sync.WaitGroup
	var conflicts sync.Map
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < rounds; i++ {
				for {
					var tx KVTX
					kv.Begin(&tx)
					v, _ := tx.Get([]byte("counter"))
					n := 0
					fmt.Sscan(string(v), &n)
					tx.Set([]byte("counter"), []byte(fmt.Sprint(n+1)))
					tx.Set([]byte(fmt.Sprintf("log/%d/%d", w, i)), []byte("done"))
					err := kv.Commit(&tx)
					if err == nil {
						break
					}
					if err != ErrConflict {
						t.Error(err)
						return
					}
					conflicts.Store(w, true)
				}
			}
		}(w)
	}
	wg.Wait()
	v, _ := kv.Get([]byte("counter"))
	if string(v) != fmt.Sprint(workers*rounds) {
		t.Fatalf("counter = %s, want %d (lost updates)", v, workers*rounds)
	}
	st := mustVerify(t, kv)
	if st.Keys != workers*rounds+1 {
		t.Fatalf("keys = %d", st.Keys)
	}
}

func TestTxSavepoint(t *testing.T) {
	kv := openKV(t, filepath.Join(t.TempDir(), "kv.db"))
	defer kv.Close()
	var tx KVTX
	kv.Begin(&tx)
	for i := 0; i < 500; i++ {
		tx.Set([]byte(fmt.Sprintf("keep%03d", i)), []byte("1"))
	}
	tx.savepoint()
	for i := 0; i < 500; i++ {
		tx.Set([]byte(fmt.Sprintf("drop%03d", i)), []byte("1"))
		tx.Del(&DeleteReq{Key: []byte(fmt.Sprintf("keep%03d", i))})
	}
	tx.rollbackToSavepoint()
	var keys []string
	for it := tx.Range([]byte("a"), CMP_GE, []byte("z"), CMP_LE); it.Valid(); it.Next() {
		k, _ := it.Deref()
		keys = append(keys, string(k))
	}
	if len(keys) != 500 || !sort.StringsAreSorted(keys) || keys[0] != "keep000" {
		t.Fatalf("after rollback: %d keys", len(keys))
	}
	kv.Commit(&tx)
	st := mustVerify(t, kv)
	if st.Keys != 500 {
		t.Fatalf("committed %d keys", st.Keys)
	}
}

func TestKVExclusiveOpen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lock.db")
	a := &KV{Path: path}
	if err := a.Open(); err != nil {
		t.Fatal(err)
	}
	// flock locks belong to the open file description, so a second open
	// in the same process conflicts too
	b := &KV{Path: path}
	if err := b.Open(); err == nil {
		t.Fatal("second open succeeded")
	}
	a.Close()
	if err := b.Open(); err != nil {
		t.Fatal(err)
	}
	b.Close()
}
