package main

import (
	"fmt"
	"strconv"

	"github.com/spf13/cobra"

	"gitlab.com/ebuildy/gitlab-ci-resources-bot/internal/config"
	"gitlab.com/ebuildy/gitlab-ci-resources-bot/internal/report"
)

func newRunCmd() *cobra.Command {
	var projectID int64
	cmd := &cobra.Command{
		Use:   "run <pipeline-id>",
		Short: "Print the resource report for one pipeline to stdout",
		Long: "Builds the same report the webhook path posts as an MR comment " +
			"and prints it to stdout (logs go to stderr).",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			pipelineID, err := strconv.ParseInt(args[0], 10, 64)
			if err != nil {
				return fmt.Errorf("invalid pipeline ID %q", args[0])
			}

			cfg, err := config.Load()
			if err != nil {
				return err
			}
			log, err := newLogger(cfg)
			if err != nil {
				return err
			}
			rep, err := newReporter(cfg, log)
			if err != nil {
				return err
			}

			data, err := rep.Build(cmd.Context(), projectID, pipelineID)
			if err != nil {
				return err
			}
			body, err := report.Render(data)
			if err != nil {
				return fmt.Errorf("render report: %w", err)
			}
			_, err = fmt.Fprintln(cmd.OutOrStdout(), body)
			return err
		},
	}
	cmd.Flags().Int64Var(&projectID, "project", 0, "GitLab project ID the pipeline belongs to (required)")
	_ = cmd.MarkFlagRequired("project")
	return cmd
}
