package ollama

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Client talks to an Ollama server.
type Client struct {
	BaseURL string
	HTTP    *http.Client
}

// New creates a client for the given base URL (e.g. http://localhost:11434).
func New(baseURL string) *Client {
	return &Client{
		BaseURL: strings.TrimRight(baseURL, "/"),
		HTTP:    &http.Client{Timeout: 0}, // no global timeout; streams are long-lived
	}
}

// ModelError classifies server-side / API errors so callers can react
// (missing model, context overflow, ...).
type ModelError struct {
	Status int
	Body   string
}

func (e *ModelError) Error() string {
	if e.Status == http.StatusNotFound {
		return fmt.Sprintf("model not found: %s", e.Body)
	}
	return fmt.Sprintf("ollama api error (HTTP %d): %s", e.Status, e.Body)
}

// IsNotFound reports whether err is a 404 (model missing) error.
func IsNotFound(err error) bool {
	var me *ModelError
	return errors.As(err, &me) && me.Status == http.StatusNotFound
}

// IsContextOverflow reports whether err indicates the prompt exceeded the
// context window. Ollama returns 400/500 with messages mentioning context.
func IsContextOverflow(err error) bool {
	if err == nil {
		return false
	}
	var me *ModelError
	if errors.As(err, &me) {
		b := strings.ToLower(me.Body)
		return strings.Contains(b, "context") && (strings.Contains(b, "exceed") || strings.Contains(b, "length") || strings.Contains(b, "too large") || strings.Contains(b, "unable to pack"))
	}
	return false
}

// IsConnError reports whether err is a connection-level failure
// (server down, refused, DNS...) as opposed to an API error.
func IsConnError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	var ne net.Error
	if errors.As(err, &ne) {
		return true
	}
	var oe *net.OpError
	if errors.As(err, &oe) {
		return true
	}
	var uerr *url.Error
	if errors.As(err, &uerr) {
		return true // dial / read errors wrapped in url.Error
	}
	return strings.Contains(err.Error(), "connection refused")
}

const retryBudget = 3 // PRD: exponential backoff, max 3 attempts

// ChatStream calls /api/chat with stream=true and invokes fn for each NDJSON
// chunk. fn may return an error to abort the stream. Transient connection
// failures before the first chunk are retried with exponential backoff.
func (c *Client) ChatStream(ctx context.Context, req ChatRequest, fn func(ChatResponse) error) error {
	req.Stream = true
	var lastErr error
	for attempt := 0; attempt < retryBudget; attempt++ {
		if attempt > 0 {
			// only retry if nothing was streamed yet
			time.Sleep(time.Duration(500*(1<<attempt)) * time.Millisecond)
		}
		got := false
		err := c.chatStreamOnce(ctx, req, func(r ChatResponse) error {
			got = true
			return fn(r)
		})
		if err == nil {
			return nil
		}
		if got || !IsConnError(err) || ctx.Err() != nil {
			return err
		}
		lastErr = err
	}
	return lastErr
}

func (c *Client) chatStreamOnce(ctx context.Context, req ChatRequest, fn func(ChatResponse) error) error {
	body, err := json.Marshal(req)
	if err != nil {
		return err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/api/chat", bytes.NewReader(body))
	if err != nil {
		return err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	resp, err := c.HTTP.Do(httpReq)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
		return &ModelError{Status: resp.StatusCode, Body: strings.TrimSpace(string(b))}
	}
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64*1024), 8<<20) // thinking chunks can be long
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var cr ChatResponse
		if err := json.Unmarshal(line, &cr); err != nil {
			return fmt.Errorf("bad stream chunk: %w", err)
		}
		if cr.Error != "" {
			return &ModelError{Status: 500, Body: cr.Error}
		}
		if err := fn(cr); err != nil {
			return err
		}
	}
	return sc.Err()
}

// Chat performs a non-streaming /api/chat call and returns the final response.
func (c *Client) Chat(ctx context.Context, req ChatRequest) (ChatResponse, error) {
	req.Stream = false
	body, err := json.Marshal(req)
	if err != nil {
		return ChatResponse{}, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/api/chat", bytes.NewReader(body))
	if err != nil {
		return ChatResponse{}, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	resp, err := c.HTTP.Do(httpReq)
	if err != nil {
		return ChatResponse{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
		return ChatResponse{}, &ModelError{Status: resp.StatusCode, Body: strings.TrimSpace(string(b))}
	}
	var cr ChatResponse
	if err := json.NewDecoder(resp.Body).Decode(&cr); err != nil {
		return ChatResponse{}, err
	}
	if cr.Error != "" {
		return ChatResponse{}, &ModelError{Status: 500, Body: cr.Error}
	}
	return cr, nil
}

// Tags lists locally installed models.
func (c *Client) Tags(ctx context.Context) ([]ModelInfo, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+"/api/tags", nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
		return nil, &ModelError{Status: resp.StatusCode, Body: strings.TrimSpace(string(b))}
	}
	var out struct {
		Models []ModelInfo `json:"models"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	return out.Models, nil
}

// Show returns details for one model.
func (c *Client) Show(ctx context.Context, name string) (ShowResponse, error) {
	payload, _ := json.Marshal(map[string]string{"model": name})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/api/show", bytes.NewReader(payload))
	if err != nil {
		return ShowResponse{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return ShowResponse{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
		return ShowResponse{}, &ModelError{Status: resp.StatusCode, Body: strings.TrimSpace(string(b))}
	}
	var out ShowResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return ShowResponse{}, err
	}
	return out, nil
}

// Ping checks that the server answers /api/tags.
func (c *Client) Ping(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()
	_, err := c.Tags(ctx)
	return err
}

// SuggestModels returns the closest matches for a model name from candidates,
// used for "did you mean" hints on 404.
func SuggestModels(want string, candidates []string) []string {
	want = strings.ToLower(want)
	type scored struct {
		name  string
		score int
	}
	var out []scored
	for _, c := range candidates {
		lc := strings.ToLower(c)
		s := 0
		if strings.HasPrefix(lc, want) || strings.HasPrefix(want, lc) {
			s += 50
		}
		if strings.Contains(lc, want) || strings.Contains(want, strings.Split(lc, ":")[0]) {
			s += 25
		}
		// common prefix length
		i := 0
		for i < len(lc) && i < len(want) && lc[i] == want[i] {
			i++
		}
		s += i
		if s > 8 {
			out = append(out, scored{c, s})
		}
	}
	// sort desc
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j].score > out[j-1].score; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	res := make([]string, 0, len(out))
	for _, s := range out {
		res = append(res, s.name)
	}
	return res
}
