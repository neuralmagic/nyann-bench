package main

import "golang.org/x/sys/unix"

const starlarkCompilerMemoryLimit = 128 << 20

func limitStarlarkCompilerMemory() error {
	limit := &unix.Rlimit{Cur: starlarkCompilerMemoryLimit, Max: starlarkCompilerMemoryLimit}
	// RLIMIT_AS counts the virtual address arena reserved by the Go runtime and
	// can kill an otherwise idle Go 1.25 process below this ceiling. Linux 4.7+
	// applies RLIMIT_DATA to anonymous mmap allocations as well as brk, which
	// keeps the compiler heap bounded without charging unused address space.
	return unix.Setrlimit(unix.RLIMIT_DATA, limit)
}
