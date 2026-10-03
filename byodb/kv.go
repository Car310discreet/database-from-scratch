package byodb

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"os"
	"path/filepath"
	"sync"
	"syscall"
)

// The database is one file of pages. Page 0 is the meta page; every other
// page is a B+tree node or a free list node.
//
// Meta page layout:
//
//	| sig | root | npages | head_page | head_seq | tail_page | tail_seq | version | crc32 |
//	| 16B |  8B  |   8B   |    8B     |    8B    |    8B     |    8B    |   8B    |  4B   |
const (
	DB_SIG    = "BuildYourOwnDB14"
	META_SIZE = 16 + 8*7 + 4
)

const mmapInitial = 64 << 20

// KV is a transactional, crash-safe key-value store built on a copy-on-write
// B+tree. Use Begin/Commit/Abort (tx.go) to read and write.
type KV struct {
	Path string

	fd   int
	tree BTree
	free FreeList
	mmap struct {
		total  int      // mapped bytes, may exceed the file size
		chunks [][]byte // multiple mappings, never remapped, so readers stay valid
	}
	page struct {
		flushed uint64            // database size in pages
		nappend uint64            // pages to be appended by this commit
		updates map[uint64][]byte // pending page writes, incl. appended pages
		fresh   map[uint64]bool   // pages allocated by this commit
		pool    []uint64          // freed fresh pages, reusable immediately
	}
	failed  bool   // the last commit failed; the on-disk meta page is unknown
	version uint64 // incremented by each commit; persisted

	mu      sync.Mutex               // serializes Begin/Commit/Abort
	ongoing map[uint64]int           // versions of open transactions (multiset)
	history []CommittedTX            // recent commits, for conflict detection
	faults  func(stage string) error // test hook for IO failure injection
}

// Open opens or creates the database file.
func (db *KV) Open() error {
	db.page.updates = map[uint64][]byte{}
	db.page.fresh = map[uint64]bool{}
	db.ongoing = map[uint64]int{}
	db.tree.get = db.pageRead
	db.tree.new = db.pageAlloc
	db.tree.del = db.pageFree
	db.free.get = db.pageRead
	db.free.new = db.pageAppend
	db.free.set = db.pageWrite

	fd, err := createFileSync(db.Path)
	if err != nil {
		return err
	}
	db.fd = fd
	// one process at a time: the free list and meta page are not shared
	if err = syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		db.Close()
		return fmt.Errorf("%s is in use by another process: %w", db.Path, err)
	}
	var st syscall.Stat_t
	if err = syscall.Fstat(fd, &st); err != nil {
		db.Close()
		return fmt.Errorf("stat: %w", err)
	}
	if err = extendMmap(db, int(st.Size)); err != nil {
		db.Close()
		return err
	}
	if st.Size == 0 {
		err = initEmpty(db)
	} else {
		err = readMeta(db, st.Size)
	}
	if err != nil {
		db.Close()
		return err
	}
	return nil
}

// Close releases the file. Transactions must be finished first.
func (db *KV) Close() {
	for _, chunk := range db.mmap.chunks {
		_ = syscall.Munmap(chunk)
	}
	db.mmap.chunks, db.mmap.total = nil, 0
	if db.fd > 0 {
		_ = syscall.Close(db.fd)
		db.fd = -1
	}
}

// createFileSync opens or creates a file and fsyncs its directory so that a
// newly created file survives a crash.
func createFileSync(file string) (int, error) {
	dirfd, err := syscall.Open(filepath.Dir(file), os.O_RDONLY|syscall.O_DIRECTORY, 0o644)
	if err != nil {
		return -1, fmt.Errorf("open directory: %w", err)
	}
	defer syscall.Close(dirfd)
	fd, err := syscall.Openat(dirfd, filepath.Base(file), os.O_RDWR|os.O_CREATE|syscall.O_CLOEXEC, 0o644)
	if err != nil {
		return -1, fmt.Errorf("open file: %w", err)
	}
	if err = syscall.Fsync(dirfd); err != nil {
		_ = syscall.Close(fd)
		return -1, fmt.Errorf("fsync directory: %w", err)
	}
	return fd, nil
}

// extendMmap maps more of the file. Existing mappings are never moved, so
// slices handed out to readers remain valid; each new chunk doubles the total.
func extendMmap(db *KV, size int) error {
	if size <= db.mmap.total {
		return nil
	}
	alloc := max(db.mmap.total, mmapInitial)
	for db.mmap.total+alloc < size {
		alloc *= 2
	}
	chunk, err := syscall.Mmap(db.fd, int64(db.mmap.total), alloc, syscall.PROT_READ, syscall.MAP_SHARED)
	if err != nil {
		return fmt.Errorf("mmap: %w", err)
	}
	db.mmap.total += alloc
	db.mmap.chunks = append(db.mmap.chunks, chunk)
	return nil
}

func mmapRead(ptr uint64, chunks [][]byte) []byte {
	start := ptr * BTREE_PAGE_SIZE
	for _, chunk := range chunks {
		end := start + BTREE_PAGE_SIZE
		if end <= uint64(len(chunk)) {
			return chunk[start:end:end]
		}
		start -= uint64(len(chunk))
	}
	panic(fmt.Sprintf("byodb: page %d is outside the mapped file", ptr))
}

// ---- page management callbacks ----

// pageRead serves BTree.get and FreeList.get for the writer.
func (db *KV) pageRead(ptr uint64) []byte {
	if node, ok := db.page.updates[ptr]; ok {
		return node
	}
	return mmapRead(ptr, db.mmap.chunks)
}

// pageAppend serves FreeList.new: grow the file by one page.
func (db *KV) pageAppend(node []byte) uint64 {
	ptr := db.page.flushed + db.page.nappend
	db.page.nappend++
	db.page.updates[ptr] = pageCopy(node)
	return ptr
}

// pageAlloc serves BTree.new: reuse a page if possible, else append.
func (db *KV) pageAlloc(node []byte) uint64 {
	var ptr uint64
	if n := len(db.page.pool); n > 0 {
		ptr, db.page.pool = db.page.pool[n-1], db.page.pool[:n-1]
	} else if ptr = db.free.PopHead(); ptr == 0 {
		ptr = db.page.flushed + db.page.nappend
		db.page.nappend++
	}
	db.page.updates[ptr] = pageCopy(node)
	db.page.fresh[ptr] = true
	return ptr
}

// pageFree serves BTree.del. A page allocated by this same commit was never
// visible to anyone, so it can be reused at once; other pages go to the free
// list tagged with the new version.
func (db *KV) pageFree(ptr uint64) {
	if db.page.fresh[ptr] {
		db.page.pool = append(db.page.pool, ptr)
		return
	}
	db.free.PushTail(ptr)
}

// pageWrite serves FreeList.set: a writable copy of an existing page.
func (db *KV) pageWrite(ptr uint64) []byte {
	if node, ok := db.page.updates[ptr]; ok {
		return node
	}
	node := pageCopy(mmapRead(ptr, db.mmap.chunks))
	db.page.updates[ptr] = node
	return node
}

func pageCopy(node []byte) []byte {
	page := make([]byte, BTREE_PAGE_SIZE)
	copy(page, node)
	return page
}

// ---- the meta page ----

func saveMeta(db *KV) []byte {
	data := make([]byte, META_SIZE)
	copy(data[:16], DB_SIG)
	fields := []uint64{
		db.tree.root, db.page.flushed,
		db.free.headPage, db.free.headSeq, db.free.tailPage, db.free.tailSeq,
		db.version,
	}
	for i, v := range fields {
		binary.LittleEndian.PutUint64(data[16+8*i:], v)
	}
	binary.LittleEndian.PutUint32(data[META_SIZE-4:], crc32.ChecksumIEEE(data[:META_SIZE-4]))
	return data
}

func loadMeta(db *KV, data []byte) {
	f := func(i int) uint64 { return binary.LittleEndian.Uint64(data[16+8*i:]) }
	db.tree.root, db.page.flushed = f(0), f(1)
	db.free.headPage, db.free.headSeq = f(2), f(3)
	db.free.tailPage, db.free.tailSeq = f(4), f(5)
	db.version = f(6)
}

var ErrCorrupted = errors.New("byodb: database file is corrupted or not a database")

func readMeta(db *KV, fileSize int64) error {
	if fileSize < BTREE_PAGE_SIZE {
		return ErrCorrupted
	}
	data := mmapRead(0, db.mmap.chunks)[:META_SIZE]
	if !bytes.Equal(data[:16], []byte(DB_SIG)) {
		return fmt.Errorf("%w: bad signature", ErrCorrupted)
	}
	if binary.LittleEndian.Uint32(data[META_SIZE-4:]) != crc32.ChecksumIEEE(data[:META_SIZE-4]) {
		return fmt.Errorf("%w: meta page checksum mismatch", ErrCorrupted)
	}
	loadMeta(db, data)
	// The file may be longer than npages (an interrupted commit appended
	// pages), but never shorter.
	npages := db.page.flushed
	ok := npages >= 2 && npages*BTREE_PAGE_SIZE <= uint64(fileSize) &&
		db.tree.root < npages &&
		0 < db.free.headPage && db.free.headPage < npages &&
		0 < db.free.tailPage && db.free.tailPage < npages &&
		db.free.headSeq <= db.free.tailSeq
	if !ok {
		return fmt.Errorf("%w: meta page fields out of range", ErrCorrupted)
	}
	return nil
}

// initEmpty writes an empty database: the meta page and one free list node.
func initEmpty(db *KV) error {
	db.page.flushed = 1
	db.free.headPage = db.pageAppend(make([]byte, BTREE_PAGE_SIZE))
	db.free.tailPage = db.free.headPage
	return updateFile(db)
}

// ---- committing to disk ----

func (db *KV) fault(stage string) error {
	if db.faults != nil {
		return db.faults(stage)
	}
	return nil
}

func writePages(db *KV) error {
	if err := extendMmap(db, int(db.page.flushed+db.page.nappend)*BTREE_PAGE_SIZE); err != nil {
		return err
	}
	for ptr, page := range db.page.updates {
		if err := db.fault("write"); err != nil {
			return err
		}
		n, err := syscall.Pwrite(db.fd, page, int64(ptr*BTREE_PAGE_SIZE))
		if err == nil && n != len(page) {
			err = errors.New("short write")
		}
		if err != nil {
			return fmt.Errorf("write page: %w", err)
		}
	}
	return nil
}

func writeMetaSync(db *KV, meta []byte) error {
	if err := db.fault("meta"); err != nil {
		return err
	}
	// A small, sector-aligned write is assumed to be power-loss atomic.
	if _, err := syscall.Pwrite(db.fd, meta, 0); err != nil {
		return fmt.Errorf("write meta page: %w", err)
	}
	if err := syscall.Fsync(db.fd); err != nil {
		return fmt.Errorf("fsync: %w", err)
	}
	return nil
}

// updateFile is the two-phase commit:
//  1. write the new pages and fsync (the old meta page still points to the
//     old tree, so a crash here is harmless);
//  2. atomically switch the meta page to the new tree and fsync.
func updateFile(db *KV) error {
	if err := writePages(db); err != nil {
		return err
	}
	if err := db.fault("fsync"); err != nil {
		return err
	}
	if err := syscall.Fsync(db.fd); err != nil {
		return fmt.Errorf("fsync: %w", err)
	}
	db.page.flushed += db.page.nappend
	resetPending(db)
	return writeMetaSync(db, saveMeta(db))
}

func resetPending(db *KV) {
	db.page.nappend = 0
	db.page.updates = map[uint64][]byte{}
	db.page.fresh = map[uint64]bool{}
	db.page.pool = db.page.pool[:0]
}

// updateOrRevert commits to disk, or restores the in-memory state captured
// in `meta` so the DB stays usable (at least for reads) after an IO error.
func updateOrRevert(db *KV, meta []byte) error {
	var err error
	if db.failed {
		// After a failed commit, the on-disk meta page could be either
		// version. Rewrite the last good one before touching any page it
		// might not protect.
		if err = writeMetaSync(db, meta); err == nil {
			db.failed = false
		}
	}
	if err == nil {
		if err = updateFile(db); err != nil {
			db.failed = true
		}
	}
	if err != nil {
		loadMeta(db, meta)
		resetPending(db)
	}
	return err
}
