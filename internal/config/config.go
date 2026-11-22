// Package config loads and saves the hearth configuration file.
package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// GenerationConfig holds sampling / generation parameters.
type GenerationConfig struct {
	Temperature float64 `json:"temperature"`
	TopP        float64 `json:"top_p"`
	TopK        int     `json:"top_k"`
	MaxTokens   int     `json:"max_tokens"`
	Think       *bool   `json:"think"` // nil = model default
}

// ContextConfig controls the model context window handling.
type ContextConfig struct {
	NumCtx       int    `json:"num_ctx"`
	AutoTrim     bool   `json:"auto_trim"`
	AutoCompact  bool   `json:"auto_compact"`   // summarize (not just trim) when near the limit
	CompactAtPct int    `json:"compact_at_pct"` // auto-compact trigger, % of num_ctx
	KeepAlive    string `json:"keep_alive"`     // model residency, e.g. "30m" (native API)
}

// AgentConfig controls the agent harness.
type AgentConfig struct {
	MaxSteps      int      `json:"max_steps"`
	Permissions   string   `json:"permissions"` // ask | risky | full (aliases: standard, yolo)
	AutoApprove   []string `json:"auto_approve"`
	DenyPatterns  []string `json:"deny_patterns"`
	BashTimeout   int      `json:"bash_timeout_sec"` // per command
	Temperature   float64  `json:"temperature"`      // 0 = use generation.temperature
	MaxToolResult int      `json:"max_tool_result_chars"`
	Shell         string   `json:"shell"`  // auto | bash | powershell | cmd
	Memory        bool     `json:"memory"` // long-term memory across sessions
}

// SearchConfig controls the web_search tool backend.
type SearchConfig struct {
	Engine     string `json:"engine"`      // duckduckgo | searxng | brave | none
	SearxngURL string `json:"searxng_url"` // e.g. http://localhost:8080
	BraveKey   string `json:"brave_key"`
	MaxResults int    `json:"max_results"`
}

// ProviderProfile is a remote OpenAI-compatible service profile (GLM,
// DeepSeek, DashScope/Qwen, Volcengine Ark ... all speak the same protocol).
type ProviderProfile struct {
	URL    string `json:"url"`             // base url, /v1 appended if missing
	APIKey string `json:"api_key"`         // secret, or "$ENV_VAR" reference
	Model  string `json:"model,omitempty"` // default model for this provider
}

// ResolvedKey expands a "$ENV_VAR" reference in the api_key field, keeping
// secrets out of the config file.
func (p ProviderProfile) ResolvedKey() string {
	k := strings.TrimSpace(p.APIKey)
	if strings.HasPrefix(k, "$") {
		return os.Getenv(k[1:])
	}
	return k
}

// CustomCommand is a user-defined prompt-template slash command.
type CustomCommand struct {
	Desc   string `json:"description"`
	Prompt string `json:"prompt"` // may contain {{input}}
	Agent  bool   `json:"agent"`  // run through agent mode
}

// UIConfig holds interface prefs.
type UIConfig struct {
	Sidebar bool `json:"sidebar"`
}

// Config is the on-disk configuration.
type Config struct {
	OllamaURL      string                     `json:"ollama_url"`
	APIStyle       string                     `json:"api_style"` // auto | ollama | openai
	Discovery      bool                       `json:"discovery"` // auto-detect local backends when the configured URL is dead
	DefaultModel   string                     `json:"default_model"`
	Theme          string                     `json:"theme"`
	Generation     GenerationConfig           `json:"generation"`
	Context        ContextConfig              `json:"context"`
	Keymap         map[string]string          `json:"keymap"`
	SystemPrompt   string                     `json:"system_prompt"`
	AutoTitle      bool                       `json:"auto_title"`
	Agent          AgentConfig                `json:"agent"`
	Search         SearchConfig               `json:"search"`
	Providers      map[string]ProviderProfile `json:"providers,omitempty"`
	ActiveProvider string                     `json:"active_provider,omitempty"` // "" = local backend
	Commands       map[string]CustomCommand   `json:"commands"`
	Favorites      []string                   `json:"model_favorites"`
	UI             UIConfig                   `json:"ui"`
}

// Defaults returns the built-in default configuration.
func Defaults() *Config {
	think := true
	return &Config{
		OllamaURL:    "http://localhost:11434",
		DefaultModel: "",
		Theme:        "dark",
		Generation: GenerationConfig{
			Temperature: 0.7,
			TopP:        0.9,
			MaxTokens:   4096,
			Think:       &think,
		},
		Context: ContextConfig{
			NumCtx:       16384,
			AutoTrim:     true,
			AutoCompact:  true,
			CompactAtPct: 85,
			KeepAlive:    "30m",
		},
		Keymap:    map[string]string{},
		AutoTitle: true,
		Agent: AgentConfig{
			MaxSteps:      25,
			Permissions:   "standard",
			BashTimeout:   120,
			MaxToolResult: 8000,
			Shell:         "auto",
			Memory:        true,
		},
		Discovery:      true,
		Search:         SearchConfig{Engine: "bing", MaxResults: 5},
		Providers:      map[string]ProviderProfile{},
		ActiveProvider: "",
		Commands:       map[string]CustomCommand{},
		UI:             UIConfig{Sidebar: true},
	}
}

// pathCandidates returns config file locations, newest-brand first with
// legacy ollama-tui locations as fallback (seamless migration: an existing
// legacy config keeps working; Save always writes the primary new path).
func pathCandidates() []string {
	if p := os.Getenv("HEARTH_CONFIG"); p != "" {
		return []string{p}
	}
	if p := os.Getenv("OLLAMA_TUI_CONFIG"); p != "" { // legacy env
		return []string{p}
	}
	var out []string
	if d, err := os.UserConfigDir(); err == nil {
		out = append(out, filepath.Join(d, "hearth", "config.json"))
		out = append(out, filepath.Join(d, "ollama-tui", "config.json")) // legacy
	}
	if home, err := os.UserHomeDir(); err == nil {
		out = append(out, filepath.Join(home, ".hearth.json"))
		out = append(out, filepath.Join(home, ".ollama-tui.json")) // legacy
	}
	return out
}

// Load reads the config file, falling back to defaults. Missing file is not
// an error; malformed JSON is.
func Load() (*Config, error) {
	cfg := Defaults()
	path := ""
	for _, p := range pathCandidates() {
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			path = p
			break
		}
	}
	if path == "" {
		return cfg, nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return cfg, err
	}
	if err := json.Unmarshal(b, cfg); err != nil {
		return nil, err
	}
	cfg.normalize()
	return cfg, nil
}

// Path returns the primary config file path (where Save writes).
func Path() string {
	ps := pathCandidates()
	if len(ps) > 0 {
		return ps[0]
	}
	return "hearth.json"
}

// Save writes the config to the primary path, creating directories.
func (c *Config) Save() error {
	p := Path()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

// normalize clamps values into sane ranges.
func (c *Config) normalize() {
	if c.OllamaURL == "" {
		c.OllamaURL = "http://localhost:11434"
	}
	c.OllamaURL = strings.TrimRight(c.OllamaURL, "/")
	if c.Generation.Temperature <= 0 || c.Generation.Temperature >= 2 {
		c.Generation.Temperature = 0.7
	}
	if c.Generation.MaxTokens <= 0 {
		c.Generation.MaxTokens = 4096
	}
	if c.Context.NumCtx < 2048 {
		c.Context.NumCtx = 2048
	}
	if c.Context.CompactAtPct <= 50 || c.Context.CompactAtPct > 100 {
		c.Context.CompactAtPct = 85
	}
	if c.Context.KeepAlive != "" && c.Context.KeepAlive != "-1" {
		if _, err := time.ParseDuration(c.Context.KeepAlive); err != nil {
			c.Context.KeepAlive = "30m"
		}
	}
	if c.Agent.MaxSteps <= 0 {
		c.Agent.MaxSteps = 25
	}
	if c.Agent.BashTimeout <= 0 {
		c.Agent.BashTimeout = 120
	}
	if c.Agent.MaxToolResult <= 0 {
		c.Agent.MaxToolResult = 8000
	}
	switch c.Agent.Permissions {
	case "standard", "yolo", "restricted":
		// legacy aliases → new three-tier names
		if c.Agent.Permissions == "standard" || c.Agent.Permissions == "restricted" {
			c.Agent.Permissions = "ask"
		} else {
			c.Agent.Permissions = "full"
		}
	case "ask", "risky", "full":
	default:
		c.Agent.Permissions = "ask"
	}
	switch c.Agent.Shell {
	case "auto", "bash", "powershell", "cmd":
	default:
		c.Agent.Shell = "auto"
	}
	switch c.Search.Engine {
	case "duckduckgo", "searxng", "brave", "bing", "none":
	default:
		c.Search.Engine = "bing" // match Defaults() and the config reference
	}
	if c.Search.MaxResults <= 0 || c.Search.MaxResults > 10 {
		c.Search.MaxResults = 5
	}
	switch c.APIStyle {
	case "auto", "ollama", "openai":
	default:
		c.APIStyle = "auto"
	}
	for name, p := range c.Providers {
		p.URL = strings.TrimRight(strings.TrimSpace(p.URL), "/")
		if p.URL == "" || name == "" {
			delete(c.Providers, name)
			continue
		}
		c.Providers[name] = p
	}
	if c.ActiveProvider != "" && c.ActiveProvider != "local" {
		if _, ok := c.Providers[c.ActiveProvider]; !ok {
			c.ActiveProvider = ""
		}
	}
	switch c.Theme {
	case "dark", "light", "dracula":
	default:
		c.Theme = "dark"
	}
}

// ---- data paths ----

// DataDir returns the base directory for app data (sessions, exports,
// memory). Prefers the hearth directory; falls back to the legacy
// ollama-tui directory when it already exists so old sessions keep working.
func DataDir() string {
	if p := os.Getenv("HEARTH_DATA"); p != "" {
		return p
	}
	if p := os.Getenv("OLLAMA_TUI_DATA"); p != "" { // legacy env
		return p
	}
	hearth, legacy := "", ""
	switch runtime.GOOS {
	case "windows":
		if d, err := os.UserConfigDir(); err == nil {
			hearth = filepath.Join(d, "hearth")
			legacy = filepath.Join(d, "ollama-tui")
		}
	default:
		if d := os.Getenv("XDG_DATA_HOME"); d != "" {
			hearth = filepath.Join(d, "hearth")
			legacy = filepath.Join(d, "ollama-tui")
		} else if home, err := os.UserHomeDir(); err == nil {
			hearth = filepath.Join(home, ".local", "share", "hearth")
			legacy = filepath.Join(home, ".local", "share", "ollama-tui")
		}
	}
	if hearth == "" {
		return "."
	}
	if st, err := os.Stat(legacy); err == nil && st.IsDir() {
		if st2, err2 := os.Stat(hearth); err2 != nil || !st2.IsDir() {
			return legacy // migrate lazily: keep using existing legacy data
		}
	}
	return hearth
}

// SessionsDir returns the directory holding session JSON files.
func SessionsDir() string {
	return filepath.Join(DataDir(), "sessions")
}
