// Package cli is AgentStats analytics for command-line tools written in Go: which commands
// coding agents, people and CI run, and where they fail. A port of the Node SDK
// (@agentstats/cli); events, spool and session files are the same.
//
// The CLI never waits on AgentStats. Finish appends one line to a spool file in the user's
// cache directory and starts the CLI's own binary again in the background to send it (New
// recognises that run and sends instead of starting the CLI). Anything that fails is dropped
// silently. Argument values are never sent: only the command name and flag names.
//
// With cobra, use agentstats.io/go/cli/cobracli. Otherwise:
//
//	func main() {
//		stats := cli.New(cli.Options{CLI: "acme", Version: version}) // first thing in main
//		stats.MarkFeedbackCommand() // you answer it: failed commands hint at it
//		if len(os.Args) > 1 && os.Args[1] == stats.FeedbackName() {
//			stats.RunFeedback(os.Args[2:]) // the feedback command; exits
//		}
//		stats.Command("deploy")
//		err := deploy()
//		stats.Error(err)
//		stats.Exit(exitCode(err)) // records, then os.Exit
//	}
package cli

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"time"

	"agentstats.io/go/internal/ingest"
)

const Version = "0.2.0"
const maxSpoolBytes = 512 * 1024

// sendEnv marks the background run that sends a spool instead of running the CLI.
const sendEnv = "AGENTSTATS_CLI_SEND_SPOOL"

// Options configure a Tracker. Zero values fall back to environment variables and defaults.
type Options struct {
	CLI     string // your CLI's name, e.g. "acme"
	Version string // shown per command in the dashboard
	// APIKey defaults to AGENTSTATS_CLI_KEY. Without a key the tracker is a no-op.
	APIKey string
	// PublishKey (al_sec_…, release CI only) replaces the command list on the dashboard before
	// the process exits. Defaults to AGENTSTATS_CLI_PUBLISH_KEY. Never ship it.
	PublishKey string
	// Notice is printed once to stderr on the first run in a terminal. Empty uses the default
	// text; NoNotice turns it off when your CLI already tells users about telemetry.
	Notice   string
	NoNotice bool
	// NoFlags stops flag names (never values) being sent; NoErrors stops error messages.
	NoFlags  bool
	NoErrors bool
	// ErrorText decides what is sent of a failed command's error: return the text to keep, or
	// "" for none. Use errors.As on ErrorInfo.Err for codes. A panic keeps nothing.
	ErrorText func(ErrorInfo) string
	// FlagNames decides which flag names are sent; return nil for none.
	FlagNames func(command string, flags []string) []string
	// NoFeedback turns off the feedback command: `acme feedback "what happened" [--command deploy]
	// [--from user]`, which agents run when a command fails them or can't do what the user needs, and
	// users ask their agent to run to pass something on. The SDK answers it (it isn't recorded as a
	// command) and the text shows on the CLI's Feedback page. cobracli adds it to your commands unless
	// you have one of that name; without cobra, route it to RunFeedback.
	NoFeedback bool
	// FeedbackName names the feedback command. Default "feedback".
	FeedbackName string
	// NoFeedbackHint stops the one line on stderr, the first time a command an agent runs fails in a
	// session, saying the feedback command is there.
	NoFeedbackHint bool
	// SessionIdle groups commands by the same actor in the same directory. Default 30 minutes.
	SessionIdle time.Duration
	Dir         string // AGENTSTATS_CLI_DIR, else ~/.cache/agentstats (or $TMPDIR/agentstats)
	Disabled    bool
	Debug       bool // AGENTSTATS_DEBUG=1

	// endpoint is set by New (ingest.URL); it isn't an option.
	endpoint string
}

// ErrorInfo is a failed command, as passed to Options.ErrorText.
type ErrorInfo struct {
	Command  string // "db migrate"
	Message  string // Err.Error()
	ExitCode int
	Err      error
}

// CommandSpec is one command your CLI offers, for the command list. Only Name is required.
type CommandSpec struct {
	Name        string    `json:"name"`
	Description string    `json:"description,omitempty"`
	Aliases     []string  `json:"aliases,omitempty"`
	Arguments   []ArgSpec `json:"arguments,omitempty"`
	Options     []OptSpec `json:"options,omitempty"`
}

type ArgSpec struct {
	Name        string   `json:"name"`
	Description string   `json:"description,omitempty"`
	Required    bool     `json:"required,omitempty"`
	Variadic    bool     `json:"variadic,omitempty"`
	Choices     []string `json:"choices,omitempty"`
}

type OptSpec struct {
	Name        string   `json:"name"` // long form when there is one: "--env"
	Short       string   `json:"short,omitempty"`
	Description string   `json:"description,omitempty"`
	TakesValue  bool     `json:"takesValue,omitempty"`
	Required    bool     `json:"required,omitempty"`
	Choices     []string `json:"choices,omitempty"`
}

// Event is one command, as written to the spool and sent to /api/ingest/cli.
type Event struct {
	Ts           int64    `json:"ts"`
	CLI          string   `json:"cli"`
	CLIVersion   *string  `json:"cliVersion"`
	Command      string   `json:"command"`
	Flags        []string `json:"flags"`
	ExitCode     int      `json:"exitCode"`
	DurationMs   int64    `json:"durationMs"`
	Unknown      bool     `json:"unknown"`
	Help         bool     `json:"help"`
	ErrorMessage *string  `json:"errorMessage"`
	Agent        string   `json:"agent"`
	AgentVersion *string  `json:"agentVersion"`
	Via          *string  `json:"via"`
	CI           *string  `json:"ci"`
	SessionID    string   `json:"sessionId"`
	OS           string   `json:"os"`
	Arch         string   `json:"arch"`
	Runtime      string   `json:"runtime"`
}

// Feedback is what an agent or its user said through the feedback command, as written to the spool
// (Type "feedback" sets it apart from commands) and sent as the batch's feedback.
type Feedback struct {
	Type         string  `json:"type"`
	Ts           int64   `json:"ts"`
	CLI          string  `json:"cli"`
	SessionID    string  `json:"sessionId"`
	Agent        string  `json:"agent"`
	AgentVersion *string `json:"agentVersion"`
	Source       string  `json:"source"` // "user" or "agent"
	Command      *string `json:"command"`
	Content      string  `json:"content"`
}

// FeedbackSummary is the feedback command's line in a command list.
const FeedbackSummary = "Tell the developers of this CLI when a command fails you or can't do what you need"

// FeedbackDescription is the feedback command's description, as agents read it in its help.
const FeedbackDescription = "Send feedback to the developers of this CLI. Use it when a command failed, did something unexpected or unhelpful, or could not do what the user needed; or when the user asks you to pass something on. Say what you ran, what happened and what you expected. Leave out personal information, secrets and the user's data."

// FeedbackHelp is the help text of the feedback command's message and options.
var FeedbackHelp = struct{ Message, Command, From string }{
	Message: "What you ran, what happened and what you expected, in a few sentences",
	Command: `The command it is about, e.g. "deploy"`,
	From:    `"user" when the user asked you to send it, "agent" (default) when you report on your own`,
}

// Tracker records the one command a CLI run executes. Safe to use when disabled: every method is a no-op.
type Tracker struct {
	opts    Options
	enabled bool
	actor   Actor
	started time.Time
	name    string
	flags   []string
	unknown bool
	help    bool
	err     error
	done    bool
	// notCommand: the feedback command ran, or feedback was sent; the run isn't recorded as a command.
	notCommand bool
	// feedbackAdded: the CLI answers the feedback command (MarkFeedbackCommand); only then does a failure hint at it.
	feedbackAdded bool
	catalog       []CommandSpec
	dirs          []string
	dir           string
	sendExec      func() (string, error)
}

// New creates the tracker. Call it first thing in main: when the CLI was started in the
// background to send events, New sends them and exits.
func New(opts Options) *Tracker {
	if spool := os.Getenv(sendEnv); spool != "" {
		os.Exit(runSender(spool))
	}
	if opts.APIKey == "" {
		opts.APIKey = os.Getenv("AGENTSTATS_CLI_KEY")
	}
	opts.endpoint = ingest.URL("cli")
	if opts.PublishKey == "" {
		opts.PublishKey = os.Getenv("AGENTSTATS_CLI_PUBLISH_KEY")
	}
	if opts.SessionIdle == 0 {
		opts.SessionIdle = 30 * time.Minute
	}
	if opts.Dir == "" {
		opts.Dir = envOr("AGENTSTATS_CLI_DIR", defaultDir())
	}
	if os.Getenv("AGENTSTATS_DEBUG") == "1" {
		opts.Debug = true
	}
	if opts.CLI == "" {
		opts.CLI = filepath.Base(os.Args[0])
	}
	t := &Tracker{opts: opts, started: time.Now(), dir: opts.Dir, sendExec: os.Executable}
	t.dirs = []string{opts.Dir}
	if tmp := envOr("AGENTSTATS_CLI_TMP_DIR", filepath.Join(os.TempDir(), "agentstats")); tmp != opts.Dir {
		t.dirs = append(t.dirs, tmp)
	}
	t.actor = DetectActor(os.Getenv, isTerminal(os.Stdin) || isTerminal(os.Stderr))
	t.enabled = opts.APIKey != "" && !opts.Disabled && !t.optedOut()
	if !t.enabled {
		t.log("disabled")
		return t
	}
	t.showNotice()
	return t
}

// FeedbackName is the feedback command's name, or "" when Options.NoFeedback is set.
func (t *Tracker) FeedbackName() string {
	if t.opts.NoFeedback {
		return ""
	}
	return or(strings.TrimSpace(t.opts.FeedbackName), "feedback")
}

// Feedback queues feedback about the CLI, sent in the background like commands. RunFeedback calls
// it; call it yourself to send feedback from a command of your own. from is "user" or "agent";
// command, the command it's about, may be "". False when nothing was queued (no text, or disabled).
func (t *Tracker) Feedback(message, command, from string) bool {
	if !t.enabled {
		return false
	}
	content := strings.TrimSpace(message)
	if content == "" {
		return false
	}
	if len(content) > 4000 {
		content = content[:4000]
	}
	if from != "user" {
		from = "agent"
	}
	var cmd *string
	if c := SafeCommandName(command); command != "" && c != "(invalid)" {
		cmd = &c
	}
	now := time.Now()
	f := Feedback{Type: "feedback", Ts: now.UnixMilli(), CLI: t.opts.CLI, Agent: t.actor.Agent, AgentVersion: strPtr(t.actor.Version), Source: from, Command: cmd, Content: content}
	if !t.appendLine(&f.SessionID, &f, now) {
		return false
	}
	t.notCommand = true
	return true
}

// MarkFeedbackCommand says the CLI answers the feedback command, so a failed command an agent runs
// hints at it. cobracli calls it when it adds the command; call it yourself when you route it to RunFeedback.
func (t *Tracker) MarkFeedbackCommand() { t.feedbackAdded = true }

// FeedbackArgs is what the feedback command was given.
type FeedbackArgs struct {
	Message, Command, From string
	Help                   bool
}

// ParseFeedbackArgs reads the feedback command's arguments (after its name): the words are the
// message; --command <name> and --from <who> (or --command=name) the options; -- ends options.
func ParseFeedbackArgs(args []string) FeedbackArgs {
	var out FeedbackArgs
	words := []string{}
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			words = append(words, args[i+1:]...)
			break
		}
		flag, value, inline := a, "", false
		if strings.HasPrefix(a, "--") {
			if k, v, ok := strings.Cut(a, "="); ok {
				flag, value, inline = k, v, true
			}
		}
		switch flag {
		case "--help", "-h":
			out.Help = true
		case "--command", "--from":
			if !inline && i+1 < len(args) {
				i++
				value = args[i]
			}
			if flag == "--command" {
				out.Command = value
			} else {
				out.From = value
			}
		default:
			words = append(words, a)
		}
	}
	out.Message = strings.Join(words, " ")
	return out
}

// RunFeedback runs the feedback command: queues what was given, prints the outcome and exits.
// cobracli calls it; without cobra, route your feedback command here: stats.RunFeedback(os.Args[2:]).
func (t *Tracker) RunFeedback(args []string) {
	t.RunFeedbackWith(ParseFeedbackArgs(args))
}

// RunFeedbackWith is RunFeedback with the arguments already parsed.
func (t *Tracker) RunFeedbackWith(a FeedbackArgs) {
	t.notCommand = true
	name := or(t.FeedbackName(), "feedback")
	switch {
	case a.Help:
		fmt.Fprintf(os.Stdout, "Usage: %s %s \"<message>\" [--command <name>] [--from user|agent]\n\n%s\n", t.opts.CLI, name, FeedbackDescription)
		t.Exit(0)
	case !t.enabled:
		fmt.Fprintf(os.Stderr, "Nothing was sent: %s's usage data is turned off on this machine.\n", t.opts.CLI)
		os.Exit(1)
	case !t.Feedback(a.Message, a.Command, a.From):
		if strings.TrimSpace(a.Message) == "" {
			fmt.Fprintf(os.Stderr, "Nothing was sent: say what you ran, what happened and what you expected, e.g. %s %s \"…\"\n", t.opts.CLI, name)
		} else {
			fmt.Fprintln(os.Stderr, "Nothing was sent: it could not be saved.")
		}
		t.Exit(1)
	default:
		fmt.Fprintf(os.Stdout, "Thanks: your feedback is on its way to the developers of %s.\n", t.opts.CLI)
		t.Exit(0)
	}
}

// Enabled reports whether events are recorded (a key is set and the user hasn't opted out).
func (t *Tracker) Enabled() bool { return t.enabled }

// Actor is who runs the CLI: a coding agent, a person, CI or unknown.
func (t *Tracker) Actor() Actor { return t.actor }

// CLI is the name events are recorded under.
func (t *Tracker) CLI() string { return t.opts.CLI }

// Command sets the command that is about to run, as a path: "deploy" or "db migrate".
// Flags are read from os.Args unless given.
func (t *Tracker) Command(name string, flags ...string) {
	name = strings.TrimSpace(name)
	if len(name) > 200 {
		name = name[:200]
	}
	if name == "" {
		name = t.opts.CLI
	}
	t.name = name
	if flags != nil {
		t.flags = flags
	}
}

// UnknownCommand records a command that doesn't exist, as a failed call. What was typed is
// only kept when it looks like a command word: anything else could be a value.
func (t *Tracker) UnknownCommand(name string) {
	t.unknown = true
	t.Command(SafeCommandName(name))
}

// Help records that help was shown, for a command or (with "") for the CLI itself.
func (t *Tracker) Help(name string) {
	t.help = true
	t.Command(name)
}

// Error attaches the error that made the command fail. Only its message is kept, as
// Options.NoErrors and Options.ErrorText decide. nil is ignored.
func (t *Tracker) Error(err error) {
	if err != nil {
		t.err = err
	}
}

// errorText applies NoErrors and ErrorText to the attached error.
func (t *Tracker) errorText(command string, exitCode int) (msg string) {
	if t.err == nil || t.opts.NoErrors {
		return ""
	}
	msg = t.err.Error()
	if t.opts.ErrorText != nil {
		defer func() {
			if r := recover(); r != nil {
				t.log("ErrorText panicked:", r)
				msg = ""
			}
		}()
		msg = t.opts.ErrorText(ErrorInfo{Command: command, Message: msg, ExitCode: exitCode, Err: t.err})
	}
	if len(msg) > 300 {
		msg = msg[:300]
	}
	return msg
}

// flagNames applies NoFlags and FlagNames.
func (t *Tracker) flagNames(command string) (out []string) {
	if t.opts.NoFlags {
		return []string{}
	}
	flags := t.flags
	if flags == nil {
		flags = FlagNames(os.Args[1:])
	}
	if t.opts.FlagNames == nil {
		return flags
	}
	defer func() {
		if r := recover(); r != nil {
			t.log("FlagNames panicked:", r)
			out = []string{}
		}
	}()
	out = []string{}
	for _, f := range t.opts.FlagNames(command, flags) {
		if len(f) > 60 {
			f = f[:60]
		}
		if len(out) < 30 {
			out = append(out, f)
		}
	}
	return out
}

// Commands sets the CLI's full command list, sent the first time and whenever it changes.
func (t *Tracker) Commands(list []CommandSpec) { t.catalog = NormalizeCatalog(list) }

// Exit records the run with this exit code, then calls os.Exit.
func (t *Tracker) Exit(code int) {
	t.Finish(code)
	os.Exit(code)
}

// SetEnabled persists the user's choice, e.g. from `acme telemetry disable`, for the next run.
func (t *Tracker) SetEnabled(on bool) {
	name := "disabled-" + slug(t.opts.CLI)
	for _, dir := range t.dirs {
		if on {
			os.Remove(filepath.Join(dir, name))
		} else if _, err := os.Stat(filepath.Join(t.dirs[0], name)); err != nil {
			os.MkdirAll(dir, 0o755)
			os.WriteFile(filepath.Join(dir, name), nil, 0o644)
		}
	}
}

// Finish records the run: one append to the spool, then a background sender. Never blocks on
// the network (except with a publish key, in release CI) and never panics. Call it once, at exit.
func (t *Tracker) Finish(exitCode int) {
	if !t.enabled || t.done {
		return
	}
	t.done = true
	defer func() {
		if r := recover(); r != nil {
			t.log("could not record:", r)
		}
	}()
	now := time.Now()
	if !t.notCommand {
		e := Event{
			Ts:           now.UnixMilli(),
			CLI:          t.opts.CLI,
			CLIVersion:   strPtr(t.opts.Version),
			Command:      or(t.name, t.opts.CLI),
			ExitCode:     exitCode,
			DurationMs:   now.Sub(t.started).Milliseconds(),
			Unknown:      t.unknown,
			Help:         t.help,
			Agent:        t.actor.Agent,
			AgentVersion: strPtr(t.actor.Version),
			Via:          strPtr(t.actor.Via),
			CI:           strPtr(t.actor.CI),
			OS:           goosName(),
			Arch:         goarchName(),
			Runtime:      "go/" + strings.TrimPrefix(runtime.Version(), "go"),
		}
		e.Flags = t.flagNames(e.Command)
		if exitCode != 0 || t.unknown {
			if msg := t.errorText(e.Command, exitCode); msg != "" {
				e.ErrorMessage = &msg
			}
		}
		t.appendLine(&e.SessionID, &e, now)
		if exitCode != 0 && !t.help && e.SessionID != "" {
			t.showFeedbackHint(e.SessionID)
		}
	}
	t.publishCatalog()
	if truthy(os.Getenv("CODEX_SANDBOX_NETWORK_DISABLED")) {
		return
	}
	for _, dir := range t.dirs {
		spool := filepath.Join(dir, t.spoolName())
		if !exists(spool) && !exists(spool+".catalog.json") {
			continue
		}
		if err := t.spawnSender(spool); err != nil {
			t.log("could not start sender:", err)
		}
	}
}

func (t *Tracker) spoolName() string { return "spool-" + hash(t.opts.APIKey) + ".jsonl" }

// appendLine appends a command or feedback to the spool in the first directory that can be
// written (a sandbox may refuse ~/.cache), setting its session id first. False when nothing was written.
func (t *Tracker) appendLine(sessionID *string, line any, now time.Time) bool {
	for _, dir := range t.dirs {
		t.dir = dir
		ok, err := t.write(sessionID, line, now)
		if err != nil {
			t.log("cannot write to", dir, err)
			continue
		}
		return ok
	}
	return false
}

func (t *Tracker) write(sessionID *string, line any, now time.Time) (bool, error) {
	if err := os.MkdirAll(t.dir, 0o755); err != nil {
		return false, err
	}
	id, err := t.session(now)
	if err != nil {
		return false, err
	}
	*sessionID = id
	spool := filepath.Join(t.dir, t.spoolName())
	written := false
	if st, err := os.Stat(spool); err == nil && st.Size() > maxSpoolBytes {
		t.log("spool full, dropped a line")
	} else {
		b, _ := json.Marshal(line)
		f, err := os.OpenFile(spool, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		if err != nil {
			return false, err
		}
		_, err = f.Write(append(b, '\n'))
		f.Close()
		if err != nil {
			return false, err
		}
		written = true
	}
	t.queueCatalog(spool)
	return written, nil
}

// showFeedbackHint: after a failed command an agent ran, once per session, one line on stderr
// saying the feedback command is there. Never for people, CI or unknown callers.
func (t *Tracker) showFeedbackHint(sessionID string) {
	name := t.FeedbackName()
	if name == "" || !t.feedbackAdded || t.opts.NoFeedbackHint || t.actor.Agent == "human" || t.actor.Agent == "ci" || t.actor.Agent == "unknown" {
		return
	}
	marker := filepath.Join(t.dir, "feedback-hint-"+slug(t.opts.CLI))
	if b, err := os.ReadFile(marker); err == nil && string(b) == sessionID {
		return
	}
	if os.WriteFile(marker, []byte(sessionID), 0o644) != nil {
		return
	}
	fmt.Fprintf(os.Stderr, "\nIf %s got something wrong here, tell its developers: %s %s \"what you ran, what happened, what you expected\"\n", t.opts.CLI, t.opts.CLI, name)
}

// session: the agent's thread when it exposes one, else the same actor in the same directory with no long gap.
func (t *Tracker) session(now time.Time) (string, error) {
	if thread := AgentThread(os.Getenv); thread != "" {
		return hash(t.actor.Agent + "|" + thread), nil
	}
	cwd, _ := os.Getwd()
	k := hash(t.opts.CLI + "|" + t.actor.Agent + "|" + cwd)
	file := filepath.Join(t.dir, "sessions.json")
	state := map[string][2]any{}
	if b, err := os.ReadFile(file); err == nil {
		json.Unmarshal(b, &state)
	}
	ms := now.UnixMilli()
	id := ""
	if prev, ok := state[k]; ok {
		if last, ok := prev[1].(float64); ok && ms-int64(last) < t.opts.SessionIdle.Milliseconds() {
			id, _ = prev[0].(string)
		}
	}
	if id == "" {
		id = hash(fmt.Sprintf("%s|%d|%d", k, ms, rand.Int64()))
	}
	state[k] = [2]any{id, ms}
	if len(state) > 200 {
		type kv struct {
			k    string
			last float64
		}
		all := []kv{}
		for key, v := range state {
			last, _ := v[1].(float64)
			if key == k {
				last = float64(ms)
			}
			all = append(all, kv{key, last})
		}
		slices.SortFunc(all, func(a, b kv) int { return int(b.last - a.last) })
		for _, x := range all[200:] {
			delete(state, x.k)
		}
	}
	b, _ := json.Marshal(state)
	return id, os.WriteFile(file, b, 0o644)
}

type catalogBody struct {
	Sig      string        `json:"sig,omitempty"`
	CLI      string        `json:"cli"`
	Version  *string       `json:"version"`
	Commands []CommandSpec `json:"commands"`
}

func (t *Tracker) queueCatalog(spool string) {
	if t.catalog == nil {
		return
	}
	body := catalogBody{CLI: t.opts.CLI, Version: strPtr(t.opts.Version), Commands: t.catalog}
	b, _ := json.Marshal(body)
	sig := hash(string(b))
	if sent, err := os.ReadFile(spool + ".catalog.sent"); err == nil && string(sent) == sig {
		return
	}
	body.Sig = sig
	b, _ = json.Marshal(body)
	os.WriteFile(spool+".catalog.json", b, 0o644)
}

// publishCatalog sends the command list with the publish key before exit (release CI only, up to 15 seconds).
func (t *Tracker) publishCatalog() {
	if t.opts.PublishKey == "" || t.catalog == nil {
		return
	}
	b, _ := json.Marshal(map[string]any{"cli": t.opts.CLI, "events": []any{}, "catalog": map[string]any{"version": strPtr(t.opts.Version), "commands": t.catalog}})
	req, _ := http.NewRequest("POST", t.opts.endpoint, bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+t.opts.APIKey)
	req.Header.Set("X-AgentStats-Publish-Key", t.opts.PublishKey)
	req.Header.Set("X-AgentStats-SDK", "cli-go/"+Version)
	res, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[agentstats] command list not published: %v\n", err)
		return
	}
	defer res.Body.Close()
	if res.StatusCode >= 300 {
		fmt.Fprintf(os.Stderr, "[agentstats] command list not published: HTTP %d\n", res.StatusCode)
		return
	}
	fmt.Fprintf(os.Stderr, "[agentstats] published %d commands\n", len(t.catalog))
}

// spawnSender starts this binary again, detached, to send the spool; unless one started in the last minute.
func (t *Tracker) spawnSender(spool string) error {
	lock := spool + ".lock"
	if st, err := os.Stat(lock); err == nil && time.Since(st.ModTime()) < time.Minute {
		return nil
	}
	exe, err := t.sendExec()
	if err != nil {
		return err
	}
	os.WriteFile(lock, []byte(fmt.Sprint(time.Now().UnixMilli())), 0o644)
	cmd := exec.Command(exe)
	cmd.Env = append(os.Environ(), sendEnv+"="+spool, "AGENTSTATS_SEND_ENDPOINT="+t.opts.endpoint, "AGENTSTATS_SEND_KEY="+t.opts.APIKey, "AGENTSTATS_SEND_DEBUG="+boolEnv(t.opts.Debug))
	if t.opts.Debug {
		cmd.Stderr = os.Stderr
	}
	detach(cmd)
	if err := cmd.Start(); err != nil {
		os.Remove(lock)
		return err
	}
	return cmd.Process.Release()
}

// runSender is the background run: takes the spool by renaming it, posts it in batches, and
// puts lines back for the next run only when the network fails.
func runSender(spool string) int {
	endpoint, key := os.Getenv("AGENTSTATS_SEND_ENDPOINT"), os.Getenv("AGENTSTATS_SEND_KEY")
	debug := os.Getenv("AGENTSTATS_SEND_DEBUG") == "1"
	logf := func(f string, a ...any) {
		if debug {
			fmt.Fprintf(os.Stderr, "[agentstats sender] "+f+"\n", a...)
		}
	}
	defer os.Remove(spool + ".lock")
	pending := spool + ".catalog.json"
	readCatalog := func() *catalogBody {
		b, err := os.ReadFile(pending)
		if err != nil {
			return nil
		}
		var c catalogBody
		if json.Unmarshal(b, &c) != nil {
			return nil
		}
		return &c
	}
	client := &http.Client{Timeout: 10 * time.Second}
	for round := 0; round < 5; round++ {
		taken := fmt.Sprintf("%s.%d.sending", spool, os.Getpid())
		// Spool lines are commands, or feedback (type "feedback"), which goes in the batch's feedback list.
		var events, feedback []json.RawMessage
		took := os.Rename(spool, taken) == nil
		if took {
			b, _ := os.ReadFile(taken)
			for _, l := range bytes.Split(b, []byte("\n")) {
				if len(bytes.TrimSpace(l)) == 0 || !json.Valid(l) {
					continue
				}
				var kind struct {
					Type string `json:"type"`
				}
				json.Unmarshal(l, &kind)
				if kind.Type == "feedback" {
					feedback = append(feedback, json.RawMessage(l))
				} else {
					events = append(events, json.RawMessage(l))
				}
			}
		}
		catalog := readCatalog()
		if len(events) == 0 && len(feedback) == 0 && catalog == nil {
			if took {
				os.Remove(taken)
			}
			break
		}
		failed := -1
		for i := 0; (i < len(events) || catalog != nil || len(feedback) > 0) && failed < 0; i += 500 {
			batch := events[min(i, len(events)):min(i+500, len(events))]
			notes := feedback[:min(20, len(feedback))]
			cli := ""
			var head struct {
				CLI string `json:"cli"`
			}
			if len(batch) > 0 {
				json.Unmarshal(batch[0], &head)
				cli = head.CLI
			} else if catalog != nil {
				cli = catalog.CLI
			} else if len(notes) > 0 {
				json.Unmarshal(notes[0], &head)
				cli = head.CLI
			}
			body := map[string]any{"cli": cli, "events": batch}
			if len(notes) > 0 {
				body["feedback"] = notes
			}
			if catalog != nil {
				body["catalog"] = map[string]any{"version": catalog.Version, "commands": catalog.Commands}
			}
			b, _ := json.Marshal(body)
			req, _ := http.NewRequest("POST", endpoint, bytes.NewReader(b))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Authorization", "Bearer "+key)
			req.Header.Set("X-AgentStats-SDK", "cli-go/"+Version)
			res, err := client.Do(req)
			if err != nil {
				logf("failed: %v", err)
				failed = i
				continue
			}
			res.Body.Close()
			logf("sent %d + %d feedback -> %d", len(batch), len(notes), res.StatusCode)
			// 429 is "later", like a 5xx: keep the batch for the next run. A 4xx is final.
			if res.StatusCode >= 500 || res.StatusCode == 429 {
				failed = i
				continue
			}
			feedback = feedback[len(notes):]
			if catalog != nil {
				os.WriteFile(spool+".catalog.sent", []byte(catalog.Sig), 0o644)
				if c := readCatalog(); c != nil && c.Sig == catalog.Sig {
					os.Remove(pending)
				}
				catalog = nil
			}
		}
		if failed >= 0 {
			back := append(append([]json.RawMessage{}, events[min(failed, len(events)):]...), feedback...)
			if len(back) > 0 {
				if f, err := os.OpenFile(spool, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644); err == nil {
					for _, e := range back {
						f.Write(append(e, '\n'))
					}
					f.Close()
				}
			}
		}
		if took {
			os.Remove(taken)
		}
		if failed >= 0 {
			break
		}
	}
	return 0
}

func (t *Tracker) optedOut() bool {
	if truthy(os.Getenv("DO_NOT_TRACK")) || truthy(os.Getenv("AGENTSTATS_DISABLED")) || truthy(os.Getenv(OptOutVariable(t.opts.CLI))) {
		return true
	}
	for _, dir := range t.dirs {
		if exists(filepath.Join(dir, "disabled-"+slug(t.opts.CLI))) {
			return true
		}
	}
	return false
}

func (t *Tracker) showNotice() {
	if t.opts.NoNotice || !isTerminal(os.Stderr) {
		return
	}
	for _, dir := range t.dirs {
		marker := filepath.Join(dir, "notice-"+slug(t.opts.CLI))
		if exists(marker) {
			return
		}
		if os.MkdirAll(dir, 0o755) != nil || os.WriteFile(marker, []byte(time.Now().Format(time.RFC3339)), 0o644) != nil {
			continue
		}
		text := t.opts.Notice
		if text == "" {
			text = fmt.Sprintf("%s collects anonymous usage data: which commands run, how long they take and whether they fail. Never arguments or file contents.\nTurn it off with %s=1 or DO_NOT_TRACK=1.", t.opts.CLI, OptOutVariable(t.opts.CLI))
		}
		fmt.Fprintf(os.Stderr, "\n%s\n\n", text)
		return
	}
}

func (t *Tracker) log(a ...any) {
	if t.opts.Debug {
		fmt.Fprintln(os.Stderr, append([]any{"[agentstats]"}, a...)...)
	}
}

// OptOutVariable is ACME_TELEMETRY_DISABLED for a CLI named "acme".
func OptOutVariable(cli string) string {
	return strings.ReplaceAll(strings.ToUpper(slug(cli)), "-", "_") + "_TELEMETRY_DISABLED"
}

// FlagNames returns flag names from argv: `--env=prod` becomes `--env`, values are skipped,
// `-pSECRET` keeps only `-p`. Stops at `--`.
func FlagNames(argv []string) []string {
	out := []string{}
	add := func(s string) {
		if len(s) > 60 {
			s = s[:60]
		}
		if !slices.Contains(out, s) {
			out = append(out, s)
		}
	}
	for _, a := range argv {
		if a == "--" {
			break
		}
		if strings.HasPrefix(a, "--") && len(a) > 2 {
			if i := strings.Index(a, "="); i >= 0 {
				a = a[:i]
			}
			add(a)
		} else if len(a) >= 2 && a[0] == '-' && isLetter(a[1]) {
			add(a[:2])
		}
	}
	if len(out) > 30 {
		out = out[:30]
	}
	return out
}

var commandWord = regexp.MustCompile(`^[A-Za-z][\w:.-]{0,39}$`)

// SafeCommandName keeps "db push"; "postgres://user:secret@host" or a pasted sentence becomes "(invalid)".
func SafeCommandName(name string) string {
	words := strings.Fields(name)
	if len(words) == 0 || len(words) > 4 {
		return "(invalid)"
	}
	for _, w := range words {
		if !commandWord.MatchString(w) {
			return "(invalid)"
		}
	}
	return strings.Join(words, " ")
}

// NormalizeCatalog trims a command list to what the server stores, dropping empty fields and duplicates.
func NormalizeCatalog(list []CommandSpec) []CommandSpec {
	cut := func(s string, n int) string {
		s = strings.TrimSpace(s)
		if len(s) > n {
			return s[:n]
		}
		return s
	}
	seen := map[string]bool{}
	out := []CommandSpec{}
	for _, c := range list {
		c.Name = cut(c.Name, 200)
		if c.Name == "" || seen[c.Name] {
			continue
		}
		seen[c.Name] = true
		c.Description = cut(c.Description, 2000)
		aliases := []string{}
		for _, a := range c.Aliases {
			if a = cut(a, 60); a != "" && len(aliases) < 10 {
				aliases = append(aliases, a)
			}
		}
		c.Aliases = aliases
		if len(c.Arguments) > 30 {
			c.Arguments = c.Arguments[:30]
		}
		if len(c.Options) > 60 {
			c.Options = c.Options[:60]
		}
		for i := range c.Arguments {
			c.Arguments[i].Description = cut(c.Arguments[i].Description, 1000)
		}
		for i := range c.Options {
			c.Options[i].Description = cut(c.Options[i].Description, 1000)
		}
		out = append(out, c)
		if len(out) >= 500 {
			break
		}
	}
	return out
}

// LeadingWords are the words typed before the first flag, stopping at `--`: candidates for a command path.
func LeadingWords(argv []string) []string {
	out := []string{}
	for _, a := range argv {
		if a == "--" || strings.HasPrefix(a, "-") {
			break
		}
		out = append(out, a)
	}
	return out
}

func defaultDir() string {
	base := os.Getenv("XDG_CACHE_HOME")
	if base == "" && runtime.GOOS == "windows" {
		base = os.Getenv("LOCALAPPDATA")
	}
	if base == "" {
		home, _ := os.UserHomeDir()
		base = filepath.Join(home, ".cache")
	}
	return filepath.Join(base, "agentstats")
}

// Node's process.platform / process.arch names, so the dashboard groups runs the same way.
func goosName() string {
	if runtime.GOOS == "windows" {
		return "win32"
	}
	return runtime.GOOS
}

func goarchName() string {
	switch runtime.GOARCH {
	case "amd64":
		return "x64"
	case "386":
		return "ia32"
	}
	return runtime.GOARCH
}

// isTerminal: a character device that isn't /dev/null (where stdin points when nothing is attached).
func isTerminal(f *os.File) bool {
	st, err := f.Stat()
	if err != nil || st.Mode()&os.ModeCharDevice == 0 {
		return false
	}
	null, err := os.Stat(os.DevNull)
	return err != nil || !os.SameFile(st, null)
}

func hash(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])[:16]
}

var nonSlug = regexp.MustCompile(`[^a-z0-9]+`)

func slug(s string) string {
	out := strings.Trim(nonSlug.ReplaceAllString(strings.ToLower(s), "-"), "-")
	if out == "" {
		return "cli"
	}
	return out
}

func truthy(v string) bool {
	return v != "" && v != "0" && !strings.EqualFold(v, "false")
}

func exists(p string) bool {
	_, err := os.Stat(p)
	return !errors.Is(err, os.ErrNotExist) && err == nil
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func strPtr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func or(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func boolEnv(b bool) string {
	if b {
		return "1"
	}
	return ""
}

func isLetter(c byte) bool { return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') }
