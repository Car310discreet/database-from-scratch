# Database from Scratch in Go

A functional, disk-backed Key-Value store built from scratch in Go, based on the principles of B+ Trees and memory mapping. 
This project uses an append-only (copy-on-write) B+ tree architecture to ensure data integrity and system reliability, similar to real-world database systems.

## Architecture and Structure

The codebase is organized in a modular structure to keep the Key-Value core separate from the application CLI logic:

- **`pkg/db/`**: Contains the B-Tree logic, data node models, and the core Disk mapping logic (including `mmap` chunks for copy-on-write functionality).
- **`cmd/db-cli/`**: Contains the main entrypoint and standard CLI command execution logic to interface with the database engine.

The disk format places a special **Master Page** containing a database signature, current root node pointer, total allocated pages, and pointers to the FreeList. 

## Storage & Free List
When updates happen, nodes are safely appended to the file. Once `sync`ed to disk, the master page is updated to point to the new B-Tree root, ensuring atomic updates and crash recovery. Any nodes that become deleted/abandoned through B-Tree merges or replacements are deposited to the **Free List**, an internal linked list of pages. This allows the system to reuse space instead of infinitely expanding the file.

## Getting Started

### Prerequisites

Ensure you have a modern `go` runtime installed.
The project relies on standard `syscall` mechanics for `mmap`.

```bash
go mod tidy
go build -o db-cli ./cmd/db-cli
```

### Running Commands

Use the `db-cli` executable to interact with the database store (`data.db`).

**Set a key-value pair**
```bash
./db-cli set mykey "This is a value"
```

**Get a value by key**
```bash
./db-cli get mykey
# Output: This is a value
```

**Delete a key**
```bash
./db-cli del mykey
# Output: OK
```

### Testing

You can use the standard Go toolchain to run tests covering the underlying `BTree` operations:
```bash
go test -v ./pkg/db/...
```
