// Package tui implements the Bubble Tea interface.
package tui

import (
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/textarea"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"

	"hearth/internal/agent"
	"hearth/internal/config"
	"hearth/internal/core"
	"hearth/internal/ollama"
	"hearth/internal/provider"
	"hearth/internal/store"
	"hearth/internal/theme"
	"hearth/internal/tools"
	"hearth/internal/version"
)

// ---- messages ----

type (
	tagsMsg struct {
		models []ollama.ModelInfo
		err    error
	}
	deltaMsg    struct{ thinking, content string }
	agentEvtMsg struct{ ev agent.Event }
	doneMsg     struct {
		err         error
		interrupted bool
		failed      bool // agent turn ended with an error — no final answer
		tokens      int
		tps         float64 // server-measured generation throughput
	}
	spinnerTickMsg struct{}
	capsMsg        struct {
		model string
		caps  []string
		err   error
	}
	configReloadedMsg struct{ err error }
)

// genState is the in-flight generation.
type genState struct {
	ctx       context.Context
	cancel    context.CancelFunc
	buf       *deltaBuf
	phase     string
	start     time.Time
	isAgent   bool
	outTokEst int           // estimated output tokens (live tok/s numerator)
	genNs     time.Duration // time spent actively receiving deltas (denominator)
	lastDelta time.Time     // arrival time of the previous delta flush
}

// turnStats is the last completed turn's throughput (local-agent style).
type turnStats struct {
	tps         float64
	dur         time.Duration
	toks        int
	failed      bool
	interrupted bool
}

// App is the root Bubble Tea model.
type App struct {
	cfg    *config.Config
	client provider.Client
	store  *store.Store
	th     theme.Theme
	km     *KeyMap
	rend   *renderer

	sess    *core.Session
	models  []ollama.ModelInfo
	caps    map[string][]string // model → capabilities (vision/tools/thinking)
	connOK  bool
	connErr error

	width, height int
	sidebar       bool
	follow        bool

	history   []string // sent-message history for ↑/↓
	histPos   int      // -1 = live input
	lastStats turnStats
	lastError string // most recent agent error, kept for the status bar

	ta            textarea.Model
	vp            viewport.Model
	helpVp        viewport.Model
	pendingImages []string
	pendingSends  []string

	gen      *genState
	perms    *agent.Manager
	toolsReg *tools.Registry
	memory   *tools.MemoryStore
	workDir  string

	panel       panelKind
	panelLst    *listPanel
	confirm     *confirmDialog
	approval    *agent.ApprovalRequest
	rename      *renamePanel
	slashCursor int
	slashHidden bool   // esc-dismissed until the input text changes
	lastBody    string // last viewport content — identical frames skip SetContent

	toast      string
	toastAt    time.Time
	quitArm    bool
	quitArmAt  time.Time
	quitNow    bool
	compacting bool
	execCmd    tea.Cmd

	prog *tea.Program
}

// New builds the application model.
func New(cfg *config.Config, client provider.Client, st *store.Store, resumeID string) (*App, error) {
	wd, _ := os.Getwd()
	app := &App{
		cfg:     cfg,
		client:  client,
		store:   st,
		th:      theme.Get(cfg.Theme),
		km:      NewKeyMap(cfg.Keymap),
		workDir: wd,
		sidebar: cfg.UI.Sidebar,
		follow:  true,
	}
	app.toolsReg = &tools.Registry{WorkDir: wd, BashTimeoutSec: cfg.Agent.BashTimeout}
	app.perms = agent.NewManager(cfg.Agent.Permissions, wd, cfg.Agent.AutoApprove, cfg.Agent.DenyPatterns)
	app.memory = newMemoryStore(cfg)

	// session: resume or create
	if resumeID != "" {
		if s, err := st.Load(resumeID); err == nil {
			app.sess = s
		}
	}
	if app.sess == nil {
		if s, err := st.Latest(); err == nil && s != nil {
			app.sess = s
		}
	}
	if app.sess == nil {
		app.sess = core.NewSession(cfg.DefaultModel)
	}
	if app.sess.SystemPrompt == "" && cfg.SystemPrompt != "" {
		app.sess.SystemPrompt = cfg.SystemPrompt
	}
	app.syncRegistry()

	app.caps = map[string][]string{}
	app.rend = newRenderer(80, app.th.GlamourStyle())
	app.ta = textarea.New()
	app.ta.Prompt = ""
	app.ta.Placeholder = "message…  (/ commands · ctrl+k palette · alt+a agent)"
	app.ta.CharLimit = 0
	app.ta.ShowLineNumbers = false
	app.ta.SetHeight(3)
	app.ta.KeyMap.InsertNewline.SetKeys("alt+enter", "ctrl+j")
	app.ta.Focus()
	app.vp = viewport.New(80, 20)
	return app, nil
}

// openFilePanel shows the fuzzy workspace file picker for @ references.
func (a *App) openFilePanel() {
	files := workspaceFiles(a.workDir, 500)
	var items []item
	for _, f := range files {
		items = append(items, item{title: f, desc: "", data: f})
	}
	a.panelLst = newListPanel(panelFile, "attach file (append #L121-135 for a line range)", items)
	a.panel = panelFile
}

// SetProgram wires the running program for goroutine → UI messaging.
func (a *App) SetProgram(p *tea.Program) { a.prog = p }

// newMemoryStore builds the long-term memory store (nil when disabled).
func newMemoryStore(cfg *config.Config) *tools.MemoryStore {
	if !cfg.Agent.Memory {
		return nil
	}
	return tools.NewMemoryStore(tools.MemoryPath())
}

// syncRegistry re-wires the tool registry to current session/config/memory.
func (a *App) syncRegistry() {
	a.toolsReg.Session = a.sess
	a.toolsReg.Memory = a.memory
	a.toolsReg.ShellPref = a.cfg.Agent.Shell
	a.toolsReg.Search = &tools.SearchBackend{
		Engine:     a.cfg.Search.Engine,
		SearxngURL: a.cfg.Search.SearxngURL,
		BraveKey:   a.cfg.Search.BraveKey,
		MaxResults: a.cfg.Search.MaxResults,
	}
}

func (a *App) Init() tea.Cmd {
	return tea.Batch(
		tea.SetWindowTitle("hearth v"+version.Version),
		fetchTagsCmd(a.client),
		spinnerTickCmd(),
	)
}

// ---- tea.Cmd helpers ----

func fetchTagsCmd(c provider.Client) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel()
		models, err := c.Tags(ctx)
		return tagsMsg{models: models, err: err}
	}
}

// fetchCapsCmd loads model capabilities via /api/show (native style only).
func fetchCapsCmd(c provider.Client, model string) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		show, err := c.Show(ctx, model)
		return capsMsg{model: model, caps: show.Capabilities, err: err}
	}
}

func spinnerTickCmd() tea.Cmd {
	return tea.Tick(120*time.Millisecond, func(time.Time) tea.Msg { return spinnerTickMsg{} })
}

// ---- Update ----

func (a *App) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		a.resize(msg.Width, msg.Height)
		return a, nil

	case tagsMsg:
		if msg.err != nil {
			a.connOK = false
			a.connErr = msg.err
			return a, nil
		}
		a.connOK = true
		a.connErr = nil
		a.models = msg.models
		if a.sess.Model == "" && len(a.models) > 0 {
			a.sess.Model = a.models[0].Name
		}
		if a.sess.Model != "" {
			return a, fetchCapsCmd(a.client, a.sess.Model)
		}
		a.refreshContent()
		return a, nil

	case spinnerTickMsg:
		spinnerFrame = (spinnerFrame + 1) % len(spinnerFrames)
		// always re-arm: the chain dies on the first idle tick otherwise —
		// turns start later and never restart it, freezing the spinner for
		// the whole task (and the compacting spinner, where gen == nil)
		return a, spinnerTickCmd()

	case queuedSendMsg:
		a.startTurn(msg.text, nil, nil)
		return a, nil

	case compactDoneMsg:
		a.compacting = false
		if msg.err != nil {
			a.toastf("compact failed: %v", msg.err)
			return a, nil
		}
		// continuity: summary block + the last verbatim pair from the
		// summarized prefix + anything added during compaction — the
		// conversation never "disconnects" after compression
		snap := a.sess.Snapshot()
		keep := a.sess.Len() - msg.upto // added during compaction
		if keep < 0 {
			keep = 0
		}
		tail := append([]core.Message(nil), snap[len(snap)-keep:]...)
		if n := len(snap) - keep; n >= 2 {
			tail = append(append([]core.Message(nil), snap[n-2:]...), tail...)
		}
		summary := []core.Message{{Role: core.RoleUser, Content: "[conversation summary]\n" + msg.summary}}
		a.sess.SetMessages(append(summary, tail...))
		a.sess.Compacted = true
		a.rend.Invalidate()
		a.saveSession()
		a.refreshContent()
		a.toastf("context compacted (%d chars summary)", len(msg.summary))
		return a, nil

	case deltaMsg:
		if a.gen != nil {
			if msg.thinking != "" && msg.content == "" {
				a.gen.phase = "thinking…"
			}
			if msg.content != "" {
				a.gen.phase = "answering…"
			}
			a.gen.outTokEst += core.EstimateTokens(msg.content) + core.EstimateTokens(msg.thinking)
			// active-generation clock: gaps under 2s are the model still
			// working (chunk spacing, thinking pauses); longer gaps are tool
			// execution / waiting — they must not dilute the live tok/s
			now := time.Now()
			if !a.gen.lastDelta.IsZero() {
				if gap := now.Sub(a.gen.lastDelta); gap < 2*time.Second {
					a.gen.genNs += gap
				}
			}
			a.gen.lastDelta = now
			a.refreshContent()
		}
		return a, nil

	case capsMsg:
		if msg.err == nil && msg.model != "" {
			a.caps[msg.model] = msg.caps
			a.refreshContent()
		}
		return a, nil

	case agentEvtMsg:
		return a, a.onAgentEvent(msg.ev)

	case doneMsg:
		a.onDone(msg)
		return a, nil

	case configReloadedMsg:
		if msg.err != nil {
			a.toastf("config reload failed: %v", msg.err)
		} else if c, err := config.Load(); err != nil {
			a.toastf("config parse error: %v", err)
		} else {
			a.cfg = c
			a.th = theme.Get(c.Theme)
			a.km = NewKeyMap(c.Keymap)
			a.perms = agent.NewManager(c.Agent.Permissions, a.workDir, c.Agent.AutoApprove, c.Agent.DenyPatterns)
			a.memory = newMemoryStore(c)
			a.toolsReg.BashTimeoutSec = c.Agent.BashTimeout
			a.syncRegistry()
			a.toastf("config reloaded")
		}
		return a, nil

	case tea.MouseMsg:
		if a.panel != panelNone {
			return a, nil // modal panels (approval/confirm/...) swallow the wheel
		}
		// wheel while the command popup is open selects entries instead of
		// scrolling the transcript
		if a.slashVisible() {
			switch msg.Type {
			case tea.MouseWheelUp:
				a.slashMove(-1)
				return a, nil
			case tea.MouseWheelDown:
				a.slashMove(1)
				return a, nil
			}
		}
		switch msg.Type {
		case tea.MouseWheelUp:
			a.follow = false
			a.vp.LineUp(3)
		case tea.MouseWheelDown:
			a.vp.LineDown(3)
			if a.vp.AtBottom() {
				a.follow = true
			}
		}
		return a, nil

	case tea.KeyMsg:
		before := a.ta.Value()
		m, cmd := a.handleKey(msg)
		if a.ta.Value() != before {
			// the draft text changed: re-show the popup, reset selection
			a.slashHidden = false
			a.slashCursor = 0
		}
		if a.execCmd != nil {
			cmd = a.execCmd
			a.execCmd = nil
		}
		return m, cmd
	}
	return a, nil
}

func (a *App) onAgentEvent(ev agent.Event) tea.Cmd {
	if a.gen == nil {
		return nil
	}
	switch ev.Kind {
	case agent.EvToolStart:
		a.gen.phase = "tool: " + ev.ToolName
		a.refreshContent()
	case agent.EvToolResult:
		a.gen.phase = "answering…"
		a.refreshContent()
	case agent.EvApproval:
		a.approval = ev.Approval
		a.panel = panelApproval
	case agent.EvTrimmed:
		// message indices shifted — the render cache keys on (idx, size)
		a.rend.Invalidate()
		a.toastf("%s", ev.Text)
		a.refreshContent()
	case agent.EvError:
		a.lastError = ev.Text // persistent: status bar shows it after the toast fades
		a.toastf("%s", ev.Text)
		a.refreshContent()
	case agent.EvTurnDone:
		if ev.Usage.Truncated {
			a.toastf("answer was cut by a token limit — raise generation.max_tokens or shrink /ctx")
		}
		if ev.Usage.TPS() > 0 {
			a.lastStats = turnStats{tps: ev.Usage.TPS(), dur: ev.Usage.Duration, toks: ev.Usage.OutputTokens,
				failed: ev.Usage.Failed, interrupted: ev.Usage.Interrupted}
		} else if ev.Usage.Failed {
			a.lastStats = turnStats{dur: ev.Usage.Duration, failed: true}
		} else if ev.Usage.Interrupted {
			a.lastStats = turnStats{dur: ev.Usage.Duration, interrupted: true}
		}
	}
	return nil
}

// ---- generation lifecycle ----

func (a *App) busy() bool { return a.gen != nil }

func (a *App) startTurn(text string, images []string, refs []core.FileRef) {
	if a.busy() || a.compacting {
		return
	}
	// context guard (PRD 4.2.2): pre-send trim when near the window
	if a.cfg.Context.AutoTrim {
		numCtx := a.sess.NumCtx(a.cfg.Context.NumCtx)
		if a.sess.UsedTokens() > numCtx*90/100 {
			if a.sess.TrimToMax(numCtx) {
				a.toastf("context trimmed to fit %d tokens", numCtx)
				a.rend.Invalidate()
			}
		}
	}
	a.sess.Add(core.Message{Role: core.RoleUser, Content: text, Images: images, Refs: refs})
	a.saveSession()

	ctx, cancel := context.WithCancel(context.Background())
	a.gen = &genState{
		ctx:    ctx,
		cancel: cancel,
		buf:    &deltaBuf{},
		phase:  "waiting for model…",
		start:  time.Now(),
	}
	a.follow = true

	if a.sess.AgentMode {
		a.gen.isAgent = true
		a.startAgentTurn(ctx)
	} else {
		a.startChatTurn(ctx)
	}
}

func (a *App) opts() ollama.Options {
	temp := a.sess.Params.Temperature
	if temp == 0 {
		temp = a.cfg.Generation.Temperature
		if a.sess.AgentMode && a.cfg.Agent.Temperature > 0 {
			temp = a.cfg.Agent.Temperature
		}
	}
	return ollama.Options{
		Temperature: temp,
		TopP:        a.cfg.Generation.TopP,
		TopK:        a.cfg.Generation.TopK,
		NumCtx:      a.sess.NumCtx(a.cfg.Context.NumCtx),
		NumPredict:  a.cfg.Generation.MaxTokens,
	}
}

func (a *App) thinkEnabled() bool {
	if a.sess.Params.Think != nil {
		return *a.sess.Params.Think
	}
	if a.cfg.Generation.Think != nil {
		return *a.cfg.Generation.Think
	}
	return false
}

func (a *App) startChatTurn(ctx context.Context) {
	buf := a.gen.buf
	req := ollama.ChatRequest{
		Model:     a.sess.Model,
		Messages:  a.chatMessages(),
		Options:   optsPtr(a.opts()),
		Think:     boolPtr(a.thinkEnabled()),
		KeepAlive: a.cfg.Context.KeepAlive,
	}
	// flusher ticker: batch deltas every 50ms (PRD 渲染节流)
	flushDone := make(chan struct{})
	go func() {
		tk := time.NewTicker(50 * time.Millisecond)
		defer tk.Stop()
		for {
			select {
			case <-ctx.Done():
				close(flushDone)
				return
			case <-tk.C:
				t, c := buf.Drain()
				if t != "" || c != "" {
					a.prog.Send(deltaMsg{thinking: t, content: c})
				}
			}
		}
	}()
	go func() {
		var promptTok, outTok int
		var evalNs int64
		var trunc bool
		err := a.client.ChatStream(ctx, req, func(r ollama.ChatResponse) error {
			buf.Add(r.Message.Thinking, r.Message.Content)
			if r.Done {
				promptTok, outTok, evalNs = r.PromptEvalCount, r.EvalCount, r.EvalDuration
				trunc = r.DoneReason == "length"
			}
			return nil
		})
		a.gen.cancel() // stops ticker
		<-flushDone
		// assemble from the cumulative buffers, not the pending tail
		th, c := buf.Total()
		interrupted := err != nil && ctx.Err() != nil
		// record the reply only when there is something to preserve;
		// a bare error/interrupt leaves the session clean for retry
		if strings.TrimSpace(c) != "" || strings.TrimSpace(th) != "" {
			a.sess.Add(core.Message{
				Role:        core.RoleAssistant,
				Content:     c,
				Thinking:    th,
				Interrupted: interrupted,
				Truncated:   trunc,
				Tokens:      outTok,
			})
		}
		var derr error
		if err != nil && !interrupted {
			derr = err
		}
		var tps float64
		if outTok > 0 {
			tps = float64(outTok) / (float64(evalNs) / 1e9)
		}
		a.prog.Send(doneMsg{err: derr, interrupted: interrupted, tokens: promptTok + outTok, tps: tps})
	}()
}

func (a *App) startAgentTurn(ctx context.Context) {
	ag := &agent.Agent{
		Client:     a.client,
		Model:      a.sess.Model,
		Opts:       a.opts(),
		Think:      a.thinkEnabled(),
		Tools:      a.toolsReg,
		Perms:      a.perms,
		MaxSteps:   a.cfg.Agent.MaxSteps,
		MaxToolLen: a.cfg.Agent.MaxToolResult,
		Sess:       a.sess,
		Memory:     a.memory,
		KeepAlive:  a.cfg.Context.KeepAlive,
	}
	events := ag.Run(ctx)
	buf := a.gen.buf
	flushDone := make(chan struct{})
	var failed bool // EvTurnDone.Usage.Failed, read before doneMsg is sent
	go func() {
		tk := time.NewTicker(50 * time.Millisecond)
		defer tk.Stop()
		for {
			select {
			case <-ctx.Done():
				close(flushDone)
				return
			case <-tk.C:
				t, c := buf.Drain()
				if t != "" || c != "" {
					a.prog.Send(deltaMsg{thinking: t, content: c})
				}
			}
		}
	}()
	go func() {
		for ev := range events {
			switch ev.Kind {
			case agent.EvThinking, agent.EvContent:
				// routed through the coalescing buffer
				if ev.Kind == agent.EvThinking {
					buf.Add(ev.Text, "")
				} else {
					buf.Add("", ev.Text)
				}
			default:
				// EvTurnDone carries Usage.Failed — read it here (goroutine
				// local, no race) so onDone learns the turn failed
				if ev.Kind == agent.EvTurnDone {
					failed = ev.Usage.Failed
				}
				a.prog.Send(agentEvtMsg{ev: ev})
			}
		}
		a.gen.cancel()
		<-flushDone
		t, c := buf.Drain()
		if t != "" || c != "" {
			a.prog.Send(deltaMsg{thinking: t, content: c})
		}
		a.prog.Send(doneMsg{failed: failed})
	}()
}

func (a *App) onDone(msg doneMsg) {
	if a.gen == nil {
		return
	}
	dur := time.Since(a.gen.start)
	a.gen.cancel()
	a.gen = nil
	a.panel = panelNone

	if msg.err != nil {
		if ollama.IsConnError(msg.err) {
			a.connOK = false
			a.toastf("connection lost: %v", msg.err)
		} else if ollama.IsNotFound(msg.err) {
			suggest := ollama.SuggestModels(a.sess.Model, provider.ModelNames(a.models))
			if len(suggest) > 0 {
				a.toastf("model %q not found — did you mean: %s? (alt+m)", a.sess.Model, strings.Join(suggest[:min(3, len(suggest))], ", "))
			} else {
				a.toastf("model %q not found — /model to pick one", a.sess.Model)
			}
		} else if ollama.IsContextOverflow(msg.err) {
			if a.cfg.Context.AutoTrim {
				// roll the failed turn back (user + empty assistant), trim
				// history, then re-queue the same user text
				if text, ok := a.sess.PopLastUserTurn(); ok {
					a.sess.TrimToMax(a.sess.NumCtx(a.cfg.Context.NumCtx))
					a.rend.Invalidate()
					a.toastf("context overflow — trimmed; retrying once")
					a.pendingSends = append([]string{text}, a.pendingSends...)
					a.saveSession()
					a.refreshContent()
					return
				}
			}
			a.toastf("context overflow — /compact or /ctx smaller")
		} else {
			a.toastf("error: %v", msg.err)
		}
		if msg.failed {
			// agent turn ended with an error and recorded no final answer — say
			// so; compaction below is skipped so the evidence stays inspectable
			a.toastf("✗ task ended with an error — see the status bar for why")
		}
	}

	// title from first message
	if a.cfg.AutoTitle {
		a.sess.AutoTitle()
	}
	a.follow = true
	a.saveSession()
	a.refreshContent()

	// record throughput (local-agent style "tok/s · last Xs")
	if msg.tps > 0 {
		a.lastStats = turnStats{tps: msg.tps, dur: dur, toks: msg.tokens}
	}

	// auto-compact near the context limit (keeps continuity: summary +
	// recent turns + scratchpad survive; runs between turns, never mid-turn).
	// Never after a FAILED turn — compacting away the transcript would erase
	// the evidence of where the task stopped.
	if a.cfg.Context.AutoCompact && !msg.interrupted && msg.err == nil && !msg.failed {
		numCtx := a.sess.NumCtx(a.cfg.Context.NumCtx)
		if a.sess.UsedTokens() >= numCtx*a.cfg.Context.CompactAtPct/100 {
			a.startCompact()
		}
	}

	// queued sends
	if len(a.pendingSends) > 0 {
		next := a.pendingSends[0]
		a.pendingSends = a.pendingSends[1:]
		go func() {
			time.Sleep(150 * time.Millisecond)
			a.prog.Send(queuedSendMsg{text: next})
		}()
	} else if !msg.interrupted && msg.err == nil {
		a.toastf("done in %.1fs", dur.Seconds())
	}
}

type queuedSendMsg struct{ text string }

// interrupt stops the current generation, keeping partial output (PRD 4.1.1).
func (a *App) interrupt() {
	if a.gen != nil {
		a.gen.cancel()
	}
}

// ---- session helpers ----

func (a *App) saveSession() {
	if a.store != nil {
		_ = a.store.Save(a.sess)
	}
}

func (a *App) newSession() {
	a.interrupt() // a running generation belongs to the old session — stop it
	a.sess = core.NewSession(a.sess.Model)
	if a.cfg.SystemPrompt != "" {
		a.sess.SystemPrompt = a.cfg.SystemPrompt
	}
	a.sess.AgentMode = false
	a.syncRegistry()
	a.rend.Invalidate()
	a.refreshContent()
	a.saveSession()
	a.toastf("new session")
}

func (a *App) loadSession(id string) {
	s, err := a.store.Load(id)
	if err != nil {
		a.toastf("load failed: %v", err)
		return
	}
	a.interrupt()
	a.sess = s
	a.syncRegistry()
	a.rend.Invalidate()
	a.follow = true
	a.refreshContent()
	a.toastf("loaded %q", s.Title)
}

func (a *App) chatMessages() []ollama.Message {
	sys := a.sess.Prompt()
	out := []ollama.Message{}
	if sys != "" {
		out = append(out, ollama.Message{Role: "system", Content: sys})
	}
	for _, m := range a.sess.Snapshot() {
		switch m.Role {
		case core.RoleUser:
			content := m.Content
			// @file refs: attach the snapshotted content blocks
			if len(m.Refs) > 0 {
				var blocks []string
				for _, r := range m.Refs {
					blocks = append(blocks, refContextBlock(r))
				}
				content = content + "\n\n" + strings.Join(blocks, "\n\n")
			}
			out = append(out, ollama.Message{Role: "user", Content: content, Images: m.Images})
		case core.RoleAssistant:
			out = append(out, ollama.Message{Role: "assistant", Content: m.Content})
		case core.RoleTool:
			out = append(out, ollama.Message{Role: "user", Content: fmt.Sprintf("[tool %s result]\n%s", m.ToolName, m.Content)})
		}
	}
	return out
}

func (a *App) resize(w, h int) {
	a.width, a.height = w, h
	bodyW := a.bodyWidth()
	a.rend.width = bodyW - 2
	a.rend.Invalidate()
	a.vp.Width = bodyW
	a.vp.Height = max(3, h-8) // header 1 + status 1 + footer 6
	a.helpVp.Width = w - 8
	a.helpVp.Height = max(3, h-8)
	a.ta.SetWidth(min(bodyW-4, 120))
	a.refreshContent()
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func (a *App) toastf(format string, args ...any) {
	a.toast = fmt.Sprintf(format, args...)
	a.toastAt = time.Now()
}

func (a *App) setAgentMode(on bool) {
	a.sess.AgentMode = on
	a.saveSession()
	if on {
		a.toastf("AGENT mode: tools enabled (alt+a to toggle)")
	} else {
		a.toastf("chat mode: tools off")
	}
}

func (a *App) setThink(on bool) {
	a.sess.Params.Think = &on
	a.saveSession()
	if on {
		a.toastf("thinking on")
	} else {
		a.toastf("thinking off")
	}
}

func (a *App) switchModelByName(name string) error {
	for _, m := range a.models {
		if strings.EqualFold(m.Name, name) || strings.Contains(strings.ToLower(m.Name), strings.ToLower(name)) {
			a.sess.Model = m.Name
			a.saveSession()
			a.toastf("model → %s", m.Name)
			// refresh capability cache in the background
			if a.prog != nil {
				go func(mm string) { a.prog.Send(fetchCapsCmd(a.client, mm)()) }(m.Name)
			}
		}
	}
	return fmt.Errorf("model %q not found locally", name)
}

func (a *App) attachImage(path string) error {
	if path == "" {
		return fmt.Errorf("usage: /img <path>")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if len(data) > 8<<20 {
		return fmt.Errorf("image too large (>8MB)")
	}
	a.pendingImages = append(a.pendingImages, base64.StdEncoding.EncodeToString(data))
	a.toastf("image attached (%s)", filepathBase(path))
	if a.sess.Model != "" && len(a.caps) > 0 && !a.hasVision() {
		a.toastf("note: model %s advertises no vision capability — the image may be ignored", a.sess.Model)
	}
	return nil
}

func filepathBase(p string) string {
	if i := strings.LastIndexByte(p, '/'); i >= 0 {
		return p[i+1:]
	}
	return p
}

func (a *App) retryLast() {
	if a.busy() {
		a.toastf("busy — esc to interrupt")
		return
	}
	// drop the last user turn (incl. its tool exchanges) and resend it
	lastUser, ok := a.sess.PopLastUserTurn()
	if !ok || strings.TrimSpace(lastUser) == "" {
		a.toastf("nothing to retry")
		return
	}
	a.rend.Invalidate()
	a.submitText(lastUser)
}

func (a *App) startCompact() {
	if a.busy() || a.compacting {
		a.toastf("busy — try again in a moment")
		return
	}
	if a.sess.Len() < 4 {
		a.toastf("nothing to compact")
		return
	}
	// snapshot the prefix being summarized; newer turns survive the replace
	n := a.sess.Len()
	a.compacting = true
	a.toastf("compacting…")
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	go func() {
		defer cancel()
		opts := a.opts()
		opts.NumPredict = 2048
		summary, err := compactSummarize(ctx, a.client, a.sess.Model, a.sess, opts, a.cfg.Context.KeepAlive)
		a.prog.Send(compactDoneMsg{summary: summary, err: err, upto: n})
	}()
}

type compactDoneMsg struct {
	summary string
	err     error
	upto    int // number of messages covered by the summary
}

func (a *App) editConfig() {
	editor := os.Getenv("EDITOR")
	if editor == "" {
		editor = "vi"
	}
	if err := a.cfg.Save(); err != nil {
		a.toastf("config save failed: %v", err)
		return
	}
	cmd := exec.Command(editor, config.Path())
	a.execCmd = tea.ExecProcess(cmd, func(err error) tea.Msg { return configReloadedMsg{err: err} })
}

func (a *App) cycleTheme(name string) error {
	if name == "" {
		switch a.cfg.Theme {
		case "dark":
			name = "dracula"
		case "dracula":
			name = "light"
		default:
			name = "dark"
		}
	}
	switch name {
	case "dark", "light", "dracula":
	default:
		return fmt.Errorf("unknown theme %q (dark|light|dracula)", name)
	}
	a.cfg.Theme = name
	a.th = theme.Get(name)
	a.rend.style = a.th.GlamourStyle()
	a.rend.Invalidate()
	_ = a.cfg.Save()
	a.toastf("theme → %s", name)
	return nil
}

// hasVision reports whether the current model advertises vision (unknown
// models — openai-style backends — are assumed capable, no warning).
func (a *App) hasVision() bool {
	caps, ok := a.caps[a.sess.Model]
	if !ok {
		return true
	}
	for _, c := range caps {
		if c == "vision" {
			return true
		}
	}
	return false
}

func optsPtr(o ollama.Options) *ollama.Options { return &o }
func boolPtr(b bool) *bool                     { return &b }

func timeSince(t time.Time) time.Duration { return time.Since(t) }
func now() time.Time                      { return time.Now() }

// decision aliases for the approval dialog
const (
	agentDecisionOnce    = agent.DecisionOnce
	agentDecisionSession = agent.DecisionSession
	agentDecisionDeny    = agent.DecisionDeny
)

// submitText sends a text turn (used by commands like /retry and custom
// prompt templates). Queues while busy.
func (a *App) submitText(text string) {
	if strings.TrimSpace(text) == "" {
		return
	}
	if a.busy() {
		a.pendingSends = append(a.pendingSends, text)
		a.toastf("queued — will send after current reply")
		return
	}
	a.startTurn(text, nil, nil)
}

// ---- panel openers ----

func (a *App) openModelPanel() {
	var items []item
	for _, m := range a.models {
		title := m.Name
		for _, f := range a.cfg.Favorites {
			if f == m.Name {
				title = "★ " + m.Name
			}
		}
		desc := fmt.Sprintf("%s %s %s", m.Details.ParameterSize, m.Details.QuantizationLevel, m.HumanSize())
		items = append(items, item{title: title, desc: desc, data: m.Name})
	}
	a.panelLst = newListPanel(panelModel, "switch model ("+a.modelLabel()+")", items)
	a.panel = panelModel
}

func (a *App) openSessionPanel() {
	var items []item
	if list, err := a.store.List(); err == nil {
		for _, sm := range list {
			if sm.MsgCount == 0 {
				continue // fresh or /clear'd sessions have nothing to resume
			}
			desc := fmt.Sprintf("%s · %d msgs", sm.Model, sm.MsgCount)
			items = append(items, item{title: sm.Title, desc: desc, data: sm.ID})
		}
	}
	a.panelLst = newListPanel(panelSession, "sessions", items)
	a.panel = panelSession
}

func (a *App) openPalette() {
	var items []item
	for _, c := range a.commandList() {
		usage := c.usage
		if usage != "" {
			usage = " " + usage
		}
		items = append(items, item{
			title: "/" + c.name + usage,
			desc:  c.desc,
			data:  c,
		})
	}
	a.panelLst = newListPanel(panelPalette, "command palette", items)
	a.panel = panelPalette
}

func (a *App) openConfirm(prompt string, onYes func()) {
	a.confirm = &confirmDialog{prompt: prompt, onYes: onYes}
	a.panel = panelConfirm
}

func (a *App) toggleFavorite(name string) {
	for i, f := range a.cfg.Favorites {
		if f == name {
			a.cfg.Favorites = append(a.cfg.Favorites[:i:i], a.cfg.Favorites[i+1:]...)
			_ = a.cfg.Save()
			return
		}
	}
	a.cfg.Favorites = append(a.cfg.Favorites, name)
	_ = a.cfg.Save()
}
