package tui

import (
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"hearth/internal/agent"
	"hearth/internal/core"
)

// handleKey routes key presses by UI state (panels first, then globals,
// then the input box).
func (a *App) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	key := msg.String()

	// an armed quit (/quit, ctrl+c double-press) quits on ANY next key —
	// checked before the ctrl+c arm dance so it can't be shadowed by it.
	// The session is saved first: a mid-turn quit must not lose the
	// messages accumulated since turn start.
	if a.quitNow {
		return a, a.doQuit()
	}

	// quit is always available (PRD: 二次按下退出)
	if a.km.Matches("quit", key) {
		if a.panel == panelApproval && a.approval != nil {
			// modal dialog: ctrl+c denies — interrupting the run behind the
			// dialog would leave it dangling with no way to answer
			a.approval.Decide(agentDecisionDeny)
			a.closeApproval()
			return a, nil
		}
		if a.busy() {
			a.interrupt()
			return a, nil
		}
		if a.quitArm && timeSince(a.quitArmAt) < 3e9 {
			return a, a.doQuit()
		}
		a.quitArm = true
		a.quitArmAt = now()
		return a, nil
	}

	// panels consume keys first
	switch a.panel {
	case panelApproval:
		return a.approvalKey(key)
	case panelConfirm:
		return a.confirmKey(key)
	case panelRename:
		return a.renameKey(msg)
	case panelHelp:
		return a.helpKey(key)
	case panelModel, panelSession, panelPalette, panelFile:
		return a.listKey(msg, key)
	}

	// global bindings
	switch {
	case a.km.Matches("palette", key):
		a.openPalette()
		return a, nil
	case a.km.Matches("sessions", key):
		a.openSessionPanel()
		return a, nil
	case a.km.Matches("models", key):
		a.openModelPanel()
		return a, nil
	case a.km.Matches("new_session", key):
		a.newSession()
		return a, nil
	case a.km.Matches("toggle_sidebar", key):
		a.sidebar = !a.sidebar
		a.resize(a.width, a.height)
		return a, nil
	case a.km.Matches("toggle_agent", key), key == "shift+tab": // local-agent style mode switch
		a.setAgentMode(!a.sess.AgentMode)
		return a, nil
	case a.km.Matches("access_mode", key):
		a.cycleAccessMode()
		return a, nil
	case a.km.Matches("copy_last", key):
		a.copyLastReply()
		return a, nil
	case a.km.Matches("toggle_think", key):
		a.setThink(!a.thinkEnabled())
		return a, nil
	case a.km.Matches("clear_screen", key):
		a.follow = true
		a.refreshContent()
		return a, nil
	case a.km.Matches("help", key):
		if a.ta.Value() == "" {
			a.panel = panelHelp
			a.helpVp.GotoTop()
			return a, nil
		}
	case a.km.Matches("interrupt", key):
		if a.busy() {
			a.interrupt()
			return a, nil
		}
	}

	// scrolling
	switch key {
	case "pgup":
		a.follow = false
		a.vp.HalfPageUp()
		return a, nil
	case "pgdown":
		a.vp.HalfPageDown()
		if a.vp.AtBottom() {
			a.follow = true
		}
		return a, nil
	case "end":
		a.follow = true
		a.vp.GotoBottom()
		return a, nil
	}

	// slash command popup: ↑/↓ select, tab completes, esc dismisses —
	// takes priority over input-history/caret navigation while visible
	if a.slashVisible() {
		switch key {
		case "up":
			a.slashMove(-1)
			return a, nil
		case "down":
			a.slashMove(1)
			return a, nil
		case "tab":
			a.slashComplete()
			return a, nil
		case "esc":
			a.slashHidden = true
			return a, nil
		}
	}

	// input box
	if key == "enter" {
		v := a.ta.Value()
		if strings.HasSuffix(v, "\\") { // backslash-continue fallback (PRD 4.1.4)
			a.ta.SetValue(strings.TrimSuffix(v, "\\") + "\n")
			return a, nil
		}
		cmd := a.submitInput()
		if a.execCmd != nil {
			cmd = a.execCmd
			a.execCmd = nil
		}
		return a, cmd
	}

	// @ opens the file picker (local-agent/opencode style)
	if key == "@" {
		// seed the "@" into the input (opencode style) — selecting a file
		// then completes it to @path; esc leaves the bare @ for editing
		a.openFilePanel()
		a.ta.InsertString("@")
		return a, nil
	}

	// ↑/↓ navigate input history when the cursor sits at the top/bottom
	// line of a multi-line draft, otherwise move the caret
	switch key {
	case "up":
		if a.ta.Line() == 0 && len(a.history) > 0 {
			a.historyPrev()
			return a, nil
		}
	case "down":
		if a.ta.Line() >= a.ta.LineCount()-1 {
			if a.historyNext() {
				return a, nil
			}
		}
	}

	var cmd tea.Cmd
	a.ta, cmd = a.ta.Update(msg)
	return a, cmd
}

// doQuit persists the session before exiting — a quit mid-turn must not
// lose the tool exchanges accumulated since turn start (they live in
// memory until onDone normally).
func (a *App) doQuit() tea.Cmd {
	a.saveSession()
	a.quitNow = true
	return tea.Quit
}

// historyPrev recalls the previous sent message.
func (a *App) historyPrev() {
	if len(a.history) == 0 {
		return
	}
	if a.histPos == -1 {
		a.histPos = len(a.history) - 1
	} else if a.histPos > 0 {
		a.histPos--
	}
	a.ta.SetValue(a.history[a.histPos])
	a.ta.CursorEnd()
}

// historyNext returns true when it consumed the key (end of history = live).
func (a *App) historyNext() bool {
	if a.histPos == -1 {
		return false
	}
	a.histPos++
	if a.histPos >= len(a.history) {
		a.histPos = -1
		a.ta.SetValue("")
		return true
	}
	a.ta.SetValue(a.history[a.histPos])
	a.ta.CursorEnd()
	return true
}

// cycleAccessMode rotates ask → risky → full (ctrl+g).
func (a *App) cycleAccessMode() {
	next := map[string]string{agent.ModeAsk: agent.ModeRisky, agent.ModeRisky: agent.ModeFull, agent.ModeFull: agent.ModeAsk}[a.perms.Mode()]
	a.perms.SetMode(next)
	a.cfg.Agent.Permissions = next
	_ = a.cfg.Save()
	a.toastf("access: %s — %s", next, agent.ModeDesc(next))
}

// copyLastReply puts the last assistant message on the clipboard (^Y).
func (a *App) copyLastReply() {
	snap := a.sess.Snapshot()
	for i := len(snap) - 1; i >= 0; i-- {
		if snap[i].Role == core.RoleAssistant && strings.TrimSpace(snap[i].Content) != "" {
			if ok, tool := copyToClipboard(snap[i].Content); ok {
				a.toastf("copied via %s (%d chars)", tool, len(snap[i].Content))
			} else {
				a.toastf("no clipboard tool found (clip/wl-copy/xclip/pbcopy)")
			}
			return
		}
	}
	a.toastf("nothing to copy yet")
}

// submitInput handles Enter: slash commands, queueing while busy, sending.
func (a *App) submitInput() tea.Cmd {
	v := strings.TrimSpace(a.ta.Value())

	// slash command completion popup visible?
	if strings.HasPrefix(v, "/") {
		word, rest := splitSlash(v)
		matches := a.slashMatches(word)
		if len(matches) > 0 {
			c := matches[min(a.slashCursor, len(matches)-1)]
			if rest != "" || word == c.name {
				a.ta.SetValue("")
				a.slashCursor = 0
				if err := c.run(a, rest); err != nil {
					a.toastf("%v", err)
				}
				a.refreshContent()
				return nil
			}
			// complete the name only
			a.ta.SetValue("/" + c.name + " ")
			return nil
		}
		a.toastf("unknown command: %s", word)
		a.ta.SetValue("")
		return nil
	}

	if v == "" && len(a.pendingImages) == 0 {
		return nil
	}
	if a.busy() {
		a.pendingSends = append(a.pendingSends, v)
		a.ta.SetValue("")
		a.toastf("queued (%d) — will send after current reply", len(a.pendingSends))
		return nil
	}
	images := a.pendingImages
	a.pendingImages = nil
	a.ta.SetValue("")

	// @file references: snapshot content now, expand at API-send time
	var refs []core.FileRef
	if strings.Contains(v, "@") {
		var missing []string
		refs, missing = CollectRefs(v, a.workDir)
		if len(missing) > 0 {
			a.toastf("missing: %s", strings.Join(missing, ", "))
		}
	}
	if len(images) > 0 {
		if v == "" {
			v = "请分析这张图片"
		}
		if !a.hasVision() {
			a.toastf("note: model %s advertises no vision capability — the image may be ignored", a.sess.Model)
		}
	}

	// input history for ↑/↓
	if strings.TrimSpace(v) != "" {
		a.history = append(a.history, v)
		if len(a.history) > 100 {
			a.history = a.history[1:]
		}
	}
	a.histPos = -1

	a.startTurn(v, images, refs)
	return nil
}

func splitSlash(v string) (word, rest string) {
	v = v[1:]
	if i := strings.IndexByte(v, ' '); i >= 0 {
		return v[:i], strings.TrimSpace(v[i+1:])
	}
	return v, ""
}

// ---- panel key handlers ----

func (a *App) listKey(msg tea.KeyMsg, key string) (tea.Model, tea.Cmd) {
	p := a.panelLst
	if p == nil {
		a.panel = panelNone
		return a, nil
	}
	switch key {
	case "esc", "ctrl+c":
		a.panel = panelNone
		return a, nil
	case "up", "ctrl+p":
		p.Move(-1)
		return a, nil
	case "down", "ctrl+n":
		p.Move(1)
		return a, nil
	case "enter":
		it, ok := p.Selected()
		if !ok {
			return a, nil
		}
		a.panel = panelNone
		switch p.kind {
		case panelModel:
			if name, ok := it.data.(string); ok {
				if err := a.switchModelByName(name); err != nil {
					a.toastf("%v", err)
				}
			}
		case panelSession:
			if id, ok := it.data.(string); ok {
				a.loadSession(id)
			}
		case panelPalette:
			if c, ok := it.data.(command); ok {
				if err := c.run(a, ""); err != nil {
					a.toastf("%v", err)
				}
			}
		case panelProvider:
			if name, ok := it.data.(string); ok {
				a.switchProvider(name)
			}
		case panelFile:
			if path, ok := it.data.(string); ok {
				// complete the seeded "@" into "@path " — a trailing bare
				// @ (the picker trigger) is replaced, not duplicated
				base := strings.TrimSuffix(a.ta.Value(), "@")
				a.ta.SetValue(base + "@" + path + " ")
				a.ta.CursorEnd()
				a.toastf("attached %s — append #L121-135 for a line range", path)
			}
		}
		a.refreshContent()
		return a, nil
	case "backspace", "ctrl+h":
		if p.query != "" {
			r := []rune(p.query)
			p.query = string(r[:len(r)-1])
			p.refilter()
		}
		return a, nil
	}

	// panel-specific actions
	if p.kind == panelModel && key == "f" {
		if it, ok := p.Selected(); ok {
			if name, ok2 := it.data.(string); ok2 {
				a.toggleFavorite(name)
				a.openModelPanel() // rebuild with new order
			}
		}
		return a, nil
	}
	if p.kind == panelSession {
		switch key {
		case "d":
			if it, ok := p.Selected(); ok {
				if id, ok2 := it.data.(string); ok2 {
					a.openConfirm("delete this session? (cannot be undone)", func() {
						if err := a.store.Delete(id); err != nil {
							a.toastf("delete failed: %v", err)
						} else {
							if a.sess.ID == id {
								a.newSession()
							}
							a.toastf("session deleted")
						}
					})
				}
			}
			return a, nil
		case "r":
			if it, ok := p.Selected(); ok {
				if id, ok2 := it.data.(string); ok2 {
					a.rename = newRenamePanel(id, it.title)
					a.panel = panelRename
				}
			}
			return a, nil
		}
	}

	// typing filters (bubbletea batches consecutive printable runes into one
	// KeyRunes msg, so match on type — a len(key)==1 check misses pastes)
	if msg.Type == tea.KeyRunes || msg.Type == tea.KeySpace {
		p.query += string(msg.Runes)
		p.refilter()
		p.cursor = 0
	}
	return a, nil
}

func (a *App) approvalKey(key string) (tea.Model, tea.Cmd) {
	req := a.approval
	if req == nil {
		a.panel = panelNone
		return a, nil
	}
	switch key {
	case "a", "1", "y", "enter":
		req.Decide(agentDecisionOnce)
		a.closeApproval()
	case "s", "2":
		req.Decide(agentDecisionSession)
		a.closeApproval()
	case "d", "3", "n", "esc":
		req.Decide(agentDecisionDeny)
		a.closeApproval()
	}
	return a, nil
}

func (a *App) closeApproval() {
	a.approval = nil
	a.panel = panelNone
	if a.gen != nil {
		a.gen.phase = "answering…"
	}
}

func (a *App) confirmKey(key string) (tea.Model, tea.Cmd) {
	c := a.confirm
	switch key {
	case "y", "enter":
		a.panel = panelNone
		if c != nil && c.onYes != nil {
			c.onYes()
		}
		a.confirm = nil
		a.refreshContent()
	case "n", "esc", "q":
		a.panel = panelNone
		a.confirm = nil
	}
	return a, nil
}

func (a *App) renameKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	key := msg.String()
	switch key {
	case "esc":
		a.panel = panelNone
		a.rename = nil
		return a, nil
	case "enter":
		if a.rename != nil {
			newTitle := strings.TrimSpace(a.rename.input.Value())
			if newTitle != "" {
				if s, err := a.store.Load(a.rename.sessID); err == nil {
					s.Title = newTitle
					_ = a.store.Save(s)
					if s.ID == a.sess.ID {
						a.sess.Title = newTitle
					}
					a.toastf("renamed")
				}
			}
		}
		a.panel = panelNone
		a.rename = nil
		return a, nil
	}
	var cmd tea.Cmd
	a.rename, cmd = renameUpdate(a.rename, msg)
	return a, cmd
}

func (a *App) helpKey(key string) (tea.Model, tea.Cmd) {
	switch key {
	case "esc", "q", "?":
		a.panel = panelNone
	case "up", "k":
		a.helpVp.LineUp(1)
	case "down", "j":
		a.helpVp.LineDown(1)
	case "pgup":
		a.helpVp.HalfPageUp()
	case "pgdown", " ":
		a.helpVp.HalfPageDown()
	}
	return a, nil
}
