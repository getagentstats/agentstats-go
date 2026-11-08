package agentstats_test

import (
	"context"
	"errors"
	"math/rand/v2"
	"os"
	"strings"
	"testing"
	"time"

	"agentstats.io/go"
	"agentstats.io/go/gosdk"
	tsmcpgo "agentstats.io/go/mcpgo"
	mcpgoclient "github.com/mark3labs/mcp-go/client"
	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// AGENTSTATS_E2E=1 AGENTSTATS_MCP_KEY=al_srv_... AGENTSTATS_ENDPOINT=http://localhost:3000/api/ingest/mcp go test -run E2E -v .
func TestE2ELocal(t *testing.T) {
	if os.Getenv("AGENTSTATS_E2E") != "1" {
		t.Skip("set AGENTSTATS_E2E=1")
	}
	ctx := context.Background()
	type spec struct {
		name, desc string
		weight     int
		errRate    float64
		maxMs      int
	}
	pick := func(specs []spec) string {
		var pool []string
		for _, s := range specs {
			for i := 0; i < s.weight; i++ {
				pool = append(pool, s.name)
			}
		}
		return pool[rand.IntN(len(pool))]
	}

	// go-sdk: github-issues
	issues := []spec{
		{"search_issues", "Searches issues across repositories. Use when the user describes a bug without a number. Returns up to 30 issues.", 7, 0.02, 200},
		{"list_issues", "Lists open issues in one repository.", 6, 0.04, 90},
		{"create_issue", "Creates an issue. Requires repo write access.", 2, 0.25, 300},
		{"close_issue", "Closes an issue.", 0, 0, 10},
	}
	s := mcp.NewServer(&mcp.Implementation{Name: "github-issues", Version: "0.9.1"}, nil)
	for _, sp := range issues {
		sp := sp
		mcp.AddTool(s, &mcp.Tool{Name: sp.name, Description: sp.desc}, func(ctx context.Context, req *mcp.CallToolRequest, in struct {
			Q string `json:"q,omitempty"`
		}) (*mcp.CallToolResult, any, error) {
			time.Sleep(time.Duration(rand.IntN(sp.maxMs)) * time.Millisecond)
			if rand.Float64() < sp.errRate {
				return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: sp.name + ": GitHub API rate limit exceeded"}}}, nil, nil
			}
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "ok"}}}, nil, nil
		})
	}
	tr := gosdk.Instrument(s, agentstats.Options{APIKey: keyFor("github-issues")})
	st, ct := mcp.NewInMemoryTransports()
	s.Connect(ctx, st, nil)
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "cursor", Version: "1.9.2"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	cs.ListTools(ctx, nil)
	for i := 0; i < 45; i++ {
		cs.CallTool(ctx, &mcp.CallToolParams{Name: pick(issues), Arguments: map[string]any{}})
	}
	tr.Shutdown(ctx)
	t.Logf("go-sdk: dropped=%d", tr.Dropped())

	// mcp-go: postgres-tools
	pg := []spec{
		{"run_query", "Runs a read-only SQL query. Returns at most 500 rows.", 9, 0.12, 250},
		{"describe_table", "Shows columns and indexes for one table.", 4, 0, 40},
		{"list_tables", "Lists tables in the current schema.", 3, 0, 20},
	}
	srv, tr2 := tsmcpgo.NewServer("postgres-tools", "1.3.0", agentstats.Options{APIKey: keyFor("postgres-tools")})
	for _, sp := range pg {
		sp := sp
		srv.AddTool(mcpgo.NewTool(sp.name, mcpgo.WithDescription(sp.desc)), func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
			time.Sleep(time.Duration(rand.IntN(sp.maxMs)) * time.Millisecond)
			if rand.Float64() < sp.errRate {
				return nil, errors.New(sp.name + ": canceling statement due to statement timeout")
			}
			return mcpgo.NewToolResultText("ok"), nil
		})
	}
	c, _ := mcpgoclient.NewInProcessClient(srv)
	c.Start(ctx)
	init := mcpgo.InitializeRequest{}
	init.Params.ProtocolVersion = mcpgo.LATEST_PROTOCOL_VERSION
	init.Params.ClientInfo = mcpgo.Implementation{Name: "claude-code", Version: "2.1.3"}
	c.Initialize(ctx, init)
	c.ListTools(ctx, mcpgo.ListToolsRequest{})
	for i := 0; i < 40; i++ {
		req := mcpgo.CallToolRequest{}
		req.Params.Name = pick(pg)
		c.CallTool(ctx, req)
	}
	tr2.Shutdown(ctx)
	t.Logf("mcp-go: dropped=%d", tr2.Dropped())
}

// keyFor reads AGENTSTATS_MCP_KEY_<SERVER> (each MCP server has its own key), falling back to AGENTSTATS_MCP_KEY.
func keyFor(server string) string {
	return os.Getenv("AGENTSTATS_MCP_KEY_" + strings.ToUpper(strings.ReplaceAll(server, "-", "_")))
}
