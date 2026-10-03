package byodb

import "encoding/binary"

// Free list node layout: a pointer to the next node, then items. Each item
// is a free page number plus the version at which it was freed, so pages
// still visible to an older snapshot are never handed out.
//
//	| next |    items (ptr + version)   | unused |
//	|  8B  |       n * (8B + 8B)        |  ...   |
const (
	FREE_LIST_HEADER = 8
	FREE_LIST_CAP    = (BTREE_PAGE_SIZE - FREE_LIST_HEADER) / 16
)

type LNode []byte

func (n LNode) getNext() uint64     { return binary.LittleEndian.Uint64(n[0:8]) }
func (n LNode) setNext(next uint64) { binary.LittleEndian.PutUint64(n[0:8], next) }

func (n LNode) getItem(idx int) (ptr, ver uint64) {
	pos := FREE_LIST_HEADER + 16*idx
	return binary.LittleEndian.Uint64(n[pos:]), binary.LittleEndian.Uint64(n[pos+8:])
}

func (n LNode) setItem(idx int, ptr, ver uint64) {
	pos := FREE_LIST_HEADER + 16*idx
	binary.LittleEndian.PutUint64(n[pos:], ptr)
	binary.LittleEndian.PutUint64(n[pos+8:], ver)
}

// FreeList is a FIFO of free pages stored in its own pages. Items are
// appended at the tail and consumed from the head. The list always has at
// least one node, which removes the empty-list special cases.
//
// Nodes are updated in place, but only by appending past the last item or
// by setting the next pointer of a full node, so the state described by the
// previous meta page is never overwritten.
type FreeList struct {
	// callbacks for page management
	get func(uint64) []byte // read a page
	new func([]byte) uint64 // append a new page
	set func(uint64) []byte // get a writable copy of an existing page

	// persisted in the meta page
	headPage uint64
	headSeq  uint64 // monotonically increasing position of the first item
	tailPage uint64
	tailSeq  uint64 // monotonically increasing position after the last item

	// in-memory state for the current commit
	maxVer uint64 // only items freed at versions <= maxVer may be reused
	curVer uint64 // version tag for items freed by the current commit
}

func seq2idx(seq uint64) int { return int(seq % FREE_LIST_CAP) }

// Count returns the number of items in the list.
func (fl *FreeList) Count() uint64 { return fl.tailSeq - fl.headSeq }

// flPop removes one item from the head. If that empties the head node, the
// node is unlinked and returned as `head` so the caller can recycle it.
func flPop(fl *FreeList) (ptr uint64, head uint64) {
	if fl.headSeq == fl.tailSeq {
		return 0, 0 // empty
	}
	node := LNode(fl.get(fl.headPage))
	ptr, ver := node.getItem(seq2idx(fl.headSeq))
	if ver > fl.maxVer {
		return 0, 0 // a snapshot may still be reading this page
	}
	fl.headSeq++
	if seq2idx(fl.headSeq) == 0 {
		head, fl.headPage = fl.headPage, node.getNext()
		assert(fl.headPage != 0, "free list lost its last node")
	}
	return ptr, head
}

// PopHead returns a reusable page, or 0 if none is available.
func (fl *FreeList) PopHead() uint64 {
	ptr, head := flPop(fl)
	if head != 0 {
		fl.PushTail(head) // the list manages its own nodes
	}
	return ptr
}

// PushTail adds a freed page.
func (fl *FreeList) PushTail(ptr uint64) {
	assert(fl.curVer > fl.maxVer, "free list versions out of order")
	LNode(fl.set(fl.tailPage)).setItem(seq2idx(fl.tailSeq), ptr, fl.curVer)
	fl.tailSeq++
	if seq2idx(fl.tailSeq) != 0 {
		return
	}
	// The tail node is full: link a new one right away so the list never
	// runs out of nodes when the head node is consumed.
	next, head := flPop(fl)
	if next == 0 {
		next = fl.new(make([]byte, BTREE_PAGE_SIZE))
	}
	LNode(fl.set(fl.tailPage)).setNext(next)
	fl.tailPage = next
	if head != 0 { // the pop emptied the head node; recycle it
		LNode(fl.set(fl.tailPage)).setItem(0, head, fl.curVer)
		fl.tailSeq++
	}
}
