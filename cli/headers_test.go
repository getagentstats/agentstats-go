package cli

import (
	"crypto/sha256"
	"encoding/hex"
	"testing"
)

func envOf(m map[string]string) (func(string) string, func(string, string) error) {
	return func(k string) string { return m[k] }, func(k, v string) error { m[k] = v; return nil }
}

func TestAgentHeaders(t *testing.T) {
	get, set := envOf(map[string]string{})
	if h := AgentHeadersFrom(get, set); len(h) != 0 {
		t.Fatalf("no agent: %v", h)
	}
	get, set = envOf(map[string]string{"CI": "true", "GITHUB_ACTIONS": "true"})
	if h := AgentHeadersFrom(get, set); len(h) != 0 {
		t.Fatalf("CI: %v", h)
	}
	get, set = envOf(map[string]string{"AI_AGENT": "claude-code_2-1-316_agent", "CLAUDE_CODE_SESSION_ID": "thread-1"})
	h := AgentHeadersFrom(get, set)
	sum := sha256.Sum256([]byte("claude-code|thread-1"))
	if h["X-AI-Agent"] != "claude-code/2.1.316" || h["X-AI-Agent-Run"] != hex.EncodeToString(sum[:])[:16] {
		t.Fatalf("claude code: %v", h)
	}
	env := map[string]string{"CODEX_SANDBOX": "seatbelt"}
	get, set = envOf(env)
	run := AgentHeadersFrom(get, set)["X-AI-Agent-Run"]
	if len(run) != 16 || env["AGENTSTATS_RUN_ID"] != run || AgentHeadersFrom(get, set)["X-AI-Agent-Run"] != run {
		t.Fatalf("run id per process tree: %s %v", run, env)
	}
}
