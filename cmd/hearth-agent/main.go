// Command hearth-agent is the headless agent harness: scriptable, CI-friendly,
// zero TUI dependencies — independently installable from hearth.
package main

import (
	"flag"
	"fmt"
	"os"
	"runtime"
	"strings"

	"hearth/internal/cli"
	"hearth/internal/config"
	"hearth/internal/version"
)

func main() {
	code := run(os.Args[1:])
	if code != 0 && runtime.GOOS == "windows" && windowSession() {
		// keep a double-clicked console window readable on failure
		fmt.Fprintln(os.Stderr, "\n[press Enter to close]")
		var line [1]byte
		_, _ = os.Stdin.Read(line[:])
	}
	os.Exit(code)
}

// windowSession heuristically detects a double-click launch (interactive
// stdin, no parent TUI). Harmless if wrong: it only adds a pause prompt.
func windowSession() bool {
	fi, err := os.Stdin.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}

func run(args []string) int {
	if len(args) == 0 {
		usage()
		return 2
	}
	switch args[0] {
	case "agent":
		return runAgentCmd(args[1:])
	case "ask":
		return runAskCmd(args[1:])
	case "models":
		fs := flag.NewFlagSet("models", flag.ExitOnError)
		u := fs.String("url", "", "backend base url override")
		style := fs.String("api-style", "", "api style override: ollama | openai")
		_ = fs.Parse(args[1:])
		return cli.Models(*u, *style)
	case "version", "--version", "-v":
		fmt.Println("hearth-agent", version.Version)
		return 0
	case "help", "--help", "-h":
		usage()
		return 0
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q — see: hearth-agent help\n", args[0])
		return 2
	}
}

func usage() {
	fmt.Printf(`hearth-agent %s — headless agent harness for local LLM backends
(scriptable / CI-friendly; the interactive TUI ships separately as hearth)

usage:
  hearth-agent agent "task"    run an agent task with tools (read/write/bash/web/memory)
  hearth-agent ask "question"  single question, no tools
  hearth-agent models          list backend models
  hearth-agent version         print version

agent flags:
  -m <model>          model name (default: config or first backend model)
  -w <dir>            working directory for the agent
  --yolo              auto-approve all tool actions (access=full)
  --perm <mode>       ask | risky | full (default: config)
  --max-steps <n>     tool step budget (default: config)
  --json              emit NDJSON events for scripting
  --show-thinking     stream model thinking to stderr
  -url <url>          backend base url override (auto-discovered if unreachable)
  -api-style <s>      ollama | openai (default: auto)

config: %s
backends auto-discovered: Ollama :11434 · LM Studio :1234 · llama.cpp :8080 · vLLM :8000 · Jan :1337
`, version.Version, config.Path())
}

func runAgentCmd(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, `usage: hearth-agent agent "task" [-m model] [-w dir] [--yolo] [--json]`)
		return 2
	}
	fs := flag.NewFlagSet("agent", flag.ExitOnError)
	model := fs.String("m", "", "model name")
	workdir := fs.String("w", "", "working directory")
	yolo := fs.Bool("yolo", false, "auto-approve all tool actions")
	perm := fs.String("perm", "", "permission mode: ask|risky|full")
	maxSteps := fs.Int("max-steps", 0, "tool step budget")
	jsonOut := fs.Bool("json", false, "NDJSON event output")
	showThink := fs.Bool("show-thinking", false, "stream thinking to stderr")
	url := fs.String("url", "", "backend base url override")
	style := fs.String("api-style", "", "api style override: ollama | openai")
	provider := fs.String("provider", "", "config provider profile (remote OpenAI-compatible)")
	_ = fs.Parse(args)

	rest := fs.Args()
	if len(rest) == 0 {
		fmt.Fprintln(os.Stderr, "missing task argument")
		return 2
	}

	return cli.RunAgent(cli.AgentOptions{
		Task:      strings.Join(rest, " "),
		Model:     *model,
		Provider:  *provider,
		WorkDir:   *workdir,
		Perms:     *perm,
		Yolo:      *yolo,
		MaxSteps:  *maxSteps,
		JSON:      *jsonOut,
		ShowThink: *showThink,
		URL:       *url,
		APIStyle:  *style,
	})
}

func runAskCmd(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, `usage: hearth-agent ask "question" [-m model] [-url url] [-api-style ollama|openai]`)
		return 2
	}
	fs := flag.NewFlagSet("ask", flag.ExitOnError)
	model := fs.String("m", "", "model name")
	url := fs.String("url", "", "backend base url override")
	style := fs.String("api-style", "", "api style override: ollama | openai")
	_ = fs.Parse(args)
	rest := fs.Args()
	if len(rest) == 0 {
		fmt.Fprintln(os.Stderr, "missing question")
		return 2
	}
	return cli.AskWithStyle(strings.Join(rest, " "), *model, *url, *style)
}
