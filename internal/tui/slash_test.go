package tui

import (
	"context"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"hearth/internal/config"
	"hearth/internal/core"
	"hearth/internal/ollama"
	"hearth/internal/store"
)

// newSlashApp builds a minimal App for slash-popup key handling tests.
func newSlashApp(t *testing.T) *App {
	t.Helper()
	cfg := config.Defaults()
	cfg.Agent.Memory = false
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	app, err := New(cfg, nil, st, "")
	if err != nil {
		t.Fatalf("app: %v", err)
	}
	return app
}

func (a *App) press(t *testing.T, key string) {
	t.Helper()
	m, _ := a.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(key)})
	_ = m
}

func TestSlashPopupArrowNavigation(t *testing.T) {
	a := newSlashApp(t)
	a.ta.SetValue("/")
	a.press(t, "down")
	if a.slashCursor != 1 {
		t.Fatalf("down: cursor = %d, want 1", a.slashCursor)
	}
	for range 3 {
		a.press(t, "down")
	}
	if a.slashCursor != 4 {
		t.Fatalf("3x down: cursor = %d, want 4", a.slashCursor)
	}
	a.press(t, "up")
	if a.slashCursor != 3 {
		t.Fatalf("up: cursor = %d, want 3", a.slashCursor)
	}
	// wrap above the top
	a.ta.SetValue("/")
	a.slashCursor = 0
	a.press(t, "up")
	n := len(a.slashMatches(""))
	if a.slashCursor != n-1 {
		t.Fatalf("wrap up: cursor = %d, want %d", a.slashCursor, n-1)
	}
}

func TestSlashPopupTabCompletes(t *testing.T) {
	a := newSlashApp(t)
	a.ta.SetValue("/mod")
	a.press(t, "tab")
	if got := a.ta.Value(); got != "/model " {
		t.Fatalf("tab: value = %q, want %q", got, "/model ")
	}
}

func TestSlashPopupEscDismissAndRetype(t *testing.T) {
	a := newSlashApp(t)
	a.ta.SetValue("/")
	a.press(t, "esc")
	if !a.slashHidden {
		t.Fatal("esc: popup not dismissed")
	}
	if a.slashPopup(80) != "" {
		t.Fatal("esc: popup still renders")
	}
	// editing the draft re-shows the popup (Update-level reset)
	a.press(t, "n")
	if a.slashHidden {
		t.Fatal("edit after esc: popup still hidden")
	}
	if a.slashCursor != 0 {
		t.Fatalf("edit after esc: cursor = %d, want 0", a.slashCursor)
	}
}

func TestSlashPopupScrollWindow(t *testing.T) {
	a := newSlashApp(t)
	a.ta.SetValue("/")
	// park the selection on /compact, past the 8-row window
	idx := -1
	for i, c := range a.slashMatches("") {
		if c.name == "compact" {
			idx = i
			break
		}
	}
	if idx < 8 {
		t.Fatalf("compact index = %d, expected beyond the window", idx)
	}
	a.slashCursor = idx
	out := a.slashPopup(80)
	if !strings.Contains(out, "> /compact") {
		t.Fatalf("selected row beyond the window not rendered: %q", out)
	}
	if strings.Contains(out, "> /model") {
		t.Fatal("first row should have scrolled out of the window")
	}
	if !strings.Contains(out, fmt.Sprintf("%d/%d", idx+1, 19)) {
		t.Fatalf("position indicator missing: %q", out)
	}
}

func TestSlashPopupWheelSelects(t *testing.T) {
	a := newSlashApp(t)
	a.ta.SetValue("/")
	a.Update(tea.MouseMsg{Type: tea.MouseWheelDown})
	if a.slashCursor != 1 {
		t.Fatalf("wheel down: cursor = %d, want 1", a.slashCursor)
	}
	a.Update(tea.MouseMsg{Type: tea.MouseWheelUp})
	if a.slashCursor != 0 {
		t.Fatalf("wheel up: cursor = %d, want 0", a.slashCursor)
	}
}

func TestClearCommandDeletesSession(t *testing.T) {
	a := newSlashApp(t)
	a.sess.Title = "doomed"
	a.sess.Add(core.Message{Role: core.RoleUser, Content: "hello"})
	a.saveSession()
	oldID := a.sess.ID

	c, ok := a.findCommand("clear")
	if !ok {
		t.Fatal("clear command missing")
	}
	if err := c.run(a, ""); err != nil {
		t.Fatalf("clear run: %v", err)
	}
	if a.panel != panelConfirm {
		t.Fatalf("clear must open the confirm dialog, panel=%v", a.panel)
	}
	a.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("y")}) // confirm

	if _, err := a.store.Load(oldID); err == nil {
		t.Fatal("session file still exists after /clear")
	}
	if a.sess.ID == oldID {
		t.Fatal("clear must start a fresh session")
	}
	if a.sess.Len() != 0 || a.sess.Notes() != "" {
		t.Fatal("fresh session must be empty (messages + notes)")
	}
	if a.panel != panelNone {
		t.Fatalf("confirm dialog not closed: %v", a.panel)
	}
}

func TestQuitCommandSavesSession(t *testing.T) {
	a := newSlashApp(t) // prog is nil here — must not panic
	// unsaved state: title change + a message added after session load
	a.sess.Title = "quit-save-test"
	a.sess.Add(core.Message{Role: core.RoleUser, Content: "mid-turn exchange not yet persisted"})
	c, ok := a.findCommand("quit")
	if !ok {
		t.Fatal("quit command missing")
	}
	if err := c.run(a, ""); err != nil {
		t.Fatalf("quit run: %v", err)
	}
	if !a.quitNow {
		t.Fatal("quit command did not arm quitNow")
	}
	loaded, err := a.store.Load(a.sess.ID)
	if err != nil {
		t.Fatalf("session was not saved on /quit: %v", err)
	}
	if loaded.Title != "quit-save-test" || loaded.Len() != a.sess.Len() {
		t.Fatalf("saved session is stale: title=%q msgs=%d (want %d)",
			loaded.Title, loaded.Len(), a.sess.Len())
	}
}

func TestQuitNowQuitsOnAnyKey(t *testing.T) {
	a := newSlashApp(t)
	a.quitNow = true
	_, cmd := a.Update(tea.KeyMsg{Type: tea.KeyCtrlC})
	if cmd == nil {
		t.Fatal("armed quit must return tea.Quit on the next key, not re-arm ctrl+c")
	}
}

func TestSpinnerAlwaysRearms(t *testing.T) {
	a := newSlashApp(t)
	frame0 := spinnerFrame
	_, cmd := a.Update(spinnerTickMsg{}) // idle: no turn running
	if cmd == nil {
		t.Fatal("spinner chain died on an idle tick — it would freeze during turns")
	}
	if spinnerFrame != (frame0+1)%len(spinnerFrames) {
		t.Fatalf("frame = %d, want %d", spinnerFrame, (frame0+1)%len(spinnerFrames))
	}
}

func TestHeaderModeBadge(t *testing.T) {
	a := newSlashApp(t)
	a.sess.AgentMode = false
	if out := a.headerView(120); !strings.Contains(stripANSI(out), "[CHAT]") {
		t.Fatalf("chat mode: header missing [CHAT]: %q", stripANSI(out))
	}
	a.sess.AgentMode = true
	if out := a.headerView(120); !strings.Contains(stripANSI(out), "[AGENT]") {
		t.Fatalf("agent mode: header missing [AGENT]: %q", stripANSI(out))
	}
}

func TestSlashPopupSelectionRunsOnEnter(t *testing.T) {
	a := newSlashApp(t)
	a.ta.SetValue("/")
	// walk down to /think, then enter completes-or-runs the selection
	var name string
	for range 12 {
		word, _ := splitSlash(strings.TrimSpace(a.ta.Value()))
		matches := a.slashMatches(word)
		if strings.HasPrefix(matches[min(a.slashCursor, len(matches)-1)].name, "think") {
			name = matches[a.slashCursor].name
			break
		}
		a.press(t, "down")
	}
	if name != "think" {
		t.Fatalf("could not reach /think by pressing down: %q", name)
	}
	a.press(t, "enter")
	if got := a.ta.Value(); got != "/think " {
		t.Fatalf("enter on selection: value = %q, want %q", got, "/think ")
	}
}

// nopClient satisfies provider.Client without touching the network.
type nopClient struct{}

func (nopClient) ChatStream(ctx context.Context, req ollama.ChatRequest, fn func(ollama.ChatResponse) error) error {
	return nil
}
func (nopClient) Chat(ctx context.Context, req ollama.ChatRequest) (ollama.ChatResponse, error) {
	return ollama.ChatResponse{}, nil
}
func (nopClient) Tags(ctx context.Context) ([]ollama.ModelInfo, error) { return nil, nil }
func (nopClient) Show(ctx context.Context, name string) (ollama.ShowResponse, error) {
	return ollama.ShowResponse{}, nil
}
func (nopClient) Ping(ctx context.Context) error { return nil }

// TestQuitExitsProgram drives a REAL bubbletea program with piped input and
// asserts the process-level quit actually happens (the /quit path sends
// tea.Quit via prog.Send — this catches any wiring break end to end).
func TestQuitExitsProgram(t *testing.T) {
	cfg := config.Defaults()
	cfg.Agent.Memory = false
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	app, err := New(cfg, nopClient{}, st, "")
	if err != nil {
		t.Fatal(err)
	}

	inR, inW := io.Pipe()
	p := tea.NewProgram(app,
		tea.WithInput(inR),
		tea.WithOutput(io.Discard),
		tea.WithoutCatchPanics(),
	)
	app.SetProgram(p)

	done := make(chan error, 1)
	go func() {
		_, err := p.Run()
		done <- err
	}()

	time.Sleep(150 * time.Millisecond) // let the program start
	if _, err := io.WriteString(inW, "/quit\r"); err != nil {
		t.Fatalf("write input: %v", err)
	}

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("program exited with error: %v", err)
		}
		// passed: the program really exited from /quit
	case <-time.After(5 * time.Second):
		p.Quit()
		t.Fatal("program did not exit within 5s of /quit")
	}
}
