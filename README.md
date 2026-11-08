# agentstats-go

Analytics for Go MCP servers. The core package has no dependencies; adapters per library:

```sh
go get agentstats.io/go
```

```go
// github.com/modelcontextprotocol/go-sdk
server := mcp.NewServer(&mcp.Implementation{Name: "my-server", Version: "1.0.0"}, nil)
tracker := gosdk.Instrument(server, agentstats.Options{}) // receiving middleware
defer tracker.Shutdown(context.Background())

// github.com/mark3labs/mcp-go: drop-in for server.NewMCPServer
s, tracker := mcpgo.NewServer("my-server", "1.0.0", agentstats.Options{})
// already using hooks? mcpgo.AddTo(yourHooks, agentstats.New(agentstats.Options{ServerName: "my-server"}))
```

Handles both the `initialize` handshake and the 2026-07-28 `server/discover` + per-request `_meta` client info. Measured overhead: ~0.4 µs on an 80 µs in-memory call (`go test -bench .`).

```sh
go test ./...
```
