// Package agentstats is the core of the AgentStats MCP analytics SDK for Go.
//
// Use an adapter for your MCP library:
//
//	agentstats.io/go/gosdk  (github.com/modelcontextprotocol/go-sdk)
//	agentstats.io/go/mcpgo  (github.com/mark3labs/mcp-go)
//
// Recording an event is a timestamp and an append under a short mutex. A background
// goroutine ships batches; on failure events are dropped, never retried inline.
// Tool arguments and results are never sent. See https://github.com/getagentstats/spec/blob/main/PROTOCOL.md.
package agentstats

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math/rand/v2"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const Version = "0.1.0"
const defaultEndpoint = "https://agentstats.io/api/ingest/mcp"

// Options configure a Tracker. Zero values fall back to environment variables and defaults.
type Options struct {
	APIKey     string // AGENTSTATS_MCP_KEY
	ServerName string // defaults to the serverInfo name the server reports
	Endpoint   string // AGENTSTATS_ENDPOINT
	// SampleRate is the fraction of tools/call requests recorded. 0 means 1.
	SampleRate float64
	// DisableErrorText stops error messages being sent.
	DisableErrorText bool
	// ErrorText decides what is sent of each failed request's message: return the text to
	// keep, or "" for none. It runs as the response is sent, so keep it cheap; a panic keeps nothing.
	ErrorText func(ErrorInfo) string
	// DisableCatalog stops the tool list being sent.
	DisableCatalog bool
	FlushInterval  time.Duration // default 2s
	MaxBatch       int           // default 250
	MaxBuffer      int           // default 10000
	Debug          bool          // AGENTSTATS_DEBUG=1
	HTTPClient     *http.Client
	// AuthInfo returns the OAuth client id and granted scopes of the request's validated access
	// token, for transports that put them in the context (mcp-go's WithHTTPContextFunc, your own
	// middleware). The gosdk adapter reads the go-sdk's TokenInfo without it. Only the client app
	// and scopes are sent (see AuthSummary), never the token or the user.
	AuthInfo func(ctx context.Context) (clientID string, scopes []string, ok bool)
}

// Auth is what's kept of a validated OAuth access token (https://github.com/getagentstats/spec/blob/main/PROTOCOL.md "Sign-in").
type Auth struct {
	Method string   `json:"method"`
	Client *string  `json:"client"`
	Scopes []string `json:"scopes"`
}

// AuthSummary keeps the client app, by its Client ID Metadata Document URL (https, with a path,
// no credentials or query), else "registered" (Dynamic Client Registration ids are random per
// installation), and the granted scopes: de-duplicated, sorted, at most 20 of at most 80 chars.
func AuthSummary(clientID string, scopes []string) *Auth {
	a := &Auth{Method: "oauth", Scopes: []string{}}
	if clientID != "" {
		client := "registered"
		if u, err := url.Parse(clientID); err == nil && u.Scheme == "https" && u.Host != "" && u.User == nil && u.RawQuery == "" && len(u.Path) > 1 {
			client = u.Host + u.Path
			if len(client) > 200 {
				client = client[:200]
			}
		}
		a.Client = &client
	}
	seen := map[string]bool{}
	for _, s := range scopes {
		if s != "" && len(s) <= 80 && !seen[s] {
			seen[s] = true
			a.Scopes = append(a.Scopes, s)
		}
	}
	sort.Strings(a.Scopes)
	if len(a.Scopes) > 20 {
		a.Scopes = a.Scopes[:20]
	}
	return a
}

// AuthFrom applies Options.AuthInfo to a request context. A panic keeps nothing.
func (t *Tracker) AuthFrom(ctx context.Context) (a *Auth) {
	if t == nil || t.opts.AuthInfo == nil || ctx == nil {
		return nil
	}
	defer func() {
		if recover() != nil {
			a = nil
		}
	}()
	if id, scopes, ok := t.opts.AuthInfo(ctx); ok {
		return AuthSummary(id, scopes)
	}
	return nil
}

// Event is one observed MCP request/response pair.
type Event struct {
	Ts            int64   `json:"ts"`
	Method        string  `json:"method"`
	Tool          *string `json:"tool"`
	DurationMs    float64 `json:"durationMs"`
	IsError       bool    `json:"isError"`
	ErrorMessage  *string `json:"errorMessage"`
	ClientName    *string `json:"clientName"`
	ClientVersion *string `json:"clientVersion"`
	SessionID     *string `json:"sessionId"`
	Auth          *Auth   `json:"auth,omitempty"`
}

// CatalogTool is the subset of a tool definition sent for lint and never-called reports.
type CatalogTool struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	InputSchema any    `json:"inputSchema,omitempty"`
}

type catalog struct {
	Tools        []CatalogTool  `json:"tools"`
	ServerInfo   map[string]any `json:"serverInfo,omitempty"`
	Instructions string         `json:"instructions,omitempty"`
}

// Tracked lists the methods worth recording.
var Tracked = map[string]bool{
	"tools/call": true, "tools/list": true, "initialize": true, "server/discover": true,
	"resources/read": true, "resources/list": true, "prompts/get": true, "prompts/list": true,
}

type clientInfo struct{ name, version string }

// Tracker buffers events and ships them in the background. Safe for concurrent use.
type Tracker struct {
	opts    Options
	enabled bool
	client  *http.Client

	mu           sync.Mutex
	buf          []Event
	serverName   string
	catalog      *catalog
	catalogSig   string
	catalogDirty bool
	sessions     map[string]clientInfo
	auths        map[string]*Auth
	dropped      int64

	inflight atomic.Int32
	wake     chan struct{}
	stop     chan struct{}
	once     sync.Once
}

// New creates a Tracker. Without an API key it returns a disabled tracker whose methods are no-ops.
func New(opts Options) *Tracker {
	if opts.APIKey == "" {
		opts.APIKey = os.Getenv("AGENTSTATS_MCP_KEY")
	}
	if opts.Endpoint == "" {
		opts.Endpoint = os.Getenv("AGENTSTATS_ENDPOINT")
	}
	if opts.Endpoint == "" {
		opts.Endpoint = defaultEndpoint
	}
	if opts.SampleRate <= 0 || opts.SampleRate > 1 {
		opts.SampleRate = 1
	}
	if opts.FlushInterval <= 0 {
		opts.FlushInterval = 2 * time.Second
	}
	if opts.MaxBatch <= 0 {
		opts.MaxBatch = 250
	}
	if opts.MaxBuffer <= 0 {
		opts.MaxBuffer = 10_000
	}
	if os.Getenv("AGENTSTATS_DEBUG") == "1" {
		opts.Debug = true
	}
	t := &Tracker{
		opts:       opts,
		enabled:    opts.APIKey != "" && os.Getenv("AGENTSTATS_DISABLED") != "1",
		client:     opts.HTTPClient,
		serverName: opts.ServerName,
		sessions:   map[string]clientInfo{},
		auths:      map[string]*Auth{},
		wake:       make(chan struct{}, 1),
		stop:       make(chan struct{}),
	}
	if t.client == nil {
		t.client = &http.Client{Timeout: 5 * time.Second}
	}
	if !t.enabled {
		t.debugf("no API key, analytics disabled")
	}
	return t
}

// Enabled reports whether events are being recorded.
func (t *Tracker) Enabled() bool { return t != nil && t.enabled }

// Sample reports whether a tools/call should be recorded under the sample rate.
func (t *Tracker) Sample() bool {
	return t.opts.SampleRate >= 1 || rand.Float64() < t.opts.SampleRate
}

func (t *Tracker) debugf(format string, args ...any) {
	if t.opts.Debug {
		log.Printf("[agentstats] "+format, args...)
	}
}

// SetClient remembers the client behind a session, from its initialize request.
func (t *Tracker) SetClient(sessionID, name, version string) {
	if !t.Enabled() {
		return
	}
	t.mu.Lock()
	if len(t.sessions) > 5000 {
		for k := range t.sessions {
			delete(t.sessions, k)
			break
		}
	}
	t.sessions[sessionID] = clientInfo{name, version}
	t.mu.Unlock()
}

// SetServerInfo records the server's name, version and instructions from its initialize result.
func (t *Tracker) SetServerInfo(name, version, instructions string) {
	if !t.Enabled() {
		return
	}
	if name == "" {
		return
	}
	t.mu.Lock()
	if t.serverName == "" {
		t.serverName = name
	}
	if t.catalog == nil {
		t.catalog = &catalog{}
	}
	t.catalog.ServerInfo = map[string]any{"name": name, "version": version}
	if instructions != "" {
		t.catalog.Instructions = instructions
	}
	t.mu.Unlock()
}

// SetInstructions records the server's instructions.
func (t *Tracker) SetInstructions(instructions string) {
	if !t.Enabled() {
		return
	}
	t.mu.Lock()
	if t.catalog == nil {
		t.catalog = &catalog{}
	}
	t.catalog.Instructions = instructions
	t.mu.Unlock()
}

// SetTools records a complete tools/list result; it is sent once whenever it changes.
func (t *Tracker) SetTools(build func() []CatalogTool, sig string) {
	if !t.Enabled() || t.opts.DisableCatalog {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if sig == t.catalogSig {
		return
	}
	t.catalogSig = sig
	if t.catalog == nil {
		t.catalog = &catalog{}
	}
	t.catalog.Tools = build()
	t.catalogDirty = true
}

// ToolsSignature is the cheap change-detection key for a tool list.
func ToolsSignature(n int, each func(i int) (name string, descLen int)) string {
	var b strings.Builder
	fmt.Fprint(&b, n)
	for i := 0; i < n; i++ {
		name, l := each(i)
		fmt.Fprintf(&b, "|%s:%d", name, l)
	}
	return b.String()
}

// ErrorInfo is a failed request, as passed to Options.ErrorText.
type ErrorInfo struct {
	Tool    string // empty unless tools/call
	Method  string
	Message string // the error, or the first text content of an isError result
	Err     error  // the Go error the handler returned, or nil for isError results
}

// Call describes one observed request/response pair.
type Call struct {
	Method, Tool, SessionID string
	Started                 time.Time
	IsError                 bool
	ErrorMessage            string
	Err                     error // the error the handler returned, if any
	// Client identity, when the adapter knows it. Otherwise it is looked up by session.
	ClientName, ClientVersion string
	// Auth is the request's sign-in summary (AuthSummary), if it carried a validated token.
	Auth *Auth
}

// errorText applies Options.ErrorText. A panicking func keeps nothing.
func (t *Tracker) errorText(c Call) (msg string) {
	if t.opts.ErrorText == nil {
		return c.ErrorMessage
	}
	defer func() {
		if r := recover(); r != nil {
			t.debugf("ErrorText panicked: %v", r)
			msg = ""
		}
	}()
	return t.opts.ErrorText(ErrorInfo{Tool: c.Tool, Method: c.Method, Message: c.ErrorMessage, Err: c.Err})
}

// Record buffers one event. It never blocks on I/O.
func (t *Tracker) Record(c Call) {
	if !t.Enabled() {
		return
	}
	e := Event{
		Ts:         time.Now().UnixMilli(),
		Method:     c.Method,
		DurationMs: float64(time.Since(c.Started).Microseconds()) / 1000,
		IsError:    c.IsError,
	}
	if c.Tool != "" {
		e.Tool = &c.Tool
	}
	if c.SessionID != "" {
		e.SessionID = &c.SessionID
	}
	if c.IsError && c.ErrorMessage != "" && !t.opts.DisableErrorText {
		if msg := t.errorText(c); msg != "" {
			if len(msg) > 300 {
				msg = msg[:300]
			}
			e.ErrorMessage = &msg
		}
	}
	if c.ClientName != "" {
		e.ClientName, e.ClientVersion = &c.ClientName, &c.ClientVersion
	}
	t.mu.Lock()
	if e.ClientName == nil {
		if ci, ok := t.sessions[c.SessionID]; ok {
			e.ClientName, e.ClientVersion = &ci.name, &ci.version
		}
	}
	// A session's sign-in, from its last request that carried one.
	if c.Auth != nil {
		if len(t.auths) >= 5000 {
			for k := range t.auths {
				delete(t.auths, k)
				break
			}
		}
		t.auths[c.SessionID] = c.Auth
		e.Auth = c.Auth
	} else if a, ok := t.auths[c.SessionID]; ok {
		e.Auth = a
	}
	if len(t.buf) >= t.opts.MaxBuffer {
		t.buf = t.buf[1:]
		t.dropped++
	}
	t.buf = append(t.buf, e)
	full := len(t.buf) >= t.opts.MaxBatch
	t.mu.Unlock()

	t.once.Do(func() { go t.loop() })
	if full {
		select {
		case t.wake <- struct{}{}:
		default:
		}
	}
}

func (t *Tracker) loop() {
	ticker := time.NewTicker(t.opts.FlushInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
		case <-t.wake:
		case <-t.stop:
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_ = t.Flush(ctx)
		cancel()
	}
}

// Flush sends buffered events now. Call it before a serverless handler returns.
func (t *Tracker) Flush(ctx context.Context) error {
	if !t.Enabled() || t.inflight.Load() >= 2 {
		return nil
	}
	t.mu.Lock()
	if len(t.buf) == 0 && !t.catalogDirty {
		t.mu.Unlock()
		return nil
	}
	n := min(len(t.buf), t.opts.MaxBatch)
	events := make([]Event, n)
	copy(events, t.buf[:n])
	t.buf = t.buf[n:]
	var cat *catalog
	if t.catalogDirty {
		c := *t.catalog
		cat = &c
		t.catalogDirty = false
	}
	name := t.serverName
	if name == "" {
		name = "unnamed-mcp-server"
	}
	dropped := t.dropped
	t.dropped = 0
	t.mu.Unlock()

	t.inflight.Add(1)
	defer t.inflight.Add(-1)
	err := t.send(ctx, name, events, cat, dropped)
	if err != nil {
		t.mu.Lock()
		t.dropped += dropped + int64(len(events))
		if cat != nil {
			t.catalogDirty = true
		}
		t.mu.Unlock()
		t.debugf("ingest failed, dropped %d events: %v", len(events), err)
	}
	return err
}

func (t *Tracker) send(ctx context.Context, name string, events []Event, cat *catalog, dropped int64) error {
	payload := map[string]any{"serverName": name, "events": events, "dropped": dropped}
	if cat != nil {
		payload["catalog"] = cat
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.opts.Endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+t.opts.APIKey)
	req.Header.Set("X-AgentStats-SDK", "go/"+Version)
	res, err := t.client.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode >= 300 {
		msg, _ := io.ReadAll(io.LimitReader(res.Body, 300))
		return fmt.Errorf("ingest rejected: HTTP %d %s", res.StatusCode, msg)
	}
	return nil
}

// Shutdown stops the background goroutine and sends what is left.
func (t *Tracker) Shutdown(ctx context.Context) error {
	if !t.Enabled() {
		return nil
	}
	select {
	case <-t.stop:
	default:
		close(t.stop)
	}
	for {
		t.mu.Lock()
		left := len(t.buf) > 0 || t.catalogDirty
		t.mu.Unlock()
		if !left {
			return nil
		}
		if err := t.Flush(ctx); err != nil {
			return err
		}
	}
}

// Dropped returns how many events have been dropped since the last successful send.
func (t *Tracker) Dropped() int64 {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.dropped
}

// LocalSessionID returns a random id for transports without sessions (stdio).
func LocalSessionID() string {
	return fmt.Sprintf("%010x", rand.Uint64()>>24)
}
