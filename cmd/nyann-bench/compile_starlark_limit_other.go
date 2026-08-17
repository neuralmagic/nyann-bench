//go:build !linux

package main

func limitStarlarkCompilerMemory() error { return nil }
