package tui

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"hearth/internal/core"
	"hearth/internal/ollama"
	"hearth/internal/provider"
)

// exportMarkdown renders a session as a Markdown file.
func ExportMarkdown(sess *core.Session) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# %s\n\n", sess.Title)
	fmt.Fprintf(&b, "- model: `%s`\n", sess.Model)
	fmt.Fprintf(&b, "- session: `%s`\n", sess.ID)
	fmt.Fprintf(&b, "- exported: %s\n\n", time.Now().Format("2006-01-02 15:04:05"))
	if sess.Prompt() != "" {
		b.WriteString("> system: " + sess.Prompt() + "\n\n")
	}
	for _, m := range sess.Snapshot() {
		switch m.Role {
		case core.RoleUser:
			fmt.Fprintf(&b, "## 🧑 user\n\n%s\n\n", m.Content)
		case core.RoleAssistant:
			fmt.Fprintf(&b, "## 🤖 assistant")
			if m.Interrupted {
				b.WriteString(" *(interrupted)*")
			}
			b.WriteString("\n\n")
			for _, tc := range m.ToolCalls {
				fmt.Fprintf(&b, "**tool call** `%s(%s)`\n\n", tc.Name, toolArgsPreview(tc))
			}
			if m.Content != "" {
				b.WriteString(m.Content + "\n\n")
			}
		case core.RoleTool:
			fmt.Fprintf(&b, "<details><summary>tool result: %s</summary>\n\n```\n%s\n```\n\n</details>\n\n",
				m.ToolName, m.Content)
		}
	}
	return b.String()
}

// saveSessionFile writes the export and returns the display path.
func saveSessionFile(sess *core.Session, path string) (string, error) {
	if path == "" {
		safe := sanitizeFilename(sess.Title)
		path = fmt.Sprintf("hearth-%s-%s.md", safe, sess.ID)
	}
	if dir := filepath.Dir(path); dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return "", err
		}
	}
	if !strings.HasSuffix(strings.ToLower(path), ".md") {
		path += ".md"
	}
	if err := os.WriteFile(path, []byte(ExportMarkdown(sess)), 0o644); err != nil {
		return "", err
	}
	return path, nil
}

func sanitizeFilename(s string) string {
	s = strings.TrimSpace(s)
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		case r == ' ':
			b.WriteRune('-')
		}
	}
	out := b.String()
	if len(out) > 40 {
		out = out[:40]
	}
	if out == "" {
		out = "chat"
	}
	return out
}

// compactSummarizer summarizes a conversation for /compact.
func compactSummarize(ctx context.Context, client provider.Client, model string, sess *core.Session, opts ollama.Options, keepAlive string) (string, error) {
	var b strings.Builder
	for _, m := range sess.Snapshot() {
		switch m.Role {
		case core.RoleUser:
			fmt.Fprintf(&b, "\n[user]: %s\n", m.Content)
		case core.RoleAssistant:
			if m.Content != "" {
				fmt.Fprintf(&b, "[assistant]: %s\n", m.Content)
			}
			for _, tc := range m.ToolCalls {
				fmt.Fprintf(&b, "[assistant used tool: %s]\n", tc.Name)
			}
		case core.RoleTool:
			fmt.Fprintf(&b, "[tool %s]: %s\n", m.ToolName, firstNLines(m.Content, 3))
		}
	}
	req := ollama.ChatRequest{
		Model: model,
		Messages: []ollama.Message{
			{Role: "system", Content: "Summarize the conversation below: keep key facts, decisions, file paths, code names and pending tasks. Be concise but complete, in the same language as the conversation. Output only the summary."},
			{Role: "user", Content: "Conversation:\n" + firstNLines(b.String(), 400)},
		},
		Options:   &opts,
		KeepAlive: keepAlive,
	}
	resp, err := client.Chat(ctx, req)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(resp.Message.Content), nil
}
