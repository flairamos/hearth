package provider

import (
	"context"
	"fmt"
	"strings"
	"time"

	"hearth/internal/ollama"
)

// Candidate is a well-known local backend endpoint.
type Candidate struct {
	URL   string
	Style string // ollama | openai
	Label string // human-readable server name
}

// DefaultCandidates lists the fixed, well-known addresses of local LLM
// servers (ollama API 地址是固定的 — plus the other common ones).
var DefaultCandidates = []Candidate{
	{URL: "http://localhost:11434", Style: "ollama", Label: "Ollama"},
	{URL: "http://localhost:11434", Style: "openai", Label: "Ollama (OpenAI /v1)"},
	{URL: "http://localhost:1234", Style: "openai", Label: "LM Studio"},
	{URL: "http://localhost:8080", Style: "openai", Label: "llama.cpp server"},
	{URL: "http://localhost:8000", Style: "openai", Label: "vLLM"},
	{URL: "http://localhost:1337", Style: "openai", Label: "Jan"},
	{URL: "http://127.0.0.1:11434", Style: "ollama", Label: "Ollama (127.0.0.1)"},
}

// probeURL pings one candidate with a short timeout.
func probeURL(ctx context.Context, c Candidate) error {
	client := New(c.URL, c.Style)
	pctx, cancel := context.WithTimeout(ctx, 1200*time.Millisecond)
	defer cancel()
	return client.Ping(pctx)
}

// Discover probes well-known local backends and returns the first live one.
// preferred (config URL) is tried first regardless of the list. timeout
// bounds the whole search.
func Discover(preferred, style string, timeout time.Duration) (Candidate, error) {
	return DiscoverIn(DefaultCandidates, preferred, style, timeout)
}

// DiscoverIn is Discover with an injectable candidate list (tests).
func DiscoverIn(candidates []Candidate, preferred, style string, timeout time.Duration) (Candidate, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	if preferred != "" {
		c := Candidate{URL: preferred, Style: normalizeStyle(preferred, style), Label: "configured"}
		if err := probeURL(ctx, c); err == nil {
			return c, nil
		}
	}
	var tried []string
	for _, c := range candidates {
		if c.URL == preferred {
			continue // already tried
		}
		if err := probeURL(ctx, c); err == nil {
			return c, nil
		}
		tried = append(tried, c.URL)
		if ctx.Err() != nil {
			break
		}
	}
	return Candidate{}, fmt.Errorf("no local LLM backend found (tried %s)", strings.Join(tried, ", "))
}

// EnsureBackend resolves the working backend: config URL first, then
// auto-discovery. It mutates cfg when discovery succeeds (callers persist).
// Returns the client and the detected candidate (label "" when the config
// URL already worked).
func EnsureBackend(url, style string, discover bool, timeout time.Duration) (Client, Candidate, error) {
	cfgCandidate := Candidate{URL: url, Style: normalizeStyle(url, style)}
	if err := probeURL(context.Background(), cfgCandidate); err == nil {
		return New(url, style), Candidate{}, nil
	}
	if !discover {
		return New(url, style), Candidate{}, fmt.Errorf("backend unreachable at %s", url)
	}
	found, err := Discover(url, style, timeout)
	if err != nil {
		return New(url, style), Candidate{}, err
	}
	return New(found.URL, found.Style), found, nil
}

// ModelNames is a small helper for "did you mean" flows.
func ModelNames(models []ollama.ModelInfo) []string {
	out := make([]string, 0, len(models))
	for _, m := range models {
		out = append(out, m.Name)
	}
	return out
}
