package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestExtractShareGPT(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sharegpt.json")
	err := os.WriteFile(path, []byte(`[
		{"conversations": [
			{"from": "human", "value": "What is Go?"},
			{"from": "gpt", "value": "Go is a programming language."},
			{"from": "human", "value": "Tell me more."},
			{"from": "gpt", "value": "It was created at Google."}
		]},
		{"conversations": [
			{"from": "human", "value": "Hello"},
			{"from": "gpt", "value": "Hi there"}
		]}
	]`), 0644)
	if err != nil {
		t.Fatal(err)
	}

	text, err := extractShareGPT(path)
	if err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(text, "What is Go?") {
		t.Error("missing human turn text")
	}
	if !strings.Contains(text, "Go is a programming language.") {
		t.Error("missing gpt turn text")
	}
	if !strings.Contains(text, "Hi there") {
		t.Error("missing second conversation")
	}
}

func TestExtractShareGPTEmpty(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "empty.json")
	if err := os.WriteFile(path, []byte(`[]`), 0644); err != nil {
		t.Fatal(err)
	}

	_, err := extractShareGPT(path)
	if err == nil {
		t.Error("expected error for empty ShareGPT file")
	}
}

func TestExtractDir(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.py"), []byte("def hello(): pass"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "b.go"), []byte("func main() {}"), 0644); err != nil {
		t.Fatal(err)
	}
	// Should be skipped
	if err := os.WriteFile(filepath.Join(dir, "img.png"), []byte("binary"), 0644); err != nil {
		t.Fatal(err)
	}

	text, err := extractDir(dir)
	if err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(text, "def hello()") {
		t.Error("missing Python content")
	}
	if !strings.Contains(text, "func main()") {
		t.Error("missing Go content")
	}
	if strings.Contains(text, "binary") {
		t.Error("should not include .png content")
	}
}

func TestDetectFormat(t *testing.T) {
	dir := t.TempDir()

	jsonPath := filepath.Join(dir, "data.json")
	os.WriteFile(jsonPath, []byte("[]"), 0644)
	if f := detectFormat(jsonPath); f != "sharegpt" {
		t.Errorf("expected sharegpt for .json, got %s", f)
	}

	txtPath := filepath.Join(dir, "data.txt")
	os.WriteFile(txtPath, []byte("hello"), 0644)
	if f := detectFormat(txtPath); f != "text" {
		t.Errorf("expected text for .txt, got %s", f)
	}

	if f := detectFormat(dir); f != "dir" {
		t.Errorf("expected dir for directory, got %s", f)
	}
}
