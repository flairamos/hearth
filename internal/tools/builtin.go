package tools

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"golang.org/x/text/encoding/simplifiedchinese"
)

const (
	maxReadBytes   = 48 << 10 // per read_file call
	maxToolOut     = 30 << 10 // generic output cap
	maxGrepMatches = 120
	maxGlobResults = 200
)

func truncOut(s string, limit int) string {
	if limit <= 0 {
		limit = maxToolOut
	}
	b := []byte(s)
	if len(b) <= limit {
		return s
	}
	return string(b[:limit]) + fmt.Sprintf("\n…[output truncated, %d more bytes]", len(b)-limit)
}

// resolve joins p with the workdir and cleans it. Empty → workdir.
func (r *Registry) resolve(p string) string {
	if p == "" {
		return r.WorkDir
	}
	if filepath.IsAbs(p) {
		return filepath.Clean(p)
	}
	return filepath.Clean(filepath.Join(r.WorkDir, p))
}

func argStr(args map[string]any, key string) string {
	if v, ok := args[key].(string); ok {
		return v
	}
	return ""
}

func argInt(args map[string]any, key string) (int, bool) {
	switch v := args[key].(type) {
	case float64:
		return int(v), true
	case int:
		return v, true
	}
	return 0, false
}

// ---- read_file ----

func readFileTool() Tool {
	return Tool{
		Name:     "read_file",
		Category: CatRead,
		Desc:     "Read a text file. Returns numbered lines. Use offset/limit for large files.",
		Schema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path":   map[string]any{"type": "string", "description": "File path (relative to working directory)"},
				"offset": map[string]any{"type": "integer", "description": "1-based start line (default 1)"},
				"limit":  map[string]any{"type": "integer", "description": "Max lines to read (default 500)"},
			},
			"required": []string{"path"},
		},
		Fn: func(ctx context.Context, reg *Registry, args map[string]any) (string, error) {
			path := reg.resolve(argStr(args, "path"))
			data, err := os.ReadFile(path)
			if err != nil {
				return "", fmt.Errorf("read %s: %w", path, err)
			}
			if len(data) > maxReadBytes {
				data = data[:maxReadBytes]
			}
			if isBinary(data) {
				return "", fmt.Errorf("%s looks binary (not UTF-8 text); cannot display", path)
			}
			lines := strings.Split(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n")
			off, _ := argInt(args, "offset")
			if off < 1 {
				off = 1
			}
			lim, hasLim := argInt(args, "limit")
			if !hasLim || lim <= 0 {
				lim = 500
			}
			if off > len(lines) {
				return fmt.Sprintf("%s has %d lines; offset %d out of range", path, len(lines), off), nil
			}
			end := off - 1 + lim
			if end > len(lines) {
				end = len(lines)
			}
			var b strings.Builder
			for i := off - 1; i < end; i++ {
				fmt.Fprintf(&b, "%6d\t%s\n", i+1, lines[i])
			}
			if end < len(lines) {
				fmt.Fprintf(&b, "…[%d more lines: re-read with offset=%d]\n", len(lines)-end, end+1)
			}
			return truncOut(b.String(), 0), nil
		},
	}
}

func isBinary(b []byte) bool {
	return bytes.ContainsRune(b[:min(len(b), 1024)], 0)
}

// ---- list_dir ----

func listDirTool() Tool {
	return Tool{
		Name:     "list_dir",
		Category: CatRead,
		Desc:     "List directory entries with sizes. Directories end with '/'.",
		Schema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path": map[string]any{"type": "string", "description": "Directory path (default: working directory)"},
			},
		},
		Fn: func(ctx context.Context, reg *Registry, args map[string]any) (string, error) {
			dir := reg.resolve(argStr(args, "path"))
			entries, err := os.ReadDir(dir)
			if err != nil {
				return "", fmt.Errorf("list %s: %w", dir, err)
			}
			sort.Slice(entries, func(i, j int) bool {
				di, dj := entries[i].IsDir(), entries[j].IsDir()
				if di != dj {
					return di
				}
				return entries[i].Name() < entries[j].Name()
			})
			var b strings.Builder
			n := 0
			for _, e := range entries {
				if n >= 500 {
					fmt.Fprintf(&b, "…[more entries omitted]\n")
					break
				}
				suffix := ""
				if e.IsDir() {
					suffix = "/"
				} else if info, err := e.Info(); err == nil {
					suffix = fmt.Sprintf("  (%d B)", info.Size())
				}
				fmt.Fprintf(&b, "%s%s\n", e.Name(), suffix)
				n++
			}
			if n == 0 {
				b.WriteString("(empty directory)\n")
			}
			return b.String(), nil
		},
	}
}

// ---- glob ----

func globTool() Tool {
	return Tool{
		Name:     "glob",
		Category: CatRead,
		Desc:     "Find files by wildcard pattern. Supports ** for recursive matching. Examples: \"src/**/*.go\", \"*.md\".",
		Schema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"pattern": map[string]any{"type": "string", "description": "Glob pattern relative to working directory"},
			},
			"required": []string{"pattern"},
		},
		Fn: func(ctx context.Context, reg *Registry, args map[string]any) (string, error) {
			pattern := argStr(args, "pattern")
			if pattern == "" {
				return "", fmt.Errorf("pattern is required")
			}
			pattern = strings.TrimPrefix(pattern, "./")
			abs := reg.resolve(pattern)
			base := reg.WorkDir
			if filepath.IsAbs(pattern) {
				base = filepath.Dir(abs)
				pattern = filepath.Base(abs)
			}
			var matches []string
			err := globWalk(base, pattern, func(p string) {
				if len(matches) < maxGlobResults {
					matches = append(matches, p)
				}
			})
			if err != nil {
				return "", err
			}
			sort.Strings(matches)
			if len(matches) == 0 {
				return fmt.Sprintf("no files match %q", pattern), nil
			}
			out := strings.Join(matches, "\n")
			if len(matches) >= maxGlobResults {
				out += "\n…[result list truncated]"
			}
			return out, nil
		},
	}
}

// globWalk walks base matching a pattern that may contain **.
func globWalk(base, pattern string, fn func(string)) error {
	// split pattern into segments; "**" matches any depth
	segs := strings.Split(filepath.ToSlash(pattern), "/")
	var walk func(dir string, idx int) error
	seen := 0
	walk = func(dir string, idx int) error {
		if seen > 20000 {
			return nil
		}
		seen++
		if idx == len(segs) {
			fn(filepath.ToSlash(mustRel(base, dir)))
			return nil
		}
		seg := segs[idx]
		if seg == "**" {
			// ** matches zero or more directories
			if err := walk(dir, idx+1); err != nil {
				return err
			}
			entries, err := os.ReadDir(dir)
			if err != nil {
				return nil
			}
			for _, e := range entries {
				if e.IsDir() && e.Name() != ".git" && e.Name() != "node_modules" {
					if err := walk(filepath.Join(dir, e.Name()), idx); err != nil {
						return err
					}
				}
			}
			return nil
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			return nil
		}
		for _, e := range entries {
			if ok, _ := matchSegment(seg, e.Name()); ok {
				p := filepath.Join(dir, e.Name())
				if idx == len(segs)-1 || e.IsDir() {
					if idx == len(segs)-1 {
						fn(filepath.ToSlash(mustRel(base, p)))
					} else if e.IsDir() {
						if err := walk(p, idx+1); err != nil {
							return err
						}
					}
				}
			}
		}
		return nil
	}
	return walk(base, 0)
}

func mustRel(base, p string) string {
	r, err := filepath.Rel(base, p)
	if err != nil {
		return p
	}
	return r
}

// matchSegment matches one path segment, tolerating {a,b} alternation.
func matchSegment(seg, name string) (bool, error) {
	if seg == "*" || seg == name {
		return true, nil
	}
	return filepath.Match(seg, name)
}

// ---- grep ----

func grepTool() Tool {
	return Tool{
		Name:     "grep",
		Category: CatRead,
		Desc:     "Search file contents with a regular expression. Returns file:line: text matches.",
		Schema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"pattern":     map[string]any{"type": "string", "description": "Regular expression (Go/RE2 syntax)"},
				"path":        map[string]any{"type": "string", "description": "Directory or file to search (default: working directory)"},
				"glob":        map[string]any{"type": "string", "description": "Only search files matching this glob, e.g. \"*.go\""},
				"ignore_case": map[string]any{"type": "boolean", "description": "Case-insensitive search"},
			},
			"required": []string{"pattern"},
		},
		Fn: func(ctx context.Context, reg *Registry, args map[string]any) (string, error) {
			pattern := argStr(args, "pattern")
			if pattern == "" {
				return "", fmt.Errorf("pattern is required")
			}
			if ic, _ := argInt(args, "ignore_case"); ic == 1 || args["ignore_case"] == true {
				pattern = "(?i)" + pattern
			}
			re, err := regexp.Compile(pattern)
			if err != nil {
				return "", fmt.Errorf("bad regex: %w", err)
			}
			root := reg.resolve(argStr(args, "path"))
			globPat := argStr(args, "glob")

			var b strings.Builder
			count := 0
			filesSearched := 0
			err = walkFiles(root, globPat, func(path string, data []byte) {
				filesSearched++
				lines := strings.Split(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n")
				for i, ln := range lines {
					if re.MatchString(ln) {
						count++
						if count > maxGrepMatches {
							return
						}
						t := ln
						if len(t) > 240 {
							t = t[:240] + "…"
						}
						fmt.Fprintf(&b, "%s:%d:%s\n", mustRel(root, path), i+1, t)
					}
				}
			})
			if err != nil {
				return "", err
			}
			if count == 0 {
				return fmt.Sprintf("no matches for %q (%d files searched)", pattern, filesSearched), nil
			}
			out := b.String()
			if count > maxGrepMatches {
				out += fmt.Sprintf("\n…[%d more matches omitted]", count-maxGrepMatches)
			}
			return out, nil
		},
	}
}

// walkFiles walks root calling fn for text files (skips .git, binaries,
// huge files). If root is a file, calls fn on it directly.
func walkFiles(root, globPat string, fn func(path string, data []byte)) error {
	st, err := os.Stat(root)
	if err != nil {
		return fmt.Errorf("search %s: %w", root, err)
	}
	if !st.IsDir() {
		data, err := os.ReadFile(root)
		if err != nil {
			return nil
		}
		if !isBinary(data) {
			fn(root, data)
		}
		return nil
	}
	return filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		name := d.Name()
		if d.IsDir() {
			switch name {
			case ".git", "node_modules", "vendor", "__pycache__", ".venv", "target", "dist", ".next":
				return filepath.SkipDir
			}
			return nil
		}
		if globPat != "" {
			ok, _ := filepath.Match(globPat, name)
			if !ok {
				return nil
			}
		}
		info, err := d.Info()
		if err != nil || info.Size() > 2<<20 || info.Size() == 0 {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil || isBinary(data) {
			return nil
		}
		fn(path, data)
		return nil
	})
}

// ---- write_file ----

func writeFileTool() Tool {
	return Tool{
		Name:     "write_file",
		Category: CatMutate,
		Desc:     "Create or overwrite a file with the given content. Parent directories are created automatically.",
		Schema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path":    map[string]any{"type": "string", "description": "File path (relative to working directory)"},
				"content": map[string]any{"type": "string", "description": "Full file content to write"},
			},
			"required": []string{"path", "content"},
		},
		Fn: func(ctx context.Context, reg *Registry, args map[string]any) (string, error) {
			path := reg.resolve(argStr(args, "path"))
			content, ok := args["content"].(string)
			if !ok {
				return "", fmt.Errorf("content must be a string")
			}
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				return "", err
			}
			if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
				return "", err
			}
			return fmt.Sprintf("wrote %s (%d bytes)", path, len(content)), nil
		},
	}
}

// ---- edit_file ----

func editFileTool() Tool {
	return Tool{
		Name:     "edit_file",
		Category: CatMutate,
		Desc:     "Replace an exact string in a file. old_string must match exactly once; include enough surrounding lines to make it unique.",
		Schema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path":       map[string]any{"type": "string", "description": "File to edit"},
				"old_string": map[string]any{"type": "string", "description": "Exact text to find (must be unique in the file)"},
				"new_string": map[string]any{"type": "string", "description": "Replacement text (empty string deletes)"},
			},
			"required": []string{"path", "old_string", "new_string"},
		},
		Fn: func(ctx context.Context, reg *Registry, args map[string]any) (string, error) {
			path := reg.resolve(argStr(args, "path"))
			oldS, ok1 := args["old_string"].(string)
			newS, ok2 := args["new_string"].(string)
			if !ok1 || !ok2 {
				return "", fmt.Errorf("old_string and new_string must be strings")
			}
			if oldS == "" {
				return "", fmt.Errorf("old_string must not be empty")
			}
			data, err := os.ReadFile(path)
			if err != nil {
				return "", fmt.Errorf("read %s: %w", path, err)
			}
			// CRLF-safe: match on normalized \n text, restore the file's
			// original line-ending convention on write (Windows files).
			crlf := strings.Contains(string(data), "\r\n")
			src := strings.ReplaceAll(string(data), "\r\n", "\n")
			count := strings.Count(src, oldS)
			switch count {
			case 0:
				// help the model recover: show a near line if possible
				hint := nearestLine(src, oldS)
				if hint != "" {
					return "", fmt.Errorf("old_string not found in %s. Closest matching line: %s", path, hint)
				}
				return "", fmt.Errorf("old_string not found in %s", path)
			case 1:
			default:
				return "", fmt.Errorf("old_string matches %d places in %s; add surrounding lines to make it unique", count, path)
			}
			edited := strings.Replace(src, oldS, newS, 1)
			if crlf {
				edited = strings.ReplaceAll(edited, "\n", "\r\n")
			}
			if err := os.WriteFile(path, []byte(edited), 0o644); err != nil {
				return "", err
			}
			return fmt.Sprintf("edited %s: replaced %d chars with %d chars", path, len(oldS), len(newS)), nil
		},
	}
}

// nearestLine finds the file line most similar to s (simple containment of
// the longest word in s).
func nearestLine(src, s string) string {
	words := strings.Fields(s)
	best, bestScore := "", 0
	for _, ln := range strings.Split(src, "\n") {
		score := 0
		for _, w := range words {
			if len(w) < 4 {
				continue
			}
			if strings.Contains(ln, w) {
				score++
			}
		}
		if score > bestScore {
			bestScore, best = score, ln
		}
	}
	if bestScore < 2 {
		return ""
	}
	t := best
	if len(t) > 200 {
		t = t[:200] + "…"
	}
	return strings.TrimSpace(t)
}

// ---- bash (shell) ----

// shellCommand picks the interpreter for a command string. Windows-first
// per product policy: auto resolves to PowerShell (native, predictable,
// bash-compatible aliases like cat/ls/rm), then cmd. bash.exe is NOT
// auto-detected on Windows because System32 ships a WSL launcher with the
// same name whose paths don't translate — opt in via agent.shell="bash".
//
// The PowerShell paths force UTF-8 console output: CJK error messages come
// back in the system codepage (GBK) otherwise and render as mojibake.
func shellCommand(pref, cmdStr string) (string, []string) {
	const psUTF8 = "[Console]::OutputEncoding=[System.Text.Encoding]::UTF8; "
	switch pref {
	case "bash":
		if runtime.GOOS == "windows" {
			return "bash.exe", []string{"-lc", cmdStr}
		}
		return "bash", []string{"-c", cmdStr}
	case "cmd":
		return "cmd", []string{"/C", cmdStr}
	case "powershell":
		return powershellName(), []string{"-NoProfile", "-NonInteractive", "-Command", psUTF8 + cmdStr}
	}
	// auto
	if runtime.GOOS == "windows" {
		if _, err := exec.LookPath("powershell.exe"); err == nil {
			return "powershell.exe", []string{"-NoProfile", "-NonInteractive", "-Command", psUTF8 + cmdStr}
		}
		return "cmd", []string{"/C", cmdStr}
	}
	return "bash", []string{"-c", cmdStr}
}

// decodeConsoleOutput rescues non-UTF-8 console output on Windows: tools
// and cmd.exe emit the system codepage (GBK on zh-CN systems), which turns
// CJK text into mojibake when read as UTF-8.
func decodeConsoleOutput(res string) string {
	if utf8.ValidString(res) {
		return res
	}
	if runtime.GOOS != "windows" {
		return res
	}
	if decoded, err := simplifiedchinese.GBK.NewDecoder().String(res); err == nil && utf8.ValidString(decoded) {
		return decoded
	}
	return res
}

func powershellName() string {
	if runtime.GOOS == "windows" {
		if _, err := exec.LookPath("pwsh.exe"); err == nil {
			return "pwsh.exe"
		}
		return "powershell.exe"
	}
	return "pwsh"
}

func bashTool() Tool {
	return Tool{
		Name:     "bash",
		Category: CatExec,
		Desc:     "Execute a shell command and return stdout/stderr plus the exit code. Working directory is the project root. On Windows this runs in PowerShell (bash if installed).",
		Schema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"command":     map[string]any{"type": "string", "description": "Shell command to run"},
				"timeout_sec": map[string]any{"type": "integer", "description": "Timeout in seconds (default from config)"},
			},
			"required": []string{"command"},
		},
		Fn: func(ctx context.Context, reg *Registry, args map[string]any) (string, error) {
			cmdStr := argStr(args, "command")
			if cmdStr == "" {
				return "", fmt.Errorf("command is required")
			}
			timeout := time.Duration(reg.bashTimeout()) * time.Second
			if t, ok := argInt(args, "timeout_sec"); ok && t > 0 {
				timeout = time.Duration(t) * time.Second
			}
			cctx, cancel := context.WithTimeout(ctx, timeout)
			defer cancel()

			name, argv := shellCommand(reg.ShellPref, cmdStr)
			cmd := exec.CommandContext(cctx, name, argv...)
			cmd.Dir = reg.WorkDir
			var out bytes.Buffer
			cmd.Stdout = &out
			cmd.Stderr = &out
			runErr := cmd.Run()
			res := decodeConsoleOutput(out.String())
			exit := 0
			if runErr != nil {
				if ee, ok := runErr.(*exec.ExitError); ok {
					exit = ee.ExitCode()
				} else {
					return "", fmt.Errorf("failed to run: %v", runErr)
				}
			}
			var b strings.Builder
			fmt.Fprintf(&b, "exit=%d\n%s", exit, truncOut(res, 0))
			if cctx.Err() == context.DeadlineExceeded {
				fmt.Fprintf(&b, "\n[timed out after %s]", timeout)
			}
			return b.String(), nil
		},
	}
}

func (r *Registry) bashTimeout() int {
	if r == nil || r.BashTimeoutSec <= 0 {
		return 120
	}
	return r.BashTimeoutSec
}
