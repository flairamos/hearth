package agent

import (
	"encoding/json"
	"path/filepath"
	"regexp"
	"strings"

	"hearth/internal/tools"
)

// Permission modes (three tiers):
//
//	ask   — 变更前询问: every mutation (write/edit/bash/memory_save) prompts.
//	risky — 有风险时询问: safe mutations auto-run; anything risky prompts.
//	        bash auto-runs only a read-only allowlist; file writes must stay
//	        inside the working directory.
//	full  — 完全自主: everything auto-runs; deny patterns still force a prompt.
const (
	ModeAsk   = "ask"
	ModeRisky = "risky"
	ModeFull  = "full"
)

// NormalizeMode maps legacy/alias names to the canonical three modes.
func NormalizeMode(m string) string {
	switch strings.ToLower(strings.TrimSpace(m)) {
	case "ask", "standard", "restricted", "default":
		return ModeAsk
	case "risky", "guarded", "risk":
		return ModeRisky
	case "full", "yolo", "auto":
		return ModeFull
	default:
		return ModeAsk
	}
}

// ModeDesc returns the human-readable description shown in the UI.
func ModeDesc(m string) string {
	switch m {
	case ModeAsk:
		return "ask: confirm every change"
	case ModeRisky:
		return "risky: ask only for risky actions"
	case ModeFull:
		return "full: fully autonomous"
	}
	return m
}

// ApprovalDecision is the user's answer to a permission prompt.
type ApprovalDecision int

const (
	DecisionOnce ApprovalDecision = iota
	DecisionSession
	DecisionDeny
)

// ApprovalRequest is sent to the UI when a tool needs approval.
type ApprovalRequest struct {
	ID       string
	Tool     string
	Summary  string // one-line human summary (command / path)
	Detail   string // pretty args JSON
	Category tools.Category
	Respond  chan ApprovalDecision
}

// Decide sends the user's decision (safe if the UI is gone).
func (r *ApprovalRequest) Decide(d ApprovalDecision) {
	defer func() { recover() }()
	r.Respond <- d
}

// safeCommandPrefixes auto-run in risky mode (read-only shell commands,
// including common Windows PowerShell equivalents).
var safeCommandPrefixes = []string{
	"ls", "cat", "head", "tail", "grep", "rg", "find", "pwd", "echo", "wc",
	"file", "stat", "du", "df", "tree", "which", "whoami", "date", "env",
	"printenv", "dir", "type", "get-childitem", "get-content", "gci", "gc",
	"git status", "git log", "git diff", "git show", "git branch",
	"git remote", "git blame", "git rev-parse",
	"python3 --version", "python --version", "node --version", "go version",
	"cargo --version", "ollama list", "ollama show", "ollama ps",
}

// builtinRiskyRe forces a prompt in every mode, including full.
var builtinRiskyRe = []*regexp.Regexp{
	regexp.MustCompile(`(?i)\brm\s+(-\w+\s+)*-?\w*r\w*f`), // rm -rf / -fr
	regexp.MustCompile(`(?i)\bmkfs(\.\w+)?\b`),
	regexp.MustCompile(`(?i)\bdd\b.*of=/dev/`),
	regexp.MustCompile(`:\(\)\{.*\};:`), // fork bomb
	regexp.MustCompile(`(?i)\b(shutdown|reboot|halt|poweroff)\b`),
	regexp.MustCompile(`(?i)\bchmod\s+-R\s+777\b`),
	regexp.MustCompile(`(?i)\bsudo\b`),
	regexp.MustCompile(`(?i)\b(curl|wget)\b[^|]*\|\s*(ba)?sh\b`), // curl | sh
	regexp.MustCompile(`(?i)\bgit\s+(push\s+(-f|--force)|reset\s+--hard|clean\s+-[fd])`),
	regexp.MustCompile(`(?i)\bdel\s+/[sq]`), // windows del /s /q
	regexp.MustCompile(`(?i)\brd\s+/s`),     // windows rd /s
	regexp.MustCompile(`(?i)\bformat\s+[a-zA-Z]:`),
	regexp.MustCompile(`(?i)remove-item\s+.*-recurse`),
	regexp.MustCompile(`(?i)>\s*/dev/(sd|nvme|hd)`),
}

// Manager gates tool execution.
type Manager struct {
	mode            string // ask | risky | full
	workDir         string
	autoApprove     map[string]bool
	denyPatterns    []*regexp.Regexp
	sessionApproved map[string]bool
}

// NewManager builds a permission manager from config values.
func NewManager(mode, workDir string, autoApprove, denyPatterns []string) *Manager {
	m := &Manager{
		mode:            NormalizeMode(mode),
		workDir:         filepath.Clean(workDir),
		autoApprove:     map[string]bool{},
		sessionApproved: map[string]bool{},
	}
	for _, t := range autoApprove {
		m.autoApprove[t] = true
	}
	for _, p := range denyPatterns {
		if re, err := regexp.Compile("(?i)" + p); err == nil {
			m.denyPatterns = append(m.denyPatterns, re)
		}
	}
	m.denyPatterns = append(m.denyPatterns, builtinRiskyRe...)
	return m
}

// Mode returns the current mode.
func (m *Manager) Mode() string { return m.mode }

// SetMode switches the permission tier at runtime (/access).
func (m *Manager) SetMode(mode string) { m.mode = NormalizeMode(mode) }

// insideWorkDir reports whether path resolves within the working directory.
func (m *Manager) insideWorkDir(path string) bool {
	if m.workDir == "" || m.workDir == "." || path == "" {
		return false
	}
	p := filepath.Clean(path)
	if !filepath.IsAbs(p) {
		p = filepath.Join(m.workDir, p)
	}
	rel, err := filepath.Rel(m.workDir, p)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// NeedsApproval reports whether the tool call requires user approval.
// Deny patterns override everything (even full mode) and force a prompt.
func (m *Manager) NeedsApproval(tool string, cat tools.Category, summary string, detail string) bool {
	for _, re := range m.denyPatterns {
		if summary != "" && re.MatchString(summary) {
			return true
		}
	}
	if m.sessionApproved[tool] {
		return false
	}
	if cat == tools.CatRead {
		return false
	}
	if m.autoApprove[tool] {
		return false
	}

	switch m.mode {
	case ModeFull:
		return false

	case ModeRisky:
		switch tool {
		case "bash":
			return !isSafeCommand(summary)
		case "write_file", "edit_file":
			return !m.insideWorkDir(extractPath(detail))
		case "memory_save":
			return false
		default:
			return true
		}

	default: // ask — confirm every change
		return true
	}
}

// isSafeCommand matches the read-only allowlist (risky-mode bash). Chained
// commands (| ; &&) must have every segment on the allowlist.
func isSafeCommand(cmd string) bool {
	c := strings.ToLower(strings.TrimSpace(cmd))
	if c == "" {
		return false
	}
	for _, part := range strings.FieldsFunc(c, func(r rune) bool { return r == '|' || r == ';' }) {
		p := strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(part), "&"))
		p = strings.TrimPrefix(p, "!")
		if p == "" {
			continue
		}
		ok := false
		for _, pre := range safeCommandPrefixes {
			if strings.HasPrefix(p, pre) {
				ok = true
				break
			}
		}
		if !ok {
			return false
		}
	}
	return true
}

// extractPath pulls the path value from a tool-call detail JSON string.
func extractPath(detail string) string {
	var raw struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal([]byte(detail), &raw); err == nil && raw.Path != "" {
		return raw.Path
	}
	return detail
}

// ApproveSession marks a tool approved for the rest of the session.
func (m *Manager) ApproveSession(tool string) {
	m.sessionApproved[tool] = true
}

// ApprovedSummary renders current session-level approvals (for the sidebar).
func (m *Manager) ApprovedSummary() string {
	var out []string
	for t := range m.sessionApproved {
		out = append(out, t)
	}
	if len(out) == 0 {
		return ""
	}
	return strings.Join(out, ", ")
}
