package byodb

import (
	"bytes"
	"fmt"
)

// Stats describes the database file.
type Stats struct {
	Version    uint64 // number of commits
	Pages      uint64 // file size in pages
	FreePages  uint64 // pages in the free list
	ListPages  uint64 // pages holding the free list itself
	TreePages  uint64 // pages reachable from the B+tree root
	TreeHeight int
	Keys       uint64 // KV pairs, excluding the sentinel
}

// Verify checks the on-disk structure of the committed state: B+tree
// ordering, balance and separators, and that every page in the file is
// exactly one of: the meta page, a tree node, a free list node, or a free
// list item.
func (kv *KV) Verify() (Stats, error) {
	kv.mu.Lock()
	defer kv.mu.Unlock()
	var st Stats
	st.Version, st.Pages = kv.version, kv.page.flushed
	owner := make([]string, kv.page.flushed)
	claim := func(ptr uint64, what string) error {
		if ptr == 0 || ptr >= kv.page.flushed {
			return fmt.Errorf("%s: page %d out of range", what, ptr)
		}
		if owner[ptr] != "" {
			return fmt.Errorf("page %d used as both %s and %s", ptr, owner[ptr], what)
		}
		owner[ptr] = what
		return nil
	}
	read := func(ptr uint64) []byte { return mmapRead(ptr, kv.mmap.chunks) }
	owner[0] = "meta"

	// the B+tree
	leafDepth := -1
	var walk func(ptr uint64, depth int, lower []byte, upper []byte) error
	walk = func(ptr uint64, depth int, lower, upper []byte) error {
		if err := claim(ptr, "tree node"); err != nil {
			return err
		}
		st.TreePages++
		node := BNode(read(ptr))
		nk := node.nkeys()
		if nk == 0 || node.nbytes() > BTREE_PAGE_SIZE {
			return fmt.Errorf("tree node %d is malformed", ptr)
		}
		if !bytes.Equal(node.getKey(0), lower) {
			return fmt.Errorf("tree node %d: first key does not match its separator", ptr)
		}
		for i := uint16(1); i < nk; i++ {
			if bytes.Compare(node.getKey(i-1), node.getKey(i)) >= 0 {
				return fmt.Errorf("tree node %d: keys out of order", ptr)
			}
		}
		if upper != nil && bytes.Compare(node.getKey(nk-1), upper) >= 0 {
			return fmt.Errorf("tree node %d: key beyond the next separator", ptr)
		}
		switch node.btype() {
		case BNODE_LEAF:
			if leafDepth < 0 {
				leafDepth = depth
			} else if leafDepth != depth {
				return fmt.Errorf("tree is not balanced")
			}
			st.Keys += uint64(nk)
		case BNODE_NODE:
			for i := uint16(0); i < nk; i++ {
				next := upper
				if i+1 < nk {
					next = node.getKey(i + 1)
				}
				if err := walk(node.getPtr(i), depth+1, node.getKey(i), next); err != nil {
					return err
				}
			}
		default:
			return fmt.Errorf("tree node %d has a bad type", ptr)
		}
		return nil
	}
	if kv.tree.root != 0 {
		if err := walk(kv.tree.root, 0, nil, nil); err != nil {
			return st, err
		}
		st.Keys-- // sentinel
		st.TreeHeight = leafDepth + 1
	}

	// the free list: its nodes and its items
	fl := &kv.free
	ptr := fl.headPage
	for {
		if err := claim(ptr, "free list node"); err != nil {
			return st, err
		}
		st.ListPages++
		if ptr == fl.tailPage {
			break
		}
		ptr = LNode(read(ptr)).getNext()
	}
	page := fl.headPage
	for seq := fl.headSeq; seq < fl.tailSeq; seq++ {
		item, ver := LNode(read(page)).getItem(seq2idx(seq))
		if ver > kv.version {
			return st, fmt.Errorf("free item %d has a future version %d", item, ver)
		}
		if err := claim(item, "free page"); err != nil {
			return st, err
		}
		st.FreePages++
		if seq2idx(seq+1) == 0 {
			page = LNode(read(page)).getNext()
		}
	}

	for p, o := range owner {
		if o == "" {
			return st, fmt.Errorf("page %d is leaked", p)
		}
	}
	return st, nil
}
