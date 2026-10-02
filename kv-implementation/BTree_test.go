package kv_implementation

import (
    "testing"
    "unsafe"
)

type C struct {
    tree  BTree
    ref   map[string]string   // the reference data
    pages map[uint64]BNode    // in-memory pages
}

func newC() *C {
    pages := map[uint64]BNode{}
    return &C{
        tree: BTree{
            get: func(ptr uint64) []byte {
                node, ok := pages[ptr]
                assert(ok)
                return node
            },
            new: func(node []byte) uint64 {
                assert(BNode(node).nbytes() <= BTREE_PAGE_SIZE)
                // Generate a fake pointer using the memory address
                ptr := uint64(uintptr(unsafe.Pointer(&node[0])))
                assert(pages[ptr] == nil)
                pages[ptr] = node
                return ptr
            },
            del: func(ptr uint64) {
                assert(pages[ptr] != nil)
                delete(pages, ptr)
            },
        },
        ref:   map[string]string{},
        pages: pages,
    }
}


func (c *C) add(key string, val string) {
    c.tree.Insert([]byte(key), []byte(val))
    c.ref[key] = val // reference data
}

func (c *C) del(key string) {
	c.tree.Delete([]byte(key))
	delete(c.ref,key)
}

// verify compares the BTree against the reference map.
func (c *C) verify(t *testing.T) bool {
	for k, v := range c.ref {
		val, found := c.tree.Get([]byte(k))
		if !found || string(val) != v {
			t.Errorf("Mismatch for key %s: expected %s, got %s", k, v, string(val))
			return false
		}
	}
	return true
}

func TestBTreeOperations(t *testing.T) {
	c := newC()

	// 1. Mutate the tree
	c.add("apple", "red")
	c.add("banana", "yellow")
	c.add("cherry", "red")

	c.del("banana")

	// 2. Verify the state
	if !c.verify(t) {
		t.Errorf("BTree state does not match reference map!")
	}
}