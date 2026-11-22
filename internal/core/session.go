// Package core holds session and message domain types shared by the TUI,
// the agent harness and the store.
package core

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"strings"
	"sync"
	"time"
	"unicode"
)

// Role is a message role.
type Role string

const (
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleSystem    Role = "system"
	RoleTool      Role = "tool"
)

// ToolCall is a tool invocation requested by the assistant.
type ToolCall struct {
	ID   string         `json:"id,omitempty"`
	Name string         `json:"name"`
	Args map[string]any `json:"args,omitempty"`
}

// FileRef is an @path#Lstart-end attachment attached to a user message.
type FileRef struct {
	Path    string `json:"path"`
	Start   int    `json:"start,omitempty"` // 1-based; 0 = whole file
	End     int    `json:"end,omitempty"`
	Content string `json:"content,omitempty"` // snapshot captured at send time
}

// Message is a single message in a session.
type Message struct {
	Role        Role       `json:"role"`
	Content     string     `json:"content"`
	Thinking    string     `json:"thinking,omitempty"` // model reasoning (display only)
	ToolCalls   []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID  string     `json:"tool_call_id,omitempty"` // tool results
	ToolName    string     `json:"tool_name,omitempty"`
	ToolError   bool       `json:"tool_error,omitempty"`
	Images      []string   `json:"images,omitempty"` // base64
	Refs        []FileRef  `json:"refs,omitempty"`   // @file attachments
	Interrupted bool       `json:"interrupted,omitempty"`
	Truncated   bool       `json:"truncated,omitempty"` // cut by a token limit (done_reason=length)
	Tokens      int        `json:"tokens,omitempty"`
	CreatedAt   time.Time  `json:"created_at"`
}

// Params are per-session generation overrides.
type Params struct {
	Temperature float64 `json:"temperature,omitempty"` // 0 = config default
	NumCtx      int     `json:"num_ctx,omitempty"`     // 0 = config default
	Think       *bool   `json:"think,omitempty"`
}

// Session is a persisted conversation. It is safe for concurrent use:
// the agent loop mutates messages from its own goroutine while the UI
// thread renders — every access goes through the internal mutex, and
// readers take copies via Snapshot/Clone.
type Session struct {
	mu sync.RWMutex

	Version      int       `json:"version"`
	ID           string    `json:"id"`
	Title        string    `json:"title"`
	Model        string    `json:"model"`
	SystemPrompt string    `json:"system_prompt,omitempty"`
	Params       Params    `json:"params,omitempty"`
	AgentMode    bool      `json:"agent_mode,omitempty"`
	Compacted    bool      `json:"compacted,omitempty"`
	Scratchpad   string    `json:"scratchpad,omitempty"` // agent short-term memory (note tool)
	Messages     []Message `json:"messages"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

// NewID returns a short random session id.
func NewID() string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return "sess_" + hex.EncodeToString(b)
}

// NewSession creates an empty session for the given model.
func NewSession(model string) *Session {
	now := time.Now()
	return &Session{
		Version:   1,
		ID:        NewID(),
		Title:     "New chat",
		Model:     model,
		Messages:  []Message{},
		CreatedAt: now,
		UpdatedAt: now,
	}
}

// Touch updates the UpdatedAt timestamp.
func (s *Session) Touch() { s.UpdatedAt = time.Now() }

// Add appends a message, stamps time and touches the session.
func (s *Session) Add(m Message) Message {
	if m.CreatedAt.IsZero() {
		m.CreatedAt = time.Now()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Messages = append(s.Messages, m)
	s.Touch()
	return m
}

// Snapshot returns a copy of the message slice for safe iteration.
func (s *Session) Snapshot() []Message {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Message, len(s.Messages))
	copy(out, s.Messages)
	return out
}

// Len is the current message count.
func (s *Session) Len() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.Messages)
}

// Clone returns a lock-free copy with an independent message slice, for
// JSON marshalling without holding the lock.
func (s *Session) Clone() *Session {
	s.mu.RLock()
	defer s.mu.RUnlock()
	c := &Session{
		Version:      s.Version,
		ID:           s.ID,
		Title:        s.Title,
		Model:        s.Model,
		SystemPrompt: s.SystemPrompt,
		Params:       s.Params,
		AgentMode:    s.AgentMode,
		Compacted:    s.Compacted,
		Scratchpad:   s.Scratchpad,
		Messages:     make([]Message, len(s.Messages)),
		CreatedAt:    s.CreatedAt,
		UpdatedAt:    s.UpdatedAt,
	}
	copy(c.Messages, s.Messages)
	return c
}

// SetMessages replaces the whole message list (/clear, /compact).
func (s *Session) SetMessages(msgs []Message) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if msgs == nil {
		msgs = []Message{}
	}
	s.Messages = msgs
	s.Touch()
}

// RemoveWhere deletes the first message matching pred (newest first scan).
func (s *Session) RemoveWhere(pred func(Message) bool) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := len(s.Messages) - 1; i >= 0; i-- {
		if pred(s.Messages[i]) {
			s.Messages = append(s.Messages[:i:i], s.Messages[i+1:]...)
			return true
		}
	}
	return false
}

// PopLastUserTurn removes everything from the most recent user message
// onward and returns its content. Used by /retry and context-overflow
// retries to roll back a failed turn.
func (s *Session) PopLastUserTurn() (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := len(s.Messages) - 1; i >= 0; i-- {
		if s.Messages[i].Role == RoleUser {
			text := s.Messages[i].Content
			s.Messages = s.Messages[:i]
			s.Touch()
			return text, true
		}
	}
	return "", false
}

// Scratchpad returns the current notes (short-term memory).
func (s *Session) Notes() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.Scratchpad
}

// SetScratchpad replaces the notes.
func (s *Session) SetScratchpad(v string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Scratchpad = v
	s.Touch()
}

// SystemPrompt returns the session system prompt.
func (s *Session) Prompt() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.SystemPrompt
}

// SetPrompt updates the session system prompt.
func (s *Session) SetPrompt(v string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.SystemPrompt = v
	s.Touch()
}

// AutoTitle sets a title from the first user message if unset/generic.
func (s *Session) AutoTitle() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.Title != "" && s.Title != "New chat" {
		return
	}
	for _, m := range s.Messages {
		if m.Role != RoleUser {
			continue
		}
		t := strings.Join(strings.Fields(m.Content), " ")
		if t == "" {
			continue
		}
		r := []rune(t)
		if len(r) > 32 {
			t = string(r[:32]) + "…"
		}
		s.Title = t
		return
	}
}

// NumCtx returns the effective context size for the session.
func (s *Session) NumCtx(def int) int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.Params.NumCtx > 0 {
		return s.Params.NumCtx
	}
	return def
}

// UsedTokens is the estimated token total of the whole message list.
func (s *Session) UsedTokens() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	n := 0
	for _, m := range s.Messages {
		n += MsgTokens(m)
	}
	return n
}

// MsgTokens estimates the token cost of one message.
func MsgTokens(m Message) int {
	n := EstimateTokens(m.Content) + EstimateTokens(m.Thinking)
	for _, tc := range m.ToolCalls {
		n += 8 + EstimateTokens(tc.Name)
		for k, v := range tc.Args {
			n += EstimateTokens(k) + EstimateTokens(ToString(v))
		}
	}
	for _, r := range m.Refs {
		n += 8 + EstimateTokens(r.Content) // @file attachments sent to the API
	}
	n += EstimateTokens(m.ToolName) + 6 // per-message overhead
	return n
}

// EstimateTokens is a CJK-aware heuristic: CJK runes ≈ 1 token each,
// other text ≈ 4 chars per token.
func EstimateTokens(s string) int {
	if s == "" {
		return 0
	}
	cjk, other := 0, 0
	for _, r := range s {
		switch {
		case r < 128:
			other++
		case unicode.Is(unicode.Han, r), unicode.Is(unicode.Hiragana, r), unicode.Is(unicode.Katakana, r),
			unicode.Is(unicode.Hangul, r), r > 0xFF00 && r < 0xFFEF:
			cjk++
		default:
			other++
		}
	}
	return cjk + (other+3)/4
}

// ToString renders an arg value compactly.
func ToString(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case nil:
		return ""
	default:
		b, _ := json.Marshal(v)
		return string(b)
	}
}

// TrimToMax drops the oldest complete turns (user → assistant, including
// tool exchanges) until the estimated token usage fits within budget. When
// only one turn remains — a long agent task is a single user turn spanning
// dozens of messages — the oldest assistant/tool exchanges inside that turn
// are dropped too, always keeping its opening user message and the newest
// exchange. The system prompt and the final answer always survive. Returns
// true if anything was dropped.
func (s *Session) TrimToMax(budget int) bool {
	budget = budget * 85 / 100 // safety margin
	s.mu.Lock()
	defer s.mu.Unlock()
	changed := false
	for s.estimateLocked() > budget {
		first := -1
		for i, m := range s.Messages {
			if m.Role == RoleUser {
				first = i
				break
			}
		}
		if first < 0 {
			break
		}
		next := -1
		for i := first + 1; i < len(s.Messages); i++ {
			if s.Messages[i].Role == RoleUser {
				next = i
				break
			}
		}
		if next < 0 {
			break // only the final turn remains — trim inside it
		}
		s.Messages = append(s.Messages[:first:first], s.Messages[next:]...)
		changed = true
	}
	if s.estimateLocked() > budget {
		changed = s.trimLastTurnLocked(budget) || changed
	}
	return changed
}

// trimLastTurnLocked drops the oldest exchange groups inside the final turn
// (caller holds mu). A group starts at each assistant message and runs up to
// the next one, so a tool result never separates from the assistant call
// that requested it; the opening user message and the newest group survive.
func (s *Session) trimLastTurnLocked(budget int) bool {
	start := 0 // no user message at all — degenerate history, still safe to trim
	for i := len(s.Messages) - 1; i >= 0; i-- {
		if s.Messages[i].Role == RoleUser {
			start = i + 1
			break
		}
	}
	if start >= len(s.Messages) {
		return false
	}
	// group boundaries: one per assistant message; the first group may be
	// anchored earlier by orphaned tool results
	bounds := []int{start}
	for i := start + 1; i < len(s.Messages); i++ {
		if s.Messages[i].Role == RoleAssistant {
			bounds = append(bounds, i)
		}
	}
	if len(bounds) <= 1 {
		return false // only the newest group remains — keep it
	}
	changed := false
	for s.estimateLocked() > budget && len(bounds) > 1 {
		drop := bounds[1] - bounds[0]
		s.Messages = append(s.Messages[:bounds[0]:bounds[0]], s.Messages[bounds[1]:]...)
		for i := range bounds {
			bounds[i] -= drop
		}
		bounds = bounds[1:]
		changed = true
	}
	return changed
}

// estimateLocked is UsedTokens without re-locking (caller holds mu).
func (s *Session) estimateLocked() int {
	n := 0
	for _, m := range s.Messages {
		n += MsgTokens(m)
	}
	return n
}

// LastUserText returns the most recent user message content.
func (s *Session) LastUserText() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for i := len(s.Messages) - 1; i >= 0; i-- {
		if s.Messages[i].Role == RoleUser {
			return s.Messages[i].Content
		}
	}
	return ""
}
