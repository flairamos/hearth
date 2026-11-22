package tui

import (
	"strings"
	"sync"
)

// deltaBuf accumulates streamed deltas with two views:
//   - Total(): cumulative text (for final message assembly and live render)
//   - Drain(): only the pending tail (for batched UI flushes)
type deltaBuf struct {
	mu    sync.Mutex
	allT  strings.Builder
	allC  strings.Builder
	tailT strings.Builder
	tailC strings.Builder
}

func (b *deltaBuf) Add(thinking, content string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if thinking != "" {
		b.allT.WriteString(thinking)
		b.tailT.WriteString(thinking)
	}
	if content != "" {
		b.allC.WriteString(content)
		b.tailC.WriteString(content)
	}
}

// Drain returns and clears the pending tail.
func (b *deltaBuf) Drain() (thinking, content string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	t, c := b.tailT.String(), b.tailC.String()
	b.tailT.Reset()
	b.tailC.Reset()
	return t, c
}

// Total returns everything accumulated so far.
func (b *deltaBuf) Total() (thinking, content string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.allT.String(), b.allC.String()
}
