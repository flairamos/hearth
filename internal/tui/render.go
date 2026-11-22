package tui

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/glamour"
	"github.com/charmbracelet/lipgloss"

	"hearth/internal/core"
	"hearth/internal/theme"
)

// renderer converts session messages into terminal text with caching.
// Markdown is rendered (glamour) only for completed assistant messages;
// the in-flight response is plain text (PRD: 渲染节流). Completed blocks
// are cached per (index, content-length) so streaming frames never
// re-render history, and the joined transcript keeps an append-only
// prefix cache so a frame only joins the newest blocks (卡顿修复:
// 全量 Join + stripANSI 每帧跑一遍是长会话流式卡顿的根源).
type renderer struct {
	width   int
	style   string
	md      *glamour.TermRenderer
	mdWidth int
	cache   map[renderKey]string

	// folded transcript prefix: messages [0, folded) already joined.
	// Validity is self-checked per refresh against the boundary message's
	// timestamp and the render width, so a missed Invalidate can't serve
	// stale text — trims/switches shift the boundary and force a rebuild.
	folded   int
	foldedAt time.Time
	foldedW  int
	prefix   string

	// incremental wrap state for the in-flight reply (liveBlock)
	live struct {
		src  string // source already wrapped & committed (complete lines)
		tail string // partial last line shown last frame
		out  string // indented wrapped rendering of src
	}
}

type renderKey struct {
	idx  int
	size int
}

func newRenderer(width int, glamourStyle string) *renderer {
	return &renderer{width: width, style: glamourStyle, cache: map[renderKey]string{}}
}

// Invalidate clears the render cache (width change, trim, model switch...).
func (r *renderer) Invalidate() {
	r.cache = map[renderKey]string{}
	r.folded, r.prefix = 0, ""
	r.live.src, r.live.tail, r.live.out = "", "", ""
}

// blankANSI reports whether a rendered block is visually empty.
func blankANSI(s string) bool {
	return strings.TrimSpace(stripANSI(s)) == ""
}

// transcript renders messages as blocks joined with "\n\n", folding the
// append-only prefix into r.prefix. Messages are immutable once added, so
// only the tail is (re)joined per frame; trims shrink history and flip the
// boundary check, rebuilding from scratch.
func (r *renderer) transcript(th theme.Theme, msgs []core.Message) string {
	if r.foldedW != r.width || r.folded > len(msgs) ||
		(r.folded > 0 && !msgs[r.folded-1].CreatedAt.Equal(r.foldedAt)) {
		r.folded, r.prefix = 0, ""
	}
	var b strings.Builder
	b.WriteString(r.prefix)
	for i := r.folded; i < len(msgs); i++ {
		blk := r.block(th, i, &msgs[i])
		if blankANSI(blk) {
			continue
		}
		if b.Len() > 0 {
			b.WriteString("\n\n")
		}
		b.WriteString(blk)
	}
	r.folded = len(msgs)
	if len(msgs) > 0 {
		r.foldedAt = msgs[len(msgs)-1].CreatedAt
	}
	r.foldedW = r.width
	r.prefix = b.String()
	return b.String()
}

func (r *renderer) mdRender(s string) string {
	w := r.width - 6
	if w < 20 {
		w = 20
	}
	if r.md == nil || r.mdWidth != w {
		gr, err := glamour.NewTermRenderer(
			glamour.WithStandardStyle(r.style),
			glamour.WithWordWrap(w),
		)
		if err != nil {
			return s
		}
		r.md, r.mdWidth = gr, w
	}
	out, err := r.md.Render(s)
	if err != nil {
		return s
	}
	return strings.TrimRight(out, "\n")
}

func indent(s string, n int) string {
	pad := strings.Repeat(" ", n)
	lines := strings.Split(s, "\n")
	for i, ln := range lines {
		lines[i] = pad + ln
	}
	return strings.Join(lines, "\n")
}

// block renders one message. idx is the message position used as the cache
// key — messages in the session are immutable once added, so completed
// blocks render exactly once (streaming deltas render via liveBlock only).
func (r *renderer) block(th theme.Theme, idx int, m *core.Message) string {
	key := renderKey{idx: idx, size: len(m.Content) + len(m.Thinking)}
	if s, ok := r.cache[key]; ok {
		return s
	}
	rendered := r.renderBlock(th, m)
	r.cache[key] = rendered
	return rendered
}

func (r *renderer) renderBlock(th theme.Theme, m *core.Message) string {
	switch m.Role {
	case core.RoleUser:
		// the compaction summary travels as a user message (API requires it)
		// but must not read as if the user typed it
		if strings.HasPrefix(m.Content, "[conversation summary]") {
			return th.Dim.Render("· " + firstLine(m.Content))
		}
		var b strings.Builder
		b.WriteString(th.UserBar.Render("▍") + th.UserTag.Render(" you") + "\n")
		body := m.Content
		if len(m.Images) > 0 {
			body = fmt.Sprintf("[image attached ×%d]\n%s", len(m.Images), body)
		}
		b.WriteString(indent(wrapPlain(body, r.width-4), 2))
		for _, r := range m.Refs {
			label := r.Path
			if r.Start > 0 {
				label = fmt.Sprintf("%s#L%d-%d", r.Path, r.Start, r.End)
			}
			size := len([]rune(r.Content))
			b.WriteString("\n" + th.Dim.Render(fmt.Sprintf("  📎 %s (%d chars)", label, size)))
		}
		return b.String()

	case core.RoleSystem:
		if strings.Contains(m.Content, "Tool budget") || strings.Contains(m.Content, "[conversation summary]") {
			return th.Dim.Render("· " + firstLine(m.Content))
		}
		return "" // hidden system messages

	case core.RoleTool:
		mark, style := "✓", th.OK
		if m.ToolError {
			mark, style = "✗", th.Err
		}
		preview := firstNLines(m.Content, 4)
		if preview != strings.TrimRight(m.Content, "\n") {
			preview += "\n…"
		}
		return style.Render(fmt.Sprintf("  ↳ %s %s", mark, m.ToolName)) + "\n" +
			indent(th.Dim.Render(preview), 4)

	case core.RoleAssistant:
		var b strings.Builder
		if len(m.ToolCalls) > 0 {
			for _, tc := range m.ToolCalls {
				b.WriteString(th.ToolTag.Render(fmt.Sprintf("⚙ %s %s", tc.Name, toolArgsPreview(tc))) + "\n")
			}
		}
		if m.Content != "" {
			b.WriteString(indent(r.mdRender(m.Content), 2))
		}
		if m.Interrupted {
			b.WriteString("\n" + th.Warn.Render("  ⏸ interrupted"))
		}
		if m.Truncated {
			b.WriteString("\n" + th.Warn.Render("  … cut by the token limit — raise generation.max_tokens or shrink /ctx"))
		}
		if len(m.ToolCalls) == 0 && m.Content != "" {
			if m.Tokens > 0 {
				b.WriteString("\n" + th.Dim.Render(fmt.Sprintf("  %d tok", m.Tokens)))
			}
		}
		return strings.TrimRight(b.String(), "\n")
	}
	return ""
}

// toolArgsPreview builds a compact arg summary for tool call lines.
func toolArgsPreview(tc core.ToolCall) string {
	switch tc.Name {
	case "bash":
		s := core.ToString(tc.Args["command"])
		return firstLine(s)
	case "read_file", "write_file", "edit_file":
		s := core.ToString(tc.Args["path"])
		return s
	case "glob":
		return core.ToString(tc.Args["pattern"])
	case "grep":
		return core.ToString(tc.Args["pattern"])
	case "list_dir":
		return core.ToString(tc.Args["path"])
	default:
		b, _ := json.Marshal(tc.Args)
		s := string(b)
		if len(s) > 80 {
			s = s[:80] + "…"
		}
		return s
	}
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i] + " …"
	}
	if len(s) > 140 {
		s = s[:140] + "…"
	}
	return s
}

// truncateRunes shortens s to n runes with an ellipsis marker.
func truncateRunes(s string, n int) string {
	if n <= 0 {
		return ""
	}
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

func firstNLines(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) <= n {
		return strings.Join(lines, "\n")
	}
	return strings.Join(lines[:n], "\n")
}

// wrapPlain soft-wraps text to width (ANSI-aware via lipgloss).
func wrapPlain(s string, width int) string {
	if width < 10 {
		width = 10
	}
	return lipgloss.NewStyle().Width(width).Render(s)
}

// liveBlock renders the in-flight response (not yet in the session).
// Wrapping is incremental: the stream is append-only, so each line is
// wrapped exactly once when it completes instead of re-wrapping the whole
// reply every frame (the O(n²) that stuttered late in long responses).
// State: src = source already wrapped & committed (complete lines),
// tail = partial last line shown last frame (re-wraps until it closes),
// out = the indented wrapped rendering of src.
func (r *renderer) liveBlock(th theme.Theme, buf *deltaBuf, phase string) string {
	_, content := buf.Total()
	if content == "" {
		return th.Dim.Render("  " + phase)
	}
	w := r.width - 4
	if w < 10 {
		w = 10
	}
	if !strings.HasPrefix(content, r.live.src) {
		r.live.src, r.live.tail, r.live.out = "", "", ""
	}
	// the uncommitted region holds the previous partial line + new deltas
	rest := content[len(r.live.src):]
	if i := strings.LastIndexByte(rest, '\n'); i >= 0 {
		r.live.src += rest[:i+1]
		r.live.tail = rest[i+1:]
		if r.live.out != "" {
			r.live.out += "\n"
		}
		r.live.out += indent(wrapPlain(strings.TrimSuffix(rest[:i+1], "\n"), w), 2)
	} else {
		r.live.tail = rest
	}

	var b strings.Builder
	b.WriteString(th.AsstBar.Render("▍") + th.AsstTag.Render(" assistant") + "\n")
	out := r.live.out
	if r.live.tail != "" {
		if out != "" {
			out += "\n"
		}
		out += indent(wrapPlain(r.live.tail, w), 2)
	}
	if out != "" {
		b.WriteString(out + "▊")
	} else {
		b.WriteString("▊")
	}
	return b.String()
}

// lastLines returns the final n non-trivial lines of s without splitting
// the whole string (thinking grows to tens of KB).
func lastLines(s string, n int) []string {
	s = strings.TrimSpace(s)
	var out []string
	for len(out) < n && s != "" {
		idx := strings.LastIndexByte(s, '\n')
		ln := s
		if idx >= 0 {
			ln, s = s[idx+1:], s[:idx]
		} else {
			s = ""
		}
		if strings.TrimSpace(ln) == "" {
			continue // empty lines render as ghost padding under lipgloss
		}
		out = append([]string{ln}, out...)
	}
	return out
}

// thinkingBlock renders the streaming thinking (last few lines, dim).
func (r *renderer) thinkingBlock(th theme.Theme, buf *deltaBuf) string {
	thinking, _ := buf.Total()
	if thinking == "" {
		return ""
	}
	out := make([]string, 0, 4)
	for _, ln := range lastLines(thinking, 4) {
		if len([]rune(ln)) > r.width-6 {
			ln = string([]rune(ln)[:max(0, r.width-8)]) + "…"
		}
		out = append(out, indent(ln, 2))
	}
	if len(out) == 0 {
		return ""
	}
	return th.Thinking.Render(strings.Join(out, "\n"))
}
