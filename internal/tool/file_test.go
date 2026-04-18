package tool

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestReadFile_PathEscape(t *testing.T) {
	dir := t.TempDir()
	rt := NewReadFileTool(dir)

	badPaths := []string{
		"../etc/passwd",
		"../../etc/passwd",
		"foo/../../etc/passwd",
	}

	for _, p := range badPaths {
		args := `{"path":"` + p + `"}`
		_, err := rt.Execute(context.Background(), args)
		if err == nil {
			t.Errorf("expected error for path %q, got nil", p)
		}
	}
}

func TestWriteFile_PathEscape(t *testing.T) {
	dir := t.TempDir()
	wt := NewWriteFileTool(dir)

	// Even with .users/ prefix, traversal out of workspace should fail.
	_, err := wt.Execute(context.Background(), `{"path":"../../escape.txt","content":"bad"}`)
	if err == nil {
		t.Error("expected error for path escape")
	}
}

func TestReadWriteFile(t *testing.T) {
	dir := t.TempDir()
	wt := NewWriteFileTool(dir)
	rt := NewReadFileTool(dir)
	ctx := context.Background()

	// Write a file — will be placed under .users/.
	result, err := wt.Execute(ctx, `{"path":"test.txt","content":"hello world"}`)
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	if result == "" {
		t.Fatal("expected non-empty result")
	}

	// Read it back via the .users/ path.
	result, err = rt.Execute(ctx, `{"path":".users/test.txt"}`)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if result != "hello world" {
		t.Fatalf("expected 'hello world', got %q", result)
	}

	// Verify on disk — file should be under .users/.
	data, _ := os.ReadFile(filepath.Join(dir, ".users", "test.txt"))
	if string(data) != "hello world" {
		t.Fatalf("file content mismatch: %q", data)
	}
}

func TestWriteFile_CreatesSubdirs(t *testing.T) {
	dir := t.TempDir()
	wt := NewWriteFileTool(dir)

	_, err := wt.Execute(context.Background(), `{"path":"sub/dir/file.txt","content":"nested"}`)
	if err != nil {
		t.Fatalf("write nested: %v", err)
	}

	// Should be under .users/sub/dir/file.txt.
	data, _ := os.ReadFile(filepath.Join(dir, ".users", "sub", "dir", "file.txt"))
	if string(data) != "nested" {
		t.Fatalf("expected 'nested', got %q", data)
	}
}

func TestWriteFile_AlreadyPrefixed(t *testing.T) {
	dir := t.TempDir()
	wt := NewWriteFileTool(dir)

	// If model already includes .users/ prefix, don't double it.
	_, err := wt.Execute(context.Background(), `{"path":".users/report.txt","content":"ok"}`)
	if err != nil {
		t.Fatalf("write: %v", err)
	}

	data, _ := os.ReadFile(filepath.Join(dir, ".users", "report.txt"))
	if string(data) != "ok" {
		t.Fatalf("expected 'ok', got %q", data)
	}
}
