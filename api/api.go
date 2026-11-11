// Package api tracks which routes AI agents call on your API, and counts every other call.
//
//	t := api.New(api.Options{}) // AGENTSTATS_API_KEY (al_api_…)
//	http.ListenAndServe(":8080", api.Middleware(t)(mux))
//
// A call counts as an agent's when it carries a Web Bot Auth signature, an X-AI-Agent header
// (cli.AgentHeaders, set while a coding agent runs the client), an AI agent's User-Agent, or a
// credential you name in Options.Caller. AgentStats checks signatures and User-Agents again
// before keeping a call. What leaves your server for an agent's call: method, route pattern,
// status, duration, the User-Agent and X-AI-Agent headers, and the caller as an HMAC of its
// credential keyed with your API key. For every other call: a count per day. Never bodies,
// query strings, cookies or credentials. Recording is a timestamp and an append under a mutex;
// a goroutine sends.
package api

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	"agentstats.io/go/internal/ingest"
)

const Version = "0.1.0"

// Options configure a Tracker. Zero values fall back to environment variables and defaults.
type Options struct {
	APIKey string // AGENTSTATS_API_KEY
	// Routes says which requests are API calls. Default: all of them, or with ServesPages only
	// paths under /api and /v1, /v2…
	Routes      func(path string) bool
	ServesPages bool
	// Caller identifies who made a call, so one agent's calls group into a session: an API key id,
	// an OAuth client and user. It's sent as an HMAC, never as is. Return agent to name the agent
	// behind a credential you know. Default: the Authorization, X-API-Key or API-Key header.
	Caller func(r *http.Request) (id, agent string)
	// NoCaller turns grouping by caller off.
	NoCaller bool
	// RouteOf returns the matched route pattern for routers other than net/http's ServeMux (whose
	// r.Pattern is read by default), e.g. chi.RouteContext(r.Context()).RoutePattern().
	RouteOf       func(r *http.Request) string
	FlushInterval time.Duration // default 5s
	MaxBatch      int           // default 100
	MaxBuffer     int           // default 10000
	Debug         bool          // AGENTSTATS_DEBUG=1
	HTTPClient    *http.Client

	// endpoint is set by New (ingest.URL); it isn't an option.
	endpoint string
}

var (
	apiPath   = regexp.MustCompile(`^/(?:api|v\d+)(?:/|$)`)
	aiAgentUA = regexp.MustCompile(`(?i)gptbot|chatgpt|oai-searchbot|openai|claude|anthropic|perplexity|google-agent|googleother|gemini|bytespider|ccbot|cohere|mistralai|meta-external|amazonbot|amzn-user|duckassistbot|youbot|manus|devin|cursor|codex|kimi|deepseek|novaact|browserbase`)
	idSegment = regexp.MustCompile(`(?i)^(\d+|[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}|[0-9a-f]{12,}|[^/@]+@[^/]+|[A-Za-z]+_[A-Za-z0-9]{12,})$`)
	longToken = regexp.MustCompile(`^[\w-]{16,}$`)
	digit     = regexp.MustCompile(`\d`)
	covered   = regexp.MustCompile(`(?i)"([a-z0-9-]+)"`)
	neverSend = map[string]bool{"cookie": true, "authorization": true, "proxy-authorization": true, "x-api-key": true}
)

// RouteFromPath is a path as a route pattern: values (ids, UUIDs, tokens, emails) become :id.
func RouteFromPath(path string) string {
	parts := strings.Split(path, "/")
	for i, s := range parts {
		v, err := url.PathUnescape(s)
		if err != nil {
			v = s
		}
		if s != "" && (idSegment.MatchString(v) || (longToken.MatchString(v) && digit.MatchString(v))) {
			parts[i] = ":id"
		}
	}
	if r := strings.Join(parts, "/"); r != "" {
		return r
	}
	return "/"
}

// Call is one agent's call as sent to AgentStats.
type Call struct {
	Ts         int64             `json:"ts"`
	Method     string            `json:"method"`
	Route      string            `json:"route"`
	Status     int               `json:"status"`
	DurationMs float64           `json:"durationMs"`
	UA         *string           `json:"ua"`
	IP         *string           `json:"ip"`
	URL        *string           `json:"url"`
	Headers    map[string]string `json:"headers"`
	Agent      *string           `json:"agent"`
	Run        *string           `json:"run"`
	Actor      *string           `json:"actor"`
	ActorAgent *string           `json:"actorAgent"`
}

// Tracker buffers agents' calls and per-day counts of the rest.
type Tracker struct {
	opts    Options
	enabled bool
	mu      sync.Mutex
	calls   []Call
	other   map[string]int
	hmacs   map[string]string
	wake    chan struct{}
	once    sync.Once
}

// New returns a Tracker; without an API key it does nothing.
func New(o Options) *Tracker {
	if o.APIKey == "" {
		o.APIKey = os.Getenv("AGENTSTATS_API_KEY")
	}
	o.endpoint = ingest.URL("api")
	if o.FlushInterval <= 0 {
		o.FlushInterval = 5 * time.Second
	}
	if o.MaxBatch <= 0 {
		o.MaxBatch = 100
	}
	if o.MaxBuffer <= 0 {
		o.MaxBuffer = 10_000
	}
	if os.Getenv("AGENTSTATS_DEBUG") == "1" {
		o.Debug = true
	}
	if o.HTTPClient == nil {
		o.HTTPClient = &http.Client{Timeout: 5 * time.Second}
	}
	enabled := strings.HasPrefix(o.APIKey, "al_api_") && os.Getenv("AGENTSTATS_DISABLED") != "1"
	if o.APIKey != "" && !strings.HasPrefix(o.APIKey, "al_api_") {
		fmt.Fprintln(os.Stderr, "[agentstats] APIKey must be an API's key (al_api_…, Project settings → APIs); API tracking is off")
	}
	return &Tracker{opts: o, enabled: enabled, other: map[string]int{}, hmacs: map[string]string{}, wake: make(chan struct{}, 1)}
}

func (t *Tracker) isAPI(path string) bool {
	if t.opts.Routes != nil {
		return t.opts.Routes(path)
	}
	return !t.opts.ServesPages || apiPath.MatchString(path)
}

type recorder struct {
	http.ResponseWriter
	status int
}

func (r *recorder) WriteHeader(code int) {
	if r.status == 0 {
		r.status = code
	}
	r.ResponseWriter.WriteHeader(code)
}

func (r *recorder) Write(b []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	return r.ResponseWriter.Write(b)
}

func (r *recorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

func ptr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// Middleware records each API call the handler serves.
func Middleware(t *Tracker) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		if t == nil || !t.enabled {
			return next
		}
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodOptions || !t.isAPI(r.URL.Path) {
				next.ServeHTTP(w, r)
				return
			}
			h := r.Header
			agent := strings.TrimSpace(h.Get("X-AI-Agent"))
			ua := h.Get("User-Agent")
			signed := h.Get("Signature-Agent") != "" && h.Get("Signature-Input") != ""
			aiUA := ua != "" && aiAgentUA.MatchString(ua)
			var callerID, callerAgent string
			// Only a custom Caller can name an agent, so other calls skip the credential.
			if !t.opts.NoCaller && (t.opts.Caller != nil || signed || aiUA || agent != "") {
				callerID, callerAgent = t.caller(r)
			}
			if !signed && !aiUA && agent == "" && callerAgent == "" {
				t.countOther()
				next.ServeHTTP(w, r)
				return
			}
			started := time.Now()
			rec := &recorder{ResponseWriter: w}
			defer func() {
				status := rec.status
				if status == 0 {
					status = http.StatusOK
				}
				if p := recover(); p != nil {
					t.finish(r, started, 500, signed, aiUA, agent, ua, callerID, callerAgent)
					panic(p)
				}
				t.finish(r, started, status, signed, aiUA, agent, ua, callerID, callerAgent)
			}()
			next.ServeHTTP(rec, r)
		})
	}
}

func (t *Tracker) caller(r *http.Request) (id, agent string) {
	defer func() {
		if recover() != nil {
			id, agent = "", ""
		}
	}()
	if t.opts.Caller != nil {
		return t.opts.Caller(r)
	}
	for _, k := range []string{"Authorization", "X-API-Key", "API-Key"} {
		if v := strings.TrimSpace(r.Header.Get(k)); v != "" {
			return v, ""
		}
	}
	return "", ""
}

func (t *Tracker) finish(r *http.Request, started time.Time, status int, signed, aiUA bool, agent, ua, callerID, callerAgent string) {
	route := RouteFromPath(r.URL.Path)
	pattern := r.Pattern
	if t.opts.RouteOf != nil {
		pattern = t.opts.RouteOf(r)
	}
	if p := fitRoute(pattern, route); p != "" {
		route = p
	}
	c := Call{
		Ts:         started.UnixMilli(),
		Method:     r.Method,
		Route:      route,
		Status:     status,
		DurationMs: float64(time.Since(started).Microseconds()) / 1000,
		UA:         ptr(ua),
		Headers:    map[string]string{},
		Agent:      ptr(truncate(agent, 120)),
		Run:        ptr(truncate(strings.TrimSpace(r.Header.Get("X-AI-Agent-Run")), 200)),
		ActorAgent: ptr(truncate(callerAgent, 120)),
	}
	if signed || aiUA {
		if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
			c.IP = &host
		}
	}
	if signed {
		scheme := "http"
		if r.TLS != nil {
			scheme = "https"
		}
		u := scheme + "://" + r.Host + r.URL.EscapedPath()
		c.URL = &u
		names := []string{"signature", "signature-input", "signature-agent"}
		for _, m := range covered.FindAllStringSubmatch(r.Header.Get("Signature-Input"), 13) {
			names = append(names, strings.ToLower(m[1]))
		}
		for _, n := range names {
			if strings.HasPrefix(n, "@") || neverSend[n] {
				continue
			}
			if v := r.Header.Get(n); v != "" {
				c.Headers[n] = v
			}
		}
	}
	if callerID != "" {
		a := t.hmac(callerID)
		c.Actor = &a
	}
	t.mu.Lock()
	if len(t.calls) >= t.opts.MaxBuffer {
		t.calls = t.calls[1:]
	}
	t.calls = append(t.calls, c)
	full := len(t.calls) >= t.opts.MaxBatch
	t.mu.Unlock()
	t.start()
	if full {
		select {
		case t.wake <- struct{}{}:
		default:
		}
	}
}

// fitRoute is the framework's pattern ("GET /users/{id}" for ServeMux) when it fits the path.
func fitRoute(pattern, route string) string {
	if i := strings.IndexByte(pattern, ' '); i >= 0 {
		pattern = pattern[i+1:]
	}
	if i := strings.IndexByte(pattern, '/'); i > 0 { // host-qualified pattern
		pattern = pattern[i:]
	}
	if !strings.HasPrefix(pattern, "/") {
		return ""
	}
	p := strings.TrimSuffix(pattern, "/")
	if p == "" {
		p = "/"
	}
	count := func(s string) int { return len(strings.FieldsFunc(s, func(r rune) bool { return r == '/' })) }
	if count(p) == count(route) || strings.Contains(p, "...}") || strings.HasSuffix(p, "*") {
		return p
	}
	return ""
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

func (t *Tracker) hmac(credential string) string {
	t.mu.Lock()
	defer t.mu.Unlock()
	if h, ok := t.hmacs[credential]; ok {
		return h
	}
	m := hmac.New(sha256.New, []byte(t.opts.APIKey))
	m.Write([]byte(credential))
	h := hex.EncodeToString(m.Sum(nil))[:32]
	if len(t.hmacs) >= 1000 {
		for k := range t.hmacs {
			delete(t.hmacs, k)
			break
		}
	}
	t.hmacs[credential] = h
	return h
}

func (t *Tracker) countOther() {
	day := time.Now().UTC().Format("2006-01-02")
	t.mu.Lock()
	t.other[day]++
	t.mu.Unlock()
	t.start()
}

func (t *Tracker) start() {
	t.once.Do(func() {
		go func() {
			tick := time.NewTicker(t.opts.FlushInterval)
			defer tick.Stop()
			for {
				select {
				case <-tick.C:
				case <-t.wake:
				}
				_ = t.Flush(context.Background())
			}
		}()
	})
}

// Flush sends what's buffered now. Failed batches are dropped, never retried inline.
func (t *Tracker) Flush(ctx context.Context) error {
	if t == nil || !t.enabled {
		return nil
	}
	t.mu.Lock()
	calls, other := t.calls, t.other
	t.calls, t.other = nil, map[string]int{}
	t.mu.Unlock()
	if len(calls) == 0 && len(other) == 0 {
		return nil
	}
	if calls == nil {
		calls = []Call{}
	}
	body, _ := json.Marshal(map[string]any{"calls": calls, "other": other})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.opts.endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+t.opts.APIKey)
	req.Header.Set("X-AgentStats-SDK", "go/"+Version)
	res, err := t.opts.HTTPClient.Do(req)
	if err != nil {
		if t.opts.Debug {
			fmt.Fprintln(os.Stderr, "[agentstats] send failed:", err)
		}
		return err
	}
	res.Body.Close()
	return nil
}
