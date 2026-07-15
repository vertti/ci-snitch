// Package main provides the ci-snitch CLI entrypoint.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"
)

var version = "dev"

func newRootCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "ci-snitch",
		Short:   "Analyze GitHub Actions CI workflow performance",
		Long:    "ci-snitch hunts for anomalies and performance trends in your CI pipelines.",
		Version: version,
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
	// Ctrl+C / SIGTERM cancel the command context so a partially hydrated
	// run aborts with an error instead of printing a truncated analysis.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	err := newRootCmd().ExecuteContext(ctx)
	stop()
	if err != nil {
		os.Exit(1)
	}
}
