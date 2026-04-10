// Package main provides the ci-snitch CLI entrypoint.
package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

var version = "dev"

func newRootCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "ci-snitch",
		Short: "Analyze GitHub Actions CI workflow performance",
		Long:  "ci-snitch hunts for anomalies and performance trends in your CI pipelines.",
	}

	cmd.AddCommand(newVersionCmd())
	cmd.AddCommand(newAnalyzeCmd())

	return cmd
}

func newVersionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the version",
		Run: func(cmd *cobra.Command, _ []string) {
			_, _ = fmt.Fprintln(cmd.OutOrStdout(), version)
		},
	}
}

func main() {
	if err := newRootCmd().Execute(); err != nil {
		os.Exit(1)
	}
}
