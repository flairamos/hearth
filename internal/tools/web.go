package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

const (
	webFetchTimeout = 20 * time.Second
	webMaxBytes     = 300 << 10
	defaultUA       = "Mozilla/5.0 (X11; Linux x86_64) hearth-agent/0.2"
)

// ---- web_fetch ----

func webFetchTool() Tool {
	return Tool{
		Name:     "web_fetch",
		Category: CatRead,
		Desc:     "Fetch a web page via HTTP GET and return its readable text content (HTML converted to plain text). Use after web_search to read a specific page.",
		Schema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"url":       map[string]any{"type": "string", "description": "Full URL including scheme, e.g. \"https://example.com/docs\""},
				"max_chars": map[string]any{"type": "integer", "description": "Max characters to return (default 6000)"},
			},
			"required": []string{"url"},
		},
		Fn: func(ctx context.Context, r *Registry, args map[string]any) (string, error) {
			raw := strings.TrimSpace(argStr(args, "url"))
			u, err := url.Parse(raw)
			if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
				return "", fmt.Errorf("invalid or unsupported URL %q (http/https only)", raw)
			}
			maxChars := 6000
			if n, ok := argInt(args, "max_chars"); ok && n > 0 && n <= 50000 {
				maxChars = n
			}
			cctx, cancel := context.WithTimeout(ctx, webFetchTimeout)
			defer cancel()
			req, err := http.NewRequestWithContext(cctx, http.MethodGet, raw, nil)
			if err != nil {
				return "", err
			}
			req.Header.Set("User-Agent", defaultUA)
			req.Header.Set("Accept", "text/html,application/json;q=0.9,*/*;q=0.8")
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				return "", fmt.Errorf("fetch failed: %w", err)
			}
			defer resp.Body.Close()
			if resp.StatusCode >= 400 {
				return "", fmt.Errorf("HTTP %d from %s", resp.StatusCode, u.Host)
			}
			body, err := io.ReadAll(io.LimitReader(resp.Body, webMaxBytes))
			if err != nil {
				return "", err
			}
			ct := resp.Header.Get("Content-Type")
			var text string
			if strings.Contains(ct, "json") {
				// pretty-print small json for readability
				var v any
				if json.Unmarshal(body, &v) == nil {
					b, _ := json.MarshalIndent(v, "", "  ")
					text = string(b)
				} else {
					text = string(body)
				}
			} else if strings.Contains(ct, "html") || strings.Contains(strings.ToLower(string(body[:min(len(body), 200)])), "<html") {
				text = htmlToText(string(body))
			} else {
				text = string(body)
			}
			text = strings.TrimSpace(text)
			if len([]rune(text)) > maxChars {
				text = string([]rune(text)[:maxChars]) + "\n…[truncated — increase max_chars or read a more specific page]"
			}
			if text == "" {
				return "(empty page)", nil
			}
			return fmt.Sprintf("URL: %s\n%s", raw, text), nil
		},
	}
}

// RE2 has no backreferences — one regex per removable block tag.
var reRemoveBlocks = []*regexp.Regexp{
	regexp.MustCompile(`(?is)<script[^>]*>.*?</script>`),
	regexp.MustCompile(`(?is)<style[^>]*>.*?</style>`),
	regexp.MustCompile(`(?is)<noscript[^>]*>.*?</noscript>`),
	regexp.MustCompile(`(?is)<svg[^>]*>.*?</svg>`),
	regexp.MustCompile(`(?is)<head[^>]*>.*?</head>`),
	regexp.MustCompile(`(?is)<script[^>]*/>`),
}

var (
	reComments = regexp.MustCompile(`(?s)<!--.*?-->`)
	reTag      = regexp.MustCompile(`(?s)<[^>]+>`)
	reBlocks   = regexp.MustCompile(`(?i)</?(p|div|br|li|tr|h[1-6]|section|article|blockquote|pre)[^>]*>`)
	entities   = []struct{ from, to string }{
		{"&amp;", "&"}, {"&lt;", "<"}, {"&gt;", ">"}, {"&quot;", "\""},
		{"&#39;", "'"}, {"&apos;", "'"}, {"&nbsp;", " "}, {"&mdash;", "—"},
		{"&ndash;", "–"}, {"&hellip;", "…"}, {"&copy;", "©"}, {"&reg;", "®"},
		{"&ldquo;", "“"}, {"&rdquo;", "”"},
	}
	reTitle     = regexp.MustCompile(`(?is)<title[^>]*>(.*?)</title>`)
	reSpaces    = regexp.MustCompile(`[ \t]{2,}`)
	reBlank     = regexp.MustCompile(`\n{3,}`)
	reDecEntity = regexp.MustCompile(`&#(\d+);`)
)

// htmlToText converts HTML into readable plain text (best effort).
func htmlToText(s string) string {
	if t := reTitle.FindStringSubmatch(s); t != nil {
		s += "\n\nPAGE TITLE: " + strings.TrimSpace(stripEntities(t[1]))
	}
	for _, re := range reRemoveBlocks {
		s = re.ReplaceAllString(s, " ")
	}
	s = reComments.ReplaceAllString(s, " ")
	s = reBlocks.ReplaceAllString(s, "\n")
	s = reTag.ReplaceAllString(s, " ")
	s = stripEntities(s)
	s = reSpaces.ReplaceAllString(s, " ")
	var out []string
	for _, ln := range strings.Split(s, "\n") {
		ln = strings.TrimSpace(ln)
		if ln != "" {
			out = append(out, ln)
		}
	}
	text := strings.Join(out, "\n")
	return reBlank.ReplaceAllString(text, "\n\n")
}

func stripEntities(s string) string {
	for _, e := range entities {
		s = strings.ReplaceAll(s, e.from, e.to)
	}
	s = reDecEntity.ReplaceAllStringFunc(s, func(m string) string {
		var n int
		fmt.Sscanf(m, "&#%d;", &n)
		if n > 0 && n < 0x10FFFF {
			return string(rune(n))
		}
		return m
	})
	return s
}

// ---- web_search ----

type searchResult struct {
	Title   string
	URL     string
	Snippet string
}

func webSearchTool() Tool {
	return Tool{
		Name:     "web_search",
		Category: CatRead,
		Desc:     "Search the web and return top results (title, URL, snippet). Follow up with web_fetch on a result URL to read the page.",
		Schema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"query": map[string]any{"type": "string", "description": "Search query"},
			},
			"required": []string{"query"},
		},
		Fn: func(ctx context.Context, r *Registry, args map[string]any) (string, error) {
			query := strings.TrimSpace(argStr(args, "query"))
			if query == "" {
				return "", fmt.Errorf("query is required")
			}
			cfg := r.Search
			if cfg == nil || cfg.Engine == "" {
				cfg = &SearchBackend{Engine: "duckduckgo", MaxResults: 5}
			}
			limit := cfg.MaxResults
			if limit <= 0 {
				limit = 5
			}
			var res []searchResult
			var err error
			switch cfg.Engine {
			case "none":
				return "", fmt.Errorf("web_search is disabled in config (search.engine=none)")
			case "searxng":
				res, err = searchSearxng(ctx, cfg.SearxngURL, query, limit)
			case "brave":
				res, err = searchBrave(ctx, cfg.BraveKey, query, limit)
			case "bing":
				res, err = searchBing(ctx, query, limit)
			default:
				res, err = searchDuckDuckGo(ctx, query, limit)
			}
			if err != nil {
				return "", err
			}
			if len(res) == 0 {
				return "no results found", nil
			}
			var b strings.Builder
			for i, sr := range res {
				fmt.Fprintf(&b, "%d. %s\n   %s\n", i+1, sr.Title, sr.URL)
				if sr.Snippet != "" {
					fmt.Fprintf(&b, "   %s\n", sr.Snippet)
				}
			}
			return strings.TrimRight(b.String(), "\n"), nil
		},
	}
}

var httpClient = &http.Client{Timeout: 15 * time.Second}

func searchSearxng(ctx context.Context, base, query string, limit int) ([]searchResult, error) {
	if base == "" {
		return nil, fmt.Errorf("search.searxng_url is not configured")
	}
	endpoint := strings.TrimRight(base, "/") + "/search?q=" + url.QueryEscape(query) + "&format=json"
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	req.Header.Set("User-Agent", defaultUA)
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var out struct {
		Results []struct {
			Title   string `json:"title"`
			URL     string `json:"url"`
			Content string `json:"content"`
		} `json:"results"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("searxng returned non-JSON (enable format=json): %w", err)
	}
	var res []searchResult
	for _, r := range out.Results {
		if len(res) >= limit {
			break
		}
		res = append(res, searchResult{Title: r.Title, URL: r.URL, Snippet: truncateRunesT(r.Content, 160)})
	}
	return res, nil
}

func searchBrave(ctx context.Context, key, query string, limit int) ([]searchResult, error) {
	if key == "" {
		return nil, fmt.Errorf("search.brave_key is not configured")
	}
	endpoint := "https://api.search.brave.com/res/v1/web/search?q=" + url.QueryEscape(query) + "&count=" + fmt.Sprint(limit)
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	req.Header.Set("X-Subscription-Token", key)
	req.Header.Set("Accept", "application/json")
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var out struct {
		Web struct {
			Results []struct {
				Title       string `json:"title"`
				URL         string `json:"url"`
				Description string `json:"description"`
			} `json:"results"`
		} `json:"web"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	var res []searchResult
	for _, r := range out.Web.Results {
		if len(res) >= limit {
			break
		}
		res = append(res, searchResult{Title: r.Title, URL: r.URL, Snippet: truncateRunesT(r.Description, 160)})
	}
	return res, nil
}

var (
	reDDGResult  = regexp.MustCompile(`(?s)<a[^>]+class="[^"]*result__a[^"]*"[^>]+href="([^"]+)"[^>]*>(.*?)</a>`)
	reDDGSnippet = regexp.MustCompile(`(?s)<a[^>]+class="[^"]*result__snippet[^"]*"[^>]*>(.*?)</a>`)
	reTags       = regexp.MustCompile(`(?s)<[^>]+>`)

	reBingAlgo  = regexp.MustCompile(`(?s)<li class="b_algo".*?</li>`)
	reBingLink  = regexp.MustCompile(`(?s)<h2[^>]*><a[^>]+href="([^"]+)"[^>]*>(.*?)</a>`)
	reBingSnip  = regexp.MustCompile(`(?s)<p class="[^"]*b_lineclamp[^"]*"[^>]*>(.*?)</p>`)
	reBingSnip2 = regexp.MustCompile(`(?s)<p[^>]*>(.*?)</p>`)
)

// searchBing scrapes Bing's HTML results (reachable from global + CN
// networks without an API key).
func searchBing(ctx context.Context, query string, limit int) ([]searchResult, error) {
	sctx, cancel := context.WithTimeout(ctx, 12*time.Second)
	defer cancel()
	endpoint := "https://www.bing.com/search?q=" + url.QueryEscape(query) + "&count=" + fmt.Sprint(limit+2)
	req, _ := http.NewRequestWithContext(sctx, http.MethodGet, endpoint, nil)
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0 Safari/537.36")
	req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9,en;q=0.8")
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 512<<10))
	if err != nil {
		return nil, err
	}
	html := string(body)
	var res []searchResult
	for _, block := range reBingAlgo.FindAllString(html, limit+2) {
		if len(res) >= limit {
			break
		}
		m := reBingLink.FindStringSubmatch(block)
		if m == nil {
			continue
		}
		snip := ""
		if sm := reBingSnip.FindStringSubmatch(block); sm != nil {
			snip = sm[1]
		} else if sm := reBingSnip2.FindStringSubmatch(block); sm != nil {
			snip = sm[1]
		}
		title := strings.TrimSpace(stripEntities(reTags.ReplaceAllString(m[2], "")))
		res = append(res, searchResult{
			Title:   title,
			URL:     m[1],
			Snippet: truncateRunesT(strings.TrimSpace(stripEntities(reTags.ReplaceAllString(snip, ""))), 180),
		})
	}
	if len(res) == 0 {
		return nil, fmt.Errorf("bing returned no parseable results (page may require JS)")
	}
	return res, nil
}

func searchDuckDuckGo(ctx context.Context, query string, limit int) ([]searchResult, error) {
	endpoint := "https://html.duckduckgo.com/html/?q=" + url.QueryEscape(query)
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	req.Header.Set("User-Agent", defaultUA)
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 512<<10))
	if err != nil {
		return nil, err
	}
	html := string(body)
	results := []searchResult{}
	snips := reDDGSnippet.FindAllStringSubmatch(html, -1)
	for i, m := range reDDGResult.FindAllStringSubmatch(html, -1) {
		if len(results) >= limit {
			break
		}
		link := decodeDDGLink(m[1])
		snip := ""
		if i < len(snips) {
			snip = strings.TrimSpace(stripEntities(reTags.ReplaceAllString(snips[i][1], "")))
		}
		results = append(results, searchResult{
			Title:   strings.TrimSpace(stripEntities(reTags.ReplaceAllString(m[2], ""))),
			URL:     link,
			Snippet: truncateRunesT(snip, 160),
		})
	}
	if len(results) == 0 && (strings.Contains(html, "anomaly") || strings.Contains(html, "blocked")) {
		return nil, fmt.Errorf("duckduckgo blocked this request; configure search.engine=bing, searxng or brave in config")
	}
	return results, nil
}

// decodeDDGLink unwraps DDG redirect links (/l/?uddg=<urlencoded>).
func decodeDDGLink(href string) string {
	if strings.HasPrefix(href, "//") {
		href = "https:" + href
	}
	if u, err := url.Parse(href); err == nil {
		if uddg := u.Query().Get("uddg"); uddg != "" {
			if dec, err := url.QueryUnescape(uddg); err == nil {
				return dec
			}
		}
		if u.Host != "" && u.Path != "" {
			return u.String()
		}
	}
	return href
}

func truncateRunesT(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
