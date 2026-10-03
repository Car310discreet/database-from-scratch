package byodb

import "bytes"

// Comparison operators for seeks and range bounds.
const (
	CMP_GE = +3 // >=
	CMP_GT = +2 // >
	CMP_LT = -2 // <
	CMP_LE = -3 // <=
)

// cmpOK reports whether `key <cmp> ref` holds.
func cmpOK(key []byte, cmp int, ref []byte) bool {
	r := bytes.Compare(key, ref)
	switch cmp {
	case CMP_GE:
		return r >= 0
	case CMP_GT:
		return r > 0
	case CMP_LT:
		return r < 0
	case CMP_LE:
		return r <= 0
	default:
		panic("byodb: bad comparison operator")
	}
}

// BIter is a position in a B+tree. Without parent pointers, it keeps the
// whole root-to-leaf path so it can climb back up to reach sibling nodes.
type BIter struct {
	tree *BTree
	path []BNode  // root first, leaf last
	pos  []uint16 // index into each node on the path
}

// SeekLE positions the iterator at the last key <= key.
func (tree *BTree) SeekLE(key []byte) *BIter {
	it := &BIter{tree: tree}
	for ptr := tree.root; ptr != 0; {
		node := BNode(tree.get(ptr))
		idx := nodeLookupLE(node, key)
		it.path = append(it.path, node)
		it.pos = append(it.pos, idx)
		if node.btype() == BNODE_LEAF {
			break
		}
		ptr = node.getPtr(idx)
	}
	return it
}

// Seek positions the iterator at the first key satisfying `key <cmp> ref`
// in the direction implied by cmp (forward for > and >=, backward otherwise).
func (tree *BTree) Seek(ref []byte, cmp int) *BIter {
	it := tree.SeekLE(ref)
	if cmp != CMP_LE && it.Valid() {
		cur, _ := it.Deref()
		if !cmpOK(cur, cmp, ref) {
			if cmp > 0 {
				it.Next()
			} else {
				it.Prev()
			}
		}
	}
	return it
}

func (it *BIter) Valid() bool {
	n := len(it.path)
	return n > 0 && it.pos[n-1] < it.path[n-1].nkeys()
}

// Deref returns the current KV pair. Precondition: Valid().
func (it *BIter) Deref() ([]byte, []byte) {
	n := len(it.path)
	leaf, idx := it.path[n-1], it.pos[n-1]
	return leaf.getKey(idx), leaf.getVal(idx)
}

func (it *BIter) Next() {
	if !it.Valid() {
		return
	}
	last := len(it.path) - 1
	if !iterMove(it, last, +1) {
		it.pos[last] = it.path[last].nkeys() // past the end
	}
}

func (it *BIter) Prev() {
	if !it.Valid() {
		return
	}
	last := len(it.path) - 1
	if !iterMove(it, last, -1) {
		it.pos[last] = 0xffff // before the beginning
	}
}

// iterMove moves one step at `level`, carrying into the parent like a digit
// overflow when the node is exhausted. Returns false at either end of the tree.
func iterMove(it *BIter, level int, dir int) bool {
	node := it.path[level]
	switch {
	case dir > 0 && it.pos[level]+1 < node.nkeys():
		it.pos[level]++
	case dir < 0 && it.pos[level] > 0:
		it.pos[level]--
	case level == 0:
		return false
	case !iterMove(it, level-1, dir):
		return false
	default:
		// the parent moved to a new child; load it
		parent := it.path[level-1]
		kid := BNode(it.tree.get(parent.getPtr(it.pos[level-1])))
		it.path[level] = kid
		if dir > 0 {
			it.pos[level] = 0
		} else {
			it.pos[level] = kid.nkeys() - 1
		}
	}
	return true
}
