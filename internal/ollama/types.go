// Package ollama implements a client for the Ollama HTTP API.
package ollama

import (
	"strconv"
	"strings"
	"time"
)

// Options mirrors Ollama's model runtime options.
type Options struct {
	Temperature float64 `json:"temperature,omitempty"`
	TopP        float64 `json:"top_p,omitempty"`
	TopK        int     `json:"top_k,omitempty"`
	NumCtx      int     `json:"num_ctx,omitempty"`
	NumPredict  int     `json:"num_predict,omitempty"`
	Seed        int     `json:"seed,omitempty"`
	RepeatPen   float64 `json:"repeat_penalty,omitempty"`
}

// ToolFunc is a function tool definition (OpenAI-style JSON schema).
type ToolFunc struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Parameters  map[string]any `json:"parameters"`
}

// Tool is a tool advertised to the model.
type Tool struct {
	Type     string   `json:"type"` // always "function"
	Function ToolFunc `json:"function"`
}

// ToolCallFunc is a tool call emitted by the model.
type ToolCallFunc struct {
	Name      string         `json:"name"`
	Arguments map[string]any `json:"arguments"`
}

type ToolCall struct {
	Function ToolCallFunc `json:"function"`
}

// Message is one entry of the /api/chat message list.
type Message struct {
	Role      string     `json:"role"`
	Content   string     `json:"content"`
	Thinking  string     `json:"thinking,omitempty"`
	ToolCalls []ToolCall `json:"tool_calls,omitempty"`
	ToolName  string     `json:"tool_name,omitempty"`
	Images    []string   `json:"images,omitempty"` // base64 (no data: prefix)
}

// ChatRequest is the /api/chat request body.
type ChatRequest struct {
	Model    string    `json:"model"`
	Messages []Message `json:"messages"`
	Stream   bool      `json:"stream"`
	Think    *bool     `json:"think,omitempty"`
	Tools    []Tool    `json:"tools,omitempty"`
	Options  *Options  `json:"options,omitempty"`
	// KeepAlive keeps the model resident between calls (native API only),
	// e.g. "30m" or "-1". Prevents reload latency in multi-step agent runs.
	KeepAlive string `json:"keep_alive,omitempty"`
}

// ChatResponse is one NDJSON chunk of /api/chat (streaming or final).
type ChatResponse struct {
	Model              string    `json:"model"`
	CreatedAt          time.Time `json:"created_at"`
	Message            Message   `json:"message"`
	Done               bool      `json:"done"`
	DoneReason         string    `json:"done_reason"`
	TotalDuration      int64     `json:"total_duration"`
	LoadDuration       int64     `json:"load_duration"`
	PromptEvalCount    int       `json:"prompt_eval_count"`
	EvalCount          int       `json:"eval_count"`
	EvalDuration       int64     `json:"eval_duration"`
	PromptEvalDuration int64     `json:"prompt_eval_duration"`
	Error              string    `json:"error"`
}

// Details is the shared details block of /api/tags and /api/show.
type Details struct {
	Family            string `json:"family"`
	ParameterSize     string `json:"parameter_size"`
	QuantizationLevel string `json:"quantization_level"`
}

// ModelInfo is one entry of /api/tags.
type ModelInfo struct {
	Name       string    `json:"name"`
	Model      string    `json:"model"`
	Size       int64     `json:"size"`
	ModifiedAt time.Time `json:"modified_at"`
	Details    Details   `json:"details"`
}

// HumanSize formats m.Size as a human readable byte string.
func (m ModelInfo) HumanSize() string {
	v := float64(m.Size)
	units := []string{"B", "KB", "MB", "GB", "TB"}
	u := 0
	for u < len(units)-1 && v >= 1024 {
		v /= 1024
		u++
	}
	prec := 1
	if u == 0 || v >= 100 {
		prec = 0
	}
	return strconv.FormatFloat(v, 'f', prec, 64) + " " + units[u]
}

// ShowResponse is the /api/show response.
type ShowResponse struct {
	Capabilities []string       `json:"capabilities"`
	Details      Details        `json:"details"`
	ModelInfo    map[string]any `json:"model_info"`
}

// ContextLength extracts the model's native context length from model_info.
// Ollama returns flat keys like "qwen35.context_length": 262144.
func (s ShowResponse) ContextLength() int {
	for k, v := range s.ModelInfo {
		if k == "context_length" || strings.HasSuffix(k, ".context_length") {
			if f, ok := v.(float64); ok {
				return int(f)
			}
		}
	}
	return 0
}

// HasCapability reports whether the model lists the given capability
// (completion, tools, vision, thinking, embedding...).
func (s ShowResponse) HasCapability(cap string) bool {
	for _, c := range s.Capabilities {
		if c == cap {
			return true
		}
	}
	return false
}
