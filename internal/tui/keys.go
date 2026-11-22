package tui

import (
	"sort"
	"strings"
)

// defaultKeys maps action names to default key sequences.
// Note: ctrl+m is physically identical to Enter in terminals (both send CR),
// so the model panel uses alt+m instead (PRD 4.1.4 conflict rule).
var defaultKeys = map[string]string{
	"send":           "enter",
	"palette":        "ctrl+k",
	"sessions":       "ctrl+p",
	"models":         "alt+m",
	"new_session":    "ctrl+n",
	"clear_screen":   "ctrl+l",
	"toggle_sidebar": "ctrl+b",
	"toggle_agent":   "alt+a",
	"toggle_think":   "ctrl+t",
	"access_mode":    "ctrl+g",
	"copy_last":      "ctrl+y",
	"interrupt":      "esc",
	"help":           "?",
	"quit":           "ctrl+c",
}

// KeyMap resolves action → key with config overrides.
type KeyMap struct {
	keys map[string]string
}

func NewKeyMap(overrides map[string]string) *KeyMap {
	km := &KeyMap{keys: map[string]string{}}
	for k, v := range defaultKeys {
		km.keys[k] = v
	}
	for k, v := range overrides {
		if _, ok := defaultKeys[k]; ok && v != "" {
			km.keys[k] = strings.ToLower(v)
		}
	}
	return km
}

// Get returns the key sequence bound to an action.
func (k *KeyMap) Get(action string) string {
	if s, ok := k.keys[action]; ok {
		return s
	}
	return ""
}

// Matches reports whether the pressed key string matches the action.
func (k *KeyMap) Matches(action, pressed string) bool {
	return pressed == k.keys[action]
}

// Sorted returns actions ordered by name for help rendering.
func (k *KeyMap) Sorted() [][2]string {
	var out [][2]string
	for a, key := range k.keys {
		out = append(out, [2]string{a, key})
	}
	sort.Slice(out, func(i, j int) bool { return out[i][0] < out[j][0] })
	return out
}
