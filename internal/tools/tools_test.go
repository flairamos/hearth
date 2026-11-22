package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func testRegistry(t *testing.T) *Registry {
	t.Helper()
	dir := t.TempDir()
	return &Registry{WorkDir: dir, BashTimeoutSec: 10}
}

func TestReadWriteEdit(t *testing.T) {
	r := testRegistry(t)
	ctx := context.Background()

	out, err := writeFileTool().Fn(ctx, r, map[string]any{"path": "src/a.txt", "content": "line1\nline2\nline3\n"})
	if err != nil || !strings.Contains(out, "wrote") {
		t.Fatalf("write_file: %v %v", out, err)
	}

	out, err = readFileTool().Fn(ctx, r, map[string]any{"path": "src/a.txt"})
	if err != nil || !strings.Contains(out, "1\tline1") || !strings.Contains(out, "3\tline3") {
		t.Fatalf("read_file: %v %v", out, err)
	}

	out, err = readFileTool().Fn(ctx, r, map[string]any{"path": "src/a.txt", "offset": float64(2), "limit": float64(1)})
	if err != nil || !strings.Contains(out, "2\tline2") || strings.Contains(out, "line1") {
		t.Fatalf("read_file offset: %v %v", out, err)
	}

	// edit: non-unique → error
	_, err = editFileTool().Fn(ctx, r, map[string]any{"path": "src/a.txt", "old_string": "line", "new_string": "x"})
	if err == nil || !strings.Contains(err.Error(), "unique") {
		t.Fatalf("expected uniqueness error, got %v", err)
	}

	_, err = editFileTool().Fn(ctx, r, map[string]any{"path": "src/a.txt", "old_string": "line2", "new_string": "LINE2"})
	if err != nil {
		t.Fatalf("edit_file: %v", err)
	}
	data, _ := os.ReadFile(filepath.Join(r.WorkDir, "src", "a.txt"))
	if !strings.Contains(string(data), "LINE2") || strings.Contains(string(data), "line2") {
		t.Fatalf("edit not applied: %q", data)
	}

	// edit: not found → helpful error
	_, err = editFileTool().Fn(ctx, r, map[string]any{"path": "src/a.txt", "old_string": "nope-not-here", "new_string": "x"})
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("expected not-found error, got %v", err)
	}
}

func TestGlobGrepList(t *testing.T) {
	r := testRegistry(t)
	ctx := context.Background()
	mk := func(p, c string) {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(r.WorkDir, p)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(r.WorkDir, p), []byte(c), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	mk("src/a/main.go", "package main\nfunc main() {}\n")
	mk("src/b/util.go", "package util\nfunc helper() {}\n")
	mk("README.md", "# hello world\n")

	out, err := globTool().Fn(ctx, r, map[string]any{"pattern": "src/**/*.go"})
	if err != nil || !strings.Contains(out, "src/a/main.go") || !strings.Contains(out, "src/b/util.go") {
		t.Fatalf("glob: %v %v", out, err)
	}

	out, err = grepTool().Fn(ctx, r, map[string]any{"pattern": "func ", "glob": "*.go"})
	if err != nil || !strings.Contains(out, "main.go:2:") || !strings.Contains(out, "util.go:2:") {
		t.Fatalf("grep: %v %v", out, err)
	}

	out, err = grepTool().Fn(ctx, r, map[string]any{"pattern": "HELLO", "ignore_case": true})
	if err != nil || !strings.Contains(out, "README.md:1:") {
		t.Fatalf("grep ignore_case: %v %v", out, err)
	}

	out, err = listDirTool().Fn(ctx, r, map[string]any{})
	if err != nil || !strings.Contains(out, "src/") || !strings.Contains(out, "README.md") {
		t.Fatalf("list_dir: %v %v", out, err)
	}
}

func TestBash(t *testing.T) {
	if _, err := os.Stat("/bin/bash"); err != nil {
		t.Skip("no bash")
	}
	r := testRegistry(t)
	out, err := bashTool().Fn(context.Background(), r, map[string]any{"command": "echo hello; ls ."})
	if err != nil || !strings.Contains(out, "exit=0") || !strings.Contains(out, "hello") {
		t.Fatalf("bash: %v %v", out, err)
	}
	out, err = bashTool().Fn(context.Background(), r, map[string]any{"command": "exit 3"})
	if err != nil || !strings.Contains(out, "exit=3") {
		t.Fatalf("bash exit code: %v %v", out, err)
	}
}

func TestBinaryDetection(t *testing.T) {
	r := testRegistry(t)
	p := filepath.Join(r.WorkDir, "bin.dat")
	_ = os.WriteFile(p, []byte{'a', 0x00, 'b'}, 0o644)
	_, err := readFileTool().Fn(context.Background(), r, map[string]any{"path": "bin.dat"})
	if err == nil || !strings.Contains(err.Error(), "binary") {
		t.Fatalf("expected binary error, got %v", err)
	}
}
