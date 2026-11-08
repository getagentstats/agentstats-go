package cli

import (
	"crypto/rand"
	"encoding/hex"
	"os"
)

// AgentHeaders returns the headers that tell your API a coding agent is making the call, for your
// API client or SDK: X-AI-Agent ("claude-code/2.1.316") and X-AI-Agent-Run. Empty unless a coding
// agent (Claude Code, Cursor, Codex…) runs this process, so a deployed app sends nothing. Hints,
// not proof: never authorize with them. The AgentStats API middleware turns them into sessions.
//
//	for k, v := range cli.AgentHeaders() { req.Header.Set(k, v) }
func AgentHeaders() map[string]string {
	return AgentHeadersFrom(os.Getenv, os.Setenv)
}

// AgentHeadersFrom is AgentHeaders with the environment passed in; setenv keeps a new run id
// (AGENTSTATS_RUN_ID) so child processes inherit it, and may be nil.
func AgentHeadersFrom(getenv func(string) string, setenv func(string, string) error) map[string]string {
	a := DetectActor(getenv, false)
	switch a.Agent {
	case "human", "ci", "unknown", "":
		return map[string]string{}
	}
	name := a.Agent
	if a.Version != "" {
		name += "/" + a.Version
	}
	return map[string]string{"X-AI-Agent": name, "X-AI-Agent-Run": agentRun(getenv, setenv, a.Agent)}
}

// agentRun is the agent's thread when it exposes one (the same session id the CLI SDK gives the
// thread's commands), else one id per process tree, kept in AGENTSTATS_RUN_ID.
func agentRun(getenv func(string) string, setenv func(string, string) error, agent string) string {
	if thread := AgentThread(getenv); thread != "" {
		return hash(agent + "|" + thread)
	}
	if id := getenv("AGENTSTATS_RUN_ID"); id != "" {
		return id
	}
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	id := hex.EncodeToString(b)
	if setenv != nil {
		_ = setenv("AGENTSTATS_RUN_ID", id)
	}
	return id
}
