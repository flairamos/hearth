package tui

import (
	"context"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"hearth/internal/agent"
	"hearth/internal/theme"
	"hearth/internal/tools"
)

func TestApprovalViewWidthBounded(t *testing.T) {
	th := theme.Get("dark")
	req := &agent.ApprovalRequest{
		Tool: "edit_file", Category: tools.CatMutate,
		Summary: "/home/firea/projects/test/index.html",
		Detail:  "{\n  \"new_string\": \"" + strings.Repeat("快捷模块内容", 100) + "\"\n}",
	}
	for w := 44; w <= 220; w += 6 {
		out := approvalView(th, req, w)
		for _, ln := range strings.Split(out, "\n") {
			if lipgloss.Width(ln) > w-2 {
				t.Fatalf("width %d: dialog line exceeds bound (%d cols): %.60q", w, lipgloss.Width(ln), ln)
			}
		}
	}
}

func newApprovalApp(t *testing.T) (*App, *agent.ApprovalRequest) {
	t.Helper()
	a := newSlashApp(t)
	req := &agent.ApprovalRequest{
		ID: "call_1", Tool: "edit_file", Category: tools.CatMutate,
		Summary: "/tmp/x.html", Detail: `{"path":"/tmp/x.html"}`,
		Respond: make(chan agent.ApprovalDecision, 1),
	}
	a.approval = req
	a.panel = panelApproval
	return a, req
}

func runesKey(s string) tea.KeyMsg { return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)} }

func TestApprovalModalSwallowsKeys(t *testing.T) {
	a, req := newApprovalApp(t)
	a.gen = &genState{ctx: context.Background(), buf: &deltaBuf{}, phase: "tool: edit_file", start: time.Now()}

	// unrelated keys change nothing: dialog stays up, nothing decides
	for _, k := range []tea.KeyMsg{
		runesKey("x"),
		runesKey("?"),
		{Type: tea.KeyCtrlK},
		{Type: tea.KeyPgUp},
		{Type: tea.KeyUp},
	} {
		a.Update(k)
		if a.panel != panelApproval || a.approval != req {
			t.Fatalf("key %v broke the modal (panel=%v)", k, a.panel)
		}
		if len(req.Respond) != 0 {
			t.Fatalf("key %v decided the request", k)
		}
	}
	if a.ta.Value() != "" {
		t.Fatalf("keys leaked into the input box: %q", a.ta.Value())
	}
}

func TestApprovalKeys(t *testing.T) {
	for _, tc := range []struct {
		key  tea.KeyMsg
		want agent.ApprovalDecision
	}{
		{runesKey("a"), agent.DecisionOnce},
		{runesKey("s"), agent.DecisionSession},
		{runesKey("d"), agent.DecisionDeny},
		{tea.KeyMsg{Type: tea.KeyEnter}, agent.DecisionOnce},
		{tea.KeyMsg{Type: tea.KeyEsc}, agent.DecisionDeny},
	} {
		a, req := newApprovalApp(t)
		a.Update(tc.key)
		if a.panel != panelNone || a.approval != nil {
			t.Fatalf("key %v: dialog not closed", tc.key)
		}
		select {
		case got := <-req.Respond:
			if got != tc.want {
				t.Fatalf("key %v: decision = %v, want %v", tc.key, got, tc.want)
			}
		default:
			t.Fatalf("key %v: no decision delivered", tc.key)
		}
	}
}

func TestApprovalCtrlCDeniesNotInterrupts(t *testing.T) {
	a, req := newApprovalApp(t)
	a.gen = &genState{ctx: context.Background(), buf: &deltaBuf{}, phase: "tool", start: time.Now()}

	a.Update(tea.KeyMsg{Type: tea.KeyCtrlC})

	if a.panel != panelNone || a.approval != nil {
		t.Fatal("ctrl+c did not close the dialog")
	}
	select {
	case got := <-req.Respond:
		if got != agent.DecisionDeny {
			t.Fatalf("decision = %v, want deny", got)
		}
	default:
		t.Fatal("no decision delivered")
	}
	if a.gen == nil {
		t.Fatal("ctrl+c interrupted the run behind the modal — it must only deny")
	}
}
