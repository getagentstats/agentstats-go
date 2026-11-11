package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"agentstats.io/go/api"
)

const secret = "sk_live_9f8e7d6c5b4a39281706f5e4d3c2b1a0"

func TestMiddlewareWithServeMux(t *testing.T) {
	var mu sync.Mutex
	var bodies []map[string]any
	var raw []string
	ingest := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var b map[string]any
		var buf strings.Builder
		_ = json.NewDecoder(r.Body).Decode(&b)
		enc, _ := json.Marshal(b)
		buf.Write(enc)
		mu.Lock()
		bodies = append(bodies, b)
		raw = append(raw, buf.String())
		mu.Unlock()
		if r.Header.Get("Authorization") != "Bearer al_api_test" {
			t.Errorf("auth header %q", r.Header.Get("Authorization"))
		}
		w.WriteHeader(202)
	}))
	defer ingest.Close()

	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/users/{id}", func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.PathValue("id"), "missing") {
			http.Error(w, "not found", 404)
			return
		}
		w.Write([]byte(`{"ok":true}`))
	})
	mux.HandleFunc("POST /v1/users", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(201) })
	t.Setenv("AGENTSTATS_URL", ingest.URL)
	tr := api.New(api.Options{APIKey: "al_api_test", FlushInterval: time.Hour})
	srv := httptest.NewServer(api.Middleware(tr)(mux))
	defer srv.Close()

	do := func(method, path string, h map[string]string) int {
		req, _ := http.NewRequest(method, srv.URL+path, nil)
		for k, v := range h {
			req.Header.Set(k, v)
		}
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		return res.StatusCode
	}
	agent := map[string]string{"X-AI-Agent": "codex/1.2", "X-AI-Agent-Run": "r9", "Authorization": "Bearer " + secret}
	do("GET", "/v1/users/u-1", agent)
	do("GET", "/v1/users/missing-9", agent)
	do("POST", "/v1/users", agent)
	do("GET", "/v1/users/u-2", map[string]string{"User-Agent": "Go-http-client/1.1"})
	do("GET", "/v1/users/u-3", map[string]string{"User-Agent": "Mozilla/5.0 (compatible; ChatGPT-User/1.0; +https://openai.com/bot)"})
	if err := tr.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}

	mu.Lock()
	defer mu.Unlock()
	var got []string
	other := 0.0
	for _, b := range bodies {
		for _, c := range b["calls"].([]any) {
			m := c.(map[string]any)
			got = append(got, m["method"].(string)+" "+m["route"].(string)+" "+jsonNum(m["status"]))
		}
		for _, n := range b["other"].(map[string]any) {
			other += n.(float64)
		}
	}
	want := []string{"GET /v1/users/{id} 200", "GET /v1/users/{id} 404", "POST /v1/users 201", "GET /v1/users/{id} 200"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("calls %v, want %v", got, want)
	}
	if other != 1 {
		t.Fatalf("other %v", other)
	}
	for _, r := range raw {
		if strings.Contains(r, secret) {
			t.Fatal("the credential must never be sent")
		}
	}
}

func jsonNum(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func TestRouteFromPath(t *testing.T) {
	for in, want := range map[string]string{"/v1/users/42": "/v1/users/:id", "/v1/users/jane@acme.com": "/v1/users/:id", "/v1/users/me": "/v1/users/me", "/v1/charges/ch_3PqRsTuVwXyZ1234": "/v1/charges/:id"} {
		if got := api.RouteFromPath(in); got != want {
			t.Errorf("%s: %s, want %s", in, got, want)
		}
	}
}
