package tools

import (
	"context"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"hearth/internal/core"
)

func TestCalculator(t *testing.T) {
	r := testRegistry(t)
	cases := []struct {
		expr string
		want string
	}{
		{"1+2*3", "7"},
		{"(1+2)*3^2/4", "6.75"},
		{"sqrt(144)", "12"},
		{"2^10", "1024"},
		{"10/4", "2.5"},
		{"-3+5", "2"},
		{"min(3,1,2)+max(4,7)", "8"},
		{"abs(-5)+floor(2.7)+ceil(2.1)", "10"},
		{"log(100)", formatNumber(math.Log(100))},
		{"pi", formatNumber(math.Pi)},
		{"2**8", "256"},
		{"100%7", "2"},
	}
	for _, c := range cases {
		out, err := calculatorTool().Fn(context.Background(), r, map[string]any{"expression": c.expr})
		if err != nil || strings.TrimSpace(out) != c.want {
			t.Errorf("calc(%q) = %q, %v; want %q", c.expr, out, err, c.want)
		}
	}
	// error cases
	for _, bad := range []string{"1/0", "sqrt(-4)", "1+", "foo(1)", "(1+2"} {
		if _, err := calculatorTool().Fn(context.Background(), r, map[string]any{"expression": bad}); err == nil {
			t.Errorf("calc(%q) should error", bad)
		}
	}
}

func TestNoteScratchpad(t *testing.T) {
	r := testRegistry(t)
	sess := core.NewSession("m")
	r.Session = sess

	out, err := noteTool().Fn(context.Background(), r, map[string]any{"action": "write", "content": "plan: 1) read 2) edit"})
	if err != nil || !strings.Contains(out, "plan") {
		t.Fatalf("note write: %v %v", out, err)
	}
	_, _ = noteTool().Fn(context.Background(), r, map[string]any{"action": "append", "content": "step1 done"})
	out, err = noteTool().Fn(context.Background(), r, map[string]any{"action": "read"})
	if err != nil || !strings.Contains(out, "step1 done") || !strings.Contains(out, "plan:") {
		t.Fatalf("note read: %v %v", out, err)
	}
	if sess.Scratchpad != "plan: 1) read 2) edit\nstep1 done" {
		t.Errorf("scratchpad = %q", sess.Scratchpad)
	}
}

func TestMemoryStore(t *testing.T) {
	dir := t.TempDir()
	m := NewMemoryStore(filepath.Join(dir, "memory.md"))

	if err := m.Append("User speaks Chinese"); err != nil {
		t.Fatal(err)
	}
	if err := m.Append("Project uses Go 1.27"); err != nil {
		t.Fatal(err)
	}
	if err := m.Append("User speaks Chinese"); err != nil { // dedupe
		t.Fatal(err)
	}
	if m.Count() != 2 {
		t.Fatalf("count = %d, want 2", m.Count())
	}

	// persists across instances
	m2 := NewMemoryStore(m.path)
	res, err := m2.Search("go", 5)
	if err != nil || len(res) != 1 || !strings.Contains(res[0], "Go 1.27") {
		t.Fatalf("search: %v %v", res, err)
	}

	recent, _ := m2.Search("", 10)
	if len(recent) != 2 || !strings.Contains(recent[0], "Go 1.27") { // newest first
		t.Fatalf("recent: %v", recent)
	}

	// content survives on disk in human-readable form
	b, _ := os.ReadFile(m.path)
	if !strings.Contains(string(b), "- [") {
		t.Errorf("file format wrong: %q", b)
	}
}

func TestWebFetch(t *testing.T) {
	r := testRegistry(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path == "/missing" {
			w.WriteHeader(404)
			return
		}
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte(`<!doctype html><html><head><title>Test Page</title><style>body{color:red}</style><script>var x=1;</script></head>
		<body><h1>Hello &amp; Welcome</h1><p>First paragraph with some content.</p>
		<a href="/x">link</a><div>Second block</div></body></html>`))
	}))
	defer srv.Close()

	out, err := webFetchTool().Fn(context.Background(), r, map[string]any{"url": srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "Test Page") || !strings.Contains(out, "Hello & Welcome") ||
		!strings.Contains(out, "First paragraph") || strings.Contains(out, "var x") || strings.Contains(out, "color:red") {
		t.Errorf("html conversion wrong:\n%s", out)
	}

	// bad URL schemes rejected
	if _, err := webFetchTool().Fn(context.Background(), r, map[string]any{"url": "file:///etc/passwd"}); err == nil {
		t.Error("file:// must be rejected")
	}
	if _, err := webFetchTool().Fn(context.Background(), r, map[string]any{"url": srv.URL + "/missing"}); err == nil {
		t.Error("404 must surface as error")
	}
}

func TestWebSearchSearxng(t *testing.T) {
	r := testRegistry(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if !strings.Contains(req.URL.RawQuery, "format=json") {
			w.WriteHeader(403)
			return
		}
		w.Write([]byte(`{"results":[{"title":"Go","url":"https://go.dev","content":"Go language"},{"title":"Rust","url":"https://rust-lang.org","content":"Rust"}]}`))
	}))
	defer srv.Close()

	r.Search = &SearchBackend{Engine: "searxng", SearxngURL: srv.URL, MaxResults: 5}
	out, err := webSearchTool().Fn(context.Background(), r, map[string]any{"query": "languages"})
	if err != nil || !strings.Contains(out, "https://go.dev") || !strings.Contains(out, "Rust") {
		t.Fatalf("searxng search: %v %v", out, err)
	}

	// engine=none disables
	r.Search = &SearchBackend{Engine: "none"}
	if _, err := webSearchTool().Fn(context.Background(), r, map[string]any{"query": "x"}); err == nil {
		t.Error("disabled search must error")
	}
}

func TestEditFileCRLF(t *testing.T) {
	r := testRegistry(t)
	p := filepath.Join(r.WorkDir, "win.txt")
	_ = os.WriteFile(p, []byte("line one\r\nline two\r\nline three\r\n"), 0o644)

	out, err := editFileTool().Fn(context.Background(), r, map[string]any{
		"path": "win.txt", "old_string": "line two\nline three", "new_string": "LINE TWO\nLINE THREE",
	})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(p)
	want := "line one\r\nLINE TWO\r\nLINE THREE\r\n"
	if string(b) != want {
		t.Errorf("CRLF edit: got %q want %q (out=%s)", b, want, out)
	}
}
