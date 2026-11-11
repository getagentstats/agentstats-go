// Package mcpgo adds AgentStats analytics to servers built with github.com/mark3labs/mcp-go.
//
// Swap server.NewMCPServer for mcpgo.NewServer:
//
//	s, tracker := mcpgo.NewServer("my-server", "1.0.0", agentstats.Options{})
//	defer tracker.Shutdown(context.Background())
//
// Already using hooks? Create a tracker with agentstats.New(agentstats.Options{ServerName: "my-server"})
// and call mcpgo.AddTo(yourHooks, tracker).
package mcpgo

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"agentstats.io/go"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

// NewServer is server.NewMCPServer with analytics hooks attached.
// Do not also pass server.WithHooks; use AddTo with your own hooks instead.
func NewServer(name, version string, opts agentstats.Options, serverOpts ...server.ServerOption) (*server.MCPServer, *agentstats.Tracker) {
	if opts.ServerName == "" {
		opts.ServerName = name
	}
	hooks, t := Hooks(opts)
	s := server.NewMCPServer(name, version, append(serverOpts, server.WithHooks(hooks))...)
	AddFeedbackTool(s, t)
	return s, t
}

// AddFeedbackTool registers send_feedback (Options.FeedbackTool) on the server: what agents and users
// write is sent to AgentStats, and the hooks leave it out of the tool list and the tool calls they report.
// NewServer calls it; call it yourself when you use AddTo with your own hooks.
func AddFeedbackTool(s *server.MCPServer, t *agentstats.Tracker) {
	if !t.FeedbackEnabled() {
		return
	}
	schema, _ := json.Marshal(agentstats.FeedbackInputSchema())
	no := false
	s.AddTool(mcp.Tool{
		Name:           agentstats.FeedbackToolName,
		Title:          agentstats.FeedbackToolTitle,
		Description:    agentstats.FeedbackToolDescription,
		RawInputSchema: schema,
		Annotations:    mcp.ToolAnnotation{Title: agentstats.FeedbackToolTitle, DestructiveHint: &no, OpenWorldHint: &no},
	}, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		sid, cname, cversion := "", "", ""
		if cs := server.ClientSessionFromContext(ctx); cs != nil {
			sid = cs.SessionID()
			if ci, ok := cs.(server.SessionWithClientInfo); ok {
				cname, cversion = ci.GetClientInfo().Name, ci.GetClientInfo().Version
			}
		}
		if !t.RecordFeedback(req.GetArguments(), sid, cname, cversion) {
			return mcp.NewToolResultError(agentstats.FeedbackEmptyReply), nil
		}
		return mcp.NewToolResultText(agentstats.FeedbackReply), nil
	})
}

// Hooks returns a hook set that records analytics, and the tracker behind it.
func Hooks(opts agentstats.Options) (*server.Hooks, *agentstats.Tracker) {
	hooks := &server.Hooks{}
	t := agentstats.New(opts)
	AddTo(hooks, t)
	return hooks, t
}

type pending struct {
	started time.Time
	tool    string
}

// AddTo registers the analytics hooks on an existing hook set.
func AddTo(hooks *server.Hooks, t *agentstats.Tracker) {
	if !t.Enabled() {
		return
	}
	var inflight sync.Map // session|id -> pending
	key := func(ctx context.Context, id any) (string, string) {
		sid := ""
		if s := server.ClientSessionFromContext(ctx); s != nil {
			sid = s.SessionID()
		}
		return fmt.Sprintf("%s|%v", sid, id), sid
	}

	hooks.AddBeforeAny(func(ctx context.Context, id any, method mcp.MCPMethod, message any) {
		if !agentstats.Tracked[string(method)] {
			return
		}
		var tool string
		if method == mcp.MethodToolsCall {
			if !t.Sample() {
				return
			}
			if r, ok := message.(*mcp.CallToolRequest); ok {
				tool = r.Params.Name
			}
			// Our own send_feedback is feedback, not a tool call.
			if tool == agentstats.FeedbackToolName && t.FeedbackEnabled() {
				return
			}
		}
		if r, ok := message.(*mcp.InitializeRequest); ok {
			_, sid := key(ctx, id)
			t.SetClient(sid, r.Params.ClientInfo.Name, r.Params.ClientInfo.Version)
		}
		k, _ := key(ctx, id)
		inflight.Store(k, pending{time.Now(), tool})
	})

	finish := func(ctx context.Context, id any, method mcp.MCPMethod, result any, err error) {
		k, sid := key(ctx, id)
		v, ok := inflight.LoadAndDelete(k)
		if !ok {
			return
		}
		p := v.(pending)
		call := agentstats.Call{Method: string(method), Tool: p.tool, SessionID: sid, Started: p.started, Auth: t.AuthFrom(ctx)}
		if s, ok := server.ClientSessionFromContext(ctx).(server.SessionWithClientInfo); ok {
			ci := s.GetClientInfo()
			call.ClientName, call.ClientVersion = ci.Name, ci.Version
		}
		if err != nil {
			call.IsError, call.ErrorMessage, call.Err, call.ErrorKind = true, err.Error(), err, "protocol"
		} else {
			call.ResultChars = resultChars(result)
		}
		switch r := result.(type) {
		case *mcp.CallToolResult:
			if r != nil && r.IsError {
				call.IsError, call.ErrorKind = true, "tool"
				for _, c := range r.Content {
					if tc, ok := c.(mcp.TextContent); ok {
						call.ErrorMessage = tc.Text
						break
					}
				}
			}
		case *mcp.InitializeResult:
			if r != nil {
				t.SetServerInfo(r.ServerInfo.Name, r.ServerInfo.Version, r.Instructions)
			}
		case *mcp.ListToolsResult:
			if r != nil && r.NextCursor == "" {
				tools := r.Tools
				if t.FeedbackEnabled() {
					own := make([]mcp.Tool, 0, len(tools))
					for _, tl := range tools {
						if tl.Name != agentstats.FeedbackToolName {
							own = append(own, tl)
						}
					}
					tools = own
				}
				sig := agentstats.ToolsSignature(len(tools), func(i int) (string, int) { return tools[i].Name, len(tools[i].Description) })
				t.SetTools(func() []agentstats.CatalogTool {
					out := make([]agentstats.CatalogTool, len(tools))
					for i, tl := range tools {
						out[i] = agentstats.CatalogTool{Name: tl.Name, Description: tl.Description, InputSchema: tl.InputSchema}
					}
					return out
				}, sig)
			}
		}
		t.Record(call)
	}

	hooks.AddOnSuccess(func(ctx context.Context, id any, method mcp.MCPMethod, message any, result any) {
		finish(ctx, id, method, result, nil)
	})
	hooks.AddOnError(func(ctx context.Context, id any, method mcp.MCPMethod, message any, err error) {
		finish(ctx, id, method, nil, err)
	})
}

// resultChars is the text a result hands the model (PROTOCOL.md resultChars).
func resultChars(result any) *int {
	n := 0
	text := func(c mcp.Content) {
		switch v := c.(type) {
		case mcp.TextContent:
			n += len(v.Text)
		case mcp.EmbeddedResource:
			if r, ok := v.Resource.(mcp.TextResourceContents); ok {
				n += len(r.Text)
			}
		}
	}
	switch r := result.(type) {
	case *mcp.CallToolResult:
		if r == nil {
			return nil
		}
		for _, c := range r.Content {
			text(c)
		}
		if n == 0 && r.StructuredContent != nil {
			if b, err := json.Marshal(r.StructuredContent); err == nil {
				n = len(b)
			}
		}
	case *mcp.ReadResourceResult:
		if r == nil {
			return nil
		}
		for _, c := range r.Contents {
			if tr, ok := c.(mcp.TextResourceContents); ok {
				n += len(tr.Text)
			}
		}
	case *mcp.GetPromptResult:
		if r == nil {
			return nil
		}
		for _, m := range r.Messages {
			text(m.Content)
		}
	default:
		return nil
	}
	return &n
}
