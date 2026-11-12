// Package cobracli records cobra commands with AgentStats: one call in place of root.Execute().
//
//	func main() {
//		stats := cli.New(cli.Options{CLI: "acme", Version: version}) // first thing in main
//		if err := cobracli.Execute(stats, rootCmd); err != nil {
//			os.Exit(1)
//		}
//	}
//
// It records the command that ran ("db migrate"), help (--help, `help <command>`, a group
// without a Run), unknown commands, flag errors and the error a RunE returned, and sends the
// command list. cobra's own output and behaviour don't change. It also adds the feedback command
// (cli.Options.NoFeedback turns it off) unless root has a command of that name.
package cobracli

import (
	"errors"
	"os"
	"regexp"
	"strings"

	"agentstats.io/go/cli"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// exitCoder is the common pattern for an error that carries its own exit code.
type exitCoder interface{ ExitCode() int }

var unknownCommand = regexp.MustCompile(`^unknown command "([^"]+)" for "([^"]+)"`)

// Execute runs root.ExecuteC() and records the run, then returns cobra's error. Exit with a
// non-zero code when it isn't nil; the run is recorded with exit code 1, or the error's own
// ExitCode() when it has one.
func Execute(stats *cli.Tracker, root *cobra.Command) error {
	if !stats.Enabled() {
		return root.Execute()
	}
	addFeedback(stats, root)
	stats.Commands(Commands(root, stats.CLI()))
	cmd, err := root.ExecuteC()
	record(stats, root, cmd, err)
	code := 0
	if err != nil {
		code = 1
		var ec exitCoder
		if errors.As(err, &ec) {
			code = ec.ExitCode()
		}
	}
	stats.Finish(code)
	return err
}

func record(stats *cli.Tracker, root, cmd *cobra.Command, err error) {
	if err != nil {
		if m := unknownCommand.FindStringSubmatch(err.Error()); m != nil {
			parent := strings.TrimSpace(strings.TrimPrefix(m[2], root.Name()))
			// Not the message: it repeats what was typed, which could be a value.
			stats.UnknownCommand(strings.TrimSpace(parent + " " + m[1]))
			return
		}
	}
	if cmd == nil {
		cmd = root
	}
	switch {
	case cmd.Name() == "help" && cmd.Parent() == root:
		// `help db migrate`: the command named after it.
		target, _, ferr := root.Find(cli.LeadingWords(os.Args[1:])[1:])
		if ferr != nil || target == nil {
			target = root
		}
		stats.Help(Path(target))
	case flagSet(cmd, "help"):
		stats.Help(Path(cmd))
	case !cmd.Runnable() && err == nil:
		// cobra shows a group's help for `db push` too, when push doesn't exist: that's an unknown command.
		if extra := unknownBelow(cmd); extra != "" {
			stats.UnknownCommand(strings.TrimSpace(Path(cmd) + " " + extra))
		} else {
			stats.Help(Path(cmd))
		}
	default:
		stats.Command(Path(cmd))
		stats.Error(err)
	}
}

// feedbackAnnotation marks the feedback command, so the command list leaves it out.
const feedbackAnnotation = "agentstats-feedback"

// addFeedback adds the feedback command to root, unless it's off or root has a command (or alias) of that name.
func addFeedback(stats *cli.Tracker, root *cobra.Command) {
	name := stats.FeedbackName()
	if name == "" {
		return
	}
	for _, c := range root.Commands() {
		if c.Name() == name || c.HasAlias(name) {
			return
		}
	}
	var command, from string
	cmd := &cobra.Command{
		Use:         name + " [message...]",
		Short:       cli.FeedbackSummary,
		Long:        cli.FeedbackDescription,
		Annotations: map[string]string{feedbackAnnotation: "true"},
		Run: func(_ *cobra.Command, args []string) {
			stats.RunFeedbackWith(cli.FeedbackArgs{Message: strings.Join(args, " "), Command: command, From: from})
		},
	}
	cmd.Flags().StringVar(&command, "command", "", cli.FeedbackHelp.Command)
	cmd.Flags().StringVar(&from, "from", "", cli.FeedbackHelp.From)
	root.AddCommand(cmd)
	stats.MarkFeedbackCommand()
}

// unknownBelow is the word typed after a group's path, if any: `shipit db push` → "push".
func unknownBelow(cmd *cobra.Command) string {
	words := cli.LeadingWords(os.Args[1:])
	depth := len(strings.Fields(Path(cmd)))
	if len(words) > depth {
		return words[depth]
	}
	return ""
}

func flagSet(cmd *cobra.Command, name string) bool {
	f := cmd.Flags().Lookup(name)
	return f != nil && f.Changed
}

// Path is the command as typed after the CLI's name: "db migrate"; "" for the root.
func Path(cmd *cobra.Command) string {
	parts := []string{}
	for c := cmd; c != nil && c.HasParent(); c = c.Parent() {
		parts = append([]string{c.Name()}, parts...)
	}
	return strings.Join(parts, " ")
}

var positional = regexp.MustCompile(`([<\[])([^>\]]+)[>\]]`)

// Commands is the command list: every runnable command (groups, help, completion and hidden ones left out).
func Commands(root *cobra.Command, cliName string) []cli.CommandSpec {
	out := []cli.CommandSpec{}
	var walk func(c *cobra.Command)
	describe := func(c *cobra.Command) cli.CommandSpec {
		name := Path(c)
		if name == "" {
			name = cliName
		}
		spec := cli.CommandSpec{Name: name, Description: c.Short, Aliases: c.Aliases}
		if spec.Description == "" {
			spec.Description = c.Long
		}
		use := strings.TrimSpace(strings.TrimPrefix(c.Use, c.Name()))
		for _, m := range positional.FindAllStringSubmatch(use, -1) {
			n := m[2]
			variadic := strings.HasSuffix(n, "...")
			spec.Arguments = append(spec.Arguments, cli.ArgSpec{Name: strings.TrimSuffix(n, "..."), Required: m[1] == "<", Variadic: variadic})
		}
		c.NonInheritedFlags().VisitAll(func(f *pflag.Flag) {
			if f.Hidden || f.Name == "help" {
				return
			}
			o := cli.OptSpec{Name: "--" + f.Name, Description: f.Usage, TakesValue: f.Value.Type() != "bool"}
			if f.Shorthand != "" {
				o.Short = "-" + f.Shorthand
			}
			if v := f.Annotations[cobra.BashCompOneRequiredFlag]; len(v) > 0 && v[0] == "true" {
				o.Required = true
			}
			spec.Options = append(spec.Options, o)
		})
		return spec
	}
	walk = func(c *cobra.Command) {
		for _, sub := range c.Commands() {
			if sub.Hidden || sub.Annotations[feedbackAnnotation] != "" || (c == root && (sub.Name() == "help" || sub.Name() == "completion")) {
				continue
			}
			if sub.Runnable() {
				out = append(out, describe(sub))
			}
			walk(sub)
		}
	}
	if root.Runnable() {
		out = append(out, describe(root))
	}
	walk(root)
	return out
}
