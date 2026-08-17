package main

import "golang.org/x/sys/unix"

const starlarkCompilerMemoryLimit = 128 << 20

func limitStarlarkCompilerMemory() error {
	limit := &unix.Rlimit{Cur: starlarkCompilerMemoryLimit, Max: starlarkCompilerMemoryLimit}
	return unix.Setrlimit(unix.RLIMIT_AS, limit)
}
