// Package cli implements the non-TUI commands (agent / ask / models).
package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"hearth/internal/agent"
	"hearth/internal/config"
	"hearth/internal/core"
	"hearth/internal/ollama"
	"hearth/internal/provider"
	"hearth/internal/tools"
)

// AgentOptions configures the headless agent run.
type AgentOptions struct {
	Task      string
	Model     string
	WorkDir   string
	Perms     string // override config permissions
	Yolo      bool
	MaxSteps  int
	JSON      bool   // NDJSON event output
	ShowThink bool   // stream thinking to stderr
	URL       string // ollama base url override
	APIStyle  string // ollama | openai | auto
	Provider  string // config provider profile (remote OpenAI-compatible)
}

// apiStyleFor resolves the effective API style (CLI flag wins over config).
func apiStyleFor(cfg *config.Config, override string) string {
	if override != "" {
		return override
	}
	return cfg.APIStyle
}

// searchBackend converts config into the tools search backend.
func searchBackend(cfg *config.Config) *tools.SearchBackend {
	return &tools.SearchBackend{
		Engine:     cfg.Search.Engine,
		SearxngURL: cfg.Search.SearxngURL,
		BraveKey:   cfg.Search.BraveKey,
		MaxResults: cfg.Search.MaxResults,
	}
}

// resolveBackend applies auto-discovery to a loaded config and returns a
// client. Found backends are persisted to config for future runs.
func resolveBackend(cfg *config.Config) provider.Client {
	client, found, err := provider.EnsureBackend(cfg.OllamaURL, cfg.APIStyle, cfg.Discovery, 6*time.Second)
	if err != nil {
		fmt.Fprintf(os.Stderr, "note: %v\n", err)
		return client
	}
	if found.URL != "" {
		cfg.OllamaURL = found.URL
		cfg.APIStyle = found.Style
		_ = cfg.Save()
		fmt.Fprintf(os.Stderr, "auto-detected %s at %s\n", found.Label, found.URL)
	}
	return client
}

// RunAgent executes one task headlessly and prints progress to stdout.
// Exit code 0 on success, 1 on failure.
func RunAgent(opts AgentOptions) int {
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintln(os.Stderr, "config error:", err)
		return 1
	}
	if opts.URL != "" {
		cfg.OllamaURL = opts.URL
	}
	wd := opts.WorkDir
	if wd == "" {
		wd, _ = os.Getwd()
	}
	if abs, err := filepath.Abs(wd); err == nil {
		wd = abs
	}

	permMode := cfg.Agent.Permissions
	if opts.Yolo {
		permMode = "yolo"
	} else if opts.Perms != "" {
		permMode = opts.Perms
	}

	cfg.APIStyle = apiStyleFor(cfg, opts.APIStyle)
	client := resolveBackend(cfg)
	model := opts.Model
	if opts.Provider != "" {
		p, ok := cfg.Providers[opts.Provider]
		if !ok {
			fmt.Fprintf(os.Stderr, "unknown provider: %s (known: local", opts.Provider)
			for n := range cfg.Providers {
				fmt.Fprintf(os.Stderr, " %s", n)
			}
			fmt.Fprintln(os.Stderr, ")")
			return 1
		}
		client = provider.NewWithKey(p.URL, "openai", p.ResolvedKey())
		if p.Model != "" && model == "" {
			model = p.Model
		}
	}
	if model == "" {
		model = cfg.DefaultModel
	}
	if model == "" {
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		models, err := client.Tags(ctx)
		cancel()
		if err != nil || len(models) == 0 {
			fmt.Fprintln(os.Stderr, "no model: pass -m or set default_model in config")
			return 1
		}
		model = models[0].Name
	}

	maxSteps := cfg.Agent.MaxSteps
	if opts.MaxSteps > 0 {
		maxSteps = opts.MaxSteps
	}

	var memory *tools.MemoryStore
	if cfg.Agent.Memory {
		memory = tools.NewMemoryStore(tools.MemoryPath())
	}
	registry := &tools.Registry{
		WorkDir:        wd,
		BashTimeoutSec: cfg.Agent.BashTimeout,
		ShellPref:      cfg.Agent.Shell,
		Memory:         memory,
		Search:         searchBackend(cfg),
	}
	perms := agent.NewManager(permMode, wd, cfg.Agent.AutoApprove, cfg.Agent.DenyPatterns)

	sess := core.NewSession(model)
	registry.Session = sess
	ag := &agent.Agent{
		Client:     client,
		Model:      model,
		Opts:       ollama.Options{NumCtx: cfg.Context.NumCtx, NumPredict: cfg.Generation.MaxTokens, Temperature: agentTemp(cfg)},
		Think:      cfg.Generation.Think == nil || *cfg.Generation.Think,
		Tools:      registry,
		Perms:      perms,
		MaxSteps:   maxSteps,
		MaxToolLen: cfg.Agent.MaxToolResult,
		Sess:       sess,
		SystemHint: agentHeadlessHint(),
		Memory:     memory,
		KeepAlive:  cfg.Context.KeepAlive,
	}

	sess.Add(core.Message{Role: core.RoleUser, Content: opts.Task})
	events := ag.Run(context.Background())

	approvedOnce := map[string]bool{}

	for ev := range events {
		switch ev.Kind {
		case agent.EvThinking:
			if opts.ShowThink && !opts.JSON {
				fmt.Fprint(os.Stderr, dim(ev.Text))
			}
		case agent.EvContent:
			if opts.JSON {
				emitJSON("content", map[string]any{"text": ev.Text})
			} else {
				fmt.Print(ev.Text)
			}
		case agent.EvToolStart:
			if opts.JSON {
				emitJSON("tool_start", map[string]any{"tool": ev.ToolName, "args": ev.ToolArgs})
			} else {
				fmt.Fprintf(os.Stderr, "\n%s tool %s %s\n", cyan("»"), bold(ev.ToolName), compact(ev.ToolArgs))
			}
		case agent.EvToolResult:
			if opts.JSON {
				emitJSON("tool_result", map[string]any{"tool": ev.ToolName, "output": ev.Text, "error": ev.ToolErr})
			} else {
				out := strings.TrimRight(ev.Text, "\n")
				if len(out) > 1200 {
					out = out[:1200] + " …[truncated]"
				}
				style := dim
				if ev.ToolErr {
					style = red
				}
				fmt.Fprintln(os.Stderr, style(indent(out, "    ")))
			}
		case agent.EvApproval:
			dec := decideHeadless(ev.Approval, approvedOnce)
			ev.Approval.Decide(dec)
		case agent.EvError:
			if opts.JSON {
				emitJSON("error", map[string]any{"message": ev.Text})
			} else {
				fmt.Fprintln(os.Stderr, red("error: "+ev.Text))
			}
		case agent.EvTrimmed:
			if opts.JSON {
				emitJSON("trimmed", map[string]any{"message": ev.Text})
			} else {
				fmt.Fprintln(os.Stderr, dim("note: "+ev.Text))
			}
		case agent.EvTurnDone:
			u := ev.Usage
			if opts.JSON {
				emitJSON("done", map[string]any{
					"steps": u.Steps, "tool_calls": u.ToolCalls,
					"prompt_tokens": u.PromptTokens, "output_tokens": u.OutputTokens,
					"seconds": u.Duration.Seconds(), "interrupted": u.Interrupted,
				})
			} else {
				tps := ""
				if u.TPS() > 0 {
					tps = fmt.Sprintf(" · %.1f tok/s", u.TPS())
				}
				fmt.Fprintf(os.Stderr, "\n%s %d steps · %d tool calls · %d tok in / %d tok out%s · %.1fs\n",
					green("✓"), u.Steps, u.ToolCalls, u.PromptTokens, u.OutputTokens, tps, u.Duration.Seconds())
			}
		}
	}
	return 0
}

// decideHeadless resolves approvals non-interactively: yolo handled earlier;
// otherwise auto-deny with a note (headless runs are unattended by default).
// Set OLLAMA_TUI_AGENT_APPROVE=1 to auto-approve everything once per tool.
func decideHeadless(req *agent.ApprovalRequest, approved map[string]bool) agent.ApprovalDecision {
	if os.Getenv("OLLAMA_TUI_AGENT_APPROVE") == "1" && !approved[req.Tool] {
		approved[req.Tool] = true
		fmt.Fprintf(os.Stderr, "auto-approved %s: %s\n", req.Tool, req.Summary)
		return agent.DecisionSession
	}
	fmt.Fprintf(os.Stderr, "denied (unattended): %s %s — set --yolo or OLLAMA_TUI_AGENT_APPROVE=1 to allow\n",
		req.Tool, req.Summary)
	return agent.DecisionDeny
}

// Ask runs a single no-tools question and streams the answer.
func Ask(question, model, url string) int {
	return AskWithStyle(question, model, url, "")
}

// AskWithStyle runs a single no-tools question with an explicit API style.
func AskWithStyle(question, model, url, style string) int {
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintln(os.Stderr, "config error:", err)
		return 1
	}
	if url != "" {
		cfg.OllamaURL = url
	}
	if style != "" {
		cfg.APIStyle = style
	}
	client := resolveBackend(cfg)
	if model == "" {
		model = cfg.DefaultModel
	}
	if model == "" {
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		models, err := client.Tags(ctx)
		cancel()
		if err != nil || len(models) == 0 {
			fmt.Fprintln(os.Stderr, "no model available")
			return 1
		}
		model = models[0].Name
	}
	think := cfg.Generation.Think == nil || *cfg.Generation.Think
	start := time.Now()
	req := ollama.ChatRequest{
		Model: model,
		Messages: []ollama.Message{
			{Role: "user", Content: question},
		},
		Options: &ollama.Options{
			Temperature: cfg.Generation.Temperature,
			NumCtx:      cfg.Context.NumCtx,
			NumPredict:  cfg.Generation.MaxTokens,
		},
		KeepAlive: cfg.Context.KeepAlive,
	}
	if !think {
		req.Think = &think
	}
	var sawContent, sawThinking strings.Builder
	var stats evalStats
	err = client.ChatStream(context.Background(), req, func(r ollama.ChatResponse) error {
		if r.Message.Content != "" {
			sawContent.WriteString(r.Message.Content)
			fmt.Print(r.Message.Content)
		}
		sawThinking.WriteString(r.Message.Thinking)
		if r.Done {
			stats = evalStats{prompt: r.PromptEvalCount, out: r.EvalCount, evalNs: r.EvalDuration, totalNs: r.TotalDuration}
		}
		return nil
	})
	// fallback: some replies land entirely in the thinking field
	if strings.TrimSpace(sawContent.String()) == "" && strings.TrimSpace(sawThinking.String()) != "" {
		fmt.Println(strings.TrimSpace(sawThinking.String()))
	}
	fmt.Println()
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	fmt.Fprintln(os.Stderr, stats.Line(time.Since(start)))
	return 0
}

// evalStats summarizes generation throughput reported by the server.
type evalStats struct {
	prompt  int
	out     int
	evalNs  int64
	totalNs int64
}

// TPS computes output tokens per second of generation (server-measured).
func (s evalStats) TPS() float64 {
	if s.evalNs <= 0 || s.out == 0 {
		return 0
	}
	return float64(s.out) / (float64(s.evalNs) / 1e9)
}

func (s evalStats) Line(elapsed time.Duration) string {
	if s.out == 0 {
		return fmt.Sprintf("✓ %d tok in / %d tok out · %.1fs", s.prompt, s.out, elapsed.Seconds())
	}
	return fmt.Sprintf("✓ %d tok in / %d tok out · %.1f tok/s (gen %.1fs, wall %.1fs)",
		s.prompt, s.out, s.TPS(), float64(s.evalNs)/1e9, elapsed.Seconds())
}

// Models lists installed models with details.
func Models(url, style string) int {
	cfg, _ := config.Load()
	if url != "" {
		cfg.OllamaURL = url
	}
	if style != "" {
		cfg.APIStyle = style
	}
	client := resolveBackend(cfg)
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	models, err := client.Tags(ctx)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	if len(models) == 0 {
		fmt.Println("no models — try: ollama pull qwen3.5:4b")
		return 0
	}
	for _, m := range models {
		caps := ""
		ctx2, cancel2 := context.WithTimeout(context.Background(), 4*time.Second)
		if show, err := client.Show(ctx2, m.Name); err == nil {
			caps = strings.Join(show.Capabilities, ",")
		}
		cancel2()
		size := "-"
		if m.Size > 0 {
			size = m.HumanSize()
		}
		quant := m.Details.QuantizationLevel
		if quant == "" {
			quant = "-"
		}
		fmt.Printf("%-28s %-8s %-8s %-8s %s\n", m.Name, orDash(m.Details.ParameterSize), quant, size, caps)
	}
	return 0
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// ---- output helpers ----

func emitJSON(kind string, data map[string]any) {
	data["event"] = kind
	b, _ := json.Marshal(data)
	fmt.Println(string(b))
}

func indent(s, pad string) string {
	lines := strings.Split(s, "\n")
	for i, ln := range lines {
		lines[i] = pad + ln
	}
	return strings.Join(lines, "\n")
}

func dim(s string) string   { return "\033[2m" + s + "\033[0m" }
func red(s string) string   { return "\033[31m" + s + "\033[0m" }
func green(s string) string { return "\033[32m" + s + "\033[0m" }
func cyan(s string) string  { return "\033[36m" + s + "\033[0m" }
func bold(s string) string  { return "\033[1m" + s + "\033[0m" }

func compact(jsonArgs string) string {
	var m map[string]any
	if err := json.Unmarshal([]byte(jsonArgs), &m); err == nil {
		var parts []string
		for k, v := range m {
			s := fmt.Sprintf("%v", v)
			if len(s) > 60 {
				s = s[:60] + "…"
			}
			parts = append(parts, k+"="+s)
		}
		return strings.Join(parts, " ")
	}
	return jsonArgs
}

// agentHeadlessHint appends headless-specific guidance to the system prompt.
func agentHeadlessHint() string {
	return "This is an unattended run. Print the final answer as Markdown. When you finish, end with a clear summary."
}

// agentTemp returns the temperature for agent runs.
func agentTemp(cfg *config.Config) float64 {
	if cfg.Agent.Temperature > 0 {
		return cfg.Agent.Temperature
	}
	return cfg.Generation.Temperature
}
