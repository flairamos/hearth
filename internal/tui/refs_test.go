package tui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"hearth/internal/core"
)

func TestCollectRefs(t *testing.T) {
	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, "a.txt"), []byte("l1\nl2\nl3\nl4\nl5\n"), 0o644)
	_ = os.MkdirAll(filepath.Join(dir, "sub"), 0o755)
	_ = os.WriteFile(filepath.Join(dir, "sub", "b.go"), []byte("package sub\n"), 0o644)

	// whole file
	refs, missing := collectRefs("look at @a.txt please", dir)
	if len(refs) != 1 || len(missing) != 0 {
		t.Fatalf("refs=%d missing=%v", len(refs), missing)
	}
	if !strings.Contains(refs[0].Content, "l3") {
		t.Errorf("whole-file content wrong: %q", refs[0].Content)
	}

	// line range
	refs, missing = collectRefs("show @a.txt#1-2 ... hmm @a.txt#2-3", dir)
	if len(refs) != 2 || len(missing) != 0 {
		t.Fatalf("refs=%d missing=%v", len(refs), missing)
	}
	if refs[1].Content != "l2\nl3" || refs[1].Start != 2 || refs[1].End != 3 {
		t.Errorf("range content: %+v", refs[1])
	}

	// out-of-range line numbers surface as missing
	refs, missing = collectRefs("@a.txt#121-135", dir)
	if len(refs) != 0 || len(missing) != 1 {
		t.Errorf("out-of-range must be reported: refs=%d missing=%v", len(refs), missing)
	}

	// L prefix variant
	refs, _ = collectRefs("@a.txt#L1-2", dir)
	if len(refs) != 1 || refs[0].Content != "l1\nl2" {
		t.Errorf("L-prefix: %+v", refs)
	}

	// missing file reported
	refs, missing = collectRefs("read @nope.txt and @a.txt", dir)
	if len(refs) != 1 || len(missing) != 1 || missing[0] != "nope.txt" {
		t.Fatalf("missing detection: refs=%d missing=%v", len(refs), missing)
	}

	// no refs → nil
	if refs, _ = collectRefs("plain text without attachments", dir); refs != nil {
		t.Error("plain text must produce no refs")
	}
}

func TestWorkspaceFiles(t *testing.T) {
	dir := t.TempDir()
	_ = os.MkdirAll(filepath.Join(dir, ".git", "objects"), 0o755)
	_ = os.MkdirAll(filepath.Join(dir, "src"), 0o755)
	_ = os.WriteFile(filepath.Join(dir, "src", "main.go"), []byte("x"), 0o644)
	_ = os.WriteFile(filepath.Join(dir, "README.md"), []byte("x"), 0o644)
	_ = os.WriteFile(filepath.Join(dir, ".git", "config"), []byte("x"), 0o644)

	files := workspaceFiles(dir, 500)
	joined := strings.Join(files, ",")
	if !strings.Contains(joined, "src/main.go") || !strings.Contains(joined, "README.md") {
		t.Errorf("picker missed files: %v", files)
	}
	if strings.Contains(joined, ".git") {
		t.Errorf(".git must be skipped: %v", files)
	}
}

func TestRefContextBlock(t *testing.T) {
	r := core.FileRef{Path: "a.txt", Start: 2, End: 3, Content: "l2\nl3"}
	block := refContextBlock(r)
	if !strings.Contains(block, "[file: a.txt#L2-3]") || !strings.Contains(block, "l2") {
		t.Errorf("block: %q", block)
	}
}
