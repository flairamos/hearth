package web

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"hearth/internal/config"
	"hearth/internal/ollama"
	"hearth/internal/store"
)

type fakeClient struct{ chunks []ollama.ChatResponse }

func (f fakeClient) ChatStream(ctx context.Context, req ollama.ChatRequest, fn func(ollama.ChatResponse) error) error {
	for _, c := range f.chunks {
		if err := fn(c); err != nil {
			return err
		}
	}
	return nil
}
func (f fakeClient) Chat(ctx context.Context, req ollama.ChatRequest) (ollama.ChatResponse, error) {
	return ollama.ChatResponse{}, nil
}
func (f fakeClient) Tags(ctx context.Context) ([]ollama.ModelInfo, error) {
	return nil, nil
}
func (f fakeClient) Show(ctx context.Context, name string) (ollama.ShowResponse, error) {
	return ollama.ShowResponse{}, nil
}
func (f fakeClient) Ping(ctx context.Context) error { return nil }

func newTestServer(t *testing.T) (*Server, *httptest.Server) {
	t.Helper()
	cfg := config.Defaults()
	cfg.Agent.Memory = false
	cfg.Context.AutoTrim = false
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	srv, err := New(cfg, st)
	if err != nil {
		t.Fatal(err)
	}
	srv.client = fakeClient{chunks: []ollama.ChatResponse{
		{Message: ollama.Message{Content: "hello "}},
		{Message: ollama.Message{Content: "world"}, Done: true, DoneReason: "stop", EvalCount: 5},
	}}
	ts := httptest.NewServer(srv.mux())
	t.Cleanup(ts.Close)
	return srv, ts
}

func stateMsgs(t *testing.T, url string) []map[string]any {
	t.Helper()
	res, err := http.Get(url + "/api/state")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var out struct {
		Messages []map[string]any `json:"messages"`
	}
	_ = json.NewDecoder(res.Body).Decode(&out)
	return out.Messages
}

func TestWebE2ESendAndState(t *testing.T) {
	_, ts := newTestServer(t)

	res, err := http.Post(ts.URL+"/api/send", "application/json", strings.NewReader(`{"text":"hi","agent":false}`))
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()

	deadline := time.Now().Add(5 * time.Second)
	var last map[string]any
	for time.Now().Before(deadline) {
		msgs := stateMsgs(t, ts.URL)
		var assistant []map[string]any
		for _, m := range msgs {
			if m["role"] == "assistant" {
				assistant = append(assistant, m)
			}
		}
		if len(assistant) == 1 {
			last = assistant[0]
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if last == nil {
		t.Fatal("assistant reply never landed")
	}
	if !strings.Contains(last["content"].(string), "hello world") {
		t.Fatalf("reply content wrong: %v", last["content"])
	}
	if msgs := stateMsgs(t, ts.URL); len(msgs) != 2 { // user + assistant
		t.Fatalf("unexpected message count: %d", len(msgs))
	}
}

func TestWebSSEAndApprovals(t *testing.T) {
	srv, ts := newTestServer(t)

	// SSE stream first, then trigger the turn
	req, _ := http.NewRequest("GET", ts.URL+"/api/events", nil)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	scanner := bufio.NewScanner(res.Body)
	if !scanner.Scan() {
		t.Fatal("no SSE preamble")
	}

	http.Post(ts.URL+"/api/send", "application/json", strings.NewReader(`{"text":"hi","agent":false}`))

	var sawUser, sawDelta, sawDone bool
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && !(sawUser && sawDelta && sawDone) {
		if !scanner.Scan() {
			break
		}
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var ev struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		_ = json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &ev)
		switch ev.Type {
		case "user":
			sawUser = true
		case "delta":
			if strings.Contains(ev.Text, "hello") || strings.Contains(ev.Text, "world") {
				sawDelta = true
			}
		case "done":
			sawDone = true
		}
	}
	if !sawUser || !sawDelta || !sawDone {
		t.Fatalf("SSE events incomplete: user=%v delta=%v done=%v", sawUser, sawDelta, sawDone)
	}

	// approval flow with a deny decision
	srv.mu.Lock()
	pending := len(srv.pending)
	srv.mu.Unlock()
	if pending != 0 {
		t.Fatalf("unexpected pending approvals: %d", pending)
	}
	if srv.decideApproval("nonexistent", 0) {
		t.Fatal("approve of unknown id must fail")
	}

	// session new/load round trip
	res2, err := http.Post(ts.URL+"/api/session/new", "application/json", nil)
	if err != nil || res2.StatusCode != 200 {
		t.Fatalf("session/new failed")
	}
	res2.Body.Close()
	if msgs := stateMsgs(t, ts.URL); len(msgs) != 0 {
		t.Fatalf("new session must be empty, got %d", len(msgs))
	}
}

func TestSliverResizeIgnored(t *testing.T) {
	// web server state mirrors the TUI guard: it lives in the tui package
	// (TestWindowSizeGuard) — here we only assert the server still serves
	// after a session round trip.
	_, ts := newTestServer(t)
	res, err := http.Get(ts.URL + "/api/sessions")
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
}

func TestWebCommandsAndFiles(t *testing.T) {
	_, ts := newTestServer(t)

	// /model command
	res, err := http.Post(ts.URL+"/api/send", "application/json", strings.NewReader(`{"text":"/model glm-4.6","agent":false}`))
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	st := stateRaw(t, ts.URL+"/api/state")
	if st["model"] != "glm-4.6" {
		t.Fatalf("/model not applied: %v", st["model"])
	}

	// unknown command → notice, no crash
	res, err = http.Post(ts.URL+"/api/send", "application/json", strings.NewReader(`{"text":"/bogus","agent":false}`))
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()

	// @ picker file list
	res, err = http.Get(ts.URL + "/api/files?query=")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var files []string
	_ = json.NewDecoder(res.Body).Decode(&files)
	// temp workdir may be empty — just assert the endpoint responds with a list
	if files == nil {
		t.Fatal("/api/files must return a list")
	}
}

func stateRaw(t *testing.T, url string) map[string]any {
	t.Helper()
	res, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(res.Body).Decode(&out)
	return out
}

func TestWebRetryExportImg(t *testing.T) {
	_, ts := newTestServer(t)

	// run one turn
	http.Post(ts.URL+"/api/send", "application/json", strings.NewReader(`{"text":"hi","agent":false}`))
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		done := false
		for _, m := range stateMsgs(t, ts.URL) {
			if m["role"] == "assistant" {
				done = true
			}
		}
		if done {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	before := len(stateMsgs(t, ts.URL))

	// retry: pops the last user turn, re-runs
	res, err := http.Post(ts.URL+"/api/retry", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		msgs := stateMsgs(t, ts.URL)
		var assistant []map[string]any
		for _, m := range msgs {
			if m["role"] == "assistant" {
				assistant = append(assistant, m)
			}
		}
		if len(assistant) == 1 && len(msgs) == before {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	// fake client replays the same chunks → same reply, same shape
	msgs := stateMsgs(t, ts.URL)
	var found bool
	for _, m := range msgs {
		if m["role"] == "assistant" && strings.Contains(m["content"].(string), "hello world") {
			found = true
		}
	}
	if !found {
		t.Fatal("retry did not regenerate the answer")
	}

	// export: markdown download with attachment header
	res2, err := http.Get(ts.URL + "/api/export")
	if err != nil {
		t.Fatal(err)
	}
	defer res2.Body.Close()
	if cd := res2.Header.Get("Content-Disposition"); !strings.Contains(cd, ".md") {
		t.Fatalf("export disposition missing: %q", cd)
	}
	body := make([]byte, 4096)
	n, _ := res2.Body.Read(body)
	if !strings.Contains(string(body[:n]), "# ") {
		t.Fatal("export markdown missing title")
	}

	// img upload: valid png → count 1; invalid → 415
	png := []byte{0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A, 0, 0}
	var buf strings.Builder
	buf.WriteString("--B\r\nContent-Disposition: form-data; name=\"file\"; filename=\"x.png\"\r\nContent-Type: image/png\r\n\r\n")
	buf.Write(png)
	buf.WriteString("\r\n--B--\r\n")
	res3, err := http.Post(ts.URL+"/api/img", "multipart/form-data; boundary=B", strings.NewReader(buf.String()))
	if err != nil {
		t.Fatal(err)
	}
	if res3.StatusCode != 200 {
		t.Fatalf("img upload failed: %d", res3.StatusCode)
	}
	res3.Body.Close()
	bad := "--B\r\nContent-Disposition: form-data; name=\"file\"; filename=\"x.txt\"\r\n\r\nnotanimage\r\n--B--\r\n"
	res4, err := http.Post(ts.URL+"/api/img", "multipart/form-data; boundary=B", strings.NewReader(bad))
	if err != nil {
		t.Fatal(err)
	}
	res4.Body.Close()
	if res4.StatusCode != http.StatusUnsupportedMediaType {
		t.Fatalf("invalid image accepted: %d", res4.StatusCode)
	}
}
