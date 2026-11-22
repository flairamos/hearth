package core

import (
	"fmt"
	"strings"
	"testing"
)

func TestEstimateTokens(t *testing.T) {
	cases := []struct {
		in   string
		want int
	}{
		{"", 0},
		{"hello", 2}, // 5 latin → ceil(5/4)=2
		{"你好世界", 4},  // 4 CJK → 4
		{"abc你好", 4}, // 3 latin + 2 cjk → 1+2? ceil(3/4)=1 → 3
		{strings.Repeat("a", 400), 100},
	}
	for _, c := range cases {
		got := EstimateTokens(c.in)
		if c.in == "abc你好" {
			if got < 3 || got > 4 {
				t.Errorf("EstimateTokens(%q) = %d, want 3..4", c.in, got)
			}
			continue
		}
		if got != c.want {
			t.Errorf("EstimateTokens(%q) = %d, want %d", c.in, got, c.want)
		}
	}
}

func TestTrimToMax(t *testing.T) {
	s := NewSession("m")
	s.Add(Message{Role: RoleSystem, Content: "sys"})
	// three fat turns
	for i := 0; i < 3; i++ {
		s.Add(Message{Role: RoleUser, Content: strings.Repeat("turn ", 500) + string(rune('a'+i))})
		s.Add(Message{Role: RoleAssistant, Content: strings.Repeat("reply ", 500)})
	}
	before := len(s.Messages)
	trimmed := s.TrimToMax(100) // tiny budget
	if !trimmed {
		t.Fatal("expected trim to happen")
	}
	if len(s.Messages) >= before {
		t.Fatalf("messages not reduced: %d → %d", before, len(s.Messages))
	}
	if s.Messages[0].Role != RoleSystem {
		t.Errorf("system prompt must survive trimming")
	}
	if s.Messages[len(s.Messages)-1].Role == RoleTool {
		t.Errorf("must not leave a dangling tool result")
	}
	// budget big enough → nothing dropped
	s2 := NewSession("m")
	s2.Add(Message{Role: RoleUser, Content: "hi"})
	s2.Add(Message{Role: RoleAssistant, Content: "hello"})
	if s2.TrimToMax(100000) {
		t.Error("no trim expected with big budget")
	}
	// single turn only → never dropped
	s3 := NewSession("m")
	s3.Add(Message{Role: RoleUser, Content: strings.Repeat("x", 9000)})
	if s3.TrimToMax(10) {
		t.Error("final turn must be preserved")
	}
}

func TestTrimToMaxSingleAgentTurn(t *testing.T) {
	// a long agent task = ONE user turn spanning many assistant/tool messages
	s := NewSession("m")
	s.Add(Message{Role: RoleUser, Content: strings.Repeat("任务 ", 400)})
	var lastAssistant Message
	for i := 0; i < 6; i++ {
		lastAssistant = s.Add(Message{
			Role:      RoleAssistant,
			Content:   fmt.Sprintf("step %d", i),
			ToolCalls: []ToolCall{{Name: "read_file", Args: map[string]any{"path": "x"}}},
		})
		s.Add(Message{Role: RoleTool, ToolName: "read_file", Content: strings.Repeat("output ", 400)})
	}
	before := s.UsedTokens()
	if !s.TrimToMax(500) {
		t.Fatal("expected intra-turn trim to happen")
	}
	if after := s.UsedTokens(); after >= before {
		t.Fatalf("usage not reduced: %d → %d", before, after)
	}
	msgs := s.Snapshot()
	if msgs[0].Role != RoleUser {
		t.Errorf("opening user message must survive: roles[0]=%s", msgs[0].Role)
	}
	// the newest exchange group survives intact: its tool result is what the
	// next model call needs, so history ends with it (not the assistant)
	if msgs[len(msgs)-1].Role != RoleTool || !msgs[len(msgs)-2].CreatedAt.Equal(lastAssistant.CreatedAt) {
		t.Errorf("newest exchange group must survive intact")
	}
	for i, m := range msgs {
		if m.Role == RoleTool && (i == 0 || msgs[i-1].Role != RoleAssistant) {
			t.Errorf("orphan tool result at %d (no preceding assistant)", i)
		}
	}
}

func TestAutoTitle(t *testing.T) {
	s := NewSession("m")
	s.Add(Message{Role: RoleUser, Content: "帮我解释一下 Rust 的生命周期是什么，越详细越好"})
	s.AutoTitle()
	if !strings.HasPrefix(s.Title, "帮我解释一下") {
		t.Errorf("title = %q", s.Title)
	}
	if len([]rune(s.Title)) > 34 {
		t.Errorf("title too long: %q", s.Title)
	}
}
