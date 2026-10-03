package db

import "encoding/binary"

const FREE_LIST_CAP = (PAGE_SIZE / 8) - 1

type LNode []byte

func (node LNode) getNext() uint64 {
	return binary.LittleEndian.Uint64(node[PAGE_SIZE-8:])
}

func (node LNode) setNext(next uint64) {
	binary.LittleEndian.PutUint64(node[PAGE_SIZE-8:], next)
}

func (node LNode) getPtr(idx int) uint64 {
	return binary.LittleEndian.Uint64(node[idx*8:])
}

func (node LNode) setPtr(idx int, ptr uint64) {
	binary.LittleEndian.PutUint64(node[idx*8:], ptr)
}

// FreeList manages recycled database pages
type FreeList struct {
	get func(uint64) []byte // read a page
	new func([]byte) uint64 // append a new page
	set func(uint64) []byte // update an existing page
	
	headPage uint64 // pointer to the list head node
	headSeq  uint64 // monotonic sequence number to index into the list head
	tailPage uint64
	tailSeq  uint64
	
	maxSeq   uint64 // saved `tailSeq` to prevent consuming newly added items in same TX
}

func seq2idx(seq uint64) int {
	return int(seq % FREE_LIST_CAP)
}

// SetMaxSeq is called upon commit to make items pending flush available for reuse.
func (fl *FreeList) SetMaxSeq() {
	fl.maxSeq = fl.tailSeq
}

func flPop(fl *FreeList) (ptr uint64, head uint64) {
	if fl.headSeq == fl.maxSeq {
		return 0, 0 // cannot advance
	}
	
	node := LNode(fl.get(fl.headPage))
	ptr = node.getPtr(seq2idx(fl.headSeq))
	fl.headSeq++
	
	// move to the next one if the head node is empty
	if seq2idx(fl.headSeq) == 0 {
		head = fl.headPage
		fl.headPage = node.getNext()
		assert(fl.headPage != 0)
	}
	return ptr, head
}

// PopHead gets 1 item from the list head. Returns 0 on failure/empty.
func (fl *FreeList) PopHead() uint64 {
	ptr, head := flPop(fl)
	if head != 0 {
		// the empty head node is recycled and fed back to the list itself
		fl.PushTail(head)
	}
	return ptr
}

// PushTail adds 1 item to the tail
func (fl *FreeList) PushTail(ptr uint64) {
	// add it to the tail node
	LNode(fl.set(fl.tailPage)).setPtr(seq2idx(fl.tailSeq), ptr)
	fl.tailSeq++
	
	// add a new tail node if it's full (the list is never empty)
	if seq2idx(fl.tailSeq) == 0 {
		// try to reuse from the list head
		next, head := flPop(fl) // may remove the head node
		
		if next == 0 {
			// or allocate a new node by appending
			next = fl.new(make([]byte, PAGE_SIZE))
		}
		
		// recycle if head popped
		if head != 0 {
			fl.PushTail(head)
		}
		
		// link to the new tail node
		LNode(fl.set(fl.tailPage)).setNext(next)
		fl.tailPage = next
	}
}
