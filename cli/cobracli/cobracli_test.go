package cobracli_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

type body struct {
	CLI     string           `json:"cli"`
	Events  []map[string]any `json:"events"`
	Catalog *struct {
		Version  string           `json:"version"`
		Commands []map[string]any `json:"commands"`
	} `json:"catalog"`
}

var binary string

func TestMain(m *testing.M) {
	dir, _ := os.MkdirTemp("", "shipit-bin")
	binary = filepath.Join(dir, "shipit")
	if out, err := exec.Command("go", "build", "-o", binary, "./testdata/shipit").CombinedOutput(); err != nil {
		panic(string(out))
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

// run starts the fixture CLI against a fake endpoint and waits for the background sender.
func run(t *testing.T, args ...string) ([]map[string]any, *body, int, string) {
	t.Helper()
	var mu sync.Mutex
	var got []body
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var x body
		json.Unmarshal(b, &x)
		if r.Header.Get("Authorization") != "Bearer al_cli_test" || r.Header.Get("X-AgentStats-SDK") == "" {
			t.Errorf("headers: %v", r.Header)
		}
		mu.Lock()
		got = append(got, x)
		mu.Unlock()
		w.WriteHeader(202)
	}))
	defer srv.Close()
	dir := t.TempDir()
	cmd := exec.Command(binary, args...)
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + dir, "AGENTSTATS_CLI_KEY=al_cli_test", "AGENTSTATS_CLI_ENDPOINT=" + srv.URL, "AGENTSTATS_CLI_DIR=" + dir, "AGENTSTATS_CLI_TMP_DIR=" + dir + "-tmp"}
	out, _ := cmd.CombinedOutput()
	started := time.Now()
	for time.Since(started) < 5*time.Second {
		mu.Lock()
		n := len(got)
		mu.Unlock()
		locks, _ := filepath.Glob(filepath.Join(dir, "*.lock"))
		if n > 0 && len(locks) == 0 {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	var events []map[string]any
	var catalog *body
	for i := range got {
		events = append(events, got[i].Events...)
		if got[i].Catalog != nil {
			catalog = &got[i]
		}
	}
	return events, catalog, cmd.ProcessState.ExitCode(), string(out)
}

func TestCobraCommands(t *testing.T) {
	cases := []struct {
		args []string
		want map[string]any
	}{
		{[]string{"deploy", "site-a", "--prod"}, map[string]any{"command": "deploy", "exitCode": 0.0, "flags": []any{"--prod"}, "errorMessage": nil}},
		{[]string{"deploy", "--env", "staging"}, map[string]any{"command": "deploy", "exitCode": 3.0, "errorMessage": "Staging is locked"}},
		{[]string{"st"}, map[string]any{"command": "status", "exitCode": 0.0}},
		{[]string{"db", "migrate", "--url", "postgres://secret@db"}, map[string]any{"command": "db migrate", "exitCode": 0.0, "flags": []any{"--url"}}},
		{[]string{"db", "migrate"}, map[string]any{"command": "db migrate", "exitCode": 1.0, "errorMessage": `required flag(s) "url" not set`}},
		{[]string{"db", "seed"}, map[string]any{"command": "db seed", "exitCode": 1.0, "errorMessage": "seed file missing"}},
		{[]string{"deploy", "--preview"}, map[string]any{"command": "deploy", "exitCode": 1.0, "errorMessage": "unknown flag: --preview"}},
		{[]string{"deplyo"}, map[string]any{"command": "deplyo", "unknown": true, "exitCode": 1.0, "errorMessage": nil}},
		// cobra shows db's help and exits 0; it's still a command that doesn't exist.
		{[]string{"db", "push"}, map[string]any{"command": "db push", "unknown": true, "exitCode": 0.0}},
		{[]string{"postgres://admin:secret@db/prod"}, map[string]any{"command": "(invalid)", "unknown": true, "exitCode": 1.0}},
		{[]string{"deploy", "--help"}, map[string]any{"command": "deploy", "help": true, "exitCode": 0.0}},
		{[]string{"help", "db", "migrate"}, map[string]any{"command": "db migrate", "help": true, "exitCode": 0.0}},
		{[]string{"db"}, map[string]any{"command": "db", "help": true, "exitCode": 0.0}},
		{[]string{"--help"}, map[string]any{"command": "shipit", "help": true, "exitCode": 0.0}},
	}
	for _, c := range cases {
		events, _, code, out := run(t, c.args...)
		name := strings.Join(c.args, " ")
		if len(events) != 1 {
			t.Fatalf("%s: %d events (exit %d)\n%s", name, len(events), code, out)
		}
		for k, v := range c.want {
			if !reflect.DeepEqual(events[0][k], v) {
				t.Errorf("%s: %s = %#v, want %#v\n%s", name, k, events[0][k], v, out)
			}
		}
		if wantCode := int(c.want["exitCode"].(float64)); code != wantCode {
			t.Errorf("%s: process exit %d, want %d", name, code, wantCode)
		}
		if b, _ := json.Marshal(events[0]); strings.Contains(string(b), "secret") {
			t.Errorf("%s: argument values left the machine: %s", name, b)
		}
		if events[0]["agent"] != "unknown" {
			t.Errorf("%s: no terminal and no agent should be unknown, got %v", name, events[0]["agent"])
		}
		if events[0]["runtime"] == nil || !strings.HasPrefix(events[0]["runtime"].(string), "go/") {
			t.Errorf("%s: runtime %v", name, events[0]["runtime"])
		}
	}
}

func TestCobraCatalog(t *testing.T) {
	_, b, _, out := run(t, "status")
	if b == nil {
		t.Fatalf("no command list sent\n%s", out)
	}
	names := []string{}
	for _, c := range b.Catalog.Commands {
		names = append(names, c["name"].(string))
	}
	if want := []string{"db migrate", "db seed", "deploy", "status"}; !reflect.DeepEqual(names, want) {
		t.Fatalf("commands %v, want %v", names, want)
	}
	deploy, _ := json.Marshal(b.Catalog.Commands[2])
	want := `{"arguments":[{"name":"dir"}],"description":"Deploy the current directory","name":"deploy","options":[{"description":"Environment to deploy to","name":"--env","short":"-e","takesValue":true},{"description":"Deploy to production","name":"--prod"}]}`
	if string(deploy) != want {
		t.Fatalf("deploy spec\n%s\nwant\n%s", deploy, want)
	}
	migrate, _ := json.Marshal(b.Catalog.Commands[0]["options"])
	if !strings.Contains(string(migrate), `"required":true`) {
		t.Fatalf("migrate --url should be required: %s", migrate)
	}
	if b.Catalog.Version != "1.0.0" {
		t.Fatalf("version %q", b.Catalog.Version)
	}
}

func TestNoKeyIsNoop(t *testing.T) {
	dir := t.TempDir()
	cmd := exec.Command(binary, "status")
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + dir, "AGENTSTATS_CLI_DIR=" + dir}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Fatalf("wrote %d files without a key", len(entries))
	}
}

// With the publish key (release CI), the command list is sent before the process exits.
func TestPublishKey(t *testing.T) {
	var mu sync.Mutex
	var published []body
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		if r.Header.Get("X-AgentStats-Publish-Key") == "al_sec_test" {
			var x body
			json.Unmarshal(b, &x)
			mu.Lock()
			published = append(published, x)
			mu.Unlock()
		}
		w.WriteHeader(202)
	}))
	defer srv.Close()
	dir := t.TempDir()
	cmd := exec.Command(binary, "--version")
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + dir, "AGENTSTATS_CLI_KEY=al_cli_test", "AGENTSTATS_CLI_PUBLISH_KEY=al_sec_test", "AGENTSTATS_CLI_ENDPOINT=" + srv.URL, "AGENTSTATS_CLI_DIR=" + dir}
	out, _ := cmd.CombinedOutput()
	mu.Lock()
	defer mu.Unlock()
	if len(published) != 1 || published[0].Catalog == nil || len(published[0].Catalog.Commands) != 4 {
		t.Fatalf("published %+v\n%s", published, out)
	}
	if !strings.Contains(string(out), "[agentstats] published 4 commands") {
		t.Fatalf("no confirmation on stderr:\n%s", out)
	}
}
