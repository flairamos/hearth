package provider

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"hearth/internal/ollama"
)

// openAIClient talks to any OpenAI-compatible chat completions endpoint
// (Ollama /v1, LM Studio, llama.cpp server, vLLM, ...). Requests and
// responses are translated to/from the ollama package's canonical types.
type openAIClient struct {
	base  string // without /v1
	style string
	http  *http.Client
}

func newOpenAIClient(base, style string) *openAIClient {
	return &openAIClient{base: base, style: style, http: &http.Client{Timeout: 0}}
}

func (c *openAIClient) url(path string) string { return c.base + "/v1" + path }

// ---- wire types ----

type oaMessage struct {
	Role       string       `json:"role"`
	Content    any          `json:"content"` // string or []parts
	Name       string       `json:"name,omitempty"`
	ToolCalls  []oaToolCall `json:"tool_calls,omitempty"`
	ToolCallID string       `json:"tool_call_id,omitempty"`
}

type oaToolCall struct {
	Index    int    `json:"index,omitempty"`
	ID       string `json:"id,omitempty"`
	Type     string `json:"type,omitempty"` // "function"
	Function oaFunc `json:"function"`
}

type oaFunc struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"` // JSON string in OpenAI wire format
}

type oaTool struct {
	Type     string    `json:"type"` // "function"
	Function oaToolDef `json:"function"`
}

type oaToolDef struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Parameters  map[string]any `json:"parameters"`
}

type oaRequest struct {
	Model         string       `json:"model"`
	Messages      []oaMessage  `json:"messages"`
	Stream        bool         `json:"stream"`
	StreamOptions *oaStreamOpt `json:"stream_options,omitempty"`
	Tools         []oaTool     `json:"tools,omitempty"`
	Temperature   float64      `json:"temperature,omitempty"`
	TopP          float64      `json:"top_p,omitempty"`
	MaxTokens     int          `json:"max_tokens,omitempty"`
	Seed          int          `json:"seed,omitempty"`
}

type oaStreamOpt struct {
	IncludeUsage bool `json:"include_usage"`
}

type oaChoiceDelta struct {
	Role             string       `json:"role"`
	Content          string       `json:"content"`
	Reasoning        string       `json:"reasoning"`
	ReasoningContent string       `json:"reasoning_content"` // DeepSeek-style
	ToolCalls        []oaToolCall `json:"tool_calls,omitempty"`
}

type oaChunk struct {
	ID      string `json:"id"`
	Choices []struct {
		Index        int           `json:"index"`
		Delta        oaChoiceDelta `json:"delta"`
		Message      *oaMsgFull    `json:"message,omitempty"` // non-stream
		FinishReason string        `json:"finish_reason"`
	} `json:"choices"`
	Usage *oaUsage `json:"usage"`
	Error *oaError `json:"error"`
}

type oaMsgFull struct {
	Role      string       `json:"role"`
	Content   string       `json:"content"`
	Reasoning string       `json:"reasoning"`
	ToolCalls []oaToolCall `json:"tool_calls,omitempty"`
}

type oaUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	TotalTokens      int `json:"total_tokens"`
	CompletionTokens int `json:"completion_tokens"`
}

type oaError struct {
	Message string `json:"message"`
	Type    string `json:"type"`
	Code    any    `json:"code"`
}

func (e *oaError) err(status int) error {
	return &ollama.ModelError{Status: status, Body: e.Message}
}

// ---- request translation ----

func contentParts(m ollama.Message) any {
	if len(m.Images) == 0 {
		return m.Content
	}
	parts := []map[string]any{{"type": "text", "text": m.Content}}
	for _, img := range m.Images {
		parts = append(parts, map[string]any{
			"type":      "image_url",
			"image_url": map[string]string{"url": "data:" + sniffImageMime(img) + ";base64," + img},
		})
	}
	return parts
}

// sniffImageMime decodes the leading bytes of a base64 image to pick the
// right data-URI media type (strict OpenAI-compatible servers validate it).
func sniffImageMime(b64 string) string {
	n := len(b64)
	if n > 32 {
		n = 32
	}
	head, err := base64.StdEncoding.DecodeString(b64[:n])
	if err != nil {
		return "image/png"
	}
	switch {
	case len(head) > 8 && head[0] == 0x89 && string(head[1:4]) == "PNG":
		return "image/png"
	case len(head) > 3 && head[0] == 0xFF && head[1] == 0xD8 && head[2] == 0xFF:
		return "image/jpeg"
	case len(head) > 6 && string(head[0:3]) == "GIF":
		return "image/gif"
	case len(head) > 12 && string(head[8:12]) == "WEBP":
		return "image/webp"
	}
	return "image/png"
}

func toOARequest(req ollama.ChatRequest) oaRequest {
	out := oaRequest{Model: req.Model, Stream: req.Stream}
	if req.Stream {
		out.StreamOptions = &oaStreamOpt{IncludeUsage: true}
	}
	for _, m := range req.Messages {
		switch m.Role {
		case "tool":
			out.Messages = append(out.Messages, oaMessage{
				Role: "tool", Content: m.Content, ToolCallID: "call_" + m.ToolName,
			})
		case "assistant":
			am := oaMessage{Role: "assistant", Content: m.Content}
			for _, tc := range m.ToolCalls {
				b, _ := json.Marshal(tc.Function.Arguments)
				am.ToolCalls = append(am.ToolCalls, oaToolCall{
					ID: "call_" + tc.Function.Name, Type: "function",
					Function: oaFunc{Name: tc.Function.Name, Arguments: string(b)},
				})
			}
			out.Messages = append(out.Messages, am)
		case "user":
			out.Messages = append(out.Messages, oaMessage{Role: "user", Content: contentParts(m)})
		default:
			out.Messages = append(out.Messages, oaMessage{Role: m.Role, Content: m.Content})
		}
	}
	for _, t := range req.Tools {
		out.Tools = append(out.Tools, oaTool{
			Type:     "function",
			Function: oaToolDef{Name: t.Function.Name, Description: t.Function.Description, Parameters: t.Function.Parameters},
		})
	}
	if req.Options != nil {
		out.Temperature = req.Options.Temperature
		out.TopP = req.Options.TopP
		out.MaxTokens = req.Options.NumPredict
		out.Seed = req.Options.Seed
	}
	return out
}

// ---- response translation helpers ----

// toolAccum merges streamed tool_call fragments (arguments arrive in pieces,
// keyed by their index field).
type toolAccum struct {
	names map[int]string
	args  map[int]*strings.Builder
}

func newToolAccum() *toolAccum {
	return &toolAccum{names: map[int]string{}, args: map[int]*strings.Builder{}}
}

func (a *toolAccum) add(tcs []oaToolCall) {
	for _, tc := range tcs {
		idx := tc.Index
		if tc.Function.Name != "" {
			a.names[idx] = tc.Function.Name
		}
		if _, ok := a.args[idx]; !ok {
			a.args[idx] = &strings.Builder{}
		}
		a.args[idx].WriteString(tc.Function.Arguments)
	}
}

func (a *toolAccum) calls() []ollama.ToolCall {
	idxs := make([]int, 0, len(a.args))
	for i := range a.args {
		idxs = append(idxs, i)
	}
	sort.Ints(idxs)
	var out []ollama.ToolCall
	for _, i := range idxs {
		name := a.names[i]
		if name == "" {
			continue
		}
		args := map[string]any{}
		if s := a.args[i].String(); s != "" {
			if err := json.Unmarshal([]byte(s), &args); err != nil {
				args = map[string]any{"_raw": s}
			}
		}
		out = append(out, ollama.ToolCall{Function: ollama.ToolCallFunc{Name: name, Arguments: args}})
	}
	return out
}

func toOllamaResponse(chunk oaChunk) ollama.ChatResponse {
	out := ollama.ChatResponse{}
	if chunk.Error != nil {
		out.Error = chunk.Error.Message
	}
	if u := chunk.Usage; u != nil {
		out.PromptEvalCount = u.PromptTokens
		out.EvalCount = u.CompletionTokens
	}
	if len(chunk.Choices) == 0 {
		return out
	}
	choice := chunk.Choices[0]
	m := ollama.Message{Role: "assistant"}
	if choice.Delta.Role != "" || choice.Delta.Content != "" || choice.Delta.Reasoning != "" || choice.Delta.ReasoningContent != "" || len(choice.Delta.ToolCalls) > 0 {
		m.Content = choice.Delta.Content
		m.Thinking = choice.Delta.Reasoning
		if m.Thinking == "" {
			m.Thinking = choice.Delta.ReasoningContent
		}
		out.Message = m
	}
	if choice.Message != nil {
		m.Content = choice.Message.Content
		m.Thinking = choice.Message.Reasoning
		out.Message = m
	}
	if choice.FinishReason == "tool_calls" || choice.FinishReason == "stop" {
		out.Done = true
		out.DoneReason = choice.FinishReason
	}
	return out
}

// ---- ChatStream ----

const retryBudget = 3

func (c *openAIClient) ChatStream(ctx context.Context, req ollama.ChatRequest, fn func(ollama.ChatResponse) error) error {
	wire := toOARequest(req)
	wire.Stream = true
	wire.StreamOptions = &oaStreamOpt{IncludeUsage: true}
	var lastErr error
	for attempt := 0; attempt < retryBudget; attempt++ {
		if attempt > 0 {
			time.Sleep(time.Duration(500*(1<<attempt)) * time.Millisecond)
		}
		got := false
		err := c.streamOnce(ctx, wire, func(r ollama.ChatResponse) error {
			got = true
			return fn(r)
		})
		if err == nil {
			return nil
		}
		if got || !ollama.IsConnError(err) || ctx.Err() != nil {
			return err
		}
		lastErr = err
	}
	return lastErr
}

func (c *openAIClient) streamOnce(ctx context.Context, wire oaRequest, fn func(ollama.ChatResponse) error) error {
	body, _ := json.Marshal(wire)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url("/chat/completions"), strings.NewReader(string(body)))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
		return parseOAError(resp.StatusCode, b)
	}

	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64*1024), 8<<20)
	accum := newToolAccum()
	doneSent := false
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "[DONE]" {
			break
		}
		var chunk oaChunk
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			return fmt.Errorf("bad SSE chunk: %w", err)
		}
		if chunk.Error != nil {
			return chunk.Error.err(500)
		}
		// collect streamed tool_call fragments
		if len(chunk.Choices) > 0 && len(chunk.Choices[0].Delta.ToolCalls) > 0 {
			for _, tc := range chunk.Choices[0].Delta.ToolCalls {
				if tc.Function.Name != "" {
					accum.names[tc.Index] = tc.Function.Name
				}
			}
			accum.add(chunk.Choices[0].Delta.ToolCalls)
		}
		r := toOllamaResponse(chunk)
		if r.Done && len(accum.args) > 0 {
			r.Message.ToolCalls = accum.calls()
			if r.Message.Content == "" {
				r.Message.Content = ""
			}
		}
		if r.Done {
			doneSent = true
		}
		if err := fn(r); err != nil {
			return err
		}
	}
	if !doneSent {
		// some servers skip finish_reason; still close the turn
		_ = fn(ollama.ChatResponse{Done: true, DoneReason: "stop", Message: ollama.Message{
			Role:      "assistant",
			ToolCalls: accum.calls(),
		}})
	}
	return sc.Err()
}

// ---- Chat ----

func (c *openAIClient) Chat(ctx context.Context, req ollama.ChatRequest) (ollama.ChatResponse, error) {
	wire := toOARequest(req)
	wire.Stream = false
	body, _ := json.Marshal(wire)
	req2, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url("/chat/completions"), strings.NewReader(string(body)))
	if err != nil {
		return ollama.ChatResponse{}, err
	}
	req2.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req2)
	if err != nil {
		return ollama.ChatResponse{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
		return ollama.ChatResponse{}, parseOAError(resp.StatusCode, b)
	}
	var chunk oaChunk
	if err := json.NewDecoder(resp.Body).Decode(&chunk); err != nil {
		return ollama.ChatResponse{}, err
	}
	if chunk.Error != nil {
		return ollama.ChatResponse{}, chunk.Error.err(resp.StatusCode)
	}
	out := toOllamaResponse(chunk)
	if len(chunk.Choices) > 0 {
		msg := chunk.Choices[0].Message
		out.Message = ollama.Message{
			Role:     "assistant",
			Content:  msg.Content,
			Thinking: firstNonEmpty(msg.Reasoning),
		}
		for _, tc := range msg.ToolCalls {
			args := map[string]any{}
			if tc.Function.Arguments != "" {
				_ = json.Unmarshal([]byte(tc.Function.Arguments), &args)
			}
			out.Message.ToolCalls = append(out.Message.ToolCalls, ollama.ToolCall{
				Function: ollama.ToolCallFunc{Name: tc.Function.Name, Arguments: args},
			})
		}
	}
	out.Done = true
	return out, nil
}

func firstNonEmpty(ss ...string) string {
	for _, s := range ss {
		if s != "" {
			return s
		}
	}
	return ""
}

// ---- models / show / ping ----

func (c *openAIClient) Tags(ctx context.Context) ([]ollama.ModelInfo, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.url("/models"), nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
		return nil, parseOAError(resp.StatusCode, b)
	}
	var out struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	var models []ollama.ModelInfo
	for _, m := range out.Data {
		models = append(models, ollama.ModelInfo{Name: m.ID, Model: m.ID})
	}
	return models, nil
}

func (c *openAIClient) Show(ctx context.Context, name string) (ollama.ShowResponse, error) {
	// OpenAI-compat has no /show; derive nothing. Capabilities unknown.
	_ = ctx
	_ = name
	return ollama.ShowResponse{}, nil
}

func (c *openAIClient) Ping(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()
	_, err := c.Tags(ctx)
	return err
}

// parseOAError converts OpenAI-style error bodies into ollama.ModelError so
// the existing classification helpers (IsNotFound/IsContextOverflow) work.
func parseOAError(status int, body []byte) error {
	var e oaError
	if err := json.Unmarshal(body, &e); err == nil && e.Message != "" {
		return e.err(status)
	}
	var alt struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(body, &alt); err == nil && alt.Error != "" {
		return &ollama.ModelError{Status: status, Body: alt.Error}
	}
	return &ollama.ModelError{Status: status, Body: strings.TrimSpace(string(body))}
}
