// Package tools implements the built-in agent tools.
package tools

import (
	"context"

	"hearth/internal/core"
)

// Category classifies a tool by risk level.
type Category string

const (
	CatRead   Category = "read"   // safe, auto-approved
	CatMutate Category = "mutate" // writes files, needs approval
	CatExec   Category = "exec"   // runs commands, needs approval
)

// Tool is a built-in agent tool.
type Tool struct {
	Name     string
	Desc     string
	Schema   map[string]any
	Category Category
	Fn       func(ctx context.Context, reg *Registry, args map[string]any) (string, error)
}

// Registry holds all built-in tools for one working directory. Session and
// Memory are optional wiring: the scratchpad (note tool) and long-term
// memory tools degrade gracefully when unset.
type Registry struct {
	WorkDir        string
	BashTimeoutSec int
	ShellPref      string         // auto | bash | powershell | cmd
	Session        *core.Session  // for the note scratchpad tool
	Memory         *MemoryStore   // nil disables memory tools
	Search         *SearchBackend // nil → duckduckgo default
}

// SearchBackend selects the web_search engine implementation.
type SearchBackend struct {
	Engine     string
	SearxngURL string
	BraveKey   string
	MaxResults int
}

// All returns every built-in tool in a stable order (read tools first,
// matching the system prompt presentation order).
func All() []Tool {
	return []Tool{
		readFileTool(),
		listDirTool(),
		globTool(),
		grepTool(),
		calculatorTool(),
		noteTool(),
		memoryRecallTool(nil),
		memorySaveTool(nil),
		webFetchTool(),
		webSearchTool(),
		writeFileTool(),
		editFileTool(),
		bashTool(),
	}
}

// Get looks up a tool by name.
func Get(name string) (Tool, bool) {
	for _, t := range All() {
		if t.Name == name {
			return t, true
		}
	}
	return Tool{}, false
}

// Names returns all tool names.
func Names() []string {
	var out []string
	for _, t := range All() {
		out = append(out, t.Name)
	}
	return out
}
