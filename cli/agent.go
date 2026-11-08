package cli

import (
	"regexp"
	"strings"
)

// Who is running the CLI: a coding agent, a person at a terminal, CI, or something unknown.
// Agents set environment variables in every command they run. These are hints, not proof:
// never use them to authorize anything. Same order and rules as the Node SDK.

// Actor is who runs the CLI.
type Actor struct {
	Agent   string // claude-code, cursor, codex, human, ci, unknown…
	Version string // the agent's version when it says (from AI_AGENT)
	Via     string // the environment variable (or "tty") the answer came from
	CI      string // CI provider, also for an agent running inside CI
}

var agents = []struct {
	agent string
	vars  []string
}{
	{"amp", []string{"AMP_CURRENT_THREAD_ID"}},
	{"claude-code", []string{"CLAUDECODE", "CLAUDE_CODE_ENTRYPOINT"}},
	{"cursor", []string{"CURSOR_AGENT"}},
	{"gemini-cli", []string{"GEMINI_CLI"}},
	{"codex", []string{"CODEX_SANDBOX", "CODEX_THREAD_ID"}},
	{"copilot-cli", []string{"COPILOT_CLI"}},
	{"opencode", []string{"OPENCODE_CLIENT", "OPENCODE"}},
	{"augment", []string{"AUGMENT_AGENT"}},
	{"antigravity", []string{"ANTIGRAVITY_AGENT"}},
}

var ciProviders = []struct {
	provider string
	vars     []string
}{
	{"github-actions", []string{"GITHUB_ACTIONS"}},
	{"gitlab", []string{"GITLAB_CI"}},
	{"circleci", []string{"CIRCLECI"}},
	{"buildkite", []string{"BUILDKITE"}},
	{"jenkins", []string{"JENKINS_URL"}},
	{"azure-pipelines", []string{"TF_BUILD"}},
	{"bitbucket", []string{"BITBUCKET_BUILD_NUMBER"}},
	{"travis", []string{"TRAVIS"}},
	{"vercel", []string{"VERCEL"}},
	{"netlify", []string{"NETLIFY"}},
}

// AgentThread is the thread or conversation id an agent exposes, to group its commands into one session.
func AgentThread(getenv func(string) string) string {
	for _, k := range []string{"CLAUDE_CODE_SESSION_ID", "CODEX_THREAD_ID", "AMP_CURRENT_THREAD_ID"} {
		if v := getenv(k); v != "" {
			return v
		}
	}
	return ""
}

var (
	nonAgent   = regexp.MustCompile(`[^a-z0-9.-]`)
	nonVersion = regexp.MustCompile(`[^0-9a-z.+-]`)
)

// ParseAIAgent reads AI_AGENT as agents set it: `claude-code_2-1-316_agent` or `devin@1`.
func ParseAIAgent(value string) (agent, version string, ok bool) {
	v := strings.ToLower(strings.TrimSpace(value))
	if v == "" {
		return "", "", false
	}
	at := strings.IndexAny(v, "@_")
	name := v
	if at >= 0 {
		name = v[:at]
	}
	name = nonAgent.ReplaceAllString(name, "")
	if len(name) > 40 {
		name = name[:40]
	}
	if name == "" {
		return "", "", false
	}
	if at >= 0 {
		raw := strings.SplitN(v[at+1:], "_", 2)[0]
		if raw != "" && raw[0] >= '0' && raw[0] <= '9' {
			if v[at] == '_' {
				raw = strings.ReplaceAll(raw, "-", ".")
			}
			version = nonVersion.ReplaceAllString(raw, "")
			if len(version) > 40 {
				version = version[:40]
			}
		}
	}
	return name, version, true
}

// CIProvider is the CI system running the CLI, "ci" for an unrecognised one, or "".
func CIProvider(getenv func(string) string) string {
	for _, p := range ciProviders {
		for _, v := range p.vars {
			if truthy(getenv(v)) {
				return p.provider
			}
		}
	}
	if truthy(getenv("CI")) || truthy(getenv("CONTINUOUS_INTEGRATION")) {
		return "ci"
	}
	return ""
}

// DetectActor works out who runs the CLI from the environment and whether it's attached to a terminal.
func DetectActor(getenv func(string) string, interactive bool) Actor {
	ci := CIProvider(getenv)
	// AI_AGENT is the proposed standard (Claude Code sets it); it wins over inferred signals.
	if agent, version, ok := ParseAIAgent(getenv("AI_AGENT")); ok {
		return Actor{Agent: agent, Version: version, Via: "AI_AGENT", CI: ci}
	}
	for _, a := range agents {
		for _, v := range a.vars {
			if truthy(getenv(v)) {
				return Actor{Agent: a.agent, Via: v, CI: ci}
			}
		}
	}
	if ci != "" {
		return Actor{Agent: "ci", CI: ci}
	}
	if interactive {
		return Actor{Agent: "human", Via: "tty"}
	}
	return Actor{Agent: "unknown"}
}
