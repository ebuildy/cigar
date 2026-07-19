package main

import (
	"fmt"
	"log/slog"
	"os"

	"gitlab.com/ebuildy/gitlab-ci-resources-bot/internal/config"
	"gitlab.com/ebuildy/gitlab-ci-resources-bot/internal/correlate"
	"gitlab.com/ebuildy/gitlab-ci-resources-bot/internal/gitlab"
	"gitlab.com/ebuildy/gitlab-ci-resources-bot/internal/metrics"
	"gitlab.com/ebuildy/gitlab-ci-resources-bot/internal/reporter"
)

// newLogger builds the JSON logger on stderr (stdout stays clean for report
// output) and installs it as the slog default.
func newLogger(cfg *config.Config) (*slog.Logger, error) {
	var level slog.Level
	if err := level.UnmarshalText([]byte(cfg.LogLevel)); err != nil {
		return nil, fmt.Errorf("invalid LOG_LEVEL: %w", err)
	}
	log := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
	slog.SetDefault(log)
	return log, nil
}

// newReporter wires the concrete GitLab and Prometheus clients into the
// reporter shared by `serve` and `run`.
func newReporter(cfg *config.Config, log *slog.Logger) (*reporter.Reporter, error) {
	gl, err := gitlab.New(cfg.GitLabURL, cfg.GitLabToken)
	if err != nil {
		return nil, err
	}
	return &reporter.Reporter{
		GitLab:            gl,
		Resolver:          correlate.NewPromResolver(cfg.PrometheusURL),
		Metrics:           metrics.NewPromSource(cfg.PrometheusURL),
		ThrottleWarnRatio: cfg.ThrottleWarnRatio,
		Log:               log,
	}, nil
}
