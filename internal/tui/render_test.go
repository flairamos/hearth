package tui

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/lipgloss"

	"hearth/internal/core"
	"hearth/internal/theme"
)

func transcriptNaive(r *renderer, th theme.Theme, msgs []core.Message) string {
	var out []string
	for i := range msgs {
		blk := r.block(th, i, &msgs[i])
		if !blankANSI(blk) {
			out = append(out, blk)
		}
	}
	return strings.Join(out, "\n\n")
}

func TestRenderCompactSummaryAsSystemLine(t *testing.T) {
	th := theme.Get("dark")
	r := newRenderer(80, th.GlamourStyle())
	m := core.Message{Role: core.RoleUser, Content: "[conversation summary]\nEarlier the user asked about X."}
	out := stripANSI(r.block(th, 0, &m))
	if strings.Contains(out, "you") {
		t.Fatalf("compaction summary must not render as a user message: %q", out)
	}
	if !strings.Contains(out, "[conversation summary]") {
		t.Fatalf("summary text missing: %q", out)
	}
}

func TestStatusViewShowsFailure(t *testing.T) {
	a := newSlashApp(t)
	a.lastStats = turnStats{failed: true}
	a.lastError = "ollama api error (HTTP 500): something broke"
	out := stripANSI(a.statusView(140))
	if !strings.Contains(out, "✗ task ended with an error") || !strings.Contains(out, "something broke") {
		t.Fatalf("status bar missing persistent failure indicator: %q", out)
	}
}

func TestRenderTruncatedMarker(t *testing.T) {
	th := theme.Get("dark")
	r := newRenderer(80, th.GlamourStyle())
	m := core.Message{Role: core.RoleAssistant, Content: "summary cut mid-sent", Truncated: true}
	out := stripANSI(r.block(th, 0, &m))
	if !strings.Contains(out, "cut by the token limit") {
		t.Fatalf("truncation marker missing: %q", out)
	}
	if strings.Contains(out, "⏸ interrupted") {
		t.Fatalf("truncated must not read as interrupted: %q", out)
	}
}

func TestStatusViewShowsInterrupted(t *testing.T) {
	a := newSlashApp(t)
	a.lastStats = turnStats{interrupted: true}
	out := stripANSI(a.statusView(140))
	if !strings.Contains(out, "⏸ last turn interrupted") {
		t.Fatalf("status bar missing interrupted indicator: %q", out)
	}
}

func TestStatusViewLiveRateUsesGenTime(t *testing.T) {
	// a long agent turn: 900 tok streamed over 1s of active generation, but
	// the turn has been running for 300s (tool work in between). The live
	// rate must reflect generation speed (~900 tok/s), not the diluted
	// per-turn average (3 tok/s).
	a := newSlashApp(t)
	a.gen = &genState{
		buf:       &deltaBuf{},
		phase:     "answering…",
		start:     time.Now().Add(-300 * time.Second),
		outTokEst: 900,
		genNs:     time.Second,
	}
	out := stripANSI(a.statusView(160))
	if strings.Contains(out, "3.0 tok/s") || strings.Contains(out, "0.3 tok/s") {
		t.Fatalf("rate still divided by whole-turn time: %q", out)
	}
	if !strings.Contains(out, "~900.0 tok/s") {
		t.Fatalf("gen-time rate missing: %q", out)
	}
}

func TestLiveBlockIncremental(t *testing.T) {
	th := theme.Get("dark")
	r := newRenderer(80, th.GlamourStyle())
	buf := &deltaBuf{}
	// frame 1: partial line
	buf.Add("", "hello wor")
	out1 := stripANSI(r.liveBlock(th, buf, "thinking…"))
	if !strings.Contains(out1, "hello wor") || !strings.Contains(out1, "assistant") {
		t.Fatalf("frame1: %q", out1)
	}
	// frame 2: the line completes and a second one starts
	buf.Add("", "ld\nsecond line")
	out2 := stripANSI(r.liveBlock(th, buf, "x"))
	for _, want := range []string{"hello world", "second line"} {
		if !strings.Contains(out2, want) {
			t.Fatalf("frame2 missing %q: %q", want, out2)
		}
	}
	if r.live.src != "hello world\n" {
		t.Fatalf("committed src = %q", r.live.src)
	}
	// frame 3: another partial — committed lines must not be re-wrapped
	before := r.live.out
	buf.Add("", " more")
	r.liveBlock(th, buf, "x")
	if r.live.out != before {
		t.Fatal("committed lines were re-wrapped")
	}
	if !strings.Contains(stripANSI(r.live.tail), "second line more") {
		t.Fatalf("tail = %q", r.live.tail)
	}
}

func TestThinkingBlockLastLines(t *testing.T) {
	th := theme.Get("dark")
	r := newRenderer(80, th.GlamourStyle())
	buf := &deltaBuf{}
	var thinking strings.Builder
	for i := 0; i < 100; i++ {
		thinking.WriteString(fmt.Sprintf("line %d\n", i))
	}
	buf.Add(thinking.String(), "")
	out := stripANSI(r.thinkingBlock(th, buf))
	if !strings.Contains(out, "line 99") {
		t.Fatalf("newest thinking line missing: %q", out)
	}
	if strings.Contains(out, "line 90") {
		t.Fatal("thinking block shows more than the last 4 lines")
	}
}

func TestListPanelScrollWindow(t *testing.T) {
	th := theme.Get("dark")
	items := make([]item, 0, 30)
	for i := 0; i < 30; i++ {
		items = append(items, item{title: fmt.Sprintf("session %02d", i)})
	}
	p := newListPanel(panelSession, "sessions", items)
	p.cursor = 20 // past the visible window
	out := stripANSI(p.view(th, 100, 40))
	if !strings.Contains(out, "> session 20") {
		t.Fatalf("selected row beyond the window not rendered: %q", out)
	}
	if strings.Contains(out, "> session 00") {
		t.Fatal("first row should have scrolled out of the window")
	}
	if !strings.Contains(out, "21/30") {
		t.Fatalf("position indicator missing: %q", out)
	}
	// wrap: down from the bottom lands back on the first row, window jumps
	for range 10 {
		p.Move(1)
	}
	if p.cursor != 0 {
		t.Fatalf("wrap-around broken: cursor = %d", p.cursor)
	}
	out = stripANSI(p.view(th, 100, 40))
	if !strings.Contains(out, "> session 00") || !strings.Contains(out, "1/30") {
		t.Fatalf("wrapped view wrong: %q", out)
	}
}

func TestSessionPanelHidesEmpty(t *testing.T) {
	a := newSlashApp(t)
	a.sess.Title = "real"
	a.sess.Add(core.Message{Role: core.RoleUser, Content: "hi"})
	a.saveSession()
	// an empty session on disk (fresh / just /clear'd)
	empty := core.NewSession("m")
	empty.Title = "cleared"
	if err := a.store.Save(empty); err != nil {
		t.Fatal(err)
	}

	a.openSessionPanel()
	if a.panel != panelSession || a.panelLst == nil {
		t.Fatal("session panel did not open")
	}
	if len(a.panelLst.items) != 1 || a.panelLst.items[0].title != "real" {
		t.Fatalf("empty session must not be listed: %+v", a.panelLst.items)
	}
	out := stripANSI(a.panelLst.view(a.th, 100, 40))
	if !strings.Contains(out, "d delete") || !strings.Contains(out, "r rename") {
		t.Fatalf("session panel hint missing delete/rename keys: %q", out)
	}
}

func TestSidebarHidesEmptySessions(t *testing.T) {
	a := newSlashApp(t)
	a.sess.Title = "current-real"
	a.sess.Add(core.Message{Role: core.RoleUser, Content: "hi"})
	a.saveSession()
	for _, name := range []string{"New chat", "你是谁"} {
		empty := core.NewSession("m")
		empty.Title = name
		if err := a.store.Save(empty); err != nil {
			t.Fatal(err)
		}
	}

	out := stripANSI(a.sidebarView(40, 20))
	if !strings.Contains(out, "current-real") {
		t.Fatalf("current session missing from the sidebar: %q", out)
	}
	if strings.Contains(out, "New chat") || strings.Contains(out, "你是谁") {
		t.Fatalf("empty sessions leaked into the sidebar: %q", out)
	}
}

func TestTranscriptPrefixCache(t *testing.T) {
	th := theme.Get("dark")
	r := newRenderer(80, th.GlamourStyle())
	msgs := []core.Message{
		{Role: core.RoleUser, Content: "hello"},
		{Role: core.RoleSystem, Content: "hidden system line"}, // blank block
		{Role: core.RoleAssistant, Content: "hi there"},
	}
	if got := r.transcript(th, msgs); got != transcriptNaive(r, th, msgs) {
		t.Fatalf("first pass mismatch:\n%q", got)
	}
	if r.folded != len(msgs) {
		t.Fatalf("prefix not folded: folded=%d", r.folded)
	}
	// append (the streaming case) → incremental join, same output
	msgs = append(msgs, core.Message{Role: core.RoleTool, ToolName: "t", Content: "res", CreatedAt: msgs[2].CreatedAt.Add(1)})
	if got := r.transcript(th, msgs); got != transcriptNaive(r, th, msgs) {
		t.Fatalf("append mismatch:\n%q", got)
	}
	// history shrank (trim / session switch) → prefix rebuild
	msgs = msgs[2:]
	if got := r.transcript(th, msgs); got != transcriptNaive(r, th, msgs) {
		t.Fatalf("shrink mismatch:\n%q", got)
	}
	// width change → rebuild
	r.width = 60
	if got := r.transcript(th, msgs); got != transcriptNaive(r, th, msgs) {
		t.Fatalf("width-change mismatch:\n%q", got)
	}
}

func BenchmarkLiveBlockFrames(b *testing.B) {
	th := theme.Get("dark")
	chunks := make([]string, 200) // ~60KB streamed reply, one chunk per frame
	for i := range chunks {
		chunks[i] = fmt.Sprintf("paragraph %d %s\n", i, strings.Repeat("text ", 30))
	}
	b.Run("incremental", func(b *testing.B) {
		for b.Loop() {
			r := newRenderer(80, th.GlamourStyle())
			buf := &deltaBuf{}
			for _, c := range chunks {
				buf.Add("", c)
				r.liveBlock(th, buf, "x")
			}
		}
	})
	b.Run("naive-rewrap", func(b *testing.B) {
		for b.Loop() {
			var all strings.Builder
			for _, c := range chunks {
				all.WriteString(c)
				_ = indent(wrapPlain(all.String(), 76)+"▊", 2) // the old per-frame path
			}
		}
	})
}

func BenchmarkTranscriptFrameAppend(b *testing.B) {
	th := theme.Get("dark")
	base := time.Now()
	msgs := make([]core.Message, 0, 300)
	for i := 0; i < 300; i++ {
		role := core.RoleUser
		if i%2 == 1 {
			role = core.RoleAssistant
		}
		msgs = append(msgs, core.Message{
			Role: role, Content: strings.Repeat("transcript line ", 50),
			CreatedAt: base.Add(time.Duration(i) * time.Second),
		})
	}
	// 100 frames, each with one more tool result appended (a streaming turn)
	frames := make([][]core.Message, 100)
	for f := range frames {
		frames[f] = append(msgs[:len(msgs):len(msgs)], core.Message{
			Role: core.RoleTool, ToolName: "bash", Content: strings.Repeat("line ", 60),
			CreatedAt: base.Add(time.Duration(f) * time.Second),
		})
	}
	r := newRenderer(80, th.GlamourStyle())
	r.transcript(th, msgs) // fold the 300-message base into the prefix
	b.ResetTimer()
	b.Run("cached", func(b *testing.B) {
		for b.Loop() {
			r.transcript(th, frames[b.N%100])
		}
	})
	b.Run("naive-join", func(b *testing.B) {
		for b.Loop() {
			m := frames[b.N%100]
			var out []string
			for i := range m {
				blk := r.block(th, i, &m[i])
				if !blankANSI(blk) {
					out = append(out, blk)
				}
			}
			_ = strings.Join(out, "\n\n")
		}
	})
}

// TestViewGeometrySweep renders the frame across many terminal geometries
// with hostile content (CJK titles, wide tables, unbreakable lines) and in
// the scrolled (follow=false) state — any frame taller than its terminal
// pushes the chrome off screen and leaves repaint ghosts.
func TestViewGeometrySweep(t *testing.T) {
	for _, geo := range [][2]int{{80, 20}, {100, 25}, {120, 30}, {130, 27}, {161, 38}, {200, 45}} {
		cols, rows := geo[0], geo[1]
		a := newSlashApp(t)
		a.resize(cols, rows)
		a.sess.Title = strings.Repeat("很长", 12) // 24 CJK cols — wider than the sidebar
		a.sess.Add(core.Message{Role: core.RoleUser, Content: strings.Repeat("宽", 200)})
		a.sess.Add(core.Message{Role: core.RoleAssistant, Content: "| 列一 | 列二 |\n|---|---|\n| " +
			strings.Repeat("表格", 40) + " | 值 |\n\n总结：这是一段很长的中文总结。"})
		a.sess.Add(core.Message{Role: core.RoleAssistant, Content: "### 📋 会话信息面板（左侧）\n内容行"})
		a.rend.Invalidate()
		a.refreshContent()

		// scrolled state
		a.follow = false
		a.vp.LineUp(10)

		out := a.View()
		if got := lipgloss.Height(out); got > rows {
			t.Fatalf("geometry %dx%d: frame height %d exceeds terminal", cols, rows, got)
		}
		// no rendered line may exceed the terminal width (would wrap physically)
		for i, ln := range strings.Split(out, "\n") {
			if w := lipgloss.Width(ln); w > cols {
				t.Fatalf("geometry %dx%d: line %d is %d cols (> %d): %.80q", cols, rows, i, w, cols, ln)
			}
		}

	}
}

// TestViewportLinesWithinWidth verifies the visible transcript lines never
// exceed the body width — a wider line makes JoinHorizontal shift the whole
// sidebar right (the "gap" beside the sessions box).
func TestViewportLinesWithinWidth(t *testing.T) {
	a := newSlashApp(t)
	a.resize(120, 30)
	a.sess.Add(core.Message{Role: core.RoleUser, Content: strings.Repeat("宽", 200)})
	a.sess.Add(core.Message{Role: core.RoleAssistant, Content: "| 列 | 值 |\n|---|---|\n| " +
		strings.Repeat("表格", 60) + " | x |\n\n### 📋 带emoji的标题 😊\n正文"})
	a.rend.Invalidate()
	a.refreshContent()
	a.follow = false
	a.vp.LineUp(5)

	for i, ln := range strings.Split(a.vp.View(), "\n") {
		if w := lipgloss.Width(ln); w > a.vp.Width {
			t.Fatalf("viewport line %d is %d cols, want ≤ %d: %.80q", i, w, a.vp.Width, ln)
		}
	}
}

func TestViewFrameHeightExact(t *testing.T) {
	a := newSlashApp(t)
	a.resize(120, 30)
	for i := 0; i < 60; i++ {
		a.sess.Add(core.Message{Role: core.RoleUser, Content: fmt.Sprintf("message line %d", i)})
		a.sess.Add(core.Message{Role: core.RoleAssistant, Content: strings.Repeat("reply ", 30)})
	}
	a.rend.Invalidate()
	a.refreshContent()

	if got := lipgloss.Height(a.View()); got != 30 {
		t.Fatalf("plain frame height = %d, want 30", got)
	}

	// a 200-char unbreakable CJK line must be clipped, not wrap the frame
	a.sess.Add(core.Message{Role: core.RoleUser, Content: strings.Repeat("宽", 200)})
	a.refreshContent()
	if got := lipgloss.Height(a.View()); got != 30 {
		t.Fatalf("wide-line frame height = %d, want 30", got)
	}

	// slash popup open — the footer grows, the transcript must shrink
	a.ta.SetValue("/c")
	if got := lipgloss.Height(a.View()); got != 30 {
		t.Fatalf("popup frame height = %d, want 30", got)
	}
}
