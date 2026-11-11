// Package gosdk adds AgentStats analytics to servers built with
// github.com/modelcontextprotocol/go-sdk.
//
//	server := mcp.NewServer(&mcp.Implementation{Name: "my-server", Version: "1.0.0"}, nil)
//	tracker := gosdk.Instrument(server, agentstats.Options{})
//	defer tracker.Shutdown(context.Background())
package gosdk

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"sync"
	"sync/atomic"
	"time"

	"agentstats.io/go"
	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Instrument adds a receiving middleware to server and returns the tracker.
// Without an API key it returns a disabled tracker and leaves server untouched.
func Instrument(server *mcp.Server, opts agentstats.Options) *agentstats.Tracker {
	t := agentstats.New(opts)
	if t.Enabled() {
		if name, version := serverImpl(server); name != "" {
			t.SetServerInfo(name, version, "")
		}
		server.AddReceivingMiddleware(Middleware(t))
	}
	return t
}

// serverImpl reads the Implementation passed to mcp.NewServer. Newer protocol versions
// attach serverInfo after middleware runs, so the middleware can't see it.
func serverImpl(server *mcp.Server) (name, version string) {
	defer func() { _ = recover() }()
	impl := reflect.ValueOf(server).Elem().FieldByName("impl")
	if !impl.IsValid() || impl.IsNil() {
		return "", ""
	}
	return impl.Elem().FieldByName("Name").String(), impl.Elem().FieldByName("Version").String()
}

// Middleware returns the receiving middleware, for composing with your own.
func Middleware(t *agentstats.Tracker) mcp.Middleware {
	// stdio sessions have no id; give each session object a stable random one.
	var local sync.Map
	sessionID := func(ss *mcp.ServerSession) string {
		if id := ss.ID(); id != "" {
			return id
		}
		if v, ok := local.Load(ss); ok {
			return v.(string)
		}
		v, _ := local.LoadOrStore(ss, agentstats.LocalSessionID())
		return v.(string)
	}

	return func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			if !agentstats.Tracked[method] {
				return next(ctx, method, req)
			}
			// send_feedback (Options.FeedbackTool): answered here, never by the server, and not a tool call.
			if method == "tools/call" && t.FeedbackEnabled() && !serverHasFeedback.Load() {
				if res, ok := answerFeedback(t, req, sessionID); ok {
					return res, nil
				}
			}
			var tool string
			if method == "tools/call" {
				if !t.Sample() {
					return next(ctx, method, req)
				}
				switch p := req.GetParams().(type) {
				case *mcp.CallToolParamsRaw:
					tool = p.Name
				case *mcp.CallToolParams:
					tool = p.Name
				}
			}
			started := time.Now()
			res, err := next(ctx, method, req)

			call := agentstats.Call{Method: method, Tool: tool, Started: started}
			if ss, ok := req.GetSession().(*mcp.ServerSession); ok && ss != nil {
				call.SessionID = sessionID(ss)
				if ip := ss.InitializeParams(); ip != nil && ip.ClientInfo != nil {
					call.ClientName, call.ClientVersion = ip.ClientInfo.Name, ip.ClientInfo.Version
				}
			}
			if call.ClientName == "" {
				call.ClientName, call.ClientVersion = metaClient(req)
			}
			call.Auth = requestAuth(ctx, t, req)
			if err != nil {
				call.IsError, call.ErrorMessage, call.Err, call.ErrorKind = true, err.Error(), err, "protocol"
				var we *jsonrpc.Error
				if errors.As(err, &we) {
					code := we.Code
					call.ErrorCode = &code
				}
			}
			observe(t, method, req, res, &call)
			t.Record(call)
			if r, ok := res.(*mcp.ListToolsResult); ok && t.FeedbackEnabled() {
				res = withFeedbackTool(r)
			}
			return res, err
		}
	}
}

// serverHasFeedback: the server lists a send_feedback tool of its own, so its calls are left alone.
var serverHasFeedback atomic.Bool

func answerFeedback(t *agentstats.Tracker, req mcp.Request, sessionID func(*mcp.ServerSession) string) (mcp.Result, bool) {
	var name string
	var args map[string]any
	switch p := req.GetParams().(type) {
	case *mcp.CallToolParamsRaw:
		name = p.Name
		_ = json.Unmarshal(p.Arguments, &args)
	case *mcp.CallToolParams:
		name = p.Name
		args, _ = p.Arguments.(map[string]any)
	}
	if name != agentstats.FeedbackToolName {
		return nil, false
	}
	var sid, cname, cversion string
	if ss, ok := req.GetSession().(*mcp.ServerSession); ok && ss != nil {
		sid = sessionID(ss)
		if ip := ss.InitializeParams(); ip != nil && ip.ClientInfo != nil {
			cname, cversion = ip.ClientInfo.Name, ip.ClientInfo.Version
		}
	}
	if !t.RecordFeedback(args, sid, cname, cversion) {
		return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: agentstats.FeedbackEmptyReply}}}, true
	}
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: agentstats.FeedbackReply}}}, true
}

// withFeedbackTool returns the tool list with send_feedback added: a copy, so the server's own list is untouched.
func withFeedbackTool(r *mcp.ListToolsResult) *mcp.ListToolsResult {
	if r == nil || r.NextCursor != "" {
		return r
	}
	for _, tl := range r.Tools {
		if tl != nil && tl.Name == agentstats.FeedbackToolName {
			serverHasFeedback.Store(true)
			return r
		}
	}
	no := false
	out := *r
	out.Tools = append(append(make([]*mcp.Tool, 0, len(r.Tools)+1), r.Tools...), &mcp.Tool{
		Name:        agentstats.FeedbackToolName,
		Title:       agentstats.FeedbackToolTitle,
		Description: agentstats.FeedbackToolDescription,
		InputSchema: agentstats.FeedbackInputSchema(),
		Annotations: &mcp.ToolAnnotations{Title: agentstats.FeedbackToolTitle, DestructiveHint: &no, OpenWorldHint: &no},
	})
	return &out
}

// resultChars is the text a result hands the model (PROTOCOL.md resultChars).
func resultChars(res mcp.Result) *int {
	n := 0
	text := func(c mcp.Content) {
		switch v := c.(type) {
		case *mcp.TextContent:
			n += len(v.Text)
		case *mcp.EmbeddedResource:
			if v.Resource != nil {
				n += len(v.Resource.Text)
			}
		}
	}
	switch r := res.(type) {
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
			if c != nil {
				n += len(c.Text)
			}
		}
	case *mcp.GetPromptResult:
		if r == nil {
			return nil
		}
		for _, m := range r.Messages {
			if m != nil {
				text(m.Content)
			}
		}
	default:
		return nil
	}
	return &n
}

// requestAuth is the sign-in summary of a request: the go-sdk's TokenInfo (set by
// auth.RequireBearerToken), with the client id a verifier puts in TokenInfo.Extra["client_id"],
// else Options.AuthInfo. UserID and the rest of Extra are never read.
func requestAuth(ctx context.Context, t *agentstats.Tracker, req mcp.Request) *agentstats.Auth {
	if extra := req.GetExtra(); extra != nil && extra.TokenInfo != nil {
		clientID, _ := extra.TokenInfo.Extra["client_id"].(string)
		return agentstats.AuthSummary(clientID, extra.TokenInfo.Scopes)
	}
	return t.AuthFrom(ctx)
}

// metaClient reads clientInfo that newer clients attach to every request's _meta.
func metaClient(req mcp.Request) (string, string) {
	p := req.GetParams()
	if p == nil {
		return "", ""
	}
	if ci, ok := p.GetMeta()[mcp.MetaKeyClientInfo].(map[string]any); ok {
		name, _ := ci["name"].(string)
		version, _ := ci["version"].(string)
		return name, version
	}
	return "", ""
}

func observe(t *agentstats.Tracker, method string, req mcp.Request, res mcp.Result, call *agentstats.Call) {
	// 2026-07-28 spec: serverInfo rides on every response's _meta.
	if res != nil {
		if si, ok := res.GetMeta()[mcp.MetaKeyServerInfo].(map[string]any); ok {
			name, _ := si["name"].(string)
			version, _ := si["version"].(string)
			t.SetServerInfo(name, version, "")
		} else if si, ok := res.GetMeta()[mcp.MetaKeyServerInfo].(*mcp.Implementation); ok && si != nil {
			t.SetServerInfo(si.Name, si.Version, "")
		}
	}
	if !call.IsError {
		call.ResultChars = resultChars(res)
	}
	switch r := res.(type) {
	case *mcp.DiscoverResult:
		if r != nil && r.Instructions != "" {
			t.SetInstructions(r.Instructions)
		}
	case *mcp.CallToolResult:
		if r != nil && r.IsError {
			call.IsError, call.ErrorKind = true, "tool"
			for _, c := range r.Content {
				if tc, ok := c.(*mcp.TextContent); ok {
					call.ErrorMessage = tc.Text
					break
				}
			}
		}
	case *mcp.InitializeResult:
		if r != nil && r.ServerInfo != nil {
			t.SetServerInfo(r.ServerInfo.Name, r.ServerInfo.Version, r.Instructions)
		}
		if p, ok := req.GetParams().(*mcp.InitializeParams); ok && p.ClientInfo != nil {
			call.ClientName, call.ClientVersion = p.ClientInfo.Name, p.ClientInfo.Version
		}
	case *mcp.ListToolsResult:
		if r == nil || r.NextCursor != "" {
			return
		}
		tools := r.Tools
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
