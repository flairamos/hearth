package tui

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"hearth/internal/core"
)

// ---- @ file references ----

var refPattern = regexp.MustCompile(`@([^\s@#]+)(?:#L?(\d+)-(\d+))?`)

// workspaceFiles gathers candidate files for the @ picker (relative paths).
func WorkspaceFiles(root string, cap int) []string {
	if cap <= 0 {
		cap = 500
	}
	var out []string
	skipDirs := map[string]bool{
		".git": true, "node_modules": true, "vendor": true, "__pycache__": true,
		".venv": true, "venv": true, "target": true, "dist": true, ".next": true,
		"build": true, ".idea": true, ".vscode": true, "__fixtures__": true,
	}
	filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if skipDirs[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		if len(out) >= cap {
			return filepath.SkipAll
		}
		rel, err := filepath.Rel(root, p)
		if err != nil || rel == "." {
			return nil
		}
		if st, err := d.Info(); err == nil && st.Size() > 1<<20 {
			return nil // skip huge files in the picker
		}
		out = append(out, filepath.ToSlash(rel))
		return nil
	})
	return out
}

// collectRefs snapshots "@path" and "@path#L121-135" tokens from the user's
// text. The display text stays as typed; the API layer appends the content
// blocks from the stored Refs so history stays reproducible.
func CollectRefs(text, workDir string) ([]core.FileRef, []string) {
	ms := refPattern.FindAllStringSubmatch(text, -1)
	if len(ms) == 0 {
		return nil, nil
	}
	var refs []core.FileRef
	var missing []string
	for _, m := range ms {
		path := strings.TrimSpace(m[1])
		start, end := 0, 0
		if m[2] != "" {
			start, _ = strconv.Atoi(m[2])
			end, _ = strconv.Atoi(m[3])
			if start < 1 {
				start = 1
			}
			if end < start {
				end = start
			}
		}
		resolved := path
		if !filepath.IsAbs(resolved) {
			resolved = filepath.Join(workDir, filepath.FromSlash(resolved))
		}
		content, err := readRefFile(resolved, start, end, 12000)
		if err != nil {
			missing = append(missing, path)
			continue
		}
		refs = append(refs, core.FileRef{Path: path, Start: start, End: end, Content: content})
	}
	return refs, missing
}

// RefContextBlock delegates to core: it renders one file ref as model
// context, shared with the agent and web layers.
func RefContextBlock(r core.FileRef) string { return core.RefContextBlock(r) }

func readRefFile(path string, start, end, maxChars int) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	text := strings.ReplaceAll(string(data), "\r\n", "\n")
	if start > 0 {
		lines := strings.Split(text, "\n")
		if start > len(lines) {
			return "", fmt.Errorf("offset %d beyond file (%d lines)", start, len(lines))
		}
		if end > len(lines) {
			end = len(lines)
		}
		text = strings.Join(lines[start-1:end], "\n")
	}
	if len([]rune(text)) > maxChars {
		text = string([]rune(text)[:maxChars]) + "\n…[truncated]"
	}
	return strings.TrimRight(text, "\n"), nil
}

// ---- clipboard (^Y) ----

// copyToClipboard copies s using the native tool for the platform
// (Windows-first: clip.exe), falling back to OSC52 which works over SSH.
func copyToClipboard(s string) (bool, string) {
	tools := [][]string{
		{"clip"},                             // Windows
		{"wl-copy"},                          // Wayland
		{"xclip", "-selection", "clipboard"}, // X11
		{"pbcopy"},                           // macOS
	}
	for _, t := range tools {
		if _, err := exec.LookPath(t[0]); err != nil {
			continue
		}
		cmd := exec.Command(t[0], t[1:]...)
		cmd.Stdin = strings.NewReader(s)
		if err := cmd.Run(); err == nil {
			return true, t[0]
		}
	}
	return false, ""
}
