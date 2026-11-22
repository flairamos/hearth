// Package web serves the browser GUI for hearth: a localhost-only server
// that reuses the same engine pieces as the TUI (agent loop, sessions,
// store, provider layer) and streams turn events to the browser via SSE.
package web

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"hearth/internal/ollama"

	"hearth/internal/agent"
	"hearth/internal/config"
	"hearth/internal/core"
	"hearth/internal/provider"
	"hearth/internal/store"
	"hearth/internal/tools"
)

//go:embed static
var staticFS embed.FS

// Server is the localhost web GUI backend. One active session at a time
// (matching the desktop model); all browsers share the same live state.
type Server struct {
	cfg    *config.Config
	store  *store.Store
	client provider.Client
	tools  *tools.Registry
	memory *tools.MemoryStore
	perms  *agent.Manager

	mu             sync.Mutex
	sess           *core.Session
	busy           bool
	gen            *webGen
	pending        map[string]*agent.ApprovalRequest
	pendingImages  []string // attached via /api/img, consumed on the next send
	activeProvider string   // "" = local backend

	hubMu sync.Mutex
	subs  map[chan []byte]struct{}
}

type webGen struct {
	cancel func()
	failed bool
	mu     sync.Mutex
	tokEst int
	start  time.Time
}

func (g *webGen) addTok(n int) {
	g.mu.Lock()
	g.tokEst += n
	g.mu.Unlock()
}

func (g *webGen) stats() (int, float64) {
	g.mu.Lock()
	defer g.mu.Unlock()
	el := time.Since(g.start).Seconds()
	if el < 0.4 {
		return g.tokEst, 0
	}
	return g.tokEst, float64(g.tokEst) / el
}

// New builds the web server with its own backend wiring (same pieces as the
// TUI: config, store, provider client, tools, memory, permissions).
func New(cfg *config.Config, st *store.Store) (*Server, error) {
	if st == nil {
		return nil, fmt.Errorf("store is required")
	}
	wd, _ := os.Getwd()
	client, _, err := provider.EnsureBackend(cfg.OllamaURL, cfg.APIStyle, cfg.Discovery, 6*time.Second)
	if err != nil {
		fmt.Fprintf(os.Stderr, "note: %v\n", err)
	}
	s := &Server{
		cfg:     cfg,
		store:   st,
		client:  client,
		subs:    map[chan []byte]struct{}{},
		pending: map[string]*agent.ApprovalRequest{},
	}
	if cfg.Agent.Memory {
		s.memory = tools.NewMemoryStore(tools.MemoryPath())
	}
	s.tools = &tools.Registry{
		WorkDir:        wd,
		BashTimeoutSec: cfg.Agent.BashTimeout,
		ShellPref:      cfg.Agent.Shell,
		Session:        s.sess,
		Memory:         s.memory,
		Search: &tools.SearchBackend{
			Engine:     cfg.Search.Engine,
			SearxngURL: cfg.Search.SearxngURL,
			BraveKey:   cfg.Search.BraveKey,
			MaxResults: cfg.Search.MaxResults,
		},
	}
	s.perms = agent.NewManager(cfg.Agent.Permissions, wd, cfg.Agent.AutoApprove, cfg.Agent.DenyPatterns)

	if s2, err := st.Latest(); err == nil && s2 != nil {
		s.sess = s2
	} else {
		s.sess = core.NewSession(cfg.DefaultModel)
		_ = st.Save(s.sess)
	}
	s.tools.Session = s.sess

	if cfg.ActiveProvider != "" {
		if p, ok := cfg.Providers[cfg.ActiveProvider]; ok {
			s.client = provider.NewWithKey(p.URL, "openai", p.ResolvedKey())
			s.activeProvider = cfg.ActiveProvider
		}
	}
	return s, nil
}

// ---- SSE hub ----

func (s *Server) broadcast(v any) {
	b, _ := json.Marshal(v)
	s.hubMu.Lock()
	defer s.hubMu.Unlock()
	for ch := range s.subs {
		select {
		case ch <- b:
		default: // slow client: drop rather than block the turn
		}
	}
}

func (s *Server) broadcastEvent(kind, text string, extra map[string]any) {
	v := map[string]any{"type": kind}
	if text != "" {
		v["text"] = text
	}
	for k, val := range extra {
		v[k] = val
	}
	s.broadcast(v)
}

// ---- approvals ----

func (s *Server) registerApproval(req *agent.ApprovalRequest) {
	s.mu.Lock()
	s.pending[req.ID] = req
	s.mu.Unlock()
	time.AfterFunc(5*time.Minute, func() { // tab closed mid-prompt: deny so the turn proceeds
		s.mu.Lock()
		_, live := s.pending[req.ID]
		if live {
			delete(s.pending, req.ID)
		}
		s.mu.Unlock()
		if live {
			req.Decide(agent.DecisionDeny)
			s.broadcastEvent("approval", "timeout — denied", map[string]any{"tool": req.Tool})
		}
	})
}

func (s *Server) decideApproval(id string, dec agent.ApprovalDecision) bool {
	s.mu.Lock()
	req, ok := s.pending[id]
	if ok {
		delete(s.pending, id)
	}
	s.mu.Unlock()
	if !ok {
		return false
	}
	req.Decide(dec)
	return true
}

// ---- turn runner (chat + agent), mirroring the TUI glue ----

func (s *Server) startTurn(text string, refs []core.FileRef, images []string) error {
	s.mu.Lock()
	if s.busy {
		s.mu.Unlock()
		return fmt.Errorf("busy")
	}
	numCtx := s.sess.NumCtx(s.cfg.Context.NumCtx)
	if s.cfg.Context.AutoTrim && s.sess.UsedTokens() > numCtx*90/100 {
		s.sess.TrimToMax(numCtx)
	}
	s.sess.Add(core.Message{Role: core.RoleUser, Content: text, Refs: refs, Images: images})
	s.sess.AutoTitle()
	_ = s.store.Save(s.sess)
	s.busy = true

	ctx, cancel := context.WithCancel(context.Background())
	gen := &webGen{cancel: cancel, start: time.Now()}
	s.gen = gen
	sess := s.sess
	client := s.client
	s.mu.Unlock()

	s.broadcastEvent("user", text, map[string]any{"images": len(images)})

	// live metrics: ctx usage + streaming rate, every 500ms
	go func() {
		tk := time.NewTicker(500 * time.Millisecond)
		defer tk.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tk.C:
				toks, tps := gen.stats()
				s.mu.Lock()
				used := sess.UsedTokens()
				s.mu.Unlock()
				pct := 0
				if numCtx > 0 {
					pct = used * 100 / numCtx
				}
				s.broadcastEvent("stats", "", map[string]any{
					"ctxUsed": used, "ctxMax": numCtx, "pct": pct, "tokens": toks, "tps": tps,
				})
			}
		}
	}()

	think := s.cfg.Generation.Think == nil || *s.cfg.Generation.Think
	opts := ollama.Options{
		Temperature: s.cfg.Generation.Temperature,
		TopP:        s.cfg.Generation.TopP,
		NumCtx:      sess.NumCtx(s.cfg.Context.NumCtx),
		NumPredict:  s.cfg.Generation.MaxTokens,
	}
	if sess.AgentMode {
		go s.runAgentTurn(ctx, gen, sess, client, opts, think)
	} else {
		go s.runChatTurn(ctx, gen, sess, client, opts, think)
	}
	return nil
}

func (s *Server) finishTurn(gen *webGen, sess *core.Session) {
	gen.cancel()
	s.mu.Lock()
	s.busy = false
	s.gen = nil
	s.mu.Unlock()
	sess.AutoTitle()
	_ = s.store.Save(sess)
	s.broadcastEvent("done", "", map[string]any{"title": sess.Title})
}

func (s *Server) runChatTurn(ctx context.Context, gen *webGen, sess *core.Session, client provider.Client, opts ollama.Options, think bool) {
	req := ollama.ChatRequest{
		Model:     sess.Model,
		Messages:  chatMessages(sess),
		Options:   &opts,
		Think:     &think,
		KeepAlive: s.cfg.Context.KeepAlive,
	}
	var content, thinking strings.Builder
	var outTok int
	var truncated bool
	doStream := func(req ollama.ChatRequest) error {
		return client.ChatStream(ctx, req, func(r ollama.ChatResponse) error {
			if r.Message.Thinking != "" {
				thinking.WriteString(r.Message.Thinking)
				s.broadcastEvent("thinking", r.Message.Thinking, nil)
			}
			if r.Message.Content != "" {
				content.WriteString(r.Message.Content)
				gen.addTok(core.EstimateTokens(r.Message.Content))
				s.broadcastEvent("delta", r.Message.Content, nil)
			}
			if r.Done {
				outTok = r.EvalCount
				truncated = r.DoneReason == "length"
			}
			return nil
		})
	}
	err := doStream(req)
	// model without thinking support → retry once without the flag (parity
	// with the agent's streamTurn fallback)
	if err != nil && req.Think != nil && strings.Contains(err.Error(), "does not support thinking") {
		content.Reset()
		thinking.Reset()
		truncated = false
		req.Think = nil
		err = doStream(req)
	}
	interrupted := err != nil && ctx.Err() != nil
	if strings.TrimSpace(content.String()) != "" || strings.TrimSpace(thinking.String()) != "" {
		sess.Add(core.Message{
			Role:        core.RoleAssistant,
			Content:     content.String(),
			Thinking:    thinking.String(),
			Interrupted: interrupted,
			Truncated:   truncated,
			Tokens:      outTok,
		})
	}
	if err != nil && !interrupted {
		s.broadcastEvent("error", err.Error(), nil)
	}
	if truncated {
		s.broadcastEvent("error", "answer was cut by a token limit — raise generation.max_tokens", nil)
	}
	s.finishTurn(gen, sess)
}

func (s *Server) runAgentTurn(ctx context.Context, gen *webGen, sess *core.Session, client provider.Client, opts ollama.Options, think bool) {
	ag := &agent.Agent{
		Client:     client,
		Model:      sess.Model,
		Opts:       opts,
		Think:      think,
		Tools:      s.tools,
		Perms:      s.perms,
		MaxSteps:   s.cfg.Agent.MaxSteps,
		MaxToolLen: s.cfg.Agent.MaxToolResult,
		Sess:       sess,
		Memory:     s.memory,
		KeepAlive:  s.cfg.Context.KeepAlive,
	}
	var failed bool
	for ev := range ag.Run(ctx) {
		switch ev.Kind {
		case agent.EvThinking:
			s.broadcastEvent("thinking", ev.Text, nil)
		case agent.EvContent:
			gen.addTok(core.EstimateTokens(ev.Text))
			s.broadcastEvent("delta", ev.Text, nil)
		case agent.EvToolStart:
			s.broadcastEvent("tool_start", "", map[string]any{"tool": ev.ToolName, "args": ev.ToolArgs})
		case agent.EvToolResult:
			s.broadcastEvent("tool_result", ev.Text, map[string]any{"tool": ev.ToolName, "error": ev.ToolErr})
		case agent.EvApproval:
			s.registerApproval(ev.Approval)
			s.broadcastEvent("approval", ev.Approval.Summary, map[string]any{
				"id": ev.Approval.ID, "tool": ev.Approval.Tool, "detail": ev.Approval.Detail,
			})
		case agent.EvError:
			s.broadcastEvent("error", ev.Text, nil)
		case agent.EvTurnDone:
			failed = ev.Usage.Failed
		}
	}
	if failed {
		s.broadcastEvent("error", "task ended with an error — no final answer recorded", nil)
	}
	gen.failed = failed
	s.finishTurn(gen, sess)
}

// resetSession abandons the current session and starts a fresh one.
func (s *Server) resetSession() {
	s.interrupt()
	s.mu.Lock()
	s.sess = core.NewSession(s.cfg.DefaultModel)
	sess := s.sess
	s.tools.Session = sess
	s.mu.Unlock()
	_ = s.store.Save(sess)
	s.broadcastEvent("reset", "", nil)
}

func chatMessages(sess *core.Session) []ollama.Message {
	out := []ollama.Message{}
	if sys := sess.Prompt(); sys != "" {
		out = append(out, ollama.Message{Role: "system", Content: sys})
	}
	for _, m := range sess.Snapshot() {
		switch m.Role {
		case core.RoleUser:
			content := m.Content
			if len(m.Refs) > 0 {
				var blocks []string
				for _, r := range m.Refs {
					blocks = append(blocks, core.RefContextBlock(r))
				}
				content = content + "\n\n" + strings.Join(blocks, "\n\n")
			}
			out = append(out, ollama.Message{Role: "user", Content: content, Images: m.Images})
		case core.RoleAssistant:
			om := ollama.Message{Role: "assistant", Content: m.Content}
			for _, tc := range m.ToolCalls {
				om.ToolCalls = append(om.ToolCalls, ollama.ToolCall{Function: ollama.ToolCallFunc{Name: tc.Name, Arguments: tc.Args}})
			}
			out = append(out, om)
		case core.RoleTool:
			out = append(out, ollama.Message{Role: "tool", Content: m.Content, ToolName: m.ToolName})
		case core.RoleSystem:
			out = append(out, ollama.Message{Role: "system", Content: m.Content})
		}
	}
	return out
}
