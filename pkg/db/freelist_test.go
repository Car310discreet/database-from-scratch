package db

import (
	"os"
	"testing"
)

func TestFreeList(t *testing.T) {
	os.Remove("test_freelist.db")
	defer os.Remove("test_freelist.db")

	kv := &KV{Path: "test_freelist.db"}
	if err := kv.Open(); err != nil {
		t.Fatalf("failed to open DB: %v", err)
	}

	// Make inserts
	keys := []string{"a", "b", "c", "d", "e"}
	for _, k := range keys {
		if err := kv.Set([]byte(k), []byte(k)); err != nil {
			t.Fatalf("failed to set %s: %v", k, err)
		}
	}

	// Delete them
	for _, k := range keys {
		if _, err := kv.Del([]byte(k)); err != nil {
			t.Fatalf("failed to del %s: %v", k, err)
		}
	}
	
	// Read stats to ensure tailSeq grew since nodes got deleted
	if kv.free.tailSeq == 0 {
		t.Fatalf("freelist tailSeq should be > 0, got %d", kv.free.tailSeq)
	}
	
	flushedBefore := kv.page.flushed

	// Insert over again, which should recycle pages!
	for _, k := range keys {
		if err := kv.Set([]byte(k), []byte(k+"_new")); err != nil {
			t.Fatalf("failed to re-set %s: %v", k, err)
		}
	}
	
	// DB total allocated pages shouldn't have drastically increased (it should reuse free nodes).
	flushedAfter := kv.page.flushed
	
	// Check results
	for _, k := range keys {
		v, _ := kv.Get([]byte(k))
		if string(v) != k+"_new" {
			t.Fatalf("expected val %s, got %s", k+"_new", string(v))
		}
	}
	
	if kv.free.headSeq == 0 {
		t.Fatalf("freelist headSeq should be > 0 (popped), got %d", kv.free.headSeq)
	}
	
	// Ideally, flushedAfter - flushedBefore is less than it would be without freelist.
	t.Logf("Flushed pages before recycle: %d, after recycle: %d", flushedBefore, flushedAfter)
	
	if err := kv.Close(); err != nil {
		t.Fatalf("failed to close DB: %v", err)
	}
}
