package byodb

import (
	"bytes"
	"errors"
	"fmt"
	"sort"
)

// ErrConflict is returned by Commit when a concurrent transaction changed
// data this transaction depended on. The transaction should be retried.
var ErrConflict = errors.New("byodb: transaction conflict, please retry")

// Update modes.
const (
	MODE_UPSERT      = 0 // insert or replace
	MODE_UPDATE_ONLY = 1 // only update existing keys
	MODE_INSERT_ONLY = 2 // only add new keys
)

// Values in a transaction's pending tree carry a 1-byte flag.
const (
	FLAG_DELETED = byte(1)
	FLAG_UPDATED = byte(2)
)

type UpdateReq struct {
	// in
	Key  []byte
	Val  []byte
	Mode int
	// out
	Added   bool   // a new key was added
	Updated bool   // a key was added or its value changed
	Old     []byte // the previous value, if any
}

type DeleteReq struct {
	Key []byte
	Old []byte // out: the deleted value
}

// KeyRange is an inclusive range [start, stop] of keys a transaction read.
type KeyRange struct {
	start, stop []byte
}

type CommittedTX struct {
	version uint64   // the version this commit produced
	writes  [][]byte // sorted keys it wrote
}

// KVTX is a transaction. Reads see a fixed snapshot plus the transaction's
// own writes; writes are buffered in memory until Commit.
type KVTX struct {
	kv       *KV
	snapshot BTree  // read-only view of the committed tree at Begin
	version  uint64 // the snapshot's version
	pending  BTree  // in-memory tree of this transaction's writes
	reads    []KeyRange
	done     bool

	// pages of the pending tree
	pages    map[uint64][]byte
	nextPage uint64
	gen      uint64 // bumped on every write, so iterators can re-seek

	// statement-level savepoint (used by the query layer)
	spActive bool
	spRoot   uint64
	spNext   uint64   // pages <= spNext may be referenced by spRoot
	deferred []uint64 // frees of such pages, applied on release
}

func (kv *KV) Begin(tx *KVTX) {
	kv.mu.Lock()
	defer kv.mu.Unlock()
	*tx = KVTX{kv: kv, version: kv.version, pages: map[uint64][]byte{}}

	tx.snapshot.root = kv.tree.root
	chunks := kv.mmap.chunks // commits only append chunks, so a copy is stable
	tx.snapshot.get = func(ptr uint64) []byte { return mmapRead(ptr, chunks) }

	tx.pending.get = func(ptr uint64) []byte {
		node, ok := tx.pages[ptr]
		assert(ok, "pending tree: dangling page")
		return node
	}
	tx.pending.new = func(node []byte) uint64 {
		tx.nextPage++
		tx.pages[tx.nextPage] = pageCopy(node)
		return tx.nextPage
	}
	tx.pending.del = func(ptr uint64) {
		if tx.spActive && ptr <= tx.spNext {
			tx.deferred = append(tx.deferred, ptr) // the savepoint may need it
		} else {
			delete(tx.pages, ptr)
		}
	}
	kv.ongoing[tx.version]++
}

// Commit applies the transaction atomically and durably, or returns an error
// (ErrConflict, or an IO error) in which case nothing was applied.
func (kv *KV) Commit(tx *KVTX) error {
	kv.mu.Lock()
	defer kv.mu.Unlock()
	if tx.done {
		return errors.New("byodb: transaction already finished")
	}
	kv.txEnd(tx)
	defer kv.trimHistory()

	if tx.pending.root == 0 {
		return nil // read-only
	}
	if detectConflicts(kv, tx) {
		return ErrConflict
	}

	meta := saveMeta(kv)
	kv.free.maxVer = kv.oldestReader()
	kv.free.curVer = kv.version + 1

	// Apply the buffered writes to the latest tree. No key we read has
	// changed since our snapshot, so the result is serializable.
	var writes [][]byte
	for it := tx.pending.SeekLE(nil); it.Valid(); it.Next() {
		key, val := it.Deref()
		if len(key) == 0 {
			continue // sentinel
		}
		writes = append(writes, bytes.Clone(key))
		if val[0] == FLAG_UPDATED {
			kv.tree.Insert(key, val[1:])
		} else {
			kv.tree.Delete(key)
		}
	}
	for _, ptr := range kv.page.pool { // unused scratch pages
		kv.free.PushTail(ptr)
	}
	kv.page.pool = kv.page.pool[:0]
	kv.version++

	if err := updateOrRevert(kv, meta); err != nil {
		return err
	}
	kv.history = append(kv.history, CommittedTX{kv.version, writes})
	return nil
}

// Abort discards the transaction.
func (kv *KV) Abort(tx *KVTX) {
	kv.mu.Lock()
	defer kv.mu.Unlock()
	if !tx.done {
		kv.txEnd(tx)
		kv.trimHistory()
	}
}

func (kv *KV) txEnd(tx *KVTX) {
	tx.done = true
	if kv.ongoing[tx.version]--; kv.ongoing[tx.version] == 0 {
		delete(kv.ongoing, tx.version)
	}
}

// oldestReader is the oldest version any open transaction can still read.
func (kv *KV) oldestReader() uint64 {
	oldest := kv.version
	for v := range kv.ongoing {
		oldest = min(oldest, v)
	}
	return oldest
}

// trimHistory drops commits that no open transaction can conflict with.
func (kv *KV) trimHistory() {
	oldest := kv.oldestReader()
	i := sort.Search(len(kv.history), func(i int) bool { return kv.history[i].version > oldest })
	if i > 0 {
		kv.history = append([]CommittedTX(nil), kv.history[i:]...)
	}
}

// detectConflicts checks whether anything this transaction read was written
// by a transaction that committed after its snapshot was taken.
func detectConflicts(kv *KV, tx *KVTX) bool {
	for i := len(kv.history) - 1; i >= 0 && kv.history[i].version > tx.version; i-- {
		if rangesOverlap(tx.reads, kv.history[i].writes) {
			return true
		}
	}
	return false
}

func rangesOverlap(reads []KeyRange, writes [][]byte) bool {
	for _, r := range reads {
		i := sort.Search(len(writes), func(i int) bool { return bytes.Compare(writes[i], r.start) >= 0 })
		if i < len(writes) && bytes.Compare(writes[i], r.stop) <= 0 {
			return true
		}
	}
	return false
}

// ---- reads and writes ----

func checkKey(key []byte) error {
	if len(key) == 0 || len(key) > BTREE_MAX_KEY_SIZE {
		return fmt.Errorf("byodb: key size must be 1..%d bytes, got %d", BTREE_MAX_KEY_SIZE, len(key))
	}
	return nil
}

func (tx *KVTX) checkOpen() error {
	if tx.done {
		return errors.New("byodb: transaction already finished")
	}
	return nil
}

// Get is a point query that sees this transaction's own writes.
func (tx *KVTX) Get(key []byte) ([]byte, bool) {
	tx.reads = append(tx.reads, KeyRange{bytes.Clone(key), bytes.Clone(key)})
	return tx.get(key)
}

func (tx *KVTX) get(key []byte) ([]byte, bool) {
	if len(key) == 0 {
		return nil, false
	}
	if val, ok := tx.pending.Get(key); ok {
		if val[0] == FLAG_DELETED {
			return nil, false
		}
		return val[1:], true
	}
	return tx.snapshot.Get(key)
}

// Update inserts or updates a key according to req.Mode. Since the outcome
// depends on the previous value, the key also counts as read.
func (tx *KVTX) Update(req *UpdateReq) (bool, error) {
	if err := tx.checkOpen(); err != nil {
		return false, err
	}
	if err := checkKey(req.Key); err != nil {
		return false, err
	}
	if len(req.Val) > BTREE_MAX_VAL_SIZE {
		return false, fmt.Errorf("byodb: value too large (%d > %d bytes)", len(req.Val), BTREE_MAX_VAL_SIZE)
	}
	old, exists := tx.Get(req.Key)
	req.Added, req.Updated, req.Old = false, false, old
	switch {
	case req.Mode == MODE_UPDATE_ONLY && !exists:
		return false, nil
	case req.Mode == MODE_INSERT_ONLY && exists:
		return false, nil
	case exists && bytes.Equal(old, req.Val):
		return false, nil
	}
	tx.pending.Insert(req.Key, append([]byte{FLAG_UPDATED}, req.Val...))
	tx.gen++
	req.Added, req.Updated = !exists, true
	return true, nil
}

// Set is Update in upsert mode.
func (tx *KVTX) Set(key, val []byte) error {
	_, err := tx.Update(&UpdateReq{Key: key, Val: val, Mode: MODE_UPSERT})
	return err
}

// Del deletes a key and reports whether it existed.
func (tx *KVTX) Del(req *DeleteReq) (bool, error) {
	if err := tx.checkOpen(); err != nil {
		return false, err
	}
	if err := checkKey(req.Key); err != nil {
		return false, err
	}
	old, exists := tx.Get(req.Key)
	req.Old = old
	if !exists {
		return false, nil
	}
	tx.pending.Insert(req.Key, []byte{FLAG_DELETED})
	tx.gen++
	return true, nil
}

// ---- statement savepoints ----

// savepoint marks the current state so a failed statement can be undone
// without aborting the whole transaction.
func (tx *KVTX) savepoint() {
	tx.spActive, tx.spRoot, tx.spNext = true, tx.pending.root, tx.nextPage
}

// release keeps the changes since the savepoint.
func (tx *KVTX) release() {
	for _, ptr := range tx.deferred {
		delete(tx.pages, ptr)
	}
	tx.deferred, tx.spActive = tx.deferred[:0], false
	tx.gen++
}

// rollbackToSavepoint undoes the changes since the savepoint. The reads are
// kept: being conservative only risks a spurious conflict.
func (tx *KVTX) rollbackToSavepoint() {
	for ptr := range tx.pages {
		if ptr > tx.spNext {
			delete(tx.pages, ptr)
		}
	}
	tx.pending.root = tx.spRoot
	tx.deferred, tx.spActive = tx.deferred[:0], false
	tx.gen++
}

// ---- range queries ----

// KVIter merges the pending writes with the snapshot, in one direction,
// within a range. It stays correct if the transaction writes during
// iteration by re-seeking the pending tree.
type KVIter struct {
	tx       *KVTX
	top, bot *BIter // pending tree, snapshot
	fwd      bool
	stop     []byte
	cmp2     int
	gen      uint64
	key, val []byte
	valid    bool
}

// Range iterates keys k with `k <cmp1> start` and `k <cmp2> stop`. It goes
// forward when cmp1 is > or >=, backward otherwise.
func (tx *KVTX) Range(start []byte, cmp1 int, stop []byte, cmp2 int) *KVIter {
	it := &KVIter{tx: tx, fwd: cmp1 > 0, stop: bytes.Clone(stop), cmp2: cmp2, gen: tx.gen}
	lo, hi := bytes.Clone(start), bytes.Clone(stop)
	if !it.fwd {
		lo, hi = hi, lo
	}
	tx.reads = append(tx.reads, KeyRange{lo, hi}) // covers phantoms too
	it.top = tx.pending.Seek(start, cmp1)
	it.bot = tx.snapshot.Seek(start, cmp1)
	it.settle()
	return it
}

func (it *KVIter) Valid() bool             { return it.valid }
func (it *KVIter) Deref() ([]byte, []byte) { return it.key, it.val }

func (it *KVIter) move(bi *BIter) {
	if it.fwd {
		bi.Next()
	} else {
		bi.Prev()
	}
}

func (it *KVIter) Next() {
	if !it.valid {
		return
	}
	if it.gen != it.tx.gen { // the pending tree changed under us
		cmp := CMP_GT
		if !it.fwd {
			cmp = CMP_LT
		}
		it.top = it.tx.pending.Seek(it.key, cmp)
		it.gen = it.tx.gen
	} else if it.top.Valid() {
		if k, _ := it.top.Deref(); bytes.Equal(k, it.key) {
			it.move(it.top)
		}
	}
	if it.bot.Valid() {
		if k, _ := it.bot.Deref(); bytes.Equal(k, it.key) {
			it.move(it.bot)
		}
	}
	it.settle()
}

// settle picks the next visible key from the two trees.
func (it *KVIter) settle() {
	for {
		var tk, tv, bk, bv []byte
		if it.top.Valid() {
			tk, tv = it.top.Deref()
		}
		if it.bot.Valid() {
			bk, bv = it.bot.Deref()
		}
		if it.top.Valid() && len(tk) == 0 { // sentinel
			it.move(it.top)
			continue
		}
		if it.bot.Valid() && len(bk) == 0 {
			it.move(it.bot)
			continue
		}
		useTop, useBot := it.top.Valid(), it.bot.Valid()
		if useTop && useBot {
			c := bytes.Compare(tk, bk)
			if !it.fwd {
				c = -c
			}
			useTop, useBot = c <= 0, c >= 0
		}
		switch {
		case !useTop && !useBot:
			it.valid = false
			return
		case useTop && tv[0] == FLAG_DELETED: // hidden by our own delete
			it.move(it.top)
			if useBot {
				it.move(it.bot)
			}
			continue
		case useTop:
			it.key, it.val = tk, tv[1:]
		default:
			it.key, it.val = bk, bv
		}
		it.valid = cmpOK(it.key, it.cmp2, it.stop)
		return
	}
}

// ---- convenience single-operation transactions ----

// Get reads a key in its own transaction.
func (kv *KV) Get(key []byte) ([]byte, bool) {
	var tx KVTX
	kv.Begin(&tx)
	defer kv.Abort(&tx)
	val, ok := tx.Get(key)
	return bytes.Clone(val), ok
}

// Set writes a key in its own transaction, retrying on conflicts.
func (kv *KV) Set(key, val []byte) error {
	return kv.retry(func(tx *KVTX) error { return tx.Set(key, val) })
}

// Del deletes a key in its own transaction, retrying on conflicts.
func (kv *KV) Del(key []byte) (deleted bool, err error) {
	err = kv.retry(func(tx *KVTX) error {
		deleted, err = tx.Del(&DeleteReq{Key: key})
		return err
	})
	return deleted, err
}

func (kv *KV) retry(fn func(tx *KVTX) error) error {
	for {
		var tx KVTX
		kv.Begin(&tx)
		if err := fn(&tx); err != nil {
			kv.Abort(&tx)
			return err
		}
		if err := kv.Commit(&tx); err != ErrConflict {
			return err
		}
	}
}
