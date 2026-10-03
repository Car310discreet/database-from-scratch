package db

import (
	"fmt"
	"golang.org/x/sys/unix"
)

// mmap chunk management for read-optimized file access

func extendMmap(db *KV, size int) error {
	if size <= db.mmap.total {
		return nil
	}
	
	alloc := max(db.mmap.total, 64<<20) // double the current address space or 64MB min
	for db.mmap.total+alloc < size {
		alloc *= 2
	}

	chunk, err := unix.Mmap(
		db.fd, 
		int64(db.mmap.total), 
		alloc, 
		unix.PROT_READ, 
		unix.MAP_SHARED,
	)
	
	if err != nil {
		return fmt.Errorf("mmap: %w", err)
	}

	db.mmap.total += alloc
	db.mmap.chunks = append(db.mmap.chunks, chunk)
	return nil
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
