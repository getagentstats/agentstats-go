// A cobra CLI with AgentStats wired in, for the tests.
package main

import (
	"errors"
	"os"

	"agentstats.io/go/cli"
	"agentstats.io/go/cli/cobracli"
	"github.com/spf13/cobra"
)

type exitError struct {
	error
	code int
}

func (e exitError) ExitCode() int { return e.code }

func main() {
	stats := cli.New(cli.Options{CLI: "shipit", Version: "1.0.0", NoNotice: true})

	root := &cobra.Command{Use: "shipit", Short: "Deploy and run apps", SilenceUsage: true, Version: "1.0.0"}
	var env string
	var prod bool
	deploy := &cobra.Command{
		Use:   "deploy [dir]",
		Short: "Deploy the current directory",
		RunE: func(cmd *cobra.Command, args []string) error {
			if env == "staging" {
				return exitError{errors.New("Staging is locked"), 3}
			}
			return nil
		},
	}
	deploy.Flags().StringVarP(&env, "env", "e", "", "Environment to deploy to")
	deploy.Flags().BoolVar(&prod, "prod", false, "Deploy to production")
	status := &cobra.Command{Use: "status", Aliases: []string{"st"}, Short: "Show the latest deployment", Run: func(*cobra.Command, []string) {}}
	db := &cobra.Command{Use: "db", Short: "Database commands"}
	migrate := &cobra.Command{Use: "migrate", Short: "Run pending migrations", Run: func(*cobra.Command, []string) {}}
	migrate.Flags().String("url", "", "Database URL")
	migrate.MarkFlagRequired("url")
	seed := &cobra.Command{Use: "seed", Short: "Fill the database", RunE: func(*cobra.Command, []string) error { return errors.New("seed file missing") }}
	db.AddCommand(migrate, seed)
	root.AddCommand(deploy, status, db, &cobra.Command{Use: "internal-debug", Hidden: true, Run: func(*cobra.Command, []string) {}})

	err := cobracli.Execute(stats, root)
	var ec interface{ ExitCode() int }
	if errors.As(err, &ec) {
		os.Exit(ec.ExitCode())
	}
	if err != nil {
		os.Exit(1)
	}
}
