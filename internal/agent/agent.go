// Package agent implements the agentic tool-calling harness.
package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"

	"hearth/internal/core"
	"hearth/internal/ollama"
	"hearth/internal/tools"
)

// ChatClient is what the agent needs from an API client (mockable in tests).
type ChatClient interface {
	ChatStream(ctx context.Context, req ollama.ChatRequest, fn func(ollama.ChatResponse) error) error
}

// EventKind classifies agent events.
type EventKind int

const (
	EvThinking   EventKind = iota // Text: thinking delta
	EvContent                     // Text: content delta
	EvToolStart                   // ToolName/ToolArgs set
	EvToolResult                  // Text: result, ToolErr flag
	EvApproval                    // Approval set — caller must decide
	EvTurnDone                    // turn complete; Message is the final assistant msg
	EvError                       // Text: error message
	EvTrimmed                     // Text: older messages trimmed to fit the context window
)

// Usage accumulates per-run statistics.
type Usage struct {
	PromptTokens int
	OutputTokens int
	EvalNs       int64 // server-measured generation time (sum over calls)
	Steps        int
	ToolCalls    int
	Interrupted  bool
	Failed       bool // the turn ended with an error — no final answer recorded
	Truncated    bool // the final answer was cut by a token limit (done_reason=length)
	Duration     time.Duration
}

// TPS is the server-measured generation throughput across the whole run.
func (u Usage) TPS() float64 {
	if u.EvalNs <= 0 || u.OutputTokens == 0 {
		return 0
	}
	return float64(u.OutputTokens) / (float64(u.EvalNs) / 1e9)
}

// Event is emitted on the agent's event stream.
type Event struct {
	Kind     EventKind
	Text     string
	ToolName string
	ToolArgs string
	ToolErr  bool
	Approval *ApprovalRequest
	Message  *core.Message
	Usage    Usage
}

// Agent runs the tool-calling loop against a model.
type Agent struct {
	Client     ChatClient
	Model      string
	Opts       ollama.Options
	Think      bool
	Tools      *tools.Registry
	Perms      *Manager
	MaxSteps   int
	MaxToolLen int
	SystemHint string // extra user/system prompt prepended to agent prompt
	Sess       *core.Session
	Memory     *tools.MemoryStore // nil = long-term memory off
	KeepAlive  string             // model residency hint (native API)
}

// Run executes one agent turn. The caller must have recorded the user's
// message in Sess already; Run appends assistant/tool messages and emits
// progress on the returned channel, which closes when the turn ends.
// Cancelling ctx interrupts: partial content is preserved.
func (a *Agent) Run(ctx context.Context) <-chan Event {
	ch := make(chan Event, 64)
	go func() {
		defer close(ch)
		start := time.Now()
		var usage Usage
		emit := func(e Event) { ch <- e }

		apiTools := ollamaTools(a.Tools)
		finish := func(msg *core.Message) {
			usage.Duration = time.Since(start)
			emit(Event{Kind: EvTurnDone, Message: msg, Usage: usage})
		}

		// 2. tool loop
		for step := 0; step < a.MaxSteps; step++ {
			usage.Steps = step + 1
			content, thinking, calls, err := a.streamTurn(ctx, apiTools, &usage, emit)
			if err != nil {
				if ctx.Err() != nil {
					msg, _ := a.appendPartial(core.RoleAssistant, content, thinking, true)
					usage.Interrupted = true
					finish(msg)
					return
				}
				// context overflow → trim and retry once
				if ollama.IsContextOverflow(err) {
					if a.Sess.TrimToMax(a.Sess.NumCtx(a.Opts.NumCtx)) {
						emit(Event{Kind: EvTrimmed, Text: "context overflow — trimmed older turns, retrying"})
						step-- // retry this step without consuming budget
						continue
					}
				}
				emit(Event{Kind: EvError, Text: err.Error()})
				usage.Failed = true // no final answer will follow this turn
				// preserve any partial output; an empty response is not recorded
				msg, _ := a.appendPartial(core.RoleAssistant, content, thinking, false)
				finish(msg)
				return
			}

			// fallback: parse tool calls from plain text when the model
			// doesn't use the native channel (common with small models)
			if len(calls) == 0 && content != "" {
				if tc, clean := parseTextToolCalls(content); len(tc) > 0 {
					calls = tc
					content = clean
				}
			}

			// small models sometimes spend the final step entirely in the
			// thinking field — promote it so the answer is visible
			// (completed messages render content only, never thinking)
			if content == "" && thinking != "" && len(calls) == 0 {
				content, thinking = thinking, ""
			}

			msg := core.Message{
				Role:      core.RoleAssistant,
				Content:   content,
				Thinking:  thinking,
				CreatedAt: time.Now(),
			}
			msg.ToolCalls = append(msg.ToolCalls, calls...)

			if len(calls) == 0 {
				if strings.TrimSpace(content) == "" {
					// recording an empty message would make the transcript
					// just stop at the last tool result with no explanation
					usage.Failed = true
					emit(Event{Kind: EvError, Text: fmt.Sprintf("model returned an empty response (step %d/%d)", step+1, a.MaxSteps)})
					finish(nil)
					return
				}
				msg.Truncated = usage.Truncated
				a.Sess.Add(msg)
				finish(&msg) // natural completion: final answer
				return
			}
			a.Sess.Add(msg)

			// 3. execute tools
			for _, call := range calls {
				usage.ToolCalls++
				a.execTool(ctx, call, emit)
			}

			// context guard between steps: one agent turn can span dozens of
			// messages, so trim (cheap, no model call) before the next call
			// would overflow — compaction still waits for turn end
			if n := a.Sess.NumCtx(a.Opts.NumCtx); a.Sess.UsedTokens() > n*90/100 {
				if a.Sess.TrimToMax(n) {
					emit(Event{Kind: EvTrimmed, Text: "context guard — trimmed older exchanges to fit the window"})
				}
			}

			// step-budget nudge to keep small models on track
			if step+2 == a.MaxSteps {
				emit(Event{Kind: EvError, Text: fmt.Sprintf("step budget: %d/%d used — wrap up soon", step+1, a.MaxSteps)})
			}
		}

		// budget exhausted → one final no-tools call to summarize
		wrapUp := ollama.Message{
			Role:    "system",
			Content: "Tool budget exhausted. Summarize what you have done and what remains, as the final answer, without any tool calls.",
		}
		content, thinking, _, err := a.streamTurn(ctx, nil, &usage, emit, wrapUp)
		if ctx.Err() != nil {
			usage.Interrupted = true
		}
		if err != nil {
			emit(Event{Kind: EvError, Text: err.Error()})
			usage.Failed = ctx.Err() == nil
		}
		if content == "" && thinking != "" {
			content, thinking = thinking, ""
		}
		msg, ok := a.appendPartial(core.RoleAssistant, content, thinking, ctx.Err() != nil)
		if ok {
			msg.Truncated = usage.Truncated
		}
		if !ok && err == nil && ctx.Err() == nil {
			usage.Failed = true
			emit(Event{Kind: EvError, Text: "wrap-up: model returned an empty response"})
		}
		finish(msg)
	}()
	return ch
}

// streamTurn streams one model response, emitting deltas and returning the
// accumulated content, thinking, and native tool calls. extra messages are
// appended transiently (never persisted).
func (a *Agent) streamTurn(ctx context.Context, apiTools []ollama.Tool, usage *Usage, emit func(Event), extra ...ollama.Message) (content, thinking string, calls []core.ToolCall, err error) {
	req := ollama.ChatRequest{
		Model:     a.Model,
		Messages:  append(a.apiMessages(), extra...),
		Options:   &a.Opts,
		KeepAlive: a.KeepAlive,
	}
	if a.Think {
		t := true
		req.Think = &t
	} else {
		f := false
		req.Think = &f
	}
	if len(apiTools) > 0 {
		req.Tools = apiTools
	}
	if err = a.doStream(ctx, req, usage, emit, &content, &thinking, &calls); err == nil {
		return content, thinking, calls, nil
	}
	// model without thinking support → retry once without the flag
	if req.Think != nil && strings.Contains(err.Error(), "does not support thinking") {
		req.Think = nil
		content, thinking, calls = "", "", nil
		err = a.doStream(ctx, req, usage, emit, &content, &thinking, &calls)
	}
	return content, thinking, calls, err
}

// doStream runs one streaming request, accumulating output and emitting
// deltas. shared buffers let the think-fallback retry start clean.
func (a *Agent) doStream(ctx context.Context, req ollama.ChatRequest, usage *Usage, emit func(Event), content, thinking *string, calls *[]core.ToolCall) error {
	var bufC, bufT strings.Builder
	err := a.Client.ChatStream(ctx, req, func(r ollama.ChatResponse) error {
		if r.Message.Thinking != "" {
			bufT.WriteString(r.Message.Thinking)
			emit(Event{Kind: EvThinking, Text: r.Message.Thinking})
		}
		if r.Message.Content != "" {
			bufC.WriteString(r.Message.Content)
			emit(Event{Kind: EvContent, Text: r.Message.Content})
		}
		if len(r.Message.ToolCalls) > 0 {
			for _, tc := range r.Message.ToolCalls {
				args := fixArgs(tc.Function.Arguments)
				*calls = append(*calls, core.ToolCall{ID: fmt.Sprintf("call_%d", len(*calls)), Name: tc.Function.Name, Args: args})
			}
		}
		if r.Done {
			usage.PromptTokens += r.PromptEvalCount
			usage.OutputTokens += r.EvalCount
			usage.EvalNs += r.EvalDuration
			// the LAST call's reason wins — it produced the final answer
			usage.Truncated = r.DoneReason == "length"
		}
		return nil
	})
	*content, *thinking = bufC.String(), bufT.String()
	return err
}

// execTool handles one tool call: permissions, execution, recording.
func (a *Agent) execTool(ctx context.Context, call core.ToolCall, emit func(Event)) {
	argsJSON, _ := json.Marshal(call.Args)
	emit(Event{Kind: EvToolStart, ToolName: call.Name, ToolArgs: string(argsJSON)})

	result, toolErr := "", error(nil)

	t, ok := tools.Get(call.Name)
	if !ok {
		toolErr = fmt.Errorf("unknown tool %q; available: %s", call.Name, strings.Join(tools.Names(), ", "))
	} else {
		summary := summarizeCall(call)
		if a.Perms.NeedsApproval(call.Name, t.Category, summary, prettyJSON(call.Args)) {
			req := &ApprovalRequest{
				ID:       call.ID,
				Tool:     call.Name,
				Summary:  summary,
				Detail:   prettyJSON(call.Args),
				Category: t.Category,
				Respond:  make(chan ApprovalDecision, 1),
			}
			emit(Event{Kind: EvApproval, Approval: req})
			select {
			case dec := <-req.Respond:
				switch dec {
				case DecisionSession:
					a.Perms.ApproveSession(call.Name)
				case DecisionDeny:
					toolErr = fmt.Errorf("user denied this tool call")
				}
			case <-ctx.Done():
				toolErr = fmt.Errorf("cancelled")
			}
		}
	}

	if toolErr == nil && t.Fn != nil {
		result, toolErr = t.Fn(ctx, a.Tools, call.Args)
	}

	out := result
	if toolErr != nil {
		out = "ERROR: " + toolErr.Error()
	}
	out = truncateRunes(out, a.MaxToolLen)
	if toolErr == nil && out == "" {
		out = "(no output)"
	}

	a.Sess.Add(core.Message{
		Role:      core.RoleTool,
		Content:   out,
		ToolName:  call.Name,
		ToolError: toolErr != nil,
		CreatedAt: time.Now(),
	})
	emit(Event{Kind: EvToolResult, ToolName: call.Name, Text: out, ToolErr: toolErr != nil})
}

// appendPartial records an assistant message that may be partial/interrupted.
// Returns false when there is nothing worth recording (empty output).
func (a *Agent) appendPartial(role core.Role, content, thinking string, interrupted bool) (*core.Message, bool) {
	if strings.TrimSpace(content) == "" && strings.TrimSpace(thinking) != "" {
		// thinking-only replies are invisible once completed — promote
		content, thinking = thinking, ""
	}
	if strings.TrimSpace(content) == "" && strings.TrimSpace(thinking) == "" {
		return nil, false
	}
	m := a.Sess.Add(core.Message{
		Role:        role,
		Content:     content,
		Thinking:    thinking,
		Interrupted: interrupted,
	})
	return &m, true
}

// apiMessages converts session history into API messages. Layout is chosen
// for Ollama's prompt prefix cache: the STABLE part (session prompt + rules)
// leads, history follows unchanged between steps, and the VOLATILE parts
// (scratchpad + long-term memory) ride in a system message at the tail so
// each agent step only re-evaluates a small suffix.
func (a *Agent) apiMessages() []ollama.Message {
	base := a.Sess.Prompt()
	if base != "" {
		base += "\n\n"
	}
	base += systemPrompt(a.Tools.WorkDir, a.MaxSteps)

	out := []ollama.Message{{Role: "system", Content: base}}
	if a.SystemHint != "" {
		out = append(out, ollama.Message{Role: "system", Content: a.SystemHint})
	}

	// count tool messages from the end to decide staleness
	toolSeen := 0
	type tagged struct {
		m     core.Message
		stale bool
	}
	var taggedMsgs []tagged
	msgSnap := a.Sess.Snapshot()
	for i := len(msgSnap) - 1; i >= 0; i-- {
		m := msgSnap[i]
		if m.Role == core.RoleTool {
			toolSeen++
			taggedMsgs = append(taggedMsgs, tagged{m, toolSeen > 6})
		} else {
			taggedMsgs = append(taggedMsgs, tagged{m, false})
		}
	}
	staleLen := a.MaxToolLen / 3
	for i := len(taggedMsgs) - 1; i >= 0; i-- {
		t := taggedMsgs[i]
		m := t.m
		content := m.Content
		if t.stale && len(content) > staleLen {
			content = content[:staleLen] + "\n…[older tool result truncated]"
		}
		switch m.Role {
		case core.RoleUser:
			// @file refs: attach the snapshotted content blocks (same as the
			// TUI chat path — without this the agent silently drops them)
			if len(m.Refs) > 0 {
				var blocks []string
				for _, r := range m.Refs {
					blocks = append(blocks, core.RefContextBlock(r))
				}
				content = content + "\n\n" + strings.Join(blocks, "\n\n")
			}
			out = append(out, ollama.Message{Role: "user", Content: content, Images: m.Images})
		case core.RoleAssistant:
			om := ollama.Message{Role: "assistant", Content: content}
			for _, tc := range m.ToolCalls {
				om.ToolCalls = append(om.ToolCalls, ollama.ToolCall{
					Function: ollama.ToolCallFunc{Name: tc.Name, Arguments: tc.Args},
				})
			}
			out = append(out, om)
		case core.RoleTool:
			out = append(out, ollama.Message{Role: "tool", Content: content, ToolName: m.ToolName})
		case core.RoleSystem:
			out = append(out, ollama.Message{Role: "system", Content: content})
		}
	}

	// volatile context rides at the tail (prefix-cache friendly): scratchpad
	// and long-term memory change between steps, so they must not sit in
	// front of the cached history.
	if tail := a.volatileContext(); tail != "" {
		out = append(out, ollama.Message{Role: "system", Content: tail})
	}
	return out
}

// volatileContext builds the tail system message: session scratchpad
// (short-term memory) plus recent long-term memory facts.
func (a *Agent) volatileContext() string {
	var parts []string
	if sp := a.Sess.Notes(); sp != "" {
		if len(sp) > 1200 {
			sp = sp[:1200] + "\n…[notes truncated]"
		}
		parts = append(parts, "## Session notes (your scratchpad)\n"+sp)
	}
	if a.Memory != nil {
		if facts, err := a.Memory.Search("", 8); err == nil && len(facts) > 0 {
			parts = append(parts, "## Long-term memory (recent facts)\n"+strings.Join(facts, "\n"))
		}
	}
	if len(parts) == 0 {
		return ""
	}
	return strings.Join(parts, "\n\n")
}

// systemPrompt is deliberately compact — small models degrade on long prompts.
func systemPrompt(workDir string, maxSteps int) string {
	return fmt.Sprintf(`You are a capable assistant working in the user's terminal.

Working directory: %s
You can call tools. Rules:
1. Think first, then act. Use tools only when needed to answer correctly.
2. Tool call arguments must be valid JSON matching the schema. Make one tool call, wait for its result, then continue.
3. Read before writing: inspect files (read_file/grep/glob) before editing.
4. Verify your edits when correctness matters (re-read the file).
5. If a tool errors, read the error and adjust — do not repeat the same call.
6. Use calculator for any non-trivial math; never guess numbers you can compute.
7. For multi-step tasks, first write a short plan with note(write), update it as you go.
8. Use memory_save for facts worth remembering across sessions; memory_recall to check what you already know.
9. web_search finds pages, web_fetch reads one URL. Cite URLs in your answer.
10. Budget: at most %d tool steps. Finish within it. Final answer: concise Markdown summary.`, workDir, maxSteps)
}

// ollamaTools converts the registry into API tool definitions.
func ollamaTools(reg *tools.Registry) []ollama.Tool {
	var out []ollama.Tool
	for _, t := range tools.All() {
		out = append(out, ollama.Tool{
			Type: "function",
			Function: ollama.ToolFunc{
				Name:        t.Name,
				Description: t.Desc,
				Parameters:  t.Schema,
			},
		})
	}
	return out
}

// fixArgs normalizes model-returned arguments (some models emit a JSON string).
func fixArgs(args map[string]any) map[string]any {
	if args == nil {
		return map[string]any{}
	}
	return args
}

// summarizeCall builds a one-line human summary for permission prompts.
func summarizeCall(call core.ToolCall) string {
	switch call.Name {
	case "bash":
		return core.ToString(call.Args["command"])
	case "write_file", "edit_file", "read_file":
		return core.ToString(call.Args["path"])
	default:
		return call.Name
	}
}

func prettyJSON(v any) string {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Sprintf("%v", v)
	}
	s := string(b)
	if len(s) > 2000 {
		s = s[:2000] + "\n…"
	}
	return s
}

func truncateRunes(s string, n int) string {
	if n <= 0 || len([]rune(s)) <= n {
		return s
	}
	r := []rune(s)
	return string(r[:n]) + "\n…[truncated]"
}

// ---- text-protocol fallback parsing ----

var (
	reToolCallTag = regexp.MustCompile(`(?s)<tool_call>\s*(.*?)\s*</tool_call>`)
	reFencedJSON  = regexp.MustCompile("(?s)```(?:json|tool_call|tool)\\s*\n(\\{.*?\\})\\s*```")
)

// parseTextToolCalls extracts tool calls the model wrote as plain text and
// returns them with those blocks removed from the content.
func parseTextToolCalls(content string) ([]core.ToolCall, string) {
	var calls []core.ToolCall
	clean := content

	extract := func(src string, re *regexp.Regexp) {
		ms := re.FindAllStringSubmatch(src, -1)
		for _, m := range ms {
			if c, ok := decodeToolJSON(m[1]); ok {
				calls = append(calls, c)
				clean = strings.Replace(clean, m[0], "", 1)
			}
		}
	}

	extract(content, reToolCallTag)
	if len(calls) == 0 {
		extract(content, reFencedJSON)
	}
	// bare JSON object as the whole content: {"name": "...", "arguments": {...}}
	if len(calls) == 0 {
		t := strings.TrimSpace(content)
		if strings.HasPrefix(t, "{") && strings.HasSuffix(t, "}") &&
			strings.Contains(t, `"name"`) && strings.Contains(t, `"arguments"`) {
			if c, ok := decodeToolJSON(t); ok {
				calls = append(calls, c)
				clean = ""
			}
		}
	}
	return calls, strings.TrimSpace(clean)
}

// decodeToolJSON parses {"name": ..., "arguments": {...}} (arguments may be
// an object or a JSON string).
func decodeToolJSON(s string) (core.ToolCall, bool) {
	var raw struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal([]byte(s), &raw); err != nil || raw.Name == "" {
		return core.ToolCall{}, false
	}
	args := map[string]any{}
	if len(raw.Arguments) > 0 {
		if err := json.Unmarshal(raw.Arguments, &args); err != nil {
			// arguments as JSON-encoded string
			var str string
			if json.Unmarshal(raw.Arguments, &str) == nil {
				_ = json.Unmarshal([]byte(str), &args)
			}
		}
	}
	if args == nil {
		args = map[string]any{}
	}
	return core.ToolCall{Name: raw.Name, Args: args}, true
}
