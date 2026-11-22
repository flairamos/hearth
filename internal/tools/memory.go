package tools

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"hearth/internal/config"
)

// ---- long-term memory (persistent across sessions) ----

// MemoryStore is a file-backed fact store at DataDir/memory.md.
// Format: one entry per line "- [2006-01-02 15:04] content".
type MemoryStore struct {
	path string
	mu   sync.Mutex
}

// NewMemoryStore creates a memory store at path (dir is created lazily).
func NewMemoryStore(path string) *MemoryStore {
	return &MemoryStore{path: path}
}

// MemoryPath returns the default memory file location.
func MemoryPath() string {
	return filepath.Join(config.DataDir(), "memory.md")
}

type memEntry struct {
	when    time.Time
	content string
}

func (m *MemoryStore) load() ([]memEntry, error) {
	b, err := os.ReadFile(m.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []memEntry
	re := regexp.MustCompile(`^- \[(\d{4}-\d{2}-\d{2} \d{2}:\d{2})\] (.*)$`)
	for _, ln := range strings.Split(string(b), "\n") {
		ln = strings.TrimRight(ln, "\r")
		if mm := re.FindStringSubmatch(ln); mm != nil {
			t, _ := time.Parse("2006-01-02 15:04", mm[1])
			out = append(out, memEntry{when: t, content: mm[2]})
		} else if strings.TrimSpace(ln) != "" {
			// legacy / externally edited lines
			out = append(out, memEntry{content: strings.TrimSpace(ln)})
		}
	}
	return out, nil
}

func (m *MemoryStore) save(entries []memEntry) error {
	if err := os.MkdirAll(filepath.Dir(m.path), 0o755); err != nil {
		return err
	}
	var b strings.Builder
	for _, e := range entries {
		if e.when.IsZero() {
			b.WriteString("- " + e.content + "\n")
		} else {
			fmt.Fprintf(&b, "- [%s] %s\n", e.when.Format("2006-01-02 15:04"), e.content)
		}
	}
	tmp := m.path + ".tmp"
	if err := os.WriteFile(tmp, []byte(b.String()), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, m.path)
}

// Append adds a durable entry (dedupes exact repeats, caps at 500 entries).
func (m *MemoryStore) Append(content string) error {
	content = strings.TrimSpace(content)
	if content == "" {
		return fmt.Errorf("empty memory entry")
	}
	if len([]rune(content)) > 400 {
		r := []rune(content)
		content = string(r[:400]) + "…"
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	entries, err := m.load()
	if err != nil {
		return err
	}
	for _, e := range entries {
		if e.content == content {
			return nil // already known
		}
	}
	entries = append(entries, memEntry{when: time.Now(), content: content})
	if len(entries) > 500 {
		entries = entries[len(entries)-500:]
	}
	return m.save(entries)
}

// Search returns matching entries (case-insensitive substring); empty query
// returns the most recent ones.
func (m *MemoryStore) Search(query string, limit int) ([]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	entries, err := m.load()
	if err != nil {
		return nil, err
	}
	if limit <= 0 {
		limit = 10
	}
	var out []string
	q := strings.ToLower(strings.TrimSpace(query))
	if q == "" {
		for i := len(entries) - 1; i >= 0 && len(out) < limit; i-- {
			out = append(out, entries[i].content)
		}
		return out, nil
	}
	// newest matches first
	for i := len(entries) - 1; i >= 0 && len(out) < limit; i-- {
		if strings.Contains(strings.ToLower(entries[i].content), q) {
			out = append(out, entries[i].content)
		}
	}
	return out, nil
}

// Count returns the number of stored entries.
func (m *MemoryStore) Count() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	entries, _ := m.load()
	return len(entries)
}

func memorySaveTool(reg *Registry) Tool {
	return Tool{
		Name:     "memory_save",
		Category: CatMutate,
		Desc:     "Save a durable fact to long-term memory that persists across sessions. Use for user preferences, project facts, decisions, and corrections. Not for temporary data — use note for that.",
		Schema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"content": map[string]any{"type": "string", "description": "One self-contained fact, e.g. \"User's project uses Go 1.22 and PostgreSQL\""},
			},
			"required": []string{"content"},
		},
		Fn: func(ctx context.Context, r *Registry, args map[string]any) (string, error) {
			if r.Memory == nil {
				return "", fmt.Errorf("long-term memory is disabled in config")
			}
			content := argStr(args, "content")
			if err := r.Memory.Append(content); err != nil {
				return "", err
			}
			return fmt.Sprintf("saved to memory: %s", content), nil
		},
	}
}

func memoryRecallTool(reg *Registry) Tool {
	return Tool{
		Name:     "memory_recall",
		Category: CatRead,
		Desc:     "Search long-term memory (persists across sessions). With query: returns matching facts. Without: returns the most recent facts.",
		Schema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"query": map[string]any{"type": "string", "description": "Keyword(s) to search for; omit to list recent memories"},
			},
		},
		Fn: func(ctx context.Context, r *Registry, args map[string]any) (string, error) {
			if r.Memory == nil {
				return "", fmt.Errorf("long-term memory is disabled in config")
			}
			res, err := r.Memory.Search(argStr(args, "query"), 10)
			if err != nil {
				return "", err
			}
			if len(res) == 0 {
				return "no matching memories", nil
			}
			var b strings.Builder
			for i, s := range res {
				fmt.Fprintf(&b, "%d. %s\n", i+1, s)
			}
			return strings.TrimRight(b.String(), "\n"), nil
		},
	}
}

// ---- short-term memory: session scratchpad (note) ----

func noteTool() Tool {
	return Tool{
		Name:     "note",
		Category: CatRead,
		Desc:     "Session scratchpad — your short-term memory inside this conversation. Write a short plan or intermediate results; the notes are always visible to you in the system prompt. Actions: write (replace), append, read.",
		Schema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"action":  map[string]any{"type": "string", "enum": []string{"write", "append", "read"}, "description": "write replaces all notes, append adds a line, read shows current notes"},
				"content": map[string]any{"type": "string", "description": "Note text (for write/append)"},
			},
			"required": []string{"action"},
		},
		Fn: func(ctx context.Context, r *Registry, args map[string]any) (string, error) {
			if r.Session == nil {
				return "", fmt.Errorf("no active session")
			}
			action := argStr(args, "action")
			content := strings.TrimRight(argStr(args, "content"), "\n")
			switch action {
			case "write":
				if len([]rune(content)) > 2000 {
					content = string([]rune(content)[:2000])
				}
				r.Session.Scratchpad = content
				return "notes updated:\n" + content, nil
			case "append":
				if len([]rune(content)) > 1000 {
					content = string([]rune(content)[:1000])
				}
				if r.Session.Scratchpad != "" {
					r.Session.Scratchpad += "\n"
				}
				r.Session.Scratchpad += content
				return "notes appended:\n" + r.Session.Scratchpad, nil
			case "read":
				if r.Session.Scratchpad == "" {
					return "(notes are empty)", nil
				}
				return r.Session.Scratchpad, nil
			default:
				return "", fmt.Errorf("action must be write, append or read")
			}
		},
	}
}
