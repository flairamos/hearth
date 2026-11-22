package tui

import (
	"fmt"
	"strconv"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"hearth/internal/agent"
)

// command is one slash command / palette action.
type command struct {
	name  string
	alias []string
	usage string
	desc  string
	run   func(a *App, args string) error
}

func (c command) match(word string) bool {
	if word == "" {
		return false
	}
	if word == c.name {
		return true
	}
	for _, a := range c.alias {
		if word == a {
			return true
		}
	}
	return false
}

func (a *App) commandList() []command {
	cmds := []command{
		{name: "model", alias: []string{"m"}, usage: "[name]", desc: "switch model",
			run: func(a *App, args string) error {
				if strings.TrimSpace(args) == "" {
					a.openModelPanel()
					return nil
				}
				return a.switchModelByName(strings.TrimSpace(args))
			}},
		{name: "new", alias: []string{"n"}, desc: "new session", run: func(a *App, _ string) error { a.newSession(); return nil }},
		{name: "sessions", alias: []string{"s"}, desc: "session list", run: func(a *App, _ string) error { a.openSessionPanel(); return nil }},
		{name: "clear", desc: "delete this session (messages + file, needs confirm)", run: func(a *App, _ string) error {
			if a.busy() {
				a.toastf("busy — esc to interrupt first")
				return nil
			}
			a.openConfirm("delete this session entirely? (messages, notes and the saved file — cannot be undone)", func() {
				id := a.sess.ID
				if err := a.store.Delete(id); err != nil {
					a.toastf("delete failed: %v", err)
					return
				}
				a.newSession() // fresh empty session on the same model
				a.toastf("session deleted — started a new one")
			})
			return nil
		}},
		{name: "system", usage: "<prompt>", desc: "set session system prompt", run: func(a *App, args string) error {
			a.sess.SystemPrompt = strings.TrimSpace(args)
			a.saveSession()
			a.toastf("system prompt set (%d chars)", len(a.sess.SystemPrompt))
			return nil
		}},
		{name: "temp", usage: "<0-2>", desc: "set temperature", run: func(a *App, args string) error {
			v, err := strconv.ParseFloat(strings.TrimSpace(args), 64)
			if err != nil || v < 0 || v > 2 {
				return fmt.Errorf("usage: /temp <0-2>")
			}
			a.sess.Params.Temperature = v
			a.saveSession()
			a.toastf("temperature = %.2f", v)
			return nil
		}},
		{name: "ctx", usage: "<tokens>", desc: "set context window (num_ctx)", run: func(a *App, args string) error {
			v, err := strconv.Atoi(strings.TrimSpace(args))
			if err != nil || v < 2048 {
				return fmt.Errorf("usage: /ctx <tokens ≥2048>")
			}
			a.sess.Params.NumCtx = v
			a.saveSession()
			a.toastf("num_ctx = %d", v)
			return nil
		}},
		{name: "think", usage: "[on|off]", desc: "toggle thinking mode", run: func(a *App, args string) error {
			switch strings.ToLower(strings.TrimSpace(args)) {
			case "on":
				a.setThink(true)
			case "off":
				a.setThink(false)
			default:
				a.setThink(!a.thinkEnabled())
			}
			return nil
		}},
		{name: "agent", alias: []string{"a"}, usage: "[on|off]", desc: "toggle agent mode (tool calling)", run: func(a *App, args string) error {
			switch strings.ToLower(strings.TrimSpace(args)) {
			case "on":
				a.setAgentMode(true)
			case "off":
				a.setAgentMode(false)
			default:
				a.setAgentMode(!a.sess.AgentMode)
			}
			return nil
		}},
		{name: "access", usage: "[ask|risky|full]", desc: "permission tier (ctrl+g cycles)", run: func(a *App, args string) error {
			v := strings.ToLower(strings.TrimSpace(args))
			if v == "" {
				a.cycleAccessMode()
				return nil
			}
			mode := agent.NormalizeMode(v)
			a.perms.SetMode(mode)
			a.cfg.Agent.Permissions = mode
			_ = a.cfg.Save()
			a.toastf("access: %s — %s", mode, agent.ModeDesc(mode))
			return nil
		}},
		{name: "img", usage: "<path>", desc: "attach an image to the next message", run: func(a *App, args string) error {
			return a.attachImage(strings.TrimSpace(args))
		}},
		{name: "save", usage: "[path]", desc: "export session as Markdown", run: func(a *App, args string) error {
			p, err := saveSessionFile(a.sess, strings.TrimSpace(args))
			if err != nil {
				return err
			}
			a.toastf("exported → %s", p)
			return nil
		}},
		{name: "compact", desc: "summarize and compress context", run: func(a *App, _ string) error {
			a.startCompact()
			return nil
		}},
		{name: "retry", desc: "regenerate the last answer", run: func(a *App, _ string) error {
			a.retryLast()
			return nil
		}},
		{name: "config", desc: "edit config in $EDITOR (hot reload)", run: func(a *App, _ string) error {
			a.editConfig()
			return nil
		}},
		{name: "theme", usage: "[dark|light|dracula]", desc: "switch theme", run: func(a *App, args string) error {
			return a.cycleTheme(strings.TrimSpace(args))
		}},
		{name: "title", usage: "<text>", desc: "rename session", run: func(a *App, args string) error {
			t := strings.TrimSpace(args)
			if t == "" {
				return fmt.Errorf("usage: /title <text>")
			}
			a.sess.Title = t
			a.saveSession()
			return nil
		}},
		{name: "help", usage: "?", desc: "help panel", run: func(a *App, _ string) error { a.panel = panelHelp; return nil }},
		{name: "quit", alias: []string{"exit", "q"}, desc: "quit (session saved)", run: func(a *App, _ string) error {
			a.quitNow = true
			a.saveSession() // flush even a mid-turn agent exchange
			// hand tea.Quit back through execCmd — prog.Send() called from
			// inside Update deadlocks (the event loop is busy running Update)
			a.execCmd = tea.Quit
			return nil
		}},
	}
	// user-defined prompt templates from config
	for name, cc := range a.cfg.Commands {
		nm, cc := name, cc
		cmds = append(cmds, command{
			name: nm,
			desc: "custom: " + cc.Desc,
			run: func(a *App, args string) error {
				prompt := strings.ReplaceAll(cc.Prompt, "{{input}}", args)
				a.submitText(prompt)
				return nil
			},
		})
	}
	return cmds
}

// findCommand matches "/word" against the registry.
func (a *App) findCommand(word string) (command, bool) {
	for _, c := range a.commandList() {
		if c.match(word) {
			return c, true
		}
	}
	return command{}, false
}

// slashMatches returns commands whose name starts with the typed prefix.
func (a *App) slashMatches(prefix string) []command {
	var out []command
	for _, c := range a.commandList() {
		if strings.HasPrefix(c.name, prefix) || fuzzyScore(c.name, prefix) > 2 {
			out = append(out, c)
		}
	}
	return out
}

// slashVisible reports whether the command popup is currently showing matches.
func (a *App) slashVisible() bool {
	if a.slashHidden {
		return false
	}
	v := strings.TrimSpace(a.ta.Value())
	if !strings.HasPrefix(v, "/") {
		return false
	}
	word, _ := splitSlash(v)
	return len(a.slashMatches(word)) > 0
}

// slashMove shifts the popup selection with wraparound.
func (a *App) slashMove(delta int) {
	v := strings.TrimSpace(a.ta.Value())
	word, _ := splitSlash(v)
	n := len(a.slashMatches(word))
	if n == 0 {
		return
	}
	a.slashCursor = (a.slashCursor + delta%n + n) % n
}

// slashComplete replaces the draft with the selected command name.
func (a *App) slashComplete() {
	v := strings.TrimSpace(a.ta.Value())
	word, _ := splitSlash(v)
	matches := a.slashMatches(word)
	if len(matches) == 0 {
		return
	}
	c := matches[min(a.slashCursor, len(matches)-1)]
	a.ta.SetValue("/" + c.name + " ")
	a.ta.CursorEnd()
}
