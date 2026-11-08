package agentstats_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"agentstats.io/go"
	"agentstats.io/go/gosdk"
	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type bearer struct {
	token string
	base  http.RoundTripper
}

func (b bearer) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.token)
	return b.base.RoundTrip(r)
}

// A go-sdk server behind auth.RequireBearerToken over Streamable HTTP, called by the go-sdk
// client with access tokens: only the client app and the scopes are sent, never the token or user.
func TestGoSDKOAuthSignIn(t *testing.T) {
	cap := newCapture(0)
	defer cap.srv.Close()
	s := mcp.NewServer(&mcp.Implementation{Name: "oauth-go", Version: "1.0.0"}, nil)
	mcp.AddTool(s, &mcp.Tool{Name: "search", Description: "Search."}, func(ctx context.Context, req *mcp.CallToolRequest, in struct{}) (*mcp.CallToolResult, any, error) {
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "ok"}}}, nil, nil
	})
	tr := gosdk.Instrument(s, agentstats.Options{APIKey: "al_srv_test", Endpoint: cap.srv.URL, FlushInterval: time.Hour})

	tokens := map[string]*auth.TokenInfo{
		"tok-claude": {Scopes: []string{"mcp:tools", "mcp:read", "mcp:read"}, Expiration: time.Now().Add(time.Hour), UserID: "user-42", Extra: map[string]any{"client_id": "https://claude.ai/oauth/mcp-client-metadata.json", "email": "ada@example.com"}},
		"tok-dcr":    {Scopes: []string{"mcp:tools"}, Expiration: time.Now().Add(time.Hour), Extra: map[string]any{"client_id": "b7e1c0de-dcr-client"}},
	}
	verify := func(ctx context.Context, token string, req *http.Request) (*auth.TokenInfo, error) {
		if ti, ok := tokens[token]; ok {
			return ti, nil
		}
		return nil, auth.ErrInvalidToken
	}
	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return s }, nil)
	srv := httptest.NewServer(auth.RequireBearerToken(verify, nil)(handler))
	defer srv.Close()

	ctx := context.Background()
	for _, token := range []string{"tok-claude", "tok-dcr"} {
		client := mcp.NewClient(&mcp.Implementation{Name: "e2e-client", Version: "1.0.0"}, nil)
		cs, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: srv.URL, HTTPClient: &http.Client{Transport: bearer{token, http.DefaultTransport}}}, nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "search", Arguments: map[string]any{}}); err != nil {
			t.Fatal(err)
		}
		cs.Close()
	}
	_ = tr.Shutdown(ctx)

	events, _, _ := cap.events()
	var got []any
	for _, e := range toolCalls(events) {
		got = append(got, e["auth"])
	}
	want := []any{
		map[string]any{"method": "oauth", "client": "claude.ai/oauth/mcp-client-metadata.json", "scopes": []any{"mcp:read", "mcp:tools"}},
		map[string]any{"method": "oauth", "client": "registered", "scopes": []any{"mcp:tools"}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("auth = %v, want %v", got, want)
	}
	raw, _ := json.Marshal(cap.bodies)
	for _, secret := range []string{"tok-claude", "tok-dcr", "user-42", "ada@example.com", "b7e1c0de"} {
		if strings.Contains(string(raw), secret) {
			t.Fatalf("%s must not be sent", secret)
		}
	}
}

func TestAuthSummary(t *testing.T) {
	for id, want := range map[string]string{
		"https://claude.ai/oauth/mcp-client-metadata.json": "claude.ai/oauth/mcp-client-metadata.json",
		"https://claude.ai":         "registered",
		"https://x.test/c?user=42":  "registered",
		"https://u:p@x.test/c":      "registered",
		"http://x.test/client.json": "registered",
		"b7e1c0de-dcr-client":       "registered",
	} {
		if got := *agentstats.AuthSummary(id, nil).Client; got != want {
			t.Errorf("%s: %s, want %s", id, got, want)
		}
	}
	if agentstats.AuthSummary("", nil).Client != nil {
		t.Error("no client id: client must be null")
	}
}
