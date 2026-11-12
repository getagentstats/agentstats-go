package cobracli_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type sent struct {
	Events   []map[string]any `json:"events"`
	Feedback []map[string]any `json:"feedback"`
	Catalog  *struct {
		Commands []map[string]any `json:"commands"`
	} `json:"catalog"`
}

type outcome struct {
	events, feedback []map[string]any
	commands         []string
	code             int
	stdout, stderr   string
}

// runFeedback runs the fixture with extra environment against an endpoint answering status.
func runFeedback(t *testing.T, dir string, env []string, status int, args ...string) outcome {
	t.Helper()
	var mu sync.Mutex
	var got []sent
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var x sent
		json.Unmarshal(b, &x)
		mu.Lock()
		got = append(got, x)
		mu.Unlock()
		w.WriteHeader(status)
	}))
	defer srv.Close()
	cmd := exec.Command(binary, args...)
	cmd.Env = append([]string{"PATH=" + os.Getenv("PATH"), "HOME=" + dir, "AGENTSTATS_CLI_KEY=al_cli_test", "AGENTSTATS_URL=" + srv.URL, "AGENTSTATS_CLI_DIR=" + dir, "AGENTSTATS_CLI_TMP_DIR=" + dir + "-tmp"}, env...)
	var stdout, stderr strings.Builder
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	cmd.Run()
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
	o := outcome{code: cmd.ProcessState.ExitCode(), stdout: stdout.String(), stderr: stderr.String()}
	for _, g := range got {
		o.events = append(o.events, g.Events...)
		o.feedback = append(o.feedback, g.Feedback...)
		if g.Catalog != nil {
			for _, c := range g.Catalog.Commands {
				o.commands = append(o.commands, c["name"].(string))
			}
		}
	}
	return o
}

func TestFeedbackCommand(t *testing.T) {
	o := runFeedback(t, t.TempDir(), []string{"CLAUDECODE=1", "CLAUDE_CODE_SESSION_ID=thread-1"}, 202, "feedback", "deploy", "says", "not linked", "--command", "deploy", "--from", "user")
	if o.code != 0 || !strings.Contains(o.stdout, "Thanks: your feedback is on its way to the developers of shipit") {
		t.Fatalf("exit %d\n%s%s", o.code, o.stdout, o.stderr)
	}
	if len(o.events) != 0 {
		t.Errorf("the feedback command is not a command: %v", o.events)
	}
	if len(o.feedback) != 1 {
		t.Fatalf("feedback: %v", o.feedback)
	}
	f := o.feedback[0]
	for k, v := range map[string]any{"content": "deploy says not linked", "command": "deploy", "source": "user", "agent": "claude-code"} {
		if f[k] != v {
			t.Errorf("%s = %#v, want %#v", k, f[k], v)
		}
	}
	if id, _ := f["sessionId"].(string); len(id) != 16 {
		t.Errorf("sessionId %v", f["sessionId"])
	}
	for _, c := range o.commands {
		if c == "feedback" {
			t.Errorf("feedback is in the command list: %v", o.commands)
		}
	}
}

func TestFeedbackNothingToSend(t *testing.T) {
	o := runFeedback(t, t.TempDir(), nil, 202, "feedback")
	if o.code != 1 || !strings.Contains(o.stderr, "Nothing was sent: say what you ran") || len(o.feedback)+len(o.events) != 0 {
		t.Fatalf("exit %d, %d events, %d feedback\n%s", o.code, len(o.events), len(o.feedback), o.stderr)
	}
}

func TestFeedbackInHelpAndOff(t *testing.T) {
	if o := runFeedback(t, t.TempDir(), nil, 202, "--help"); !strings.Contains(o.stdout, "feedback") {
		t.Errorf("help doesn't list feedback:\n%s", o.stdout)
	}
	o := runFeedback(t, t.TempDir(), []string{"FIXTURE_NO_FEEDBACK=1"}, 202, "feedback", "hi")
	if len(o.events) != 1 || o.events[0]["unknown"] != true || len(o.feedback) != 0 {
		t.Errorf("NoFeedback: %v %v", o.events, o.feedback)
	}
}

func TestFeedbackHint(t *testing.T) {
	dir := t.TempDir()
	agent := []string{"CLAUDECODE=1", "CLAUDE_CODE_SESSION_ID=thread-2"}
	if o := runFeedback(t, dir, agent, 202, "deploy", "--env", "staging"); !strings.Contains(o.stderr, `If shipit got something wrong here, tell its developers: shipit feedback "`) {
		t.Errorf("no hint:\n%s", o.stderr)
	}
	if o := runFeedback(t, dir, agent, 202, "deploy", "--env", "staging"); strings.Contains(o.stderr, "shipit feedback") {
		t.Errorf("hint twice in one session:\n%s", o.stderr)
	}
	if o := runFeedback(t, t.TempDir(), nil, 202, "deploy", "--env", "staging"); strings.Contains(o.stderr, "shipit feedback") {
		t.Errorf("hint for a non-agent:\n%s", o.stderr)
	}
}

func TestFeedbackKeptWhenTheEndpointFails(t *testing.T) {
	dir := t.TempDir()
	if o := runFeedback(t, dir, nil, 503, "feedback", "first try"); len(o.feedback) != 1 {
		t.Fatalf("not sent: %v", o.feedback)
	}
	o := runFeedback(t, dir, nil, 202, "status")
	if len(o.feedback) != 1 || o.feedback[0]["content"] != "first try" || len(o.events) != 1 {
		t.Errorf("not resent: %v %v", o.feedback, o.events)
	}
}
