// Command hearth is the interactive terminal chat client & agent UI for
// local LLM backends — the hearth where your models stay warm. The headless
// automation harness ships separately as hearth-agent (independently
// installable).
package main

import (
	"flag"
	"fmt"
	"os"
	"runtime"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"hearth/internal/config"
	"hearth/internal/provider"
	"hearth/internal/store"
	"hearth/internal/tui"
	"hearth/internal/version"
	"hearth/internal/web"
)

func main() {
	args := os.Args[1:]
	if len(args) == 0 {
		runTUI(args)
		return
	}
	switch args[0] {
	case "version", "--version", "-v":
		fmt.Println("hearth", version.Version)
	case "help", "--help", "-h":
		usage()
	default:
		// flags for TUI mode
		runTUI(args)
	}
}

func usage() {
	fmt.Printf(`hearth %s — interactive terminal chat client & agent UI for local LLM backends
(headless automation ships separately as hearth-agent)

usage:
  hearth                       start the TUI
  hearth [-s <session-id>]     resume a session
  hearth version               print version

tui flags:
  -url <url>          backend base url override (auto-discovered if unreachable)
  -api-style <s>      ollama | openai (default: auto)
  -s <session-id>     resume session by id
  -m <model>          model override

config: %s
sessions: %s
backends auto-discovered: Ollama :11434 · LM Studio :1234 · llama.cpp :8080 · vLLM :8000 · Jan :1337
`, version.Version, config.Path(), config.SessionsDir())
}

func runTUI(args []string) {
	fs := flag.NewFlagSet("hearth", flag.ExitOnError)
	urlFlag := fs.String("url", "", "backend base url override")
	sessionID := fs.String("s", "", "resume session id")
	modelFlag := fs.String("m", "", "model override")
	styleFlag := fs.String("api-style", "", "api style override: ollama | openai")
	webFlag := fs.Bool("web", false, "browser GUI on localhost instead of the TUI")
	addrFlag := fs.String("addr", "127.0.0.1:8787", "web UI listen address")
	noOpen := fs.Bool("no-open", false, "do not open the browser automatically")
	_ = fs.Parse(args)

	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintln(os.Stderr, "config error:", err)
		os.Exit(1)
	}
	if *urlFlag != "" {
		cfg.OllamaURL = strings.TrimRight(*urlFlag, "/")
	}
	if *styleFlag != "" {
		cfg.APIStyle = *styleFlag
	}
	if *modelFlag != "" {
		cfg.DefaultModel = *modelFlag
	}

	if *webFlag {
		st, err := store.New(config.SessionsDir())
		if err != nil {
			fmt.Fprintln(os.Stderr, "storage error:", err)
			os.Exit(1)
		}
		srv, err := web.New(cfg, st)
		if err != nil {
			fmt.Fprintln(os.Stderr, "web error:", err)
			os.Exit(1)
		}
		if err := srv.Run(*addrFlag, !*noOpen); err != nil {
			fmt.Fprintln(os.Stderr, "web server:", err)
			os.Exit(1)
		}
		return
	}

	// backend resolution: config URL first, then auto-discovery of the
	// well-known local addresses (Ollama :11434, LM Studio :1234, ...)
	fmt.Fprintln(os.Stderr, "probing local backends…")
	client, found, err := provider.EnsureBackend(cfg.OllamaURL, cfg.APIStyle, cfg.Discovery, 6*time.Second)
	if err != nil {
		// keep going: the TUI shows the guided error card with retry
		fmt.Fprintf(os.Stderr, "note: %v\n", err)
	} else if found.URL != "" {
		cfg.OllamaURL = found.URL
		cfg.APIStyle = found.Style
		_ = cfg.Save() // remember for next start
		fmt.Fprintf(os.Stderr, "auto-detected %s at %s (saved to config)\n", found.Label, found.URL)
	}

	st, err := store.New(config.SessionsDir())
	if err != nil {
		fmt.Fprintln(os.Stderr, "storage error:", err)
		pauseIfDoubleClicked()
		os.Exit(1)
	}

	app, err := tui.New(cfg, client, st, *sessionID)
	if err != nil {
		fmt.Fprintln(os.Stderr, "init error:", err)
		pauseIfDoubleClicked()
		os.Exit(1)
	}

	p := tea.NewProgram(app,
		tea.WithAltScreen(),
		tea.WithMouseCellMotion(),
	)
	app.SetProgram(p)

	// DECAWM off: over-wide lines clip at the right edge instead of wrapping.
	// Our width math can disagree with the terminal's actual emoji/ambiguous
	// glyph rendering; a wrapped line makes the frame taller than the screen,
	// which scrolls the header/status/input chrome away (消失的输入框).
	fmt.Fprint(os.Stdout, "\x1b[?7l")
	_, runErr := p.Run()
	fmt.Fprint(os.Stdout, "\x1b[?7h") // always restore autowrap

	if runErr != nil {
		fmt.Fprintln(os.Stderr, "fatal:", runErr)
		pauseIfDoubleClicked()
		os.Exit(1)
	}
}

// pauseIfDoubleClicked keeps the console window open on Windows when the
// app was launched by double-click and hit a fatal error, so the message
// is readable instead of the window flashing shut.
func pauseIfDoubleClicked() {
	if runtime.GOOS != "windows" {
		return
	}
	fmt.Fprintln(os.Stderr, "\n[press Enter to close]")
	var line [1]byte
	_, _ = os.Stdin.Read(line[:])
}
