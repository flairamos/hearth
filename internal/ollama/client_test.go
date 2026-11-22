package ollama

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestChatStreamNDJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/chat" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/x-ndjson")
		w.Write([]byte(`{"message":{"content":"hel"},"done":false}` + "\n"))
		w.Write([]byte(`{"message":{"content":"lo"},"done":false}` + "\n"))
		w.Write([]byte(`{"message":{"content":""},"done":true,"done_reason":"stop","eval_count":10,"prompt_eval_count":5}` + "\n"))
	}))
	defer srv.Close()

	c := New(srv.URL)
	var got strings.Builder
	var done, evalCount, promptEval bool
	err := c.ChatStream(context.Background(), ChatRequest{Model: "m"}, func(r ChatResponse) error {
		got.WriteString(r.Message.Content)
		if r.Done {
			done = true
			evalCount = r.EvalCount == 10
			promptEval = r.PromptEvalCount == 5
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.String() != "hello" || !done || !evalCount || !promptEval {
		t.Fatalf("stream result: %q done=%v eval=%v prompt=%v", got.String(), done, evalCount, promptEval)
	}
}

func TestChatStreamInStreamError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"error":"model is busy"}` + "\n"))
	}))
	defer srv.Close()
	c := New(srv.URL)
	err := c.ChatStream(context.Background(), ChatRequest{Model: "m"}, func(ChatResponse) error { return nil })
	if err == nil || !strings.Contains(err.Error(), "busy") {
		t.Fatalf("want in-stream error, got %v", err)
	}
}

func TestChatStreamRetryOnConnFailure(t *testing.T) {
	attempts := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		if attempts == 1 {
			// kill the connection abruptly
			panic(http.ErrAbortHandler)
		}
		w.Write([]byte(`{"message":{"content":"ok"},"done":true}` + "\n"))
	}))
	defer srv.Close()
	c := New(srv.URL)
	var out strings.Builder
	err := c.ChatStream(context.Background(), ChatRequest{Model: "m"}, func(r ChatResponse) error {
		out.WriteString(r.Message.Content)
		return nil
	})
	if err != nil || out.String() != "ok" {
		t.Fatalf("retry failed: err=%v out=%q attempts=%d", err, out.String(), attempts)
	}
}

func TestChatStreamNoRetryAfterData(t *testing.T) {
	// once the callback delivered data, a callback error must surface as-is
	// (no retry) — callers own their partial state.
	attempts := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		w.Write([]byte(`{"message":{"content":"x"},"done":false}` + "\n"))
	}))
	defer srv.Close()
	c := New(srv.URL)
	sentinel := errSentinel("boom")
	err := c.ChatStream(context.Background(), ChatRequest{Model: "m"}, func(ChatResponse) error {
		return sentinel
	})
	if err != sentinel {
		t.Fatalf("callback error must propagate unwrapped: %v", err)
	}
	if attempts != 1 {
		t.Fatalf("must not retry after data was streamed, attempts=%d", attempts)
	}
}

type errSentinel string

func (e errSentinel) Error() string { return string(e) }

func TestTagsAndShow(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/tags":
			json.NewEncoder(w).Encode(map[string]any{"models": []map[string]any{{
				"name": "qwen3.5:4b", "size": 3324173934,
				"details": map[string]any{"parameter_size": "4.2B", "quantization_level": "Q4_K_M", "family": "qwen35"},
			}}})
		case "/api/show":
			json.NewEncoder(w).Encode(map[string]any{
				"capabilities": []string{"tools", "vision", "thinking"},
				"model_info": map[string]any{
					"qwen35.context_length": float64(262144),
				},
			})
		}
	}))
	defer srv.Close()
	c := New(srv.URL)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	models, err := c.Tags(ctx)
	if err != nil || len(models) != 1 || models[0].Details.ParameterSize != "4.2B" {
		t.Fatalf("tags: %v %+v", err, models)
	}
	if hs := models[0].HumanSize(); hs != "3.1 GB" {
		t.Errorf("HumanSize = %q", hs)
	}
	show, err := c.Show(ctx, "qwen3.5:4b")
	if err != nil || !show.HasCapability("tools") {
		t.Fatalf("show: %v %+v", err, show)
	}
	if show.ContextLength() != 262144 {
		t.Errorf("context length = %d", show.ContextLength())
	}
}

func TestNotFoundClassification(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(404)
		w.Write([]byte(`{"error":"model 'nope' not found"}`))
	}))
	defer srv.Close()
	c := New(srv.URL)
	_, err := c.Tags(context.Background())
	if !IsNotFound(err) {
		t.Fatalf("IsNotFound=false for %v", err)
	}
}

func TestSuggestModels(t *testing.T) {
	cands := []string{"qwen3.5:4b", "qwen2.5:7b", "llama3.2:3b", "qwen3.5:8b"}
	got := SuggestModels("qwen3.5", cands)
	if len(got) == 0 || !strings.HasPrefix(got[0], "qwen3.5") {
		t.Errorf("suggest = %v", got)
	}
}
