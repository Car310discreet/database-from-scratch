package kv_implementation

func assert(condition bool) {
    if !condition {
        panic("assertion failed")
    }
}