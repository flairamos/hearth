package store

import (
	"os"
	"path/filepath"
	"testing"

	"hearth/internal/core"
)

func TestSaveLoadListDelete(t *testing.T) {
	dir := t.TempDir()
	s, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}

	sess := core.NewSession("qwen3.5:4b")
	sess.Title = "test session"
	sess.AgentMode = true
	sess.Add(core.Message{Role: core.RoleUser, Content: "你好"})
	sess.Add(core.Message{
		Role:      core.RoleAssistant,
		Content:   "answer",
		Thinking:  "thinking text",
		ToolCalls: []core.ToolCall{{Name: "bash", Args: map[string]any{"command": "ls"}}},
	})
	sess.Add(core.Message{Role: core.RoleTool, Content: "out", ToolName: "bash"})

	if err := s.Save(sess); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, sess.ID+".json")); err != nil {
		t.Fatal("session file missing")
	}

	loaded, err := s.Load(sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Title != "test session" || loaded.Model != "qwen3.5:4b" || !loaded.AgentMode {
		t.Errorf("roundtrip mismatch: %+v", loaded)
	}
	if len(loaded.Messages) != 3 {
		t.Fatalf("messages = %d", len(loaded.Messages))
	}
	if loaded.Messages[1].Thinking != "thinking text" || len(loaded.Messages[1].ToolCalls) != 1 {
		t.Errorf("assistant message fields lost: %+v", loaded.Messages[1])
	}

	list, err := s.List()
	if err != nil || len(list) != 1 || list[0].Title != "test session" {
		t.Fatalf("list: %v %+v", err, list)
	}

	latest, err := s.Latest()
	if err != nil || latest.ID != sess.ID {
		t.Fatalf("latest: %v", err)
	}

	if err := s.Delete(sess.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Load(sess.ID); err == nil {
		t.Fatal("deleted session still loads")
	}

	// invalid ids rejected
	if _, err := s.Load("../etc/passwd"); err == nil {
		t.Fatal("path traversal must be rejected")
	}
}
