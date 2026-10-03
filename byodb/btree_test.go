package byodb

import (
	"bytes"
	"fmt"
	"math/rand"
	"sort"
	"testing"
)

// memTree is a B+tree backed by a map, tracking page ownership strictly.
type memTree struct {
	tree  BTree
	pages map[uint64]BNode
	next  uint64
	ref   map[string]string
}

func newMemTree() *memTree {
	c := &memTree{pages: map[uint64]BNode{}, ref: map[string]string{}}
	c.tree.get = func(ptr uint64) []byte {
		node, ok := c.pages[ptr]
		assert(ok, "get: dangling page")
		return node
	}
	c.tree.new = func(node []byte) uint64 {
		assert(BNode(node).nbytes() <= BTREE_PAGE_SIZE, "new: oversized node")
		c.next++
		c.pages[c.next] = append([]byte(nil), node[:BTREE_PAGE_SIZE]...)
		return c.next
	}
	c.tree.del = func(ptr uint64) {
		_, ok := c.pages[ptr]
		assert(ok, "del: double free")
		delete(c.pages, ptr)
	}
	return c
}

func (c *memTree) add(key, val string) {
	c.tree.Insert([]byte(key), []byte(val))
	c.ref[key] = val
}

func (c *memTree) del(key string) bool {
	delete(c.ref, key)
	return c.tree.Delete([]byte(key))
}

// verify checks structure, ordering, page accounting, and content.
func (c *memTree) verify(t *testing.T) {
	t.Helper()
	if c.tree.root == 0 {
		if len(c.ref) != 0 {
			t.Fatal("empty tree but reference has keys")
		}
		return
	}
	seen := map[uint64]bool{}
	leafDepth := -1
	var walk func(ptr uint64, depth int, lower []byte)
	walk = func(ptr uint64, depth int, lower []byte) {
		if seen[ptr] {
			t.Fatalf("page %d reachable twice", ptr)
		}
		seen[ptr] = true
		node := BNode(c.tree.get(ptr))
		nk := node.nkeys()
		if nk == 0 {
			t.Fatalf("empty node %d", ptr)
		}
		if !bytes.Equal(node.getKey(0), lower) {
			t.Fatalf("separator mismatch at page %d", ptr)
		}
		for i := uint16(1); i < nk; i++ {
			if bytes.Compare(node.getKey(i-1), node.getKey(i)) >= 0 {
				t.Fatalf("keys out of order in page %d", ptr)
			}
		}
		if node.btype() == BNODE_LEAF {
			if leafDepth < 0 {
				leafDepth = depth
			} else if leafDepth != depth {
				t.Fatal("tree is not height-balanced")
			}
			return
		}
		for i := uint16(0); i < nk; i++ {
			walk(node.getPtr(i), depth+1, node.getKey(i))
		}
	}
	walk(c.tree.root, 0, nil)
	if len(seen) != len(c.pages) {
		t.Fatalf("page leak: %d reachable, %d allocated", len(seen), len(c.pages))
	}

	// full forward scan must equal the sorted reference (plus the sentinel)
	keys := make([]string, 0, len(c.ref))
	for k := range c.ref {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	it := c.tree.SeekLE(nil)
	k, _ := it.Deref()
	if len(k) != 0 {
		t.Fatal("first key is not the sentinel")
	}
	it.Next()
	for _, want := range keys {
		if !it.Valid() {
			t.Fatal("iterator ended early")
		}
		k, v := it.Deref()
		if string(k) != want || string(v) != c.ref[want] {
			t.Fatalf("scan mismatch: got %q want %q", k, want)
		}
		it.Next()
	}
	if it.Valid() {
		t.Fatal("iterator has extra keys")
	}
}

func TestBTreeBasic(t *testing.T) {
	c := newMemTree()
	c.add("k", "v")
	c.verify(t)
	for i := 0; i < 20000; i++ {
		c.add(fmt.Sprintf("key%08d", (i*7919)%20000), fmt.Sprintf("val%d", i))
	}
	c.verify(t)
	for i := 0; i < 20000; i += 2 {
		if !c.del(fmt.Sprintf("key%08d", i)) {
			t.Fatal("delete failed")
		}
	}
	if c.del("nope") {
		t.Fatal("deleted a missing key")
	}
	c.verify(t)
	for i := 1; i < 20000; i += 2 {
		c.del(fmt.Sprintf("key%08d", i))
	}
	c.del("k")
	c.verify(t)
	if len(c.pages) != 1 {
		t.Fatalf("tree should shrink to one leaf, has %d pages", len(c.pages))
	}
}

func TestBTreeRandomSizes(t *testing.T) {
	for seed := int64(1); seed <= 20; seed++ {
		testBTreeRandom(t, seed)
	}
}

func testBTreeRandom(t *testing.T, seed int64) {
	rng := rand.New(rand.NewSource(seed))
	c := newMemTree()
	randBytes := func(min, max int) string {
		b := make([]byte, min+rng.Intn(max-min+1))
		rng.Read(b)
		return string(b)
	}
	var keys []string
	for round := 0; round < 4000; round++ {
		switch op := rng.Intn(10); {
		case op < 6 || len(keys) == 0: // insert, sometimes with huge KVs
			kmax, vmax := 30, 100
			if rng.Intn(10) == 0 {
				kmax, vmax = BTREE_MAX_KEY_SIZE, BTREE_MAX_VAL_SIZE
			}
			k := randBytes(1, kmax)
			c.add(k, randBytes(0, vmax))
			keys = append(keys, k)
		case op < 8: // overwrite
			k := keys[rng.Intn(len(keys))]
			c.add(k, randBytes(0, 200))
		default: // delete
			i := rng.Intn(len(keys))
			c.del(keys[i])
			keys[i] = keys[len(keys)-1]
			keys = keys[:len(keys)-1]
		}
		if round%100 == 0 {
			c.verify(t)
		}
	}
	c.verify(t)
}

func TestBTreeSeek(t *testing.T) {
	c := newMemTree()
	for i := 0; i < 1000; i += 10 {
		c.add(fmt.Sprintf("%04d", i), "")
	}
	cases := []struct {
		ref  string
		cmp  int
		want string
	}{
		{"0500", CMP_GE, "0500"}, {"0500", CMP_GT, "0510"},
		{"0500", CMP_LE, "0500"}, {"0500", CMP_LT, "0490"},
		{"0505", CMP_GE, "0510"}, {"0505", CMP_LE, "0500"},
		{"0990", CMP_GT, ""}, {"9999", CMP_LE, "0990"},
	}
	for _, tc := range cases {
		it := c.tree.Seek([]byte(tc.ref), tc.cmp)
		got := ""
		if it.Valid() {
			k, _ := it.Deref()
			got = string(k)
		}
		if got != tc.want {
			t.Errorf("Seek(%s, %d) = %q, want %q", tc.ref, tc.cmp, got, tc.want)
		}
	}
	// backward scan across leaves
	n := 0
	for it := c.tree.Seek([]byte("0990"), CMP_LE); it.Valid(); it.Prev() {
		n++
	}
	if n != 101 { // 100 keys + sentinel
		t.Fatalf("backward scan saw %d keys", n)
	}
}
