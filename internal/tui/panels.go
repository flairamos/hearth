package tui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"

	"hearth/internal/agent"
	"hearth/internal/theme"
)

type panelKind int

const (
	panelNone panelKind = iota
	panelModel
	panelSession
	panelPalette
	panelFile
	panelProvider
	panelHelp
	panelApproval
	panelConfirm
	panelRename
)

type item struct {
	title string
	desc  string
	data  any
}

// listPanel is the shared filterable list used by model/session/palette.
type listPanel struct {
	kind     panelKind
	title    string
	items    []item
	filtered []int
	query    string
	cursor   int
}

func newListPanel(kind panelKind, title string, items []item) *listPanel {
	p := &listPanel{kind: kind, title: title, items: items}
	p.refilter()
	return p
}

// refilter re-applies fuzzy subsequence filtering over items, clamping the
// cursor into range (backspace can shrink the result set below it — without
// the clamp the selection marker vanishes, like the old slash-popup bug).
func (p *listPanel) refilter() {
	q := strings.ToLower(p.query)
	p.filtered = p.filtered[:0]
	if q == "" {
		for i := range p.items {
			p.filtered = append(p.filtered, i)
		}
	} else {
		type sc struct {
			idx, score int
		}
		var scored []sc
		for i, it := range p.items {
			s := fuzzyScore(strings.ToLower(it.title), q)
			if s > 0 {
				scored = append(scored, sc{i, s})
			}
		}
		for i := 0; i < len(scored); i++ {
			for j := i; j > 0 && scored[j].score > scored[j-1].score; j-- {
				scored[j], scored[j-1] = scored[j-1], scored[j]
			}
		}
		for _, s := range scored {
			p.filtered = append(p.filtered, s.idx)
		}
	}
	if p.cursor >= len(p.filtered) {
		p.cursor = max(0, len(p.filtered)-1)
	}
}

// fuzzyScore ranks a title against a query: exact substring matches dominate
// (earlier position = better), loose subsequence matches are only a weak
// fallback so "main" doesn't rank "ollama"-containing paths above "main.go".
func fuzzyScore(s, q string) int {
	if q == "" {
		return 1
	}
	if idx := strings.Index(strings.ToLower(s), strings.ToLower(q)); idx >= 0 {
		return 1000 - idx
	}
	score, qi, streak := 0, 0, 0
	for _, r := range s {
		if qi < len([]rune(q)) && r == rune(q[qi]) {
			score += 1 + streak
			streak++
			qi++
		} else {
			streak = 0
		}
	}
	if qi < len([]rune(q)) {
		return 0
	}
	if score > 100 {
		score = 100 // subsequence matches never outrank substrings
	}
	return score
}

func (p *listPanel) Move(d int) {
	n := len(p.filtered)
	if n == 0 {
		return
	}
	p.cursor = (p.cursor + d + n) % n
}

func (p *listPanel) Selected() (item, bool) {
	if len(p.filtered) == 0 || p.cursor >= len(p.filtered) {
		return item{}, false
	}
	return p.items[p.filtered[p.cursor]], true
}

// view renders the panel box.
func (p *listPanel) view(th theme.Theme, width, height int) string {
	w := width - 8
	if w > 72 {
		w = 72
	}
	maxRows := height / 2
	if maxRows < 5 {
		maxRows = 5
	}

	var b strings.Builder
	b.WriteString(th.Accent.Render(p.title) + "\n")
	if p.kind != panelHelp {
		b.WriteString(th.Dim.Render("filter: "+p.query+" ▌") + "\n\n")
	}
	// scroll window
	start := 0
	if p.cursor >= maxRows {
		start = p.cursor - maxRows + 1
	}
	for row, idx := range p.filtered {
		if row < start || row >= start+maxRows {
			continue
		}
		it := p.items[idx]
		line := it.title
		if len([]rune(line)) > w-4 {
			line = string([]rune(line)[:w-4])
		}
		if row == p.cursor {
			b.WriteString(th.Selected.Render("> "+line) + "  " + th.Dim.Render(it.desc) + "\n")
		} else {
			b.WriteString("  " + line + "  " + th.Dim.Render(it.desc) + "\n")
		}
	}
	if len(p.filtered) == 0 {
		b.WriteString(th.Dim.Render("  (no matches)") + "\n")
	}
	hint := "↑/↓ select · enter confirm · esc close · type to filter"
	if n := len(p.filtered); n > 0 {
		if p.kind == panelSession {
			hint = fmt.Sprintf("↑/↓ %d/%d · enter open · d delete · r rename · esc close", p.cursor+1, n)
		} else {
			hint = fmt.Sprintf("↑/↓ %d/%d · enter confirm · esc close · type to filter", p.cursor+1, n)
		}
	}
	b.WriteString("\n" + th.Dim.Render(hint))
	return th.PanelBox.Render(b.String())
}

// confirmDialog asks a yes/no question.
type confirmDialog struct {
	prompt string
	detail string
	onYes  func()
}

func (c *confirmDialog) view(th theme.Theme, width int) string {
	var b strings.Builder
	b.WriteString(th.Warn.Render("? "+c.prompt) + "\n")
	if c.detail != "" {
		b.WriteString(th.Dim.Render(indent(c.detail, 2)) + "\n")
	}
	b.WriteString("\n" + th.Dim.Render("y = yes · n / esc = no"))
	return th.DialogBox.Render(b.String())
}

// approvalView renders the modal permission dialog. Every line is wrapped
// to the available width — the args JSON contains long escaped strings that
// would otherwise blow the box past the terminal edge.
func approvalView(th theme.Theme, req *agent.ApprovalRequest, width int) string {
	maxW := width - 8 // border(2) + padding(2) + breathing room
	if maxW > 96 {
		maxW = 96
	}
	if maxW < 40 {
		maxW = 40
	}
	var b strings.Builder
	cat := string(req.Category)
	icon := "✎"
	if req.Category == "exec" {
		icon = "❯_"
	}
	b.WriteString(wrapPlain(th.Warn.Render(fmt.Sprintf("⚠ tool needs approval: %s %s", icon, req.Tool))+" "+
		th.Dim.Render("["+cat+"]"), maxW-4) + "\n")
	b.WriteString(indent(wrapPlain(req.Summary, maxW-4), 2) + "\n")
	if req.Detail != "" && req.Detail != "{}" {
		for i, ln := range strings.Split(req.Detail, "\n") {
			if i >= 12 {
				break
			}
			b.WriteString(th.Dim.Render(indent(wrapPlain(strings.TrimRight(ln, "\r"), maxW-4), 2)) + "\n")
		}
	}
	b.WriteString("\n" + wrapPlain(
		th.Accent.Render("[a]")+th.Dim.Render(" allow once   ")+
			th.Accent.Render("[s]")+th.Dim.Render(" always this session   ")+
			th.Err.Render("[d]")+th.Dim.Render(" deny"), maxW-4))
	return th.DialogBox.Render(b.String())
}

// renamePanel edits a session title inline.
type renamePanel struct {
	input  textinput.Model
	sessID string
}

func newRenamePanel(id, current string) *renamePanel {
	ti := textinput.New()
	ti.SetValue(current)
	ti.Focus()
	ti.Width = 50
	return &renamePanel{input: ti, sessID: id}
}

func (r *renamePanel) view(th theme.Theme, width int) string {
	return th.PanelBox.Render(th.Accent.Render("rename session") + "\n" + r.input.View() + "\n" +
		th.Dim.Render("enter confirm · esc cancel"))
}

func renameUpdate(r *renamePanel, msg tea.Msg) (*renamePanel, tea.Cmd) {
	var cmd tea.Cmd
	r.input, cmd = r.input.Update(msg)
	return r, cmd
}
