package tui

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"hearth/internal/agent"
	"hearth/internal/provider"
)

// refreshContent re-renders the transcript into the viewport. Reads a
// snapshot so concurrent agent mutations can't tear the render; block
// renders are cached per message and the joined transcript keeps an
// append-only prefix cache, so a streaming frame only joins the newest
// blocks instead of re-joining the whole session.
func (a *App) refreshContent() {
	bodyW := a.bodyWidth()
	a.rend.width = bodyW - 2

	content := a.rend.transcript(a.th, a.sess.Snapshot())
	if a.busy() && a.gen != nil {
		var tail []string
		if tb := a.rend.thinkingBlock(a.th, a.gen.buf); tb != "" {
			tail = append(tail, tb)
		}
		tail = append(tail, a.rend.liveBlock(a.th, a.gen.buf, a.gen.phase))
		live := strings.Join(tail, "\n\n")
		if content != "" {
			content = content + "\n\n" + live
		} else {
			content = live
		}
	}
	if content == "" {
		content = a.emptyState()
	}
	// consecutive frames often produce identical text (tool phases between
	// deltas) — skip the viewport line re-split entirely
	if content != a.lastBody {
		a.lastBody = content
		a.vp.SetContent(content)
	}
	if a.follow {
		a.vp.GotoBottom()
	}
}

// emptyState is the welcome screen shown when the session has no messages.
// The app title lives in the header already — no duplicate branding here.
// NOTE: never pass "\n" inside lipgloss Render() — trailing newlines render
// as ghost padding lines (width = widest line) that break the layout.
func (a *App) emptyState() string {
	mode := "chat"
	if a.sess.AgentMode {
		mode = "agent (tools on)"
	}
	lines := []string{
		a.th.Dim.Render(fmt.Sprintf("model: %s · mode: %s · api: %s", a.modelLabel(), mode,
			provider.StyleFor(a.cfg.OllamaURL, a.cfg.APIStyle))),
		a.th.Dim.Render("workdir: " + a.workDir),
		"",
		"Type a message to start, or try:",
		a.th.Dim.Render("  /model     pick a model"),
		a.th.Dim.Render("  /agent     enable tool calling (agent mode)"),
		a.th.Dim.Render("  ctrl+k     command palette"),
		a.th.Dim.Render("  ?          help"),
	}
	if len(a.models) == 0 {
		lines = append(lines, "", a.th.Warn.Render("no local models found — run: ollama pull qwen3.5:4b"))
	}
	return strings.Join(lines, "\n")
}

func (a *App) modelLabel() string {
	if a.sess.Model == "" {
		return "(none)"
	}
	return a.sess.Model
}

// stripANSI removes SGR sequences for emptiness checks.
func stripANSI(s string) string {
	var b strings.Builder
	inEsc := false
	for _, r := range s {
		if inEsc {
			if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') {
				inEsc = false
			}
			continue
		}
		if r == 0x1b {
			inEsc = true
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// sidebarWidth is the outer width of the right-hand sidebar (0 when hidden).
// The header/status styles add one column of padding on each side, so the
// main column is bodyWidth()+2 cells wide; the sidebar takes exactly the
// rest of the terminal.
func (a *App) sidebarWidth() int {
	if a.sidebar && a.width >= 90 {
		return 25
	}
	return 0
}

// bodyWidth is the content width of the main column (excluding the header's
// one-column padding on each side).
func (a *App) bodyWidth() int {
	if side := a.sidebarWidth(); side > 0 {
		return a.width - side - 2
	}
	return a.width
}

// View renders the whole interface.
func (a *App) View() string {
	if a.width == 0 {
		return "starting…"
	}
	// startup failure card (PRD 4.1.1)
	if !a.connOK && a.connErr != nil && len(a.models) == 0 {
		return a.errorCard()
	}
	if timeSince(a.toastAt) > 4e9 {
		a.toast = ""
	}

	bodyW := a.bodyWidth()
	sideW := a.sidebarWidth()

	header := a.headerView(bodyW)
	status := a.statusView(bodyW)
	footer := a.footerView(bodyW)
	// the transcript area absorbs whatever height is left: measured footer
	// height (slash popup, toasts) varies — a fixed formula would make the
	// frame taller than the terminal and scroll the chrome off screen
	bodyH := max(3, a.height-lipgloss.Height(header)-lipgloss.Height(status)-lipgloss.Height(footer))

	content := a.bodyView(bodyW, bodyH)

	// panels replace the transcript area, centered (clean, no compositing)
	switch a.panel {
	case panelModel, panelSession, panelPalette, panelFile:
		content = lipgloss.Place(bodyW, bodyH, lipgloss.Center, lipgloss.Center,
			a.panelLst.view(a.th, a.width, a.height))
	case panelHelp:
		content = lipgloss.Place(bodyW, bodyH, lipgloss.Center, lipgloss.Center, a.helpView())
	case panelApproval:
		if a.approval != nil {
			content = lipgloss.Place(bodyW, bodyH, lipgloss.Center, lipgloss.Center,
				approvalView(a.th, a.approval, bodyW))
		}
	case panelConfirm:
		if a.confirm != nil {
			content = lipgloss.Place(bodyW, bodyH, lipgloss.Center, lipgloss.Center,
				a.confirm.view(a.th, a.width))
		}
	case panelRename:
		if a.rename != nil {
			content = lipgloss.Place(bodyW, bodyH, lipgloss.Center, lipgloss.Center,
				a.rename.view(a.th, a.width))
		}
	}

	main := lipgloss.JoinVertical(lipgloss.Left, header, content, status, footer)
	// pin the main column width before joining: one oversized line (e.g. the
	// slash popup) must not push the sidebar sideways
	mainW := a.width
	if sideW > 0 {
		mainW = bodyW + 2
	}
	main = hardClip(main, mainW)
	if sideW > 0 {
		main = lipgloss.JoinHorizontal(lipgloss.Top, main, a.sidebarView(sideW, a.height))
	}
	main = hardClip(main, a.width)
	return main
}

func (a *App) headerView(w int) string {
	// mode badge is always visible (CHAT/AGENT in the agent amber) so the
	// toggle state reads at a glance
	mode := "[CHAT]"
	if a.sess.AgentMode {
		mode = "[AGENT]"
	}
	badges := " " + a.th.Warn.Render(mode)
	switch a.perms.Mode() {
	case agent.ModeFull:
		badges += " " + a.th.Err.Render("[full]")
	case agent.ModeRisky:
		badges += " " + a.th.Warn.Render("[risky]")
	default:
		badges += " " + a.th.Dim.Render("[ask]")
	}
	if a.thinkEnabled() {
		badges += " " + a.th.Dim.Render("[think]")
	}
	left := a.th.Accent.Render("◆ hearth") +
		a.th.Dim.Render("  "+a.modelLabel()) +
		a.th.Dim.Render("  ·  "+truncateRunes(a.sess.Title, max(0, w-44)))
	// ctx usage at the right edge (local-agent style), before the conn dot
	numCtx := a.sess.NumCtx(a.cfg.Context.NumCtx)
	ctxPart := a.th.Dim.Render(fmt.Sprintf("ctx %s/%d ", humanTok(a.sess.UsedTokens()), numCtx))
	right := ""
	if a.connOK {
		right = a.th.OK.Render("● ollama")
	} else {
		right = a.th.Err.Render("○ offline")
	}
	gap := w - lipgloss.Width(stripANSI(left)) - lipgloss.Width(stripANSI(right)) -
		lipgloss.Width(stripANSI(badges)) - lipgloss.Width(stripANSI(ctxPart))
	if gap < 1 {
		gap = 1
	}
	return a.th.StatusBar.Render(left + badges + strings.Repeat(" ", gap) + ctxPart + right)
}

func (a *App) bodyView(w, h int) string {
	a.vp.Width = w
	a.vp.Height = h
	return hardClip(a.vp.View(), w)
}

// hardClip caps every physical line at width (ANSI-aware). Any line wider
// than the screen would wrap in the terminal and push the header/status/
// input frame off screen — transcript blocks pre-wrap themselves, so this
// is only a safety net for stray oversized lines.
func hardClip(frame string, width int) string {
	lines := strings.Split(frame, "\n")
	for i, ln := range lines {
		if lipgloss.Width(ln) > width {
			lines[i] = ansi.Truncate(ln, width, "")
		}
	}
	return strings.Join(lines, "\n")
}

func (a *App) statusView(w int) string {
	if a.compacting && a.gen == nil {
		return a.th.StatusBar.Render(" ctx " + humanTok(a.sess.UsedTokens()) + "  " + spinnerView() + " " + a.th.Accent.Render("compacting context…"))
	}
	// token usage bar (PRD 4.2.2)
	numCtx := a.sess.NumCtx(a.cfg.Context.NumCtx)
	used := a.sess.UsedTokens()
	pct := 0
	if numCtx > 0 {
		pct = used * 100 / numCtx
	}
	barW := 10
	filled := pct * barW / 100
	bar := strings.Repeat("▮", filled) + strings.Repeat("▯", barW-filled)
	barStyle := a.th.Dim
	if pct > 95 {
		barStyle = a.th.Err
	} else if pct > 80 {
		barStyle = a.th.Warn
	}
	left := fmt.Sprintf("ctx %s/%d %s %d%%", humanTok(used), numCtx, barStyle.Render(bar), pct)

	// live/final throughput (local-agent style). The live rate divides by
	// active generation time only — tool execution between steps produces
	// zero tokens and would otherwise dilute the number into single digits.
	throughput := ""
	if a.gen != nil {
		sp := spinnerView()
		phase := a.gen.phase
		if a.gen.genNs > 400*time.Millisecond && a.gen.outTokEst > 0 {
			phase = fmt.Sprintf("%s · ~%.1f tok/s", phase, float64(a.gen.outTokEst)/a.gen.genNs.Seconds())
		}
		throughput = "  " + sp + " " + a.th.Accent.Render(phase)
	} else if a.lastStats.failed {
		why := a.lastError
		if why == "" {
			why = "no final answer recorded"
		}
		throughput = "  " + a.th.Err.Render("✗ task ended with an error") +
			a.th.Dim.Render(" · "+truncateRunes(why, 60))
	} else if a.lastStats.interrupted {
		throughput = "  " + a.th.Dim.Render("⏸ last turn interrupted")
	} else if a.lastStats.tps > 0 {
		throughput = "  " + a.th.Dim.Render(fmt.Sprintf("⚡ %.1f tok/s · last %.1fs · %d tok",
			a.lastStats.tps, a.lastStats.dur.Seconds(), a.lastStats.toks))
	} else if !a.follow {
		throughput = "  " + a.th.Warn.Render("↓ newer messages (End)")
	} else if timeSince(a.quitArmAt) < 3e9 && a.quitArm {
		throughput = "  " + a.th.Err.Render("press ctrl+c again to quit")
	}

	right := ""
	if len(a.pendingSends) > 0 {
		right += fmt.Sprintf("queued:%d ", len(a.pendingSends))
	}
	if len(a.pendingImages) > 0 {
		right += fmt.Sprintf("img:%d ", len(a.pendingImages))
	}
	keyHints := a.th.Dim.Render("^K palette · ^P sessions · alt+a agent · ^G access · ^Y copy · ? help")
	body := left + throughput
	rightW := lipgloss.Width(stripANSI(right))
	hintW := lipgloss.Width(stripANSI(keyHints))
	avail := w - rightW - hintW - 2
	if lipgloss.Width(stripANSI(body)) > avail {
		keyHints = ""
		hintW = 0
		avail = w - rightW - 2
	}
	pad := avail - lipgloss.Width(stripANSI(body))
	if pad < 1 {
		pad = 1
	}
	return a.th.StatusBar.Render(truncateRunes(body, avail) + strings.Repeat(" ", pad) + right + keyHints)
}

func (a *App) footerView(w int) string {
	box := a.th.InputBox.Render(a.ta.View())
	// reserve the toast line even when empty so the layout never shifts
	box = a.toastView(w) + "\n" + box
	if strings.HasPrefix(strings.TrimSpace(a.ta.Value()), "/") {
		box = a.slashPopup(w) + "\n" + box
	}
	return box
}

func (a *App) toastView(w int) string {
	if a.toast == "" {
		return ""
	}
	return a.th.Warn.Render(" " + truncateRunes(a.toast, w-4))
}

func (a *App) slashPopup(w int) string {
	if a.slashHidden {
		return ""
	}
	v := strings.TrimSpace(a.ta.Value())
	if !strings.HasPrefix(v, "/") {
		return ""
	}
	word, _ := splitSlash(v)
	matches := a.slashMatches(word)
	if len(matches) == 0 {
		return a.th.Dim.Render(" no matching command")
	}
	if a.slashCursor >= len(matches) {
		a.slashCursor = 0
	}
	// the popup shows a scrolling window of slashWindow rows; shift the
	// start so the selected row always stays visible
	const slashWindow = 8
	start := 0
	if a.slashCursor >= slashWindow {
		start = a.slashCursor - slashWindow + 1
	}
	end := min(len(matches), start+slashWindow)
	var b strings.Builder
	for i := start; i < end; i++ {
		c := matches[i]
		mark := "  "
		style := a.th.Dim
		if i == a.slashCursor {
			mark = "> "
			style = a.th.Selected
		}
		usage := ""
		if c.usage != "" {
			usage = " " + c.usage
		}
		b.WriteString(style.Render(fmt.Sprintf("%s/%s%s", mark, c.name, usage)) + "  " + a.th.Dim.Render(c.desc) + "\n")
	}
	b.WriteString(a.th.Dim.Render(fmt.Sprintf("  ↑↓ %d/%d · tab complete · enter run · esc dismiss",
		a.slashCursor+1, len(matches))))
	return a.th.PanelBox.Render(strings.TrimRight(b.String(), "\n"))
}

// sidebarView renders the right-hand panel. w is the full panel width and h is
// the height budget: the box is capped to h rows, because an over-tall sidebar
// makes the composed frame taller than the terminal - bubbletea then scrolls
// the header and the box's top border off screen (the missing border bug).
// Every line is styled WITHOUT embedded newlines (lipgloss renders trailing
// newlines as ghost padding lines) and truncated to the box inner width; the
// box gets a fixed Width so JoinHorizontal stays stable.
func (a *App) sidebarView(w, h int) string {
	if w < 12 {
		w = 12
	}
	inner := w - 4 // borders + padding
	// display-width aware (ANSI + CJK): rune counting lets wide chars
	// overflow the sidebar box and wrap
	fit := func(s string, width int) string {
		if width < 1 {
			return ""
		}
		return ansi.Truncate(s, width, "…")
	}

	// fixed tail: separator + info block, kept visible when the session list
	// has to be shortened
	tail := []string{"", a.th.Accent.Render(fit(" info", inner))}
	mode := "chat"
	if a.sess.AgentMode {
		mode = "agent"
	}
	tail = append(tail, a.th.Dim.Render(fit(" mode:   "+mode, inner)))
	if a.sess.AgentMode {
		if ap := a.perms.ApprovedSummary(); ap != "" {
			tail = append(tail, a.th.Dim.Render(fit(" allow:  "+ap, inner)))
		}
		tail = append(tail, a.th.Dim.Render(fit(fmt.Sprintf(" steps:  ≤%d", a.cfg.Agent.MaxSteps), inner)))
	}
	tail = append(tail, a.th.Dim.Render(fit(" api:    "+provider.StyleFor(a.cfg.OllamaURL, a.cfg.APIStyle), inner)))
	if a.memory != nil && a.memory.Count() > 0 {
		tail = append(tail, a.th.Dim.Render(fit(fmt.Sprintf(" memory: %d facts", a.memory.Count()), inner)))
	}
	tail = append(tail, a.th.Dim.Render(fit(" theme:  "+a.cfg.Theme, inner)))
	tail = append(tail, a.th.Dim.Render(fit(" dir:    "+a.workDir, inner)))

	lines := []string{a.th.Accent.Render(fit(" sessions", inner))}
	if list, err := a.store.List(); err == nil {
		// same rule as the sessions panel: empty sessions (fresh / cleared)
		// are not listed - except the current one, so you can see where you are
		visible := list[:0]
		for _, sm := range list {
			if sm.MsgCount == 0 && sm.ID != a.sess.ID {
				continue
			}
			visible = append(visible, sm)
		}
		// row budget inside the border: title and info tail are fixed, and
		// a truncated list spends one row on the more hint
		innerH := h - 2
		if innerH < 1 {
			innerH = 1
		}
		budget := innerH - len(lines) - len(tail)
		if budget < 0 {
			budget = 0
		}
		shown := budget
		if shown > 12 {
			shown = 12 // keep the sidebar from listing the whole history
		}
		truncated := len(visible) > shown
		if truncated && shown > 0 {
			shown--
		}
		for i, sm := range visible {
			if i >= shown {
				break
			}
			prefix, style := "  ", a.th.Dim
			if sm.ID == a.sess.ID {
				prefix, style = "> ", a.th.Selected
			}
			// fit the title AFTER the prefix: fitting it to the full inner
			// width first made the selected row two cells wider than the box
			lines = append(lines, style.Render(prefix+fit(sm.Title, inner-2)))
		}
		if truncated {
			lines = append(lines, a.th.Dim.Render(fit(fmt.Sprintf(" …+%d more (ctrl+p)", len(visible)-shown), inner)))
		}
	}
	lines = append(lines, tail...)
	// last-resort clamp: never emit a box taller than the frame
	if maxContent := max(1, h-2); len(lines) > maxContent {
		lines = lines[:maxContent]
	}
	// Width() excludes the border: the box must render w cells in total
	return a.th.PanelBox.Width(w - 2).Render(strings.Join(lines, "\n"))
}

func humanTok(n int) string {
	if n >= 1024 {
		return fmt.Sprintf("%.1fk", float64(n)/1024)
	}
	return fmt.Sprintf("%d", n)
}

func (a *App) errorCard() string {
	lines := []string{
		"",
		a.th.Err.Render("  ✖ cannot reach ollama"),
		"",
		"  " + a.connErr.Error(),
		"",
		a.th.Dim.Render("  check:"),
		a.th.Dim.Render("    1. is ollama running?   → ollama serve"),
		a.th.Dim.Render("    2. correct address?     → " + a.cfg.OllamaURL),
		a.th.Dim.Render("    3. test:                → curl " + a.cfg.OllamaURL + "/api/tags"),
		"",
		a.th.Accent.Render("  [r]") + " retry   " + a.th.Accent.Render("[q]") + " quit",
	}
	return lipgloss.Place(a.width, a.height, lipgloss.Center, lipgloss.Center,
		a.th.DialogBox.Render(strings.Join(lines, "\n")))
}

func (a *App) helpView() string {
	var b strings.Builder
	b.WriteString(a.th.Accent.Render("keyboard") + "\n\n")
	for _, kv := range a.km.Sorted() {
		b.WriteString(fmt.Sprintf("  %-16s %s\n", a.th.Selected.Render(kv[1]), kv[0]))
	}
	b.WriteString("\n  pgup/pgdown      scroll · end bottom · mouse wheel works\n\n")
	b.WriteString(a.th.Accent.Render("commands") + "\n\n")
	for _, c := range a.commandList() {
		usage := c.usage
		if usage != "" {
			usage = " " + usage
		}
		b.WriteString(fmt.Sprintf("  /%-12s %s\n", c.name+usage, a.th.Dim.Render(c.desc)))
	}
	b.WriteString("\n" + a.th.Dim.Render("esc/q close"))
	a.helpVp.SetContent(b.String())
	return a.th.PanelBox.Render(a.helpVp.View())
}

// spinnerView returns the current spinner frame.
func spinnerView() string {
	if spinnerFrame < 0 || spinnerFrame >= len(spinnerFrames) {
		spinnerFrame = 0
	}
	return spinnerFrames[spinnerFrame]
}

var spinnerFrames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}
var spinnerFrame = 0
