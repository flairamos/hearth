// Package provider selects the API client by config style:
// "ollama" (native NDJSON) or "openai" (OpenAI-compatible /v1 endpoints).
// Both expose the same interface built on the ollama package's canonical
// types, so the agent and TUI work unchanged with any backend
// (Ollama /v1, LM Studio, llama.cpp server, vLLM, ...).
package provider

import (
	"context"
	"strings"

	"hearth/internal/ollama"
)

// Client is the full surface used by the TUI and CLI.
type Client interface {
	ChatStream(ctx context.Context, req ollama.ChatRequest, fn func(ollama.ChatResponse) error) error
	Chat(ctx context.Context, req ollama.ChatRequest) (ollama.ChatResponse, error)
	Tags(ctx context.Context) ([]ollama.ModelInfo, error)
	Show(ctx context.Context, name string) (ollama.ShowResponse, error)
	Ping(ctx context.Context) error
}

// normalizeStyle resolves "auto" by probing the URL shape.
func normalizeStyle(url, style string) string {
	switch style {
	case "ollama", "openai":
		return style
	default: // auto
		if strings.HasSuffix(strings.TrimRight(url, "/"), "/v1") {
			return "openai"
		}
		return "ollama"
	}
}

// StyleFor returns the effective API style for display purposes.
func StyleFor(url, style string) string {
	return normalizeStyle(url, style)
}

// New builds the right client for a base URL and style.
//
//	base "http://localhost:11434"      style auto    → ollama native
//	base "http://localhost:11434/v1"   style auto    → openai compatible
//	base "http://host:1234"            style openai  → http://host:1234/v1
func New(baseURL, style string) Client {
	return NewWithKey(baseURL, style, "")
}

// NewWithKey is New with a bearer token for OpenAI-compatible providers
// (GLM, DeepSeek, DashScope, Volcengine Ark ...).
func NewWithKey(baseURL, style, apiKey string) Client {
	style = normalizeStyle(baseURL, style)
	switch style {
	case "openai":
		base := strings.TrimSuffix(strings.TrimRight(baseURL, "/"), "/v1")
		return newOpenAIClient(base, style, apiKey)
	default:
		base := strings.TrimRight(baseURL, "/")
		base = strings.TrimSuffix(base, "/v1") // tolerate /v1 in ollama mode
		return ollama.New(base)
	}
}
