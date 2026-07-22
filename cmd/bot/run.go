package main

import (
	"fmt"
	"strconv"

	"github.com/spf13/cobra"
	"go.uber.org/zap"

	"gitlab.com/ebuildy/gitlab-ci-resources-bot/internal/config"
	"gitlab.com/ebuildy/gitlab-ci-resources-bot/internal/report"
)

func newRunCmd() *cobra.Command {
	var projectID int64
	cmd := &cobra.Command{
		Use:   "run <pipeline-id>",
		Short: "Print the resource report for one pipeline to stdout",
		Long: "Builds the same report the webhook path posts as an MR comment " +
			"and prints it to stdout. JSON logs are also written to stdout; " +
			"use --log-level error to keep the output to just the report.",
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
			log := logger
			rep, err := newReporter(cfg, log)
			if err != nil {
				return err
			}

			log.Debug("building report",
				zap.Int64("project_id", projectID),
				zap.Int64("pipeline_id", pipelineID))
			data, err := rep.Build(cmd.Context(), projectID, pipelineID)
			if err != nil {
				return err
			}
			log.Info("report built",
				zap.Int64("pipeline_id", pipelineID),
				zap.Int("jobs", len(data.Jobs)))
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
