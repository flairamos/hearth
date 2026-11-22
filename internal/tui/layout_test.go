package tui

import (
	"fmt"
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"

	"hearth/internal/core"
)

// layoutApp builds an app with n extra sessions on disk and a current session
// holding one message, so both the transcript and the sidebar have content.
func layoutApp(t *testing.T, n int, title string) *App {
	t.Helper()
	a := newSlashApp(t)
	a.sess.Title = title
	a.sess.AgentMode = true
	a.sess.Add(core.Message{Role: core.RoleUser, Content: "hi"})
	a.saveSession()
	for i := 0; i < n; i++ {
		s := core.NewSession("m")
		s.Title = fmt.Sprintf("session %02d", i)
		s.Add(core.Message{Role: core.RoleUser, Content: "hi"})
		if err := a.store.Save(s); err != nil {
			t.Fatalf("save session %d: %v", i, err)
		}
	}
	return a
}

// frameBorders locates the sidebar box and verifies it is a closed rectangle:
// both top/bottom corner pairs exist at the expected column, every interior row
// starts its left border at that column and ends with the right border.
func frameBorders(t *testing.T, lines []string, width int) {
	t.Helper()
	col := func(plain string, idx int) int { return lipgloss.Width(plain[:idx]) }
	// hasAt reports whether ch occurs in plain at display column want.
	hasAt := func(plain, ch string, want int) bool {
		for off := 0; off < len(plain); {
			i := strings.Index(plain[off:], ch)
			if i < 0 {
				return false
			}
			i += off
			if col(plain, i) == want {
				return true
			}
			off = i + len(ch)
		}
		return false
	}
	want := width - 25
	top, bottom := -1, -1
	for i, ln := range lines {
		plain := stripANSI(ln)
		if hasAt(plain, "╭", want) && strings.HasSuffix(plain, "╮") {
			top = i
		}
		if hasAt(plain, "╰", want) && strings.HasSuffix(plain, "╯") {
			bottom = i
		}
	}
	if top < 0 {
		t.Fatalf("sidebar top border missing (expected corner at column %d)", want)
	}
	if bottom < 0 {
		t.Fatalf("sidebar bottom border missing (expected corner at column %d)", want)
	}
	if bottom <= top {
		t.Fatalf("sidebar box has no interior (top=%d bottom=%d)", top, bottom)
	}
	for i := top + 1; i < bottom; i++ {
		plain := stripANSI(lines[i])
		if !hasAt(plain, "│", want) {
			t.Fatalf("row %d: sidebar left border missing at column %d", i, want)
		}
		if !strings.HasSuffix(plain, "│") {
			t.Fatalf("row %d: sidebar right border missing", i)
		}
	}
}

// checkFrame asserts the composed view is exactly terminal-sized and the
// sidebar box is closed.
func checkFrame(t *testing.T, a *App) {
	t.Helper()
	lines := strings.Split(a.View(), "\n")
	if len(lines) != a.height {
		t.Fatalf("%dx%d: frame height = %d, want %d", a.width, a.height, len(lines), a.height)
	}
	for i, ln := range lines {
		if got := lipgloss.Width(ln); got != a.width {
			t.Fatalf("%dx%d: line %d width = %d, want %d", a.width, a.height, i, got, a.width)
		}
	}
	if a.sidebar && a.width >= 90 {
		frameBorders(t, lines, a.width)
	}
}

func TestSidebarFrameFitsTerminal(t *testing.T) {
	for _, sz := range []struct{ w, h int }{{90, 16}, {100, 20}, {120, 30}, {160, 40}} {
		for _, n := range []int{0, 1, 12, 20} {
			a := layoutApp(t, n, "current session")
			a.resize(sz.w, sz.h)
			checkFrame(t, a)
			// scrolling to the newest message must not change the frame shape
			a.follow = true
			a.vp.GotoBottom()
			a.refreshContent()
			checkFrame(t, a)
			if plain := stripANSI(a.View()); !strings.Contains(plain, " dir:") {
				t.Fatalf("%dx%d n=%d: sidebar info block lost: %q", sz.w, sz.h, n, plain)
			}
		}
	}
}

func TestSidebarLongTitleKeepsBorder(t *testing.T) {
	a := layoutApp(t, 3, strings.Repeat("很长的会话标题", 6))
	a.resize(110, 26)
	checkFrame(t, a)
}

func TestSidebarHiddenBelowThreshold(t *testing.T) {
	a := layoutApp(t, 2, "current session")
	a.resize(80, 24)
	checkFrame(t, a)
}
