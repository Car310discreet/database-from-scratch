package db

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"syscall"
)

// Master page size
const PAGE_SIZE = 4096

// Signature for the database to ensure it's a valid DB file
var DB_SIG = []byte("BuildYourOwnDB01")

type KV struct {
	Path string
	fp   *os.File
	fd   int
	tree BTree
	
	free FreeList
	
	mmap struct {
		file   int      // file size, can be larger than database size
		total  int      // mmap size, can be larger than file size
		chunks [][]byte // chunks of memory-mapped data
	}

	page struct {
		flushed uint64             // database size in number of pages
		nappend int                // number of pages to append
		updates map[uint64][]byte  // staging area for appended/updated pages
	}
}

// Master page format:
// [0:16]  Signature
// [16:24] Root Page Pointer (uint64)
// [24:32] Number of Allocated Pages (uint64)
// [32:40] FreeList Head Page (uint64)
// [40:48] FreeList Head Seq (uint64)
// [48:56] FreeList Tail Page (uint64)
// [56:64] FreeList Tail Seq (uint64)
// ... padding up to PAGE_SIZE

func masterLoad(db *KV) error {
	if db.mmap.file == 0 {
		// New file
		db.page.flushed = 2 // Page 0 is reserved for master page, 1 for FreeList
		db.free.headPage = 1
		db.free.tailPage = 1
		return nil
	}

	data := db.mmap.chunks[0]
	root := data[16:24]
	used := data[24:32]
	sig := data[:16]

	if !bytes.Equal(sig, DB_SIG) {
		return errors.New("bad signature")
	}

	db.tree.root = binary.LittleEndian.Uint64(root)
	db.page.flushed = binary.LittleEndian.Uint64(used)
	
	db.free.headPage = binary.LittleEndian.Uint64(data[32:40])
	db.free.headSeq = binary.LittleEndian.Uint64(data[40:48])
	db.free.tailPage = binary.LittleEndian.Uint64(data[48:56])
	db.free.tailSeq = binary.LittleEndian.Uint64(data[56:64])
	db.free.maxSeq = db.free.tailSeq // Initialize maxSeq for memory usage
	
	return nil
}

func masterSave(db *KV) error {
	var data [PAGE_SIZE]byte
	copy(data[:16], DB_SIG)
	binary.LittleEndian.PutUint64(data[16:24], db.tree.root)
	binary.LittleEndian.PutUint64(data[24:32], db.page.flushed)
	
	binary.LittleEndian.PutUint64(data[32:40], db.free.headPage)
	binary.LittleEndian.PutUint64(data[40:48], db.free.headSeq)
	binary.LittleEndian.PutUint64(data[48:56], db.free.tailPage)
	binary.LittleEndian.PutUint64(data[56:64], db.free.tailSeq)

	_, err := db.fp.WriteAt(data[:], 0)
	return err
}

func (db *KV) Open() error {
	// Open file
	fp, err := os.OpenFile(db.Path, os.O_RDWR|os.O_CREATE, 0644)
	if err != nil {
		return fmt.Errorf("OpenFile: %w", err)
	}

	db.fp = fp
	db.fd = int(fp.Fd())

	// Read file size
	st, err := fp.Stat()
	if err != nil {
		return fmt.Errorf("Stat: %w", err)
	}
	db.mmap.file = int(st.Size())

	// Initialize mmap
	if err := extendMmap(db, db.mmap.file); err != nil {
		return err
	}

	// Initialize state
	db.page.updates = make(map[uint64][]byte)
	if err := masterLoad(db); err != nil {
		return fmt.Errorf("masterLoad: %w", err)
	}

	if db.mmap.file == 0 {
		db.page.updates[1] = make([]byte, PAGE_SIZE) // empty FreeList node
	}

	pageRead := func(ptr uint64) []byte {
		// From updates (pending flushed)
		if node, ok := db.page.updates[ptr]; ok {
			return node
		}
		// From mmap
		offset := ptr * PAGE_SIZE
		for _, chunk := range db.mmap.chunks {
			if int(offset)+PAGE_SIZE <= len(chunk) {
				return chunk[offset : offset+PAGE_SIZE]
			}
			offset -= uint64(len(chunk))
		}
		panic("bad ptr")
	}

	pageAppend := func(node []byte) uint64 {
		assert(len(node) <= PAGE_SIZE)
		ptr := db.page.flushed + uint64(db.page.nappend)
		db.page.nappend++
		db.page.updates[ptr] = node
		return ptr
	}

	pageWrite := func(ptr uint64) []byte {
		if node, ok := db.page.updates[ptr]; ok {
			return node
		}
		node := make([]byte, PAGE_SIZE)
		copy(node, pageRead(ptr))
		db.page.updates[ptr] = node
		return node
	}

	// Setup Callbacks
	db.tree.get = pageRead
	db.free.get = pageRead
	
	db.free.new = pageAppend
	db.free.set = pageWrite

	db.tree.new = func(node []byte) uint64 {
		assert(len(node) <= PAGE_SIZE)
		if ptr := db.free.PopHead(); ptr != 0 {
			db.page.updates[ptr] = node
			return ptr
		}
		return pageAppend(node)
	}

	db.tree.del = db.free.PushTail

	return nil
}

func (db *KV) Close() error {
	for _, chunk := range db.mmap.chunks {
		syscall.Munmap(chunk)
	}
	return db.fp.Close()
}

func (db *KV) Get(key []byte) ([]byte, bool) {
	return db.tree.Get(key)
}

func (db *KV) Set(key []byte, val []byte) error {
	db.tree.Insert(key, val)
	return updateFile(db)
}

func (db *KV) Del(key []byte) (bool, error) {
	deleted := db.tree.Delete(key)
	return deleted, updateFile(db)
}

func updateFile(db *KV) error {
	// Append updated pages
	for ptr, node := range db.page.updates {
		offset := int64(ptr * PAGE_SIZE)
		if _, err := db.fp.WriteAt(node, offset); err != nil {
			return err
		}
	}
	
	// Fsync the appends (ensure data is safely on disk before updating master page)
	if err := db.fp.Sync(); err != nil {
		return err
	}

	db.page.flushed += uint64(db.page.nappend)
	db.page.nappend = 0
	db.page.updates = make(map[uint64][]byte)
	
	db.free.SetMaxSeq()

	// Update Master Page
	if err := masterSave(db); err != nil {
		return err
	}

	// Fsync master page
	if err := db.fp.Sync(); err != nil {
		return err
	}

	// Extend mmap if necessary considering the appended pages
	if err := extendMmap(db, int(db.page.flushed*PAGE_SIZE)); err != nil {
		return err
	}

	return nil
}
