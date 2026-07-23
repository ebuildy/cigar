package main

import (
	"fmt"
	"strconv"

	"github.com/spf13/cobra"
	"go.uber.org/zap"

	"gitlab.com/ebuildy/gitlab-ci-resources-bot/internal/chart"
	"gitlab.com/ebuildy/gitlab-ci-resources-bot/internal/config"
	"gitlab.com/ebuildy/gitlab-ci-resources-bot/internal/gitlab"
	"gitlab.com/ebuildy/gitlab-ci-resources-bot/internal/metrics"
	"gitlab.com/ebuildy/gitlab-ci-resources-bot/internal/report"
	"gitlab.com/ebuildy/gitlab-ci-resources-bot/internal/reporter"
)

func newRunCmd() *cobra.Command {
	var projectID int64
	var job string
	cmd := &cobra.Command{
		Use:   "run <pipeline-id>",
		Short: "Print the resource report for one pipeline to stdout",
		Long: "Builds the same report the webhook path posts as an MR comment " +
			"and prints it to stdout. With --job, prints the usage details for a " +
			"single job (by ID or name) instead, including CPU/memory/network " +
			"charts as inline markdown. JSON logs are also written to stdout; " +
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

			if job != "" {
				return printJobDetails(cmd, rep, cfg, projectID, pipelineID, job)
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
	cmd.Flags().StringVar(&job, "job", "", "Print usage details for a single job (by numeric ID or name) instead of the whole pipeline")
	_ = cmd.MarkFlagRequired("project")
	return cmd
}

// printJobDetails resolves one job of the pipeline (by ID or name) and prints
// its usage details to stdout: the report's numeric usage block plus inline
// markdown CPU/memory/network charts over the job's run window.
func printJobDetails(cmd *cobra.Command, rep *reporter.Reporter, cfg *config.Config, projectID, pipelineID int64, sel string) error {
	ctx := cmd.Context()
	out := cmd.OutOrStdout()

	jobs, err := rep.GitLab.PipelineJobs(ctx, projectID, pipelineID)
	if err != nil {
		return err
	}
	j, ok := findJob(jobs, sel)
	if !ok {
		return fmt.Errorf("no job matching %q in pipeline %d", sel, pipelineID)
	}

	var (
		usage *metrics.JobUsage
		pod   string
	)
	if !j.StartedAt.IsZero() && !j.FinishedAt.IsZero() {
		p, found, err := rep.Resolver.PodForJob(ctx, projectID, j.ID, j.StartedAt, j.FinishedAt)
		if err != nil {
			return fmt.Errorf("correlate pod for job %d: %w", j.ID, err)
		}
		if found {
			pod = p
			if usage, err = rep.Metrics.PodUsage(ctx, pod, j.StartedAt, j.FinishedAt); err != nil {
				return fmt.Errorf("query usage for pod %q: %w", pod, err)
			}
		}
	}

	// Numeric usage via the shared report renderer (one-job report). A nil usage
	// makes Render emit its "no resource data" notice rather than fake zeros.
	data := report.Data{
		PipelineID:        pipelineID,
		Status:            "job: " + j.Name,
		Jobs:              []report.JobReport{{Stage: j.Stage, Name: j.Name, Usage: usage}},
		ThrottleWarnRatio: cfg.ThrottleWarnRatio,
		RanJobs:           1,
	}
	body, err := report.Render(data)
	if err != nil {
		return fmt.Errorf("render report: %w", err)
	}
	if _, err := fmt.Fprintln(out, body); err != nil {
		return err
	}

	// Time-series charts (markdown, ideal for a terminal) when a pod correlated
	// and the source supports range queries.
	if pod == "" {
		return nil
	}
	ss, ok := rep.Metrics.(metrics.SeriesSource)
	if !ok {
		return nil
	}
	series, err := ss.PodSeries(ctx, pod, j.StartedAt, j.FinishedAt)
	if err != nil {
		return fmt.Errorf("query series for pod %q: %w", pod, err)
	}
	if series.Empty() {
		return nil
	}
	for _, c := range []struct {
		title string
		unit  chart.Unit
		lines []chart.Series
	}{
		{"CPU (cores)", chart.UnitNone, []chart.Series{lineToSeries(series.CPU)}},
		{"Memory (bytes)", chart.UnitBytes, []chart.Series{lineToSeries(series.Memory)}},
		{"Network (bytes/s)", chart.UnitBytesPerSec, []chart.Series{lineToSeries(series.NetRx), lineToSeries(series.NetTx)}},
	} {
		md, err := chart.Render(chart.Markdown, c.title, c.unit, c.lines)
		if err != nil {
			return fmt.Errorf("render %s chart: %w", c.title, err)
		}
		if _, err := out.Write(md); err != nil {
			return err
		}
		if _, err := fmt.Fprintln(out); err != nil {
			return err
		}
	}
	return nil
}

// findJob matches a job by numeric ID first (when sel parses as an int), then by
// exact name.
func findJob(jobs []gitlab.Job, sel string) (gitlab.Job, bool) {
	if id, err := strconv.ParseInt(sel, 10, 64); err == nil {
		for _, j := range jobs {
			if j.ID == id {
				return j, true
			}
		}
	}
	for _, j := range jobs {
		if j.Name == sel {
			return j, true
		}
	}
	return gitlab.Job{}, false
}

func lineToSeries(l metrics.Line) chart.Series {
	pts := make([]chart.Point, len(l.Points))
	for i, p := range l.Points {
		pts[i] = chart.Point{X: p.T, Y: p.V}
	}
	return chart.Series{Label: l.Label, Points: pts}
}
