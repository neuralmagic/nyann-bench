package controlapi

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/neuralmagic/nyann-bench/pkg/config"
)

const (
	starlarkCompileTimeout = 3 * time.Second
	starlarkCompilerOutput = 1 << 20
	starlarkCompilerError  = 8 << 10
)

type cappedBuffer struct {
	bytes.Buffer
	limit    int
	overflow bool
}

func (b *cappedBuffer) Write(value []byte) (int, error) {
	written := len(value)
	remaining := b.limit - b.Len()
	if remaining > 0 {
		if remaining < len(value) {
			_, _ = b.Buffer.Write(value[:remaining])
		} else {
			_, _ = b.Buffer.Write(value)
		}
	}
	if len(value) > remaining {
		b.overflow = true
	}
	return written, nil
}

func (s *Server) compileStarlark(ctx context.Context, source string) (*config.ScenarioConfig, error) {
	select {
	case s.starlarkCompileSlots <- struct{}{}:
		defer func() { <-s.starlarkCompileSlots }()
	case <-ctx.Done():
		return nil, fmt.Errorf("waiting for Starlark compiler capacity: %w", ctx.Err())
	}
	if s.options.starlarkCompiler != nil {
		return s.options.starlarkCompiler(ctx, source)
	}
	compileCtx, cancel := context.WithTimeout(ctx, starlarkCompileTimeout)
	defer cancel()

	command := exec.CommandContext(compileCtx, s.options.StarlarkCompilerPath, "compile-starlark")
	command.Stdin = strings.NewReader(source)
	command.Env = append(filteredEnvironment("GOMEMLIMIT", "GOGC"), "GOMEMLIMIT=64MiB", "GOGC=20")
	stdout := &cappedBuffer{limit: starlarkCompilerOutput}
	stderr := &cappedBuffer{limit: starlarkCompilerError}
	command.Stdout = stdout
	command.Stderr = stderr
	if err := command.Run(); err != nil {
		if compileCtx.Err() != nil {
			return nil, fmt.Errorf("Starlark compilation exceeded %s", starlarkCompileTimeout)
		}
		message := strings.TrimSpace(stderr.String())
		if message == "" {
			message = err.Error()
		}
		return nil, fmt.Errorf("isolated Starlark compilation failed: %s", message)
	}
	if stdout.overflow {
		return nil, fmt.Errorf("compiled Starlark scenario exceeds %d bytes", starlarkCompilerOutput)
	}
	var scenario config.ScenarioConfig
	if err := decodeStrict(stdout.Bytes(), &scenario); err != nil {
		return nil, fmt.Errorf("decoding isolated Starlark result: %w", err)
	}
	return &scenario, nil
}

func filteredEnvironment(names ...string) []string {
	blocked := make(map[string]struct{}, len(names))
	for _, name := range names {
		blocked[name] = struct{}{}
	}
	result := make([]string, 0, len(os.Environ()))
	for _, value := range os.Environ() {
		name, _, _ := strings.Cut(value, "=")
		if _, found := blocked[name]; !found {
			result = append(result, value)
		}
	}
	return result
}
