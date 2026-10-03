package byodb

import (
	"bytes"
	"encoding/binary"
)

// Node layout (every node is exactly one page on disk):
//
//	| type | nkeys |  pointers  |  offsets   | key-values | unused |
//	|  2B  |  2B   | nkeys * 8B | nkeys * 2B |    ...     |        |
//
// Each key-value pair:
//
//	| klen | vlen | key | val |
//	|  2B  |  2B  | ... | ... |
//
// Internal nodes use pointers + keys (empty values); leaves use keys +
// values (zero pointers). An internal node with n children stores n keys,
// key i being a copy of the smallest key reachable through child i.
const (
	HEADER             = 4
	BTREE_PAGE_SIZE    = 4096
	BTREE_MAX_KEY_SIZE = 1000
	BTREE_MAX_VAL_SIZE = 3000
)

const (
	BNODE_NODE = 1 // internal node
	BNODE_LEAF = 2 // leaf node
)

func init() {
	// One maximal KV (plus the 1-byte flag used by transaction-local trees)
	// must fit in a node, which also guarantees internal nodes hold >= 2 keys.
	node1max := HEADER + 8 + 2 + 4 + BTREE_MAX_KEY_SIZE + BTREE_MAX_VAL_SIZE + 1
	assert(node1max <= BTREE_PAGE_SIZE, "a single KV must fit in one page")
}

func assert(cond bool, msg string) {
	if !cond {
		panic("byodb: assertion failed: " + msg)
	}
}

// BNode is a node interpreted directly from its on-disk bytes.
type BNode []byte

func (n BNode) btype() uint16 { return binary.LittleEndian.Uint16(n[0:2]) }
func (n BNode) nkeys() uint16 { return binary.LittleEndian.Uint16(n[2:4]) }

func (n BNode) setHeader(btype, nkeys uint16) {
	binary.LittleEndian.PutUint16(n[0:2], btype)
	binary.LittleEndian.PutUint16(n[2:4], nkeys)
}

func (n BNode) getPtr(idx uint16) uint64 {
	assert(idx < n.nkeys(), "getPtr: index out of range")
	return binary.LittleEndian.Uint64(n[HEADER+8*int(idx):])
}

func (n BNode) setPtr(idx uint16, ptr uint64) {
	assert(idx < n.nkeys(), "setPtr: index out of range")
	binary.LittleEndian.PutUint64(n[HEADER+8*int(idx):], ptr)
}

// The offset list stores, for i in 1..nkeys, where KV i starts relative to
// the beginning of the KV area. Offset 0 is implicit.
func (n BNode) offsetPos(idx uint16) int {
	assert(1 <= idx && idx <= n.nkeys(), "offsetPos: index out of range")
	return HEADER + 8*int(n.nkeys()) + 2*int(idx-1)
}

func (n BNode) getOffset(idx uint16) uint16 {
	if idx == 0 {
		return 0
	}
	return binary.LittleEndian.Uint16(n[n.offsetPos(idx):])
}

func (n BNode) setOffset(idx uint16, off uint16) {
	binary.LittleEndian.PutUint16(n[n.offsetPos(idx):], off)
}

func (n BNode) kvPos(idx uint16) int {
	assert(idx <= n.nkeys(), "kvPos: index out of range")
	return HEADER + 10*int(n.nkeys()) + int(n.getOffset(idx))
}

func (n BNode) getKey(idx uint16) []byte {
	assert(idx < n.nkeys(), "getKey: index out of range")
	pos := n.kvPos(idx)
	klen := int(binary.LittleEndian.Uint16(n[pos:]))
	return n[pos+4 : pos+4+klen : pos+4+klen]
}

func (n BNode) getVal(idx uint16) []byte {
	assert(idx < n.nkeys(), "getVal: index out of range")
	pos := n.kvPos(idx)
	klen := int(binary.LittleEndian.Uint16(n[pos:]))
	vlen := int(binary.LittleEndian.Uint16(n[pos+2:]))
	start := pos + 4 + klen
	return n[start : start+vlen : start+vlen]
}

// nbytes is the used size of the node.
func (n BNode) nbytes() int { return n.kvPos(n.nkeys()) }

// nodeAppendKV writes KV number idx. KVs must be appended in order because
// each one derives its offset from the previous one.
func nodeAppendKV(dst BNode, idx uint16, ptr uint64, key, val []byte) {
	dst.setPtr(idx, ptr)
	pos := dst.kvPos(idx)
	binary.LittleEndian.PutUint16(dst[pos:], uint16(len(key)))
	binary.LittleEndian.PutUint16(dst[pos+2:], uint16(len(val)))
	copy(dst[pos+4:], key)
	copy(dst[pos+4+len(key):], val)
	dst.setOffset(idx+1, dst.getOffset(idx)+4+uint16(len(key)+len(val)))
}

// nodeAppendRange copies n KVs from src[srcIdx:] into dst[dstIdx:].
func nodeAppendRange(dst, src BNode, dstIdx, srcIdx, n uint16) {
	for i := uint16(0); i < n; i++ {
		nodeAppendKV(dst, dstIdx+i, src.getPtr(srcIdx+i), src.getKey(srcIdx+i), src.getVal(srcIdx+i))
	}
}

// nodeLookupLE returns the index of the last key <= key. Key 0 of any node we
// search is always <= key (the empty sentinel on the leftmost path, or the
// separator the parent used to route us here), so the result is well defined.
func nodeLookupLE(node BNode, key []byte) uint16 {
	lo, hi := uint16(1), node.nkeys()
	for lo < hi {
		mid := lo + (hi-lo)/2
		if bytes.Compare(node.getKey(mid), key) <= 0 {
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	return lo - 1
}

func leafInsert(dst, old BNode, idx uint16, key, val []byte) {
	dst.setHeader(BNODE_LEAF, old.nkeys()+1)
	nodeAppendRange(dst, old, 0, 0, idx)
	nodeAppendKV(dst, idx, 0, key, val)
	nodeAppendRange(dst, old, idx+1, idx, old.nkeys()-idx)
}

func leafUpdate(dst, old BNode, idx uint16, key, val []byte) {
	dst.setHeader(BNODE_LEAF, old.nkeys())
	nodeAppendRange(dst, old, 0, 0, idx)
	nodeAppendKV(dst, idx, 0, key, val)
	nodeAppendRange(dst, old, idx+1, idx+1, old.nkeys()-idx-1)
}

func leafDelete(dst, old BNode, idx uint16) {
	dst.setHeader(BNODE_LEAF, old.nkeys()-1)
	nodeAppendRange(dst, old, 0, 0, idx)
	nodeAppendRange(dst, old, idx, idx+1, old.nkeys()-idx-1)
}

func nodeMerge(dst, left, right BNode) {
	dst.setHeader(left.btype(), left.nkeys()+right.nkeys())
	nodeAppendRange(dst, left, 0, 0, left.nkeys())
	nodeAppendRange(dst, right, left.nkeys(), 0, right.nkeys())
}

// nodeReplaceKidN replaces child idx with zero or more new children.
func nodeReplaceKidN(tree *BTree, dst, old BNode, idx uint16, kids ...BNode) {
	inc := uint16(len(kids))
	dst.setHeader(BNODE_NODE, old.nkeys()+inc-1)
	nodeAppendRange(dst, old, 0, 0, idx)
	for i, kid := range kids {
		nodeAppendKV(dst, idx+uint16(i), tree.new(kid), kid.getKey(0), nil)
	}
	nodeAppendRange(dst, old, idx+inc, idx+1, old.nkeys()-(idx+1))
}

// nodeReplace2Kid replaces the adjacent children idx and idx+1 with one link.
func nodeReplace2Kid(dst, old BNode, idx uint16, ptr uint64, key []byte) {
	dst.setHeader(BNODE_NODE, old.nkeys()-1)
	nodeAppendRange(dst, old, 0, 0, idx)
	nodeAppendKV(dst, idx, ptr, key, nil)
	nodeAppendRange(dst, old, idx+1, idx+2, old.nkeys()-(idx+2))
}

// nodeSplit2 splits old into two nodes; the right one is guaranteed to fit.
func nodeSplit2(left, right, old BNode) {
	nk := old.nkeys()
	assert(nk >= 2, "split: need at least 2 keys")
	leftBytes := func(n uint16) int { return HEADER + 10*int(n) + int(old.getOffset(n)) }
	rightBytes := func(n uint16) int { return old.nbytes() - leftBytes(n) + HEADER }

	nleft := nk / 2
	for nleft > 1 && leftBytes(nleft) > BTREE_PAGE_SIZE {
		nleft--
	}
	for nleft < nk-1 && rightBytes(nleft) > BTREE_PAGE_SIZE {
		nleft++
	}
	assert(rightBytes(nleft) <= BTREE_PAGE_SIZE, "split: right half too big")

	left.setHeader(old.btype(), nleft)
	nodeAppendRange(left, old, 0, 0, nleft)
	right.setHeader(old.btype(), nk-nleft)
	nodeAppendRange(right, old, 0, nleft, nk-nleft)
}

// nodeSplit3 splits an oversized node into 1-3 page-sized nodes.
func nodeSplit3(old BNode) (uint16, [3]BNode) {
	if old.nbytes() <= BTREE_PAGE_SIZE {
		return 1, [3]BNode{old[:BTREE_PAGE_SIZE]}
	}
	left := BNode(make([]byte, 2*BTREE_PAGE_SIZE)) // may be split again
	right := BNode(make([]byte, BTREE_PAGE_SIZE))
	nodeSplit2(left, right, old)
	if left.nbytes() <= BTREE_PAGE_SIZE {
		return 2, [3]BNode{left[:BTREE_PAGE_SIZE], right}
	}
	leftleft := BNode(make([]byte, BTREE_PAGE_SIZE))
	middle := BNode(make([]byte, BTREE_PAGE_SIZE))
	nodeSplit2(leftleft, middle, left)
	assert(leftleft.nbytes() <= BTREE_PAGE_SIZE, "split: 3-way split failed")
	return 3, [3]BNode{leftleft, middle, right}
}

// BTree is a copy-on-write B+tree. Page management is delegated to callbacks
// so the same code serves the on-disk tree and in-memory trees.
type BTree struct {
	root uint64              // page number of the root; 0 means empty
	get  func(uint64) []byte // dereference a page (must not be modified)
	new  func([]byte) uint64 // allocate a page with the given content
	del  func(uint64)        // release a page
}

// Get performs a point lookup.
func (tree *BTree) Get(key []byte) ([]byte, bool) {
	if tree.root == 0 {
		return nil, false
	}
	node := BNode(tree.get(tree.root))
	for {
		idx := nodeLookupLE(node, key)
		if node.btype() == BNODE_LEAF {
			if bytes.Equal(key, node.getKey(idx)) {
				return node.getVal(idx), true
			}
			return nil, false
		}
		node = BNode(tree.get(node.getPtr(idx)))
	}
}

func treeInsert(tree *BTree, node BNode, key, val []byte) BNode {
	// The result may temporarily exceed one page; the caller splits it.
	dst := BNode(make([]byte, 2*BTREE_PAGE_SIZE))
	idx := nodeLookupLE(node, key)
	switch node.btype() {
	case BNODE_LEAF:
		if bytes.Equal(key, node.getKey(idx)) {
			leafUpdate(dst, node, idx, key, val)
		} else {
			leafInsert(dst, node, idx+1, key, val)
		}
	case BNODE_NODE:
		kptr := node.getPtr(idx)
		knode := treeInsert(tree, BNode(tree.get(kptr)), key, val)
		nsplit, split := nodeSplit3(knode)
		tree.del(kptr)
		nodeReplaceKidN(tree, dst, node, idx, split[:nsplit]...)
	default:
		panic("byodb: corrupted node type")
	}
	return dst
}

// Insert adds a key or replaces its value.
func (tree *BTree) Insert(key, val []byte) {
	assert(len(key) <= BTREE_MAX_KEY_SIZE && len(val) <= BTREE_MAX_VAL_SIZE+1, "KV too large")
	if tree.root == 0 {
		root := BNode(make([]byte, BTREE_PAGE_SIZE))
		root.setHeader(BNODE_LEAF, 2)
		// The empty key is a sentinel: it is the smallest possible key, so
		// every lookup finds a node position that is <= the search key.
		nodeAppendKV(root, 0, 0, nil, nil)
		nodeAppendKV(root, 1, 0, key, val)
		tree.root = tree.new(root)
		return
	}
	node := treeInsert(tree, BNode(tree.get(tree.root)), key, val)
	nsplit, split := nodeSplit3(node)
	tree.del(tree.root)
	if nsplit > 1 {
		root := BNode(make([]byte, BTREE_PAGE_SIZE))
		root.setHeader(BNODE_NODE, nsplit)
		for i, knode := range split[:nsplit] {
			nodeAppendKV(root, uint16(i), tree.new(knode), knode.getKey(0), nil)
		}
		tree.root = tree.new(root)
	} else {
		tree.root = tree.new(split[0])
	}
}

// shouldMerge decides whether a shrunken child should merge with a sibling.
func shouldMerge(tree *BTree, node BNode, idx uint16, updated BNode) (int, BNode) {
	if updated.nbytes() > BTREE_PAGE_SIZE/4 {
		return 0, nil
	}
	if idx > 0 {
		sibling := BNode(tree.get(node.getPtr(idx - 1)))
		if sibling.nbytes()+updated.nbytes()-HEADER <= BTREE_PAGE_SIZE {
			return -1, sibling
		}
	}
	if idx+1 < node.nkeys() {
		sibling := BNode(tree.get(node.getPtr(idx + 1)))
		if sibling.nbytes()+updated.nbytes()-HEADER <= BTREE_PAGE_SIZE {
			return +1, sibling
		}
	}
	return 0, nil
}

func treeDelete(tree *BTree, node BNode, key []byte) BNode {
	idx := nodeLookupLE(node, key)
	switch node.btype() {
	case BNODE_LEAF:
		if !bytes.Equal(key, node.getKey(idx)) {
			return nil
		}
		dst := BNode(make([]byte, BTREE_PAGE_SIZE))
		leafDelete(dst, node, idx)
		return dst
	case BNODE_NODE:
		return nodeDelete(tree, node, idx, key)
	default:
		panic("byodb: corrupted node type")
	}
}

func nodeDelete(tree *BTree, node BNode, idx uint16, key []byte) BNode {
	kptr := node.getPtr(idx)
	updated := treeDelete(tree, BNode(tree.get(kptr)), key)
	if updated == nil {
		return nil
	}
	tree.del(kptr)

	// Deleting a child's smallest key replaces its separator here with the
	// next key, which may be longer, so this node can grow and even overflow.
	dst := BNode(make([]byte, 2*BTREE_PAGE_SIZE))
	if updated.nbytes() > BTREE_PAGE_SIZE {
		nsplit, split := nodeSplit3(updated)
		nodeReplaceKidN(tree, dst, node, idx, split[:nsplit]...)
		return dst
	}
	dir, sibling := shouldMerge(tree, node, idx, updated)
	switch {
	case dir < 0:
		merged := BNode(make([]byte, BTREE_PAGE_SIZE))
		nodeMerge(merged, sibling, updated)
		tree.del(node.getPtr(idx - 1))
		nodeReplace2Kid(dst, node, idx-1, tree.new(merged), merged.getKey(0))
	case dir > 0:
		merged := BNode(make([]byte, BTREE_PAGE_SIZE))
		nodeMerge(merged, updated, sibling)
		tree.del(node.getPtr(idx + 1))
		nodeReplace2Kid(dst, node, idx, tree.new(merged), merged.getKey(0))
	case updated.nkeys() == 0:
		// An empty child with no sibling to merge into is simply unlinked;
		// the parent may become empty too and is handled one level up.
		nodeReplaceKidN(tree, dst, node, idx)
	default:
		nodeReplaceKidN(tree, dst, node, idx, updated)
	}
	return dst
}

// Delete removes a key and reports whether it existed.
func (tree *BTree) Delete(key []byte) bool {
	if tree.root == 0 || len(key) == 0 { // never delete the sentinel
		return false
	}
	updated := treeDelete(tree, BNode(tree.get(tree.root)), key)
	if updated == nil {
		return false
	}
	tree.del(tree.root)
	nsplit, split := nodeSplit3(updated)
	switch {
	case nsplit > 1: // the root grew (see nodeDelete) and must split
		root := BNode(make([]byte, BTREE_PAGE_SIZE))
		root.setHeader(BNODE_NODE, nsplit)
		for i, knode := range split[:nsplit] {
			nodeAppendKV(root, uint16(i), tree.new(knode), knode.getKey(0), nil)
		}
		tree.root = tree.new(root)
	case updated.btype() == BNODE_NODE && updated.nkeys() == 1:
		tree.root = updated.getPtr(0) // drop a level
	default:
		tree.root = tree.new(split[0])
	}
	return true
}
