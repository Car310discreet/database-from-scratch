# byodb — a database from scratch in Go

A small but complete relational database built along the architecture of
*Build Your Own Database From Scratch in Go* (2nd ed.): a copy-on-write B+tree
in a memory-mapped file, a crash-safe commit protocol, a free list, tables and
secondary indexes encoded as ordered keys, snapshot-isolated concurrent
transactions, and a small SQL-like query language with an interactive shell.

The code is an independent implementation of the book's design (about 4,000
lines plus 1,200 lines of tests), standard library only.

## Build and run

Requires Go 1.22+ on Linux or macOS (uses `mmap`, `fsync` and `flock`).

```sh
go test ./...                 # full suite (add -race, or -short to skip the crash test)
go build -o byodb ./cmd/byodb
./byodb demo.db               # interactive shell
./byodb -c "select * from t;" demo.db
./byodb demo.db < script.sql  # stops at the first error, exit status 1
```

```
byodb> create table people (id int, name string, city string,
   ...>   primary key (id), index (city));
byodb> insert into people values (1, 'ada', 'london'), (2, 'alan', 'wilmslow');
byodb> select name from people index by city = 'london';
byodb> begin;
byodb*> update people set city = 'manchester' index by id = 2;
byodb*> commit;
byodb> .verify
```

Shell meta-commands: `.tables`, `.schema TABLE`, `.verify` (full structural
check plus page statistics), `.help`, `.quit`.

## Map from the book to the code

| Chapters | Topic | File |
|---|---|---|
| 1–4 | B+tree node format, insert, split, delete, merge (copy-on-write) | `btree.go` |
| 5 | Append-only KV store, `mmap`, meta page, two-phase `fsync` commit | `kv.go` |
| 6 | Free list for page reuse | `freelist.go` |
| 7–8 | Tables on KV, range iterators, order-preserving encoding | `table.go`, `iter.go`, `encoding.go` |
| 9 | Secondary indexes | `table.go` |
| 10–11 | Atomic transactions, snapshot isolation, optimistic concurrency control | `tx.go` |
| 12–13 | SQL parser and interpreter | `ql_parse.go`, `ql_exec.go` |
| — | Structural checker (`Verify`) | `check.go` |
| — | Shell | `cmd/byodb/main.go` |

## The query language

Types are `int` (64-bit) and `string` (bytes). Every table needs a primary key.

```sql
create table t (a int, b string, c int, primary key (a), index (b, c));
drop table t;
insert into t values (1, 'x', 2), (2, 'y', 3);
insert into t (b, a, c) values ('z', 3, 4);
upsert into t values (1, 'changed', 2);        -- insert or replace
select a, b + '!' as s, c * 2 from t
    index by a >= 1 and a < 10                 -- range on an index prefix
    filter c % 2 = 0 and not (b = 'x')         -- arbitrary predicate
    limit 5, 10;                               -- offset, count
select * from t index by (b, c) > ('x', 2) and b <= 'y';   -- tuple ranges
update t set c = c + 1 index by b = 'y';       -- key columns may change too
delete from t filter c > 100;
begin; ...; commit;    -- or rollback;
show tables; describe t;
```

`index by` picks the index whose leading columns match the ones named; a
single `<`/`<=` bound scans in descending order. Without `index by`, the whole
table is scanned by primary key. Operators: `+ - * / %`, comparisons (also on
tuples), `and`, `or`, `not`, unary `-`; `+` concatenates strings.

Outside `begin`/`commit` every statement is its own transaction and is retried
automatically on conflict. Inside a transaction a failing statement is rolled
back on its own (statement-level savepoint) and the transaction stays open; a
conflict at `commit` rolls the whole transaction back and reports it.

## Design notes

**Storage.** 4 KB pages. Nodes are `type | nkeys | child pointers | offsets |
key-value pairs`; keys up to 1000 bytes and values up to 3000 bytes, so any
node splits into at most three pages. Updates never overwrite live pages:
a commit writes new pages, `fsync`s, then writes the meta page (root, page
count, free-list pointers, version, CRC32) and `fsync`s again. A crash at any
point leaves either the old or the new meta page. If a write fails, the
in-memory state is reverted and the previous meta page is rewritten before
the next commit. The file is mapped in growing chunks that are never
remapped, so pointers into old chunks stay valid for open readers.

**Free list.** An unrolled linked list of page numbers stored in the file
itself, popped from the head and pushed at the tail. Each freed page is
tagged with the version that freed it, and a page is not reused while any
open transaction could still read it. Pages allocated and freed within the
same commit are recycled immediately instead of going through the list.

**Relational layer.** Each table and index gets a 4-byte key prefix. Values
are encoded so that byte order equals value order (sign-flipped big-endian
integers; strings escaped and null-terminated), so range scans are just B+tree
range scans. Secondary index keys are extended with the primary key columns,
making them unique and letting a lookup return to the row. Schemas live in an
internal table; the prefix counter in another.

**Concurrency.** Readers never block: each transaction reads a snapshot (the
root at `begin`) and buffers writes in a private in-memory tree, merging both
when iterating. Commits are serialized. At commit, the key ranges a
transaction read (including scanned ranges, so phantoms are caught) are
checked against the writes of transactions that committed since it began;
any overlap aborts it with `ErrConflict`. Read-only transactions always
succeed.

**Beyond the book / deviations.**
- A delete can replace a parent's separator key with a longer one and
  overflow the parent; `nodeDelete` handles this by splitting the updated
  node (found by randomized testing against a reference model).
- Conflict detection tracks read *ranges*, not just point keys.
- Statement-level savepoints inside explicit transactions.
- CRC on the meta page, and `Verify()` which proves that every page is
  accounted for exactly once (tree, free list, or free item — no leaks).
- An exclusive `flock` stops two processes from opening the same file.

## Tests

- `btree_test.go` — random operations checked against a sorted-map model,
  including maximum-size keys and values; seek/iteration edge cases.
- `kv_test.go` — persistence, space reuse, large transactions, injected write
  and `fsync` failures, corruption detection, isolation, read-your-writes,
  conflicts (including phantoms), concurrent counters, savepoints, locking.
- `db_test.go` — order-preserving encoding, SQL semantics and errors,
  index scans checked against a reference, space reclaimed by `drop table`,
  concurrent bank transfers with a consistency-checking reader, parser
  precedence.
- `crash_test.go` — repeatedly `SIGKILL`s a writer process mid-commit and
  checks that the file verifies and every acknowledged commit survived.

Limitations: single process (many goroutines), no `NULL`, no joins or
aggregates, and commits are serialized. Killing the process is tested; power
loss relies on the OS honoring `fsync`.
