package web

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"time"

	"hearth/internal/agent"
	"hearth/internal/core"
	"hearth/internal/ollama"
	"hearth/internal/provider"
	"hearth/internal/tui"
)

// mux builds the API + static routes.
func (s *Server) mux() *http.ServeMux {
	m := http.NewServeMux()
	m.HandleFunc("GET /", s.handleIndex)
	m.HandleFunc("GET /style.css", s.handleStatic("static/style.css", "text/css"))
	m.HandleFunc("GET /app.js", s.handleStatic("static/app.js", "text/javascript"))
	m.HandleFunc("GET /api/state", s.handleState)
	m.HandleFunc("GET /api/sessions", s.handleSessions)
	m.HandleFunc("GET /api/files", s.handleFiles)
	m.HandleFunc("GET /api/commands", s.handleCommands)
	m.HandleFunc("GET /api/events", s.handleEvents)
	m.HandleFunc("POST /api/session/new", s.handleSessionNew)
	m.HandleFunc("POST /api/session/load", s.handleSessionLoad)
	m.HandleFunc("POST /api/send", s.handleSend)
	m.HandleFunc("POST /api/interrupt", s.handleInterrupt)
	m.HandleFunc("POST /api/agent-mode", s.handleAgentMode)
	m.HandleFunc("POST /api/retry", s.handleRetry)
	m.HandleFunc("POST /api/img", s.handleImg)
	m.HandleFunc("GET /api/export", s.handleExport)
	m.HandleFunc("POST /api/approve", s.handleApprove)
	return m
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	b, _ := staticFS.ReadFile("static/index.html")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(b)
}

func (s *Server) handleStatic(name, ctype string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		b, _ := staticFS.ReadFile(name)
		w.Header().Set("Content-Type", ctype+"; charset=utf-8")
		_, _ = w.Write(b)
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// handleState returns the active session snapshot + UI state.
func (s *Server) handleState(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess := s.sess
	msgs := []map[string]any{}
	for _, m := range sess.Snapshot() {
		switch m.Role {
		case core.RoleUser:
			msgs = append(msgs, map[string]any{"role": "user", "content": m.Content})
		case core.RoleAssistant:
			msgs = append(msgs, map[string]any{"role": "assistant", "content": m.Content})
		case core.RoleTool:
			msgs = append(msgs, map[string]any{"role": "tool", "toolName": m.ToolName, "content": m.Content, "toolError": m.ToolError})
		}
	}
	provider := s.activeProvider
	if provider == "" {
		provider = "local"
	}
	writeJSON(w, map[string]any{
		"sessionID": sess.ID,
		"title":     sess.Title,
		"model":     sess.Model,
		"provider":  provider,
		"agentMode": sess.AgentMode,
		"busy":      s.busy,
		"messages":  msgs,
	})
}

func (s *Server) handleSessions(w http.ResponseWriter, r *http.Request) {
	list, err := s.store.List()
	if err != nil {
		writeJSON(w, []any{})
		return
	}
	out := []map[string]any{}
	for _, m := range list {
		if m.MsgCount == 0 && m.ID != s.activeSessionID() {
			continue
		}
		out = append(out, map[string]any{"id": m.ID, "title": m.Title, "msgCount": m.MsgCount})
	}
	writeJSON(w, out)
}

func (s *Server) activeSessionID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sess.ID
}

func (s *Server) handleSessionNew(w http.ResponseWriter, r *http.Request) {
	s.interrupt()
	s.mu.Lock()
	s.sess = core.NewSession(s.cfg.DefaultModel)
	sess := s.sess
	s.tools.Session = sess
	s.mu.Unlock()
	_ = s.store.Save(sess)
	writeJSON(w, map[string]any{"sessionID": sess.ID})
}

func (s *Server) handleSessionLoad(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	if id == "" {
		http.Error(w, "missing id", http.StatusBadRequest)
		return
	}
	sess, err := s.store.Load(id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	s.interrupt()
	s.mu.Lock()
	s.sess = sess
	s.tools.Session = sess
	s.mu.Unlock()
	msgs := []map[string]any{}
	for _, m := range sess.Snapshot() {
		switch m.Role {
		case core.RoleUser:
			msgs = append(msgs, map[string]any{"role": "user", "content": m.Content})
		case core.RoleAssistant:
			msgs = append(msgs, map[string]any{"role": "assistant", "content": m.Content})
		case core.RoleTool:
			msgs = append(msgs, map[string]any{"role": "tool", "toolName": m.ToolName, "content": m.Content, "toolError": m.ToolError})
		}
	}
	writeJSON(w, map[string]any{"sessionID": sess.ID, "messages": msgs})
}

func (s *Server) handleSend(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Text  string `json:"text"`
		Agent bool   `json:"agent"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || strings.TrimSpace(req.Text) == "" {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	s.sess.AgentMode = req.Agent
	s.mu.Unlock()
	// slash commands execute server-side; nothing goes to the model
	if s.runWebCommand(req.Text) {
		writeJSON(w, map[string]any{"ok": true, "command": true})
		return
	}
	refs, missing := tui.CollectRefs(req.Text, s.tools.WorkDir)
	if len(missing) > 0 {
		s.broadcastEvent("notice", "missing files: "+strings.Join(missing, ", "), nil)
	}
	s.mu.Lock()
	s.sess.AgentMode = req.Agent
	images := s.pendingImages
	s.pendingImages = nil
	s.mu.Unlock()
	if err := s.startTurn(req.Text, refs, images); err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

// handleCommands lists the web slash commands for the frontend popup.
func (s *Server) handleCommands(w http.ResponseWriter, r *http.Request) {
	type cinfo struct {
		Name  string `json:"name"`
		Usage string `json:"usage"`
		Desc  string `json:"desc"`
	}
	out := []cinfo{
		{"new", "", "新建会话"},
		{"clear", "", "删除当前会话（消息 + 文件）"},
		{"compact", "", "摘要压缩上下文"},
		{"model", "[name]", "切换模型"},
		{"provider", "[local|name]", "切换服务商档案"},
		{"system", "[prompt]", "设置会话系统提示"},
		{"temp", "[0-2]", "设置温度"},
		{"ctx", "[tokens]", "设置上下文窗口"},
		{"think", "[on|off]", "思考模式开关"},
		{"agent", "[on|off]", "agent 模式开关"},
		{"title", "[text]", "重命名会话"},
		{"help", "", "显示命令列表"},
	}
	writeJSON(w, out)
}

// /api/files lists workspace files for the @ picker.
func (s *Server) handleFiles(w http.ResponseWriter, r *http.Request) {
	query := strings.ToLower(r.URL.Query().Get("query"))
	files := tui.WorkspaceFiles(s.tools.WorkDir, 200)
	var out []string
	for _, f := range files {
		if query == "" || strings.Contains(strings.ToLower(f), query) {
			out = append(out, f)
			if len(out) >= 8 {
				break
			}
		}
	}
	writeJSON(w, out)
}

// runWebCommand executes web-safe slash commands server-side; returns true
// when the text was a command (nothing is sent to the model).
func (s *Server) runWebCommand(text string) bool {
	if !strings.HasPrefix(text, "/") {
		return false
	}
	name, rest, _ := strings.Cut(strings.TrimSpace(text[1:]), " ")
	rest = strings.TrimSpace(rest)
	notice := func(format string, a ...any) {
		s.broadcastEvent("notice", fmt.Sprintf(format, a...), nil)
	}
	switch name {
	case "new":
		s.resetSession()
		notice("new session started")
	case "clear":
		_ = s.store.Delete(s.activeSessionID())
		s.resetSession()
		notice("session deleted — started a fresh one")
	case "compact":
		if summary, err := s.compactNow(); err != nil {
			notice("compact failed: %v", err)
		} else {
			s.applyCompact(summary)
			notice("context compacted (%d chars summary)", len(summary))
		}
	case "model":
		if rest == "" {
			notice("usage: /model <name> (current: %s)", s.currentModel())
			break
		}
		s.setModel(rest)
		notice("model → %s", rest)
	case "provider":
		if rest == "" || rest == "local" {
			client, _, err := provider.EnsureBackend(s.cfg.OllamaURL, s.cfg.APIStyle, s.cfg.Discovery, 6*time.Second)
			if err != nil {
				notice("local backend: %v", err)
				break
			}
			s.setClient(client, "")
			notice("provider → local (%s)", s.currentModel())
			break
		}
		p, ok := s.cfg.Providers[rest]
		if !ok {
			known := "local"
			for n := range s.cfg.Providers {
				known += " " + n
			}
			notice("unknown provider: %s (known: %s)", rest, known)
			break
		}
		s.setClient(provider.NewWithKey(p.URL, "openai", p.ResolvedKey()), rest)
		if p.Model != "" {
			s.setModel(p.Model)
		}
		notice("provider → %s (cloud: file contents will be uploaded)", rest)
	case "system":
		s.setPrompt(rest)
		notice("system prompt set (%d chars)", len(rest))
	case "temp":
		v, err := strconv.ParseFloat(rest, 64)
		if err != nil || v < 0 || v > 2 {
			notice("usage: /temp <0-2>")
			break
		}
		s.setTemperature(v)
		notice("temperature = %.2f", v)
	case "ctx":
		v, err := strconv.Atoi(rest)
		if err != nil || v < 2048 {
			notice("usage: /ctx <tokens ≥2048>")
			break
		}
		s.setNumCtx(v)
		notice("context window = %d", v)
	case "think":
		on := rest != "off"
		s.setThink(on)
		notice("thinking %s", map[bool]string{true: "on", false: "off"}[on])
	case "agent":
		on := rest != "off"
		s.setAgentModePersist(on)
		notice("agent mode %s", map[bool]string{true: "on", false: "off"}[on])
	case "title":
		if rest == "" {
			notice("usage: /title <text>")
			break
		}
		s.setTitle(rest)
		notice("title → %s", rest)
	case "retry":
		if err := s.retryTurn(); err != nil {
			notice("%v", err)
		}
	case "save", "export":
		s.broadcastEvent("export", "", map[string]any{"trigger": true})
		notice("export started — check your downloads")
	case "img":
		notice("web: use the 📷 button beside the input to attach an image")
	case "help":
		notice("commands: /new /clear /compact /retry /model /provider /system /temp /ctx /think /agent /title /save /help")
	default:
		notice("unknown command: /%s — type /help", name)
	}
	return true
}

// ---- small state setters (web equivalents of the TUI commands) ----

func (s *Server) setClient(client provider.Client, name string) {
	s.mu.Lock()
	s.client = client
	s.activeProvider = name
	s.cfg.ActiveProvider = name
	s.mu.Unlock()
	_ = s.cfg.Save()
	s.broadcastEvent("provider", name, nil)
}

func (s *Server) setModel(m string) {
	s.mu.Lock()
	s.sess.Model = m
	sess := s.sess
	s.mu.Unlock()
	_ = s.store.Save(sess)
	s.broadcastEvent("model", m, nil)
}

func (s *Server) currentModel() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sess.Model
}

func (s *Server) setPrompt(v string) {
	s.mu.Lock()
	s.sess.SetPrompt(v)
	sess := s.sess
	s.mu.Unlock()
	_ = s.store.Save(sess)
}

func (s *Server) setTemperature(v float64) {
	s.mu.Lock()
	s.sess.Params.Temperature = v
	sess := s.sess
	s.mu.Unlock()
	_ = s.store.Save(sess)
}

func (s *Server) setNumCtx(v int) {
	s.mu.Lock()
	s.sess.Params.NumCtx = v
	sess := s.sess
	s.mu.Unlock()
	_ = s.store.Save(sess)
}

func (s *Server) setThink(on bool) {
	s.mu.Lock()
	s.sess.Params.Think = &on
	sess := s.sess
	s.mu.Unlock()
	_ = s.store.Save(sess)
}

func (s *Server) setAgentModePersist(on bool) {
	s.mu.Lock()
	s.sess.AgentMode = on
	sess := s.sess
	s.mu.Unlock()
	_ = s.store.Save(sess)
}

func (s *Server) setTitle(v string) {
	s.mu.Lock()
	s.sess.Title = v
	sess := s.sess
	s.mu.Unlock()
	_ = s.store.Save(sess)
}

// compactNow summarizes the active session (web mirror of /compact).
func (s *Server) compactNow() (string, error) {
	s.mu.Lock()
	sess, client := s.sess, s.client
	s.mu.Unlock()
	var b strings.Builder
	for _, m := range sess.Snapshot() {
		switch m.Role {
		case core.RoleUser:
			fmt.Fprintf(&b, "\n[user]: %s\n", m.Content)
		case core.RoleAssistant:
			if m.Content != "" {
				fmt.Fprintf(&b, "[assistant]: %s\n", m.Content)
			}
		}
	}
	req := ollama.ChatRequest{
		Model: sess.Model,
		Messages: []ollama.Message{
			{Role: "system", Content: "Summarize the conversation below: keep key facts, decisions, file paths, code names and pending tasks. Be concise but complete, in the same language as the conversation. Output only the summary."},
			{Role: "user", Content: "Conversation:\n" + firstNLines(b.String(), 400)},
		},
		Options:   &ollama.Options{NumPredict: 2048},
		KeepAlive: s.cfg.Context.KeepAlive,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	resp, err := client.Chat(ctx, req)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(resp.Message.Content), nil
}

// firstNLines caps text at n lines (line-based, unlike byte caps which
// butcher CJK summaries).
func firstNLines(s string, n int) string {
	lines := strings.Split(s, "\n")
	if len(lines) <= n {
		return s
	}
	return strings.Join(lines[:n], "\n") + "\n…"
}

// applyCompact swaps the transcript for the summary.
func (s *Server) applyCompact(summary string) {
	s.mu.Lock()
	sess := s.sess
	sess.SetMessages([]core.Message{{Role: core.RoleUser, Content: "[conversation summary]\n" + summary}})
	sess.Compacted = true
	sess.AutoTitle()
	s.mu.Unlock()
	_ = s.store.Save(sess)
	s.broadcastEvent("reset", "", nil)
}

func (s *Server) handleInterrupt(w http.ResponseWriter, r *http.Request) {
	s.interrupt()
	writeJSON(w, map[string]any{"ok": true})
}

func (s *Server) interrupt() {
	s.mu.Lock()
	gen := s.gen
	s.mu.Unlock()
	if gen != nil {
		gen.cancel()
	}
}

func (s *Server) handleAgentMode(w http.ResponseWriter, r *http.Request) {
	var req struct {
		On bool `json:"on"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	s.mu.Lock()
	s.sess.AgentMode = req.On
	sess := s.sess
	s.mu.Unlock()
	_ = s.store.Save(sess)
	writeJSON(w, map[string]any{"ok": true})
}

func (s *Server) handleApprove(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID       string `json:"id"`
		Decision string `json:"decision"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	dec := agent.DecisionOnce
	switch req.Decision {
	case "session":
		dec = agent.DecisionSession
	case "deny":
		dec = agent.DecisionDeny
	}
	if !s.decideApproval(req.ID, dec) {
		http.Error(w, "no pending approval", http.StatusNotFound)
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

// handleRetry pops the last user turn and re-runs it.
func (s *Server) handleRetry(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	sess := s.sess
	s.mu.Unlock()
	text, ok := sess.PopLastUserTurn()
	if !ok {
		http.Error(w, "nothing to retry", http.StatusBadRequest)
		return
	}
	sess.TrimToMax(sess.NumCtx(s.cfg.Context.NumCtx))
	_ = s.store.Save(sess)
	s.broadcastEvent("reset", "", nil)
	if err := s.startTurn(text, nil, nil); err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

// handleImg accepts an image upload (multipart "file", ≤8MB) and attaches
// it to the next message.
func (s *Server) handleImg(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 8<<20)
	file, _, err := r.FormFile("file")
	if err != nil {
		http.Error(w, "missing file field", http.StatusBadRequest)
		return
	}
	defer file.Close()
	data, err := io.ReadAll(file)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if !isImageUpload(data) {
		http.Error(w, "only png/jpeg/webp/gif images are supported", http.StatusUnsupportedMediaType)
		return
	}
	s.mu.Lock()
	s.pendingImages = append(s.pendingImages, base64.StdEncoding.EncodeToString(data))
	n := len(s.pendingImages)
	s.mu.Unlock()
	writeJSON(w, map[string]any{"ok": true, "count": n})
}

func isImageUpload(data []byte) bool {
	if len(data) < 8 {
		return false
	}
	png := []byte{0x89, 'P', 'N', 'G'}
	if string(data[:4]) == string(png) {
		return true
	}
	if data[0] == 0xFF && data[1] == 0xD8 {
		return true
	}
	if string(data[:4]) == "RIFF" && len(data) >= 12 && string(data[8:12]) == "WEBP" {
		return true
	}
	return strings.HasPrefix(string(data), "GIF8")
}

// handleExport downloads the active session as Markdown.
func (s *Server) handleExport(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	sess := s.sess
	s.mu.Unlock()
	md := tui.ExportMarkdown(sess)
	w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
	w.Header().Set("Content-Disposition",
		fmt.Sprintf("attachment; filename=\"hearth-%s-%s.md\"", sanitizeName(sess.Title), sess.ID))
	_, _ = w.Write([]byte(md))
}

func sanitizeName(s string) string {
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

// retryTurn pops the last user turn and re-runs it (web /retry).
func (s *Server) retryTurn() error {
	s.mu.Lock()
	sess := s.sess
	s.mu.Unlock()
	text, ok := sess.PopLastUserTurn()
	if !ok {
		return fmt.Errorf("nothing to retry")
	}
	sess.TrimToMax(sess.NumCtx(s.cfg.Context.NumCtx))
	_ = s.store.Save(sess)
	s.broadcastEvent("reset", "", nil)
	return s.startTurn(text, nil, nil)
}

// handleEvents streams turn events as Server-Sent Events.
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	ch := make(chan []byte, 128)
	s.hubMu.Lock()
	s.subs[ch] = struct{}{}
	s.hubMu.Unlock()
	defer func() {
		s.hubMu.Lock()
		delete(s.subs, ch)
		s.hubMu.Unlock()
	}()
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	fmt.Fprint(w, ": connected\n\n")
	flusher.Flush()
	for {
		select {
		case <-r.Context().Done():
			return
		case b := <-ch:
			fmt.Fprintf(w, "data: %s\n\n", b)
			flusher.Flush()
		}
	}
}

// Run starts the localhost server and opens the browser.
func (s *Server) Run(addr string, openBrowser bool) error {
	srv := &http.Server{
		Addr:              addr,
		Handler:           s.mux(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	url := "http://" + addr + "/"
	fmt.Fprintf(os.Stderr, "hearth web UI → %s (Ctrl+C 退出)\n", url)
	if openBrowser {
		go func() {
			time.Sleep(400 * time.Millisecond)
			switch runtime.GOOS {
			case "windows":
				_ = exec.Command("rundll32", "url.dll,FileProtocolHandler", url).Start()
			default:
				_ = exec.Command("xdg-open", url).Start()
			}
		}()
	}
	return srv.ListenAndServe()
}
