package agent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"hearth/internal/core"
	"hearth/internal/ollama"
	"hearth/internal/tools"
)

// fakeClient replays scripted stream chunks.
type fakeClient struct {
	script [][]ollama.ChatResponse // one script per ChatStream call
	errs   map[int]error           // call index → error (fails without streaming)
	calls  int
}

func (f *fakeClient) ChatStream(ctx context.Context, req ollama.ChatRequest, fn func(ollama.ChatResponse) error) error {
	if err, ok := f.errs[f.calls]; ok {
		f.calls++
		return err
	}
	if f.calls >= len(f.script) {
		return nil
	}
	s := f.script[f.calls]
	f.calls++
	for _, r := range s {
		if err := fn(r); err != nil {
			return err
		}
	}
	return nil
}

func chunk(content string) ollama.ChatResponse {
	return ollama.ChatResponse{Message: ollama.Message{Content: content}}
}

func toolCallChunk(name string, args map[string]any) ollama.ChatResponse {
	return ollama.ChatResponse{Message: ollama.Message{
		ToolCalls: []ollama.ToolCall{{Function: ollama.ToolCallFunc{Name: name, Arguments: args}}},
	}}
}

func newTestAgent(t *testing.T, client ChatClient) (*Agent, *core.Session) {
	t.Helper()
	sess := core.NewSession("test-model")
	sess.Params.NumCtx = 8192
	return &Agent{
		Client:     client,
		Model:      "test-model",
		Opts:       ollama.Options{NumCtx: 8192},
		Think:      false,
		Tools:      &tools.Registry{WorkDir: t.TempDir(), BashTimeoutSec: 5},
		Perms:      NewManager("yolo", t.TempDir(), nil, nil),
		MaxSteps:   5,
		MaxToolLen: 4000,
		Sess:       sess,
	}, sess
}

func TestAgentLoopNativeToolCalls(t *testing.T) {
	dir := t.TempDir()
	client := &fakeClient{script: [][]ollama.ChatResponse{
		{toolCallChunk("write_file", map[string]any{"path": "out.txt", "content": "hello agent"})},
		{toolCallChunk("read_file", map[string]any{"path": "out.txt"})},
		{chunk("done: file says "), chunk("hello agent")},
	}}
	ag, sess := newTestAgent(t, client)
	ag.Tools.WorkDir = dir

	sess.Add(core.Message{Role: core.RoleUser, Content: "make a file"})
	var gotToolStart, gotResult, done bool
	var lastResult string
	for ev := range ag.Run(context.Background()) {
		switch ev.Kind {
		case EvToolStart:
			gotToolStart = true
		case EvToolResult:
			gotResult = true
			lastResult = ev.Text
		case EvTurnDone:
			done = true
		}
	}
	if !gotToolStart || !gotResult || !done {
		t.Fatalf("events: toolStart=%v result=%v done=%v", gotToolStart, gotResult, done)
	}
	if client.calls != 3 {
		t.Errorf("expected 3 model calls, got %d", client.calls)
	}
	out, err := os.ReadFile(filepath.Join(ag.Tools.WorkDir, "out.txt"))
	if err != nil || !strings.Contains(string(out), "hello agent") {
		t.Errorf("file not written by agent: %v %q", err, out)
	}
	if !strings.Contains(lastResult, "hello agent") {
		t.Errorf("tool result event: %q", lastResult)
	}
	// history shape: user, assistant(tool), tool, assistant(tool), tool, assistant(final)
	roles := []core.Role{}
	for _, m := range sess.Messages {
		roles = append(roles, m.Role)
	}
	want := []core.Role{core.RoleUser, core.RoleAssistant, core.RoleTool, core.RoleAssistant, core.RoleTool, core.RoleAssistant}
	if len(roles) != len(want) {
		t.Fatalf("history roles = %v", roles)
	}
	for i := range want {
		if roles[i] != want[i] {
			t.Fatalf("history roles = %v, want %v", roles, want)
		}
	}
}

func TestAgentContextGuardTrimsMidTurn(t *testing.T) {
	big := strings.Repeat("output ", 400) // ≈700 estimated tokens per tool result
	client := &fakeClient{script: [][]ollama.ChatResponse{
		{toolCallChunk("note", map[string]any{"action": "append", "content": big})},
		{toolCallChunk("note", map[string]any{"action": "append", "content": big})},
		{chunk("done")},
	}}
	ag, sess := newTestAgent(t, client)
	ag.Tools.Session = sess  // note tool needs the scratchpad wiring
	sess.Params.NumCtx = 300 // tiny window → the guard trips between steps
	sess.Add(core.Message{Role: core.RoleUser, Content: "long task"})

	var trimmed bool
	for ev := range ag.Run(context.Background()) {
		if ev.Kind == EvTrimmed {
			trimmed = true
		}
	}
	if !trimmed {
		t.Fatal("expected a mid-turn context guard trim event")
	}
	roles := []core.Role{}
	for _, m := range sess.Messages {
		roles = append(roles, m.Role)
	}
	if roles[0] != core.RoleUser || roles[len(roles)-1] != core.RoleAssistant {
		t.Fatalf("history roles = %v, want user…assistant", roles)
	}
	for i, m := range sess.Messages {
		if m.Role == core.RoleTool && (i == 0 || sess.Messages[i-1].Role != core.RoleAssistant) {
			t.Fatalf("orphan tool result at %d (roles=%v)", i, roles)
		}
	}
}

func TestAgentThinkingOnlyFinalPromoted(t *testing.T) {
	// small models sometimes emit the final answer only in the thinking
	// field — it must be promoted to content or the reply is invisible
	client := &fakeClient{script: [][]ollama.ChatResponse{
		{toolCallChunk("write_file", map[string]any{"path": "out.txt", "content": "x"})},
		{{Message: ollama.Message{Thinking: "the answer is 42"}}, chunk("")},
	}}
	ag, sess := newTestAgent(t, client)
	ag.Tools.WorkDir = t.TempDir()
	sess.Add(core.Message{Role: core.RoleUser, Content: "do it"})

	var failed bool
	for ev := range ag.Run(context.Background()) {
		if ev.Kind == EvTurnDone {
			failed = ev.Usage.Failed
		}
	}
	if failed {
		t.Fatal("thinking-only reply must be promoted, not marked failed")
	}
	msgs := sess.Snapshot()
	if !strings.Contains(msgs[len(msgs)-1].Content, "the answer is 42") {
		t.Fatalf("promoted content missing: %q", msgs[len(msgs)-1].Content)
	}
}

func TestAgentEmptyFinalResponseFlagged(t *testing.T) {
	// empty final response must be flagged — otherwise the transcript just
	// stops at the last tool result and the UI reports success
	client := &fakeClient{script: [][]ollama.ChatResponse{
		{toolCallChunk("write_file", map[string]any{"path": "out.txt", "content": "x"})},
		{chunk("")},
	}}
	ag, sess := newTestAgent(t, client)
	ag.Tools.WorkDir = t.TempDir()
	sess.Add(core.Message{Role: core.RoleUser, Content: "do it"})

	var sawErr, failed bool
	for ev := range ag.Run(context.Background()) {
		switch ev.Kind {
		case EvError:
			sawErr = true
		case EvTurnDone:
			failed = ev.Usage.Failed
		}
	}
	if !sawErr || !failed {
		t.Fatalf("sawErr=%v failed=%v — empty final response must be flagged", sawErr, failed)
	}
	// no invisible empty assistant message left at the end of the transcript
	last := sess.Snapshot()[sess.Len()-1]
	if last.Role == core.RoleAssistant && strings.TrimSpace(last.Content) == "" && len(last.ToolCalls) == 0 {
		t.Fatal("empty assistant message recorded at the end")
	}
}

func TestAgentTruncatedFinalFlagged(t *testing.T) {
	// done_reason=length means the answer was cut by num_predict/num_ctx —
	// it must surface, not look like a complete reply
	client := &fakeClient{script: [][]ollama.ChatResponse{
		{toolCallChunk("write_file", map[string]any{"path": "out.txt", "content": "x"})},
		{
			{Message: ollama.Message{Content: "summary cut mid-sent"}},
			{Done: true, DoneReason: "length", Message: ollama.Message{}},
		},
	}}
	ag, sess := newTestAgent(t, client)
	ag.Tools.WorkDir = t.TempDir()
	sess.Add(core.Message{Role: core.RoleUser, Content: "do it"})

	var truncated bool
	var msgTruncated bool
	for ev := range ag.Run(context.Background()) {
		if ev.Kind == EvTurnDone {
			truncated = ev.Usage.Truncated
			if ev.Message != nil {
				msgTruncated = ev.Message.Truncated
			}
		}
	}
	if !truncated {
		t.Fatal("done_reason=length must set Usage.Truncated")
	}
	if !msgTruncated {
		t.Fatal("final message must carry the Truncated flag")
	}
	last := sess.Snapshot()[sess.Len()-1]
	if !last.Truncated {
		t.Fatal("session message lost the Truncated flag")
	}
}

func TestAgentTurnFailedFlag(t *testing.T) {
	client := &fakeClient{
		script: [][]ollama.ChatResponse{
			{toolCallChunk("write_file", map[string]any{"path": "out.txt", "content": "x"})},
		},
		errs: map[int]error{1: errors.New("connection reset by peer")},
	}
	ag, sess := newTestAgent(t, client)
	ag.Tools.WorkDir = t.TempDir()
	sess.Add(core.Message{Role: core.RoleUser, Content: "do it"})

	var sawErr, failed bool
	for ev := range ag.Run(context.Background()) {
		switch ev.Kind {
		case EvError:
			sawErr = true
		case EvTurnDone:
			failed = ev.Usage.Failed
		}
	}
	if !sawErr {
		t.Fatal("expected an EvError event")
	}
	if !failed {
		t.Fatal("turn error must be flagged in Usage.Failed — otherwise the UI treats the task as completed")
	}
}

func TestAgentTextFallbackParsing(t *testing.T) {
	client := &fakeClient{script: [][]ollama.ChatResponse{
		{chunk("I'll read the file.\n<tool_call>{\"name\":\"read_file\",\"arguments\":{\"path\":\"x.txt\"}}</tool_call>")},
		{chunk("got it")},
	}}
	ag, sess := newTestAgent(t, client)

	sess.Add(core.Message{Role: core.RoleUser, Content: "read x"})
	toolRan := false
	for ev := range ag.Run(context.Background()) {
		if ev.Kind == EvToolStart && ev.ToolName == "read_file" {
			toolRan = true
		}
	}
	if !toolRan {
		t.Fatal("fallback text tool call was not parsed/executed")
	}
	// the tool_call tag must be stripped from displayed content
	if strings.Contains(sess.Messages[1].Content, "<tool_call>") {
		t.Errorf("tag left in content: %q", sess.Messages[1].Content)
	}
}

func TestAgentDenyFlow(t *testing.T) {
	client := &fakeClient{script: [][]ollama.ChatResponse{
		{toolCallChunk("bash", map[string]any{"command": "echo hi"})},
		{chunk("understood, skipping")},
	}}
	ag, sess := newTestAgent(t, client)
	ag.Perms = NewManager("standard", t.TempDir(), nil, nil) // bash requires approval
	sess.Add(core.Message{Role: core.RoleUser, Content: "run something"})

	approvalSeen := false
	for ev := range ag.Run(context.Background()) {
		if ev.Kind == EvApproval {
			approvalSeen = true
			ev.Approval.Decide(DecisionDeny)
		}
	}
	if !approvalSeen {
		t.Fatal("no approval request for bash in standard mode")
	}
	// the tool message should record the denial
	last := sess.Messages[len(sess.Messages)-2] // tool result before final answer
	if last.Role != core.RoleTool || !strings.Contains(last.Content, "denied") {
		t.Errorf("denial not recorded: %q", last.Content)
	}
}

func TestPermissionManager(t *testing.T) {
	// ask mode: every change prompts
	m := NewManager("standard", t.TempDir(), []string{"write_file"}, []string{"rm\\s+-rf"})
	if m.NeedsApproval("read_file", tools.CatRead, "/etc/passwd", "{}") {
		t.Error("read tools must auto-approve")
	}
	if !m.NeedsApproval("bash", tools.CatExec, "ls", "{}") {
		t.Error("bash must require approval in ask mode")
	}
	if m.NeedsApproval("write_file", tools.CatMutate, "out.txt", "{}") {
		t.Error("write_file should be pre-approved via config")
	}
	if !m.NeedsApproval("bash", tools.CatExec, "rm -rf /", "{}") {
		t.Error("deny pattern must force approval even in full mode")
	}
	m.ApproveSession("bash")
	if m.NeedsApproval("bash", tools.CatExec, "ls", "{}") {
		t.Error("session approval must stick")
	}

	// full mode: everything auto
	mFull := NewManager("yolo", t.TempDir(), nil, nil)
	if mFull.NeedsApproval("bash", tools.CatExec, "ls -la", "{}") {
		t.Error("full must auto-approve")
	}
	if !mFull.NeedsApproval("bash", tools.CatExec, "sudo rm file", "{}") {
		t.Error("builtin risky patterns must prompt even in full mode")
	}

	// risky mode: safe mutations auto, risky ones prompt
	wd := t.TempDir()
	mRisky := NewManager("risky", wd, nil, nil)
	if mRisky.NeedsApproval("bash", tools.CatExec, "ls -la", "{}") {
		t.Error("risky mode must auto-run allowlisted commands")
	}
	if !mRisky.NeedsApproval("bash", tools.CatExec, "python3 script.py", "{}") {
		t.Error("risky mode must prompt for non-allowlisted commands")
	}
	if mRisky.NeedsApproval("write_file", tools.CatMutate, "", `{"path":"src/a.txt"}`) {
		t.Error("risky mode must auto-approve writes inside workdir")
	}
	// a parent-of-workdir path is outside on every platform (/etc/hosts is
	// rooted-but-driveless on Windows, which filepath.Join pulls INSIDE)
	if !mRisky.NeedsApproval("write_file", tools.CatMutate, "",
		`{"path":"`+filepath.ToSlash(filepath.Dir(wd))+`/hosts"}`) {
		t.Error("risky mode must prompt for writes outside workdir")
	}
	if mRisky.NeedsApproval("memory_save", tools.CatMutate, "", "{}") {
		t.Error("memory_save is safe in risky mode")
	}
	if mRisky.Mode() != ModeRisky {
		t.Errorf("mode = %q", mRisky.Mode())
	}
	if NormalizeMode("yolo") != ModeFull || NormalizeMode("standard") != ModeAsk {
		t.Error("legacy aliases must normalize")
	}
	mRisky.SetMode("full")
	if mRisky.NeedsApproval("bash", tools.CatExec, "make build", "{}") {
		t.Error("SetMode must take effect")
	}
}

func TestParseTextToolCallsVariants(t *testing.T) {
	cases := []struct {
		in    string
		count int
		name  string
	}{
		{`<tool_call>{"name":"bash","arguments":{"command":"ls"}}</tool_call>`, 1, "bash"},
		{"before\n<tool_call>\n{\"name\": \"read_file\", \"arguments\": {\"path\": \"a\"}}\n</tool_call>\nafter", 1, "read_file"},
		{"```json\n{\"name\":\"grep\",\"arguments\":{\"pattern\":\"x\"}}\n```", 1, "grep"},
		{`{"name":"list_dir","arguments":{}}`, 1, "list_dir"},
		{`<tool_call>{"name":"bash","arguments":"{\"command\":\"ls\"}"}</tool_call>`, 1, "bash"},
		{"no tools here", 0, ""},
	}
	for i, c := range cases {
		calls, clean := parseTextToolCalls(c.in)
		if len(calls) != c.count {
			t.Errorf("case %d: got %d calls, want %d", i, len(calls), c.count)
			continue
		}
		if c.count > 0 && calls[0].Name != c.name {
			t.Errorf("case %d: name = %q, want %q", i, calls[0].Name, c.name)
		}
		if c.count > 0 && strings.Contains(clean, "<tool_call>") && i < 2 {
			t.Errorf("case %d: tag not cleaned: %q", i, clean)
		}
	}
	// arguments-as-string must decode
	calls, _ := parseTextToolCalls(`<tool_call>{"name":"bash","arguments":"{\"command\":\"ls\"}"}</tool_call>`)
	if len(calls) == 1 && calls[0].Args["command"] != "ls" {
		t.Errorf("string arguments not decoded: %v", calls[0].Args)
	}
}

func TestStaleToolResultTruncation(t *testing.T) {
	client := &fakeClient{script: [][]ollama.ChatResponse{
		{toolCallChunk("read_file", map[string]any{"path": strings.Repeat("a", 10)})},
		{chunk("first done")},
		{toolCallChunk("read_file", map[string]any{"path": strings.Repeat("b", 10)})},
		{chunk("second done")},
	}}
	ag, sess := newTestAgent(t, client)
	ag.MaxToolLen = 5000
	for range ag.Run(context.Background()) {
	}
	// after the run, first tool result (stale) should be truncated in apiMessages
	// verify indirectly: build and inspect
	msgs := ag.apiMessages()
	fullCount := 0
	for _, m := range msgs {
		if m.Role == "tool" && strings.Contains(m.Content, "[older tool result truncated]") {
			fullCount++
		}
	}
	// only tool results beyond the last 6 are stale — with 2 results none are
	if fullCount != 0 {
		t.Errorf("unexpected truncation with few results: %d", fullCount)
	}
	_ = sess
}
