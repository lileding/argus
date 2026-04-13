package tool

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestFileTool_PathEscape(t *testing.T) {
	dir := t.TempDir()
	ft := NewFileTool(dir)

	// These should all fail.
	badPaths := []string{
		"../etc/passwd",
		"../../etc/passwd",
		"/etc/passwd",
		"foo/../../etc/passwd",
	}

	for _, p := range badPaths {
		args := `{"action":"read","path":"` + p + `"}`
		_, err := ft.Execute(context.Background(), args)
		if err == nil {
			t.Errorf("expected error for path %q, got nil", p)
		}
	}
}

func TestFileTool_ReadWrite(t *testing.T) {
	dir := t.TempDir()
	ft := NewFileTool(dir)
	ctx := context.Background()

	// Write a file.
	writeArgs := `{"action":"write","path":"test.txt","content":"hello world"}`
	result, err := ft.Execute(ctx, writeArgs)
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	if result == "" {
		t.Fatal("expected non-empty result")
	}

	// Read it back.
	readArgs := `{"action":"read","path":"test.txt"}`
	result, err = ft.Execute(ctx, readArgs)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if result != "hello world" {
		t.Fatalf("expected 'hello world', got %q", result)
	}

	// Verify the file is actually in the workspace.
	data, _ := os.ReadFile(filepath.Join(dir, "test.txt"))
	if string(data) != "hello world" {
		t.Fatalf("file content mismatch: %q", data)
	}
}
