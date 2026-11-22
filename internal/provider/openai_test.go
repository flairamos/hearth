package provider

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"hearth/internal/ollama"
)

func TestStyleDetection(t *testing.T) {
	cases := []struct {
		url, style, want string
	}{
		{"http://localhost:11434", "auto", "ollama"},
		{"http://localhost:11434/v1", "auto", "openai"},
		{"http://localhost:11434/v1/", "auto", "openai"},
		{"http://x:1234", "openai", "openai"},
		{"http://localhost:11434/v1", "ollama", "ollama"},
	}
	for _, c := range cases {
		if got := StyleFor(c.url, c.style); got != c.want {
			t.Errorf("StyleFor(%q,%q)=%q want %q", c.url, c.style, got, c.want)
		}
	}
}

func TestOpenAIChatStreamSSE(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/v1/chat/completions") {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		chunks := []string{
			`{"choices":[{"index":0,"delta":{"role":"assistant","reasoning":"think "},"finish_reason":null}]}`,
			`{"choices":[{"index":0,"delta":{"reasoning":"hard"},"finish_reason":null}]}`,
			`{"choices":[{"index":0,"delta":{"content":"Hel"},"finish_reason":null}]}`,
			`{"choices":[{"index":0,"delta":{"content":"lo"},"finish_reason":null}]}`,
			`{"choices":[{"index":0,"delta":{"content":""},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":4}}`,
		}
		for _, c := range chunks {
			w.Write([]byte("data: " + c + "\n\n"))
		}
		w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer srv.Close()

	// URL WITHOUT /v1 → client appends /v1
	c := New(srv.URL, "openai")
	var content, thinking strings.Builder
	var done bool
	var pt, ct int
	err := c.ChatStream(context.Background(), ollama.ChatRequest{Model: "m"}, func(r ollama.ChatResponse) error {
		content.WriteString(r.Message.Content)
		thinking.WriteString(r.Message.Thinking)
		if r.Done {
			done = true
			pt, ct = r.PromptEvalCount, r.EvalCount
		}
		return nil
	})
	if err != nil || !done || content.String() != "Hello" || thinking.String() != "think hard" || pt != 10 || ct != 4 {
		t.Fatalf("stream: err=%v done=%v content=%q think=%q pt=%d ct=%d", err, done, content.String(), thinking.String(), pt, ct)
	}
}

func TestOpenAIChatStreamToolCallFragments(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		chunks := []string{
			`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"get_w","arguments":"{\"ci"}}]}}]}`,
			`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"ty\":\"Beijing\"}"}}]}}]}`,
			`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":1,"id":"call_2","type":"function","function":{"name":"second","arguments":"{}"}}]}}]}`,
			`{"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
		}
		for _, c := range chunks {
			w.Write([]byte("data: " + c + "\n\n"))
		}
		w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer srv.Close()
	c := New(srv.URL+"/v1", "openai") // with /v1 suffix
	var last ollama.ChatResponse
	err := c.ChatStream(context.Background(), ollama.ChatRequest{Model: "m"}, func(r ollama.ChatResponse) error {
		last = r
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(last.Message.ToolCalls) != 2 {
		t.Fatalf("tool calls = %d: %+v", len(last.Message.ToolCalls), last.Message.ToolCalls)
	}
	tc := last.Message.ToolCalls[0]
	if tc.Function.Name != "get_w" || tc.Function.Arguments["city"] != "Beijing" {
		t.Errorf("fragment merge failed: %+v", tc)
	}
}

func TestOpenAINonStreamAndErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/v1/chat/completions") {
			var req map[string]any
			json.NewDecoder(r.Body).Decode(&req)
			if req["model"] == "badmodel" {
				w.WriteHeader(404)
				w.Write([]byte(`{"error":{"message":"model 'badmodel' not found","type":"not_found_error"}}`))
				return
			}
			json.NewEncoder(w).Encode(map[string]any{
				"choices": []map[string]any{{
					"index": 0,
					"message": map[string]any{
						"role": "assistant", "content": "answer",
						"reasoning": "why",
						"tool_calls": []map[string]any{{
							"id": "call_x", "type": "function",
							"function": map[string]any{"name": "t1", "arguments": `{"a":1}`},
						}},
					},
					"finish_reason": "tool_calls",
				}},
				"usage": map[string]any{"prompt_tokens": 5, "completion_tokens": 7},
			})
			return
		}
		w.Write([]byte(`{"data":[{"id":"m1"},{"id":"m2"}]}`))
	}))
	defer srv.Close()
	c := New(srv.URL, "openai")

	resp, err := c.Chat(context.Background(), ollama.ChatRequest{Model: "m"})
	if err != nil || resp.Message.Content != "answer" || resp.Message.Thinking != "why" ||
		len(resp.Message.ToolCalls) != 1 || resp.Message.ToolCalls[0].Function.Arguments["a"] != float64(1) {
		t.Fatalf("chat: err=%v resp=%+v", err, resp)
	}

	_, err = c.Chat(context.Background(), ollama.ChatRequest{Model: "badmodel"})
	if !ollama.IsNotFound(err) {
		t.Fatalf("IsNotFound=false for %v", err)
	}

	models, err := c.Tags(context.Background())
	if err != nil || len(models) != 2 || models[0].Name != "m1" {
		t.Fatalf("tags: %v %+v", err, models)
	}
}

func TestOpenAIRequestTranslation(t *testing.T) {
	// verify tool result + assistant tool_calls + vision content translation
	think := false
	req := ollama.ChatRequest{
		Model: "m",
		Messages: []ollama.Message{
			{Role: "user", Content: "look", Images: []string{"abc123"}},
			{Role: "assistant", Content: "calling", ToolCalls: []ollama.ToolCall{{
				Function: ollama.ToolCallFunc{Name: "t1", Arguments: map[string]any{"x": 1}},
			}}},
			{Role: "tool", Content: "result", ToolName: "t1"},
		},
		Options: &ollama.Options{Temperature: 0.3, NumCtx: 8192, NumPredict: 100},
		Think:   &think,
	}
	wire := toOARequest(req)
	if wire.MaxTokens != 100 || wire.Temperature != 0.3 {
		t.Errorf("options mapping wrong: %+v", wire)
	}
	if len(wire.Messages) != 3 {
		t.Fatalf("messages = %d", len(wire.Messages))
	}
	// user message with images becomes content parts
	parts, ok := wire.Messages[0].Content.([]map[string]any)
	if !ok || len(parts) != 2 || parts[1]["type"] != "image_url" {
		t.Errorf("vision parts wrong: %+v", wire.Messages[0].Content)
	}
	// tool result carries matching tool_call_id
	if wire.Messages[2].ToolCallID != "call_t1" {
		t.Errorf("tool_call_id = %q", wire.Messages[2].ToolCallID)
	}
	if os.Getenv("FAIL") != "" {
		t.Skip()
	}
}
