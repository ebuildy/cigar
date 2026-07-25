package main

import (
	"context"
	"fmt"
	"strconv"

	"github.com/spf13/cobra"
	"go.uber.org/zap"

	"gitlab.com/ebuildy/gitlab-ci-resources-bot/internal/advice"
	"gitlab.com/ebuildy/gitlab-ci-resources-bot/internal/config"
	"gitlab.com/ebuildy/gitlab-ci-resources-bot/internal/reporter"
)

func newAdviseCmd() *cobra.Command {
	var projectID int64
	var job string
	cmd := &cobra.Command{
		Use:   "advise <pipeline-id>",
		Short: "Print resource-usage recommendations for one pipeline",
		Long: "Runs the same advice rules the `advise` note command uses and prints " +
			"the recommendations to stdout. With --job, only that job is analyzed. " +
			"JSON logs are also written to stdout; use --log-level error to keep the " +
			"output to just the advice.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			pipelineID, err := strconv.ParseInt(args[0], 10, 64)
			if err != nil {
				return fmt.Errorf("invalid pipeline ID %q", args[0])
			}
			cfg, err := config.Load(cfgViper)
			if err != nil {
				return err
			}
			log := logger
			rep, err := newReporter(cfg, log)
			if err != nil {
				return err
			}
			eng, err := newAdviceEngine(cfg)
			if err != nil {
				return err
			}
			log.Debug("building advice",
				zap.Int64("project_id", projectID),
				zap.Int64("pipeline_id", pipelineID),
				zap.String("job", job))
			all, err := rep.Advise(cmd.Context(), projectID, pipelineID, job, eng)
			if err != nil {
				return err
			}
			_, err = fmt.Fprintln(cmd.OutOrStdout(), advice.Render(pipelineID, all))
			return err
		},
	}
	cmd.Flags().Int64Var(&projectID, "project", 0, "GitLab project ID the pipeline belongs to (required)")
	cmd.Flags().StringVar(&job, "job", "", "Only analyze this job (numeric ID or exact name)")
	_ = cmd.MarkFlagRequired("project")
	return cmd
}

// reporterAdvisor adapts a Reporter plus its engine to command.Advisor, so the
// note handler depends on the interface rather than on the reporter.
type reporterAdvisor struct {
	rep *reporter.Reporter
	eng *advice.Engine
}

func (a reporterAdvisor) Advise(ctx context.Context, projectID, pipelineID int64, jobFilter string) ([]advice.Advice, error) {
	return a.rep.Advise(ctx, projectID, pipelineID, jobFilter, a.eng)
}
