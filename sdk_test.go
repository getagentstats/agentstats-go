package agentstats_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"agentstats.io/go"
	"agentstats.io/go/gosdk"
	tsmcpgo "agentstats.io/go/mcpgo"
	mcpgoclient "github.com/mark3labs/mcp-go/client"
	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type capture struct {
	mu     sync.Mutex
	bodies []map[string]any
	delay  time.Duration
	srv    *httptest.Server
}

func newCapture(delay time.Duration) *capture {
	c := &capture{delay: delay}
	c.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		c.mu.Lock()
		c.bodies = append(c.bodies, body)
		c.mu.Unlock()
		time.Sleep(c.delay)
		w.WriteHeader(202)
	}))
	return c
}

func (c *capture) events() (events []map[string]any, catalog map[string]any, name string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, b := range c.bodies {
		name, _ = b["serverName"].(string)
		for _, e := range b["events"].([]any) {
			events = append(events, e.(map[string]any))
		}
		if cat, ok := b["catalog"].(map[string]any); ok {
			catalog = cat
		}
	}
	return
}

func toolCalls(events []map[string]any) (out []map[string]any) {
	for _, e := range events {
		if e["method"] == "tools/call" {
			out = append(out, e)
		}
	}
	return
}

// ---------- official go-sdk ----------

type addIn struct {
	A float64 `json:"a"`
	B float64 `json:"b"`
}

func newGoSDK(opts agentstats.Options) (*mcp.ClientSession, *agentstats.Tracker) {
	ctx := context.Background()
	s := mcp.NewServer(&mcp.Implementation{Name: "go-fixture", Version: "1.2.3"}, nil)
	mcp.AddTool(s, &mcp.Tool{Name: "add", Description: "Adds two numbers. Returns the sum."}, func(ctx context.Context, req *mcp.CallToolRequest, in addIn) (*mcp.CallToolResult, any, error) {
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "ok"}}}, nil, nil
	})
	mcp.AddTool(s, &mcp.Tool{Name: "fail", Description: "Always fails."}, func(ctx context.Context, req *mcp.CallToolRequest, in struct{}) (*mcp.CallToolResult, any, error) {
		r := &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: "boom: upstream 503"}}}
		return r, nil, nil
	})
	t := gosdk.Instrument(s, opts)
	st, ct := mcp.NewInMemoryTransports()
	if _, err := s.Connect(ctx, st, nil); err != nil {
		panic(err)
	}
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "9.9.9"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		panic(err)
	}
	return cs, t
}

func TestGoSDKRecords(t *testing.T) {
	cap := newCapture(0)
	defer cap.srv.Close()
	cs, tr := newGoSDK(agentstats.Options{APIKey: "al_srv_test", Endpoint: cap.srv.URL, FlushInterval: time.Hour})
	ctx := context.Background()
	if _, err := cs.ListTools(ctx, nil); err != nil {
		t.Fatal(err)
	}
	cs.CallTool(ctx, &mcp.CallToolParams{Name: "add", Arguments: map[string]any{"a": 2, "b": 3}})
	cs.CallTool(ctx, &mcp.CallToolParams{Name: "fail", Arguments: map[string]any{}})
	tr.Shutdown(ctx)

	events, catalog, name := cap.events()
	assertRecorded(t, events, catalog, name, "go-fixture")
}

// ---------- mark3labs/mcp-go ----------

func newMcpGo(opts agentstats.Options) (*mcpgoclient.Client, *agentstats.Tracker) {
	s, tr := tsmcpgo.NewServer("mcpgo-fixture", "1.2.3", opts)
	s.AddTool(mcpgo.NewTool("add", mcpgo.WithDescription("Adds two numbers. Returns the sum."), mcpgo.WithNumber("a"), mcpgo.WithNumber("b")),
		func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
			return mcpgo.NewToolResultText("ok"), nil
		})
	s.AddTool(mcpgo.NewTool("fail", mcpgo.WithDescription("Always fails.")),
		func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
			return mcpgo.NewToolResultError("boom: upstream 503"), nil
		})
	c, err := mcpgoclient.NewInProcessClient(s)
	if err != nil {
		panic(err)
	}
	ctx := context.Background()
	if err := c.Start(ctx); err != nil {
		panic(err)
	}
	init := mcpgo.InitializeRequest{}
	init.Params.ProtocolVersion = mcpgo.LATEST_PROTOCOL_VERSION
	init.Params.ClientInfo = mcpgo.Implementation{Name: "test-client", Version: "9.9.9"}
	if _, err := c.Initialize(ctx, init); err != nil {
		panic(err)
	}
	return c, tr
}

func callMcpGo(c *mcpgoclient.Client, name string, args map[string]any) {
	req := mcpgo.CallToolRequest{}
	req.Params.Name = name
	req.Params.Arguments = args
	c.CallTool(context.Background(), req)
}

func TestMcpGoRecords(t *testing.T) {
	cap := newCapture(0)
	defer cap.srv.Close()
	c, tr := newMcpGo(agentstats.Options{APIKey: "al_srv_test", Endpoint: cap.srv.URL, FlushInterval: time.Hour})
	ctx := context.Background()
	if _, err := c.ListTools(ctx, mcpgo.ListToolsRequest{}); err != nil {
		t.Fatal(err)
	}
	callMcpGo(c, "add", map[string]any{"a": 2, "b": 3})
	callMcpGo(c, "fail", map[string]any{})
	tr.Shutdown(ctx)

	events, catalog, name := cap.events()
	assertRecorded(t, events, catalog, name, "mcpgo-fixture")
}

func assertRecorded(t *testing.T, events []map[string]any, catalog map[string]any, name, wantName string) {
	t.Helper()
	if name != wantName {
		t.Errorf("serverName = %q, want %q", name, wantName)
	}
	calls := toolCalls(events)
	if len(calls) != 2 || calls[0]["tool"] != "add" || calls[0]["isError"] != false || calls[1]["tool"] != "fail" || calls[1]["isError"] != true {
		t.Fatalf("unexpected tool calls: %v", calls)
	}
	if calls[1]["errorMessage"] != "boom: upstream 503" {
		t.Errorf("errorMessage = %v", calls[1]["errorMessage"])
	}
	if calls[0]["clientName"] != "test-client" || calls[0]["clientVersion"] != "9.9.9" {
		t.Errorf("client = %v %v", calls[0]["clientName"], calls[0]["clientVersion"])
	}
	if calls[0]["sessionId"] == nil {
		t.Error("missing sessionId")
	}
	if catalog == nil || len(catalog["tools"].([]any)) != 2 {
		t.Errorf("catalog = %v", catalog)
	}
	raw, _ := json.Marshal(events)
	if strings.Contains(string(raw), `"a":2`) {
		t.Error("arguments leaked")
	}
}

func TestSlowEndpointNeverBlocks(t *testing.T) {
	cap := newCapture(3 * time.Second)
	defer cap.srv.Close()
	cs, _ := newGoSDK(agentstats.Options{APIKey: "al_srv_test", Endpoint: cap.srv.URL, FlushInterval: 10 * time.Millisecond, MaxBatch: 1})
	start := time.Now()
	for i := 0; i < 50; i++ {
		cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "add", Arguments: map[string]any{"a": i, "b": 1}})
	}
	if d := time.Since(start); d > 500*time.Millisecond {
		t.Fatalf("50 calls took %v with a slow endpoint", d)
	}
}

func TestDeadEndpointBoundsBuffer(t *testing.T) {
	cs, tr := newGoSDK(agentstats.Options{APIKey: "al_srv_test", Endpoint: "http://127.0.0.1:1", FlushInterval: time.Hour, MaxBuffer: 100, MaxBatch: 100000})
	for i := 0; i < 300; i++ {
		cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "add", Arguments: map[string]any{"a": 1, "b": 1}})
	}
	if tr.Dropped() < 200 {
		t.Fatalf("dropped = %d, want >= 200", tr.Dropped())
	}
}

func TestNoKeyIsNoop(t *testing.T) {
	t.Setenv("AGENTSTATS_MCP_KEY", "")
	tr := agentstats.New(agentstats.Options{})
	if tr.Enabled() {
		t.Fatal("tracker enabled without a key")
	}
}

// ---------- overhead ----------

func benchGoSDK(b *testing.B, instrumented bool) {
	opts := agentstats.Options{Endpoint: "http://127.0.0.1:1", FlushInterval: time.Hour}
	if instrumented {
		opts.APIKey = "al_srv_bench"
	} else {
		b.Setenv("AGENTSTATS_MCP_KEY", "")
	}
	cs, _ := newGoSDK(opts)
	ctx := context.Background()
	params := &mcp.CallToolParams{Name: "add", Arguments: map[string]any{"a": 1, "b": 2}}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		cs.CallTool(ctx, params)
	}
}

func BenchmarkGoSDKBaseline(b *testing.B)     { benchGoSDK(b, false) }
func BenchmarkGoSDKInstrumented(b *testing.B) { benchGoSDK(b, true) }

func TestErrorTextDecidesWhatIsKept(t *testing.T) {
	errorsWith := func(opts agentstats.Options) []map[string]any {
		cap := newCapture(0)
		defer cap.srv.Close()
		opts.APIKey, opts.Endpoint, opts.FlushInterval = "al_srv_test", cap.srv.URL, time.Hour
		cs, tr := newGoSDK(opts)
		ctx := context.Background()
		cs.CallTool(ctx, &mcp.CallToolParams{Name: "fail", Arguments: map[string]any{}})
		tr.Shutdown(ctx)
		events, _, _ := cap.events()
		return toolCalls(events)
	}
	if c := errorsWith(agentstats.Options{DisableErrorText: true}); c[0]["isError"] != true || c[0]["errorMessage"] != nil {
		t.Fatalf("DisableErrorText: %v", c[0])
	}
	var seen agentstats.ErrorInfo
	c := errorsWith(agentstats.Options{ErrorText: func(e agentstats.ErrorInfo) string { seen = e; return e.Tool + " failed" }})
	if c[0]["errorMessage"] != "fail failed" || seen.Method != "tools/call" || seen.Message == "" {
		t.Fatalf("ErrorText: %v %+v", c[0], seen)
	}
	if c := errorsWith(agentstats.Options{ErrorText: func(agentstats.ErrorInfo) string { return "" }}); c[0]["errorMessage"] != nil {
		t.Fatalf("empty ErrorText: %v", c[0])
	}
	if c := errorsWith(agentstats.Options{ErrorText: func(agentstats.ErrorInfo) string { panic("bug") }}); c[0]["isError"] != true || c[0]["errorMessage"] != nil {
		t.Fatalf("panicking ErrorText: %v", c[0])
	}
}
