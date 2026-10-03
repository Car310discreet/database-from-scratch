package db

func assert(condition bool) {
    if !condition {
        panic("assertion failed")
    }
}