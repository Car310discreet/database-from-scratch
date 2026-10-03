package main

import (
	"database_from_scratch/pkg/db"
	"fmt"
	"os"
)

func main() {
	if len(os.Args) < 2 {
		printUsage()
		os.Exit(1)
	}

	command := os.Args[1]

	kv := &db.KV{Path: "data.db"}
	if err := kv.Open(); err != nil {
		fmt.Fprintf(os.Stderr, "failed to open database: %v\n", err)
		os.Exit(1)
	}
	defer kv.Close()

	switch command {
	case "get":
		if len(os.Args) < 3 {
			fmt.Println("Usage: db-cli get <key>")
			os.Exit(1)
		}
		key := []byte(os.Args[2])
		val, ok := kv.Get(key)
		if !ok {
			fmt.Printf("Key '%s' not found.\n", os.Args[2])
		} else {
			fmt.Printf("%s\n", string(val))
		}

	case "set":
		if len(os.Args) < 4 {
			fmt.Println("Usage: db-cli set <key> <value>")
			os.Exit(1)
		}
		key := []byte(os.Args[2])
		val := []byte(os.Args[3])
		if err := kv.Set(key, val); err != nil {
			fmt.Fprintf(os.Stderr, "failed to set key: %v\n", err)
			os.Exit(1)
		}
		fmt.Println("OK")

	case "del":
		if len(os.Args) < 3 {
			fmt.Println("Usage: db-cli del <key>")
			os.Exit(1)
		}
		key := []byte(os.Args[2])
		deleted, err := kv.Del(key)
		if err != nil {
			fmt.Fprintf(os.Stderr, "failed to delete key: %v\n", err)
			os.Exit(1)
		}
		if deleted {
			fmt.Println("OK")
		} else {
			fmt.Printf("Key '%s' not found.\n", string(key))
		}

	default:
		fmt.Printf("Unknown command '%s'\n", command)
		printUsage()
		os.Exit(1)
	}
}

func printUsage() {
	fmt.Println("db-cli: A simple Key-Value store CLI")
	fmt.Println("Usage:")
	fmt.Println("  db-cli get <key>")
	fmt.Println("  db-cli set <key> <value>")
	fmt.Println("  db-cli del <key>")
}
