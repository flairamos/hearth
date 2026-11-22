package core

import (
	"strings"
	"sync"
	"testing"
)

// TestSessionConcurrency hammers one session from multiple goroutines the
// way the app does: agent loop appends, UI thread snapshots/renders, store
// clones for save. Run with -race to validate.
func TestSessionConcurrency(t *testing.T) {
	s := NewSession("m")
	s.Add(Message{Role: RoleSystem, Content: "sys"})

	var wg sync.WaitGroup
	stop := make(chan struct{})

	// writer goroutine (agent loop)
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			s.Add(Message{Role: RoleAssistant, Content: strings.Repeat("x", 50) + string(rune('a'+i%26))})
		}
	}()

	// reader goroutines (UI render + save)
	for g := 0; g < 3; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				_ = s.Snapshot()
				_ = s.UsedTokens()
				_ = s.Clone()
				_ = s.LastUserText()
				_ = s.Notes()
			}
		}()
	}

	// scratchpad writer (note tool)
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			s.SetScratchpad(strings.Repeat("n", i%100))
		}
	}()

	// trimmer (auto context trim)
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			s.TrimToMax(100000)
		}
	}()

	// brief hammering window
	for i := 0; i < 2000; i++ {
		s.Add(Message{Role: RoleUser, Content: "turn"})
	}
	close(stop)
	wg.Wait()

	if s.Len() < 2000 {
		t.Fatalf("messages lost: %d", s.Len())
	}
	c := s.Clone()
	if len(c.Messages) != s.Len() {
		t.Fatal("clone mismatch")
	}
}

func TestPopLastUserTurn(t *testing.T) {
	s := NewSession("m")
	s.Add(Message{Role: RoleUser, Content: "first"})
	s.Add(Message{Role: RoleAssistant, Content: "reply"})
	s.Add(Message{Role: RoleUser, Content: "second"})
	s.Add(Message{Role: RoleAssistant, Content: "reply2"})

	text, ok := s.PopLastUserTurn()
	if !ok || text != "second" {
		t.Fatalf("pop = %q %v", text, ok)
	}
	if s.Len() != 2 {
		t.Fatalf("len = %d, want 2", s.Len())
	}
	// next pop removes the first turn
	text, ok = s.PopLastUserTurn()
	if !ok || text != "first" || s.Len() != 0 {
		t.Fatalf("pop = %q %v len=%d", text, ok, s.Len())
	}
	// empty session
	if _, ok := s.PopLastUserTurn(); ok {
		t.Fatal("pop on empty should fail")
	}
}
