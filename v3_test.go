package agentstats_test

import (
	"context"
	"testing"
	"time"

	"agentstats.io/go"
	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func (c *capture) feedback() (out []map[string]any) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, b := range c.bodies {
		if list, ok := b["feedback"].([]any); ok {
			for _, f := range list {
				out = append(out, f.(map[string]any))
			}
		}
	}
	return
}

func toolNames(cat map[string]any) (out []string) {
	for _, tl := range cat["tools"].([]any) {
		out = append(out, tl.(map[string]any)["name"].(string))
	}
	return
}

// What failed and how much text a result handed the model (PROTOCOL.md errorKind, resultChars).
func assertV3(t *testing.T, events []map[string]any) {
	t.Helper()
	calls := toolCalls(events)
	if len(calls) < 2 {
		t.Fatalf("calls: %v", calls)
	}
	if calls[0]["errorKind"] != nil || calls[0]["resultChars"] != float64(2) {
		t.Errorf("add: errorKind=%v resultChars=%v, want none and 2", calls[0]["errorKind"], calls[0]["resultChars"])
	}
	if calls[1]["errorKind"] != "tool" || calls[1]["resultChars"] != nil {
		t.Errorf("fail: errorKind=%v resultChars=%v, want tool and none", calls[1]["errorKind"], calls[1]["resultChars"])
	}
	for _, e := range events {
		if e["method"] == "tools/list" && e["resultChars"] != nil {
			t.Errorf("tools/list should carry no resultChars: %v", e)
		}
	}
}

func assertFeedback(t *testing.T, cap *capture, listed []string, reply string) {
	t.Helper()
	if len(listed) == 0 || listed[len(listed)-1] != agentstats.FeedbackToolName {
		t.Errorf("tool list = %v, want send_feedback added", listed)
	}
	if reply != agentstats.FeedbackReply {
		t.Errorf("reply = %q", reply)
	}
	fb := cap.feedback()
	if len(fb) != 1 || fb[0]["content"] != "add rounds decimals" || fb[0]["source"] != "user" || fb[0]["tool"] != "add" {
		t.Fatalf("feedback = %v", fb)
	}
	events, catalog, _ := cap.events()
	for _, n := range toolNames(catalog) {
		if n == agentstats.FeedbackToolName {
			t.Errorf("catalog includes send_feedback: %v", toolNames(catalog))
		}
	}
	for _, c := range toolCalls(events) {
		if c["tool"] == agentstats.FeedbackToolName {
			t.Errorf("send_feedback recorded as a tool call")
		}
	}
}

func TestGoSDKV3(t *testing.T) {
	cap := newCapture(0)
	defer cap.srv.Close()
	t.Setenv("AGENTSTATS_URL", cap.srv.URL)
	cs, tr := newGoSDK(agentstats.Options{APIKey: "al_srv_test", FlushInterval: time.Hour})
	ctx := context.Background()
	cs.ListTools(ctx, nil)
	cs.CallTool(ctx, &mcp.CallToolParams{Name: "add", Arguments: map[string]any{"a": 2, "b": 3}})
	cs.CallTool(ctx, &mcp.CallToolParams{Name: "fail", Arguments: map[string]any{}})
	tr.Shutdown(ctx)
	events, _, _ := cap.events()
	assertV3(t, events)
}

func TestGoSDKFeedbackTool(t *testing.T) {
	cap := newCapture(0)
	defer cap.srv.Close()
	t.Setenv("AGENTSTATS_URL", cap.srv.URL)
	cs, tr := newGoSDK(agentstats.Options{APIKey: "al_srv_test", FlushInterval: time.Hour, FeedbackTool: true})
	ctx := context.Background()
	list, err := cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	var listed []string
	for _, tl := range list.Tools {
		listed = append(listed, tl.Name)
	}
	cs.CallTool(ctx, &mcp.CallToolParams{Name: "add", Arguments: map[string]any{"a": 1, "b": 1}})
	r, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: agentstats.FeedbackToolName, Arguments: map[string]any{"content": "add rounds decimals", "source": "user", "tool": "add"}})
	if err != nil {
		t.Fatal(err)
	}
	tr.Shutdown(ctx)
	assertFeedback(t, cap, listed, r.Content[0].(*mcp.TextContent).Text)
}

func TestMcpGoV3(t *testing.T) {
	cap := newCapture(0)
	defer cap.srv.Close()
	t.Setenv("AGENTSTATS_URL", cap.srv.URL)
	c, tr := newMcpGo(agentstats.Options{APIKey: "al_srv_test", FlushInterval: time.Hour})
	ctx := context.Background()
	c.ListTools(ctx, mcpgo.ListToolsRequest{})
	callMcpGo(c, "add", map[string]any{"a": 2, "b": 3})
	callMcpGo(c, "fail", map[string]any{})
	tr.Shutdown(ctx)
	events, _, _ := cap.events()
	assertV3(t, events)
}

func TestMcpGoFeedbackTool(t *testing.T) {
	cap := newCapture(0)
	defer cap.srv.Close()
	t.Setenv("AGENTSTATS_URL", cap.srv.URL)
	c, tr := newMcpGo(agentstats.Options{APIKey: "al_srv_test", FlushInterval: time.Hour, FeedbackTool: true})
	ctx := context.Background()
	list, err := c.ListTools(ctx, mcpgo.ListToolsRequest{})
	if err != nil {
		t.Fatal(err)
	}
	var listed []string
	for _, tl := range list.Tools {
		if tl.Name != agentstats.FeedbackToolName {
			listed = append(listed, tl.Name)
		}
	}
	// mcp-go lists tools sorted by name; the test only needs it present.
	listed = append(listed, agentstats.FeedbackToolName)
	callMcpGo(c, "add", map[string]any{"a": 1, "b": 1})
	req := mcpgo.CallToolRequest{}
	req.Params.Name = agentstats.FeedbackToolName
	req.Params.Arguments = map[string]any{"content": "add rounds decimals", "source": "user", "tool": "add"}
	r, err := c.CallTool(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	tr.Shutdown(ctx)
	present := false
	for _, tl := range list.Tools {
		present = present || tl.Name == agentstats.FeedbackToolName
	}
	if !present {
		t.Fatalf("send_feedback not listed")
	}
	assertFeedback(t, cap, listed, r.Content[0].(mcpgo.TextContent).Text)
}
