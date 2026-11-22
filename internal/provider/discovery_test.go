package provider

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func ollamaMock(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/tags" {
			_ = json.NewEncoder(w).Encode(map[string]any{"models": []map[string]any{{"name": "m1"}}})
			return
		}
		if r.URL.Path == "/v1/models" {
			_ = json.NewEncoder(w).Encode(map[string]any{"data": []map[string]any{{"id": "m1"}}})
			return
		}
		http.NotFound(w, r)
	}))
}

func TestDiscoverPreferredAlive(t *testing.T) {
	srv := ollamaMock(t)
	c, err := Discover(srv.URL, "auto", 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if c.URL != srv.URL || c.Label != "configured" {
		t.Errorf("preferred candidate not used: %+v", c)
	}
}

func TestDiscoverInSkipsDeadAndFindsLive(t *testing.T) {
	srv := ollamaMock(t)
	cands := []Candidate{
		{URL: "http://127.0.0.1:1", Style: "ollama", Label: "dead"},
		{URL: srv.URL, Style: "ollama", Label: "Mock Ollama"},
		{URL: "http://127.0.0.1:2", Style: "openai", Label: "also dead"},
	}
	c, err := DiscoverIn(cands, "http://127.0.0.1:9", "auto", 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if c.URL != srv.URL || c.Label != "Mock Ollama" {
		t.Errorf("fallback picked wrong candidate: %+v", c)
	}
}

func TestDiscoverInFailsWithProbeList(t *testing.T) {
	dead := []Candidate{
		{URL: "http://127.0.0.1:1", Style: "ollama", Label: "dead"},
		{URL: "http://127.0.0.1:2", Style: "openai", Label: "dead too"},
	}
	_, err := DiscoverIn(dead, "http://127.0.0.1:3", "auto", 3*time.Second)
	if err == nil {
		t.Fatal("expected failure with all candidates dead")
	}
	for _, want := range []string{"127.0.0.1:1", "127.0.0.1:2"} {
		if !contains(err.Error(), want) {
			t.Errorf("error should list probed candidate %s: %v", want, err)
		}
	}
}

func TestEnsureBackendUsesConfigWhenAlive(t *testing.T) {
	srv := ollamaMock(t)
	client, found, err := EnsureBackend(srv.URL, "auto", true, 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if found.URL != "" {
		t.Errorf("config URL alive but discovery reported %+v", found)
	}
	models, err := client.Tags(context.Background())
	if err != nil || len(models) != 1 {
		t.Fatalf("tags via EnsureBackend client: %v %v", models, err)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
