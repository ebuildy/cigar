package main

import (
	"fmt"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"

	"gitlab.com/ebuildy/gitlab-ci-resources-bot/internal/config"
	"gitlab.com/ebuildy/gitlab-ci-resources-bot/internal/correlate"
	"gitlab.com/ebuildy/gitlab-ci-resources-bot/internal/gitlab"
	"gitlab.com/ebuildy/gitlab-ci-resources-bot/internal/metrics"
	"gitlab.com/ebuildy/gitlab-ci-resources-bot/internal/reporter"
)

// newLogger builds the zap JSON logger writing to stdout at the given level
// (debug, info, warn, error). It also installs itself as the zap global so
// any code reaching for zap.L()/zap.S() shares this configuration.
func newLogger(level string) (*zap.Logger, error) {
	var lvl zapcore.Level
	if err := lvl.UnmarshalText([]byte(level)); err != nil {
		return nil, fmt.Errorf("invalid log level %q (want debug, info, warn or error): %w", level, err)
	}
	cfg := zap.NewProductionConfig()
	cfg.Level = zap.NewAtomicLevelAt(lvl)
	cfg.Encoding = "json"
	cfg.OutputPaths = []string{"stdout"}
	cfg.ErrorOutputPaths = []string{"stdout"}
	cfg.EncoderConfig.EncodeTime = zapcore.ISO8601TimeEncoder
	log, err := cfg.Build()
	if err != nil {
		return nil, fmt.Errorf("build logger: %w", err)
	}
	zap.ReplaceGlobals(log)
	return log, nil
}

// newReporter wires the concrete GitLab and Prometheus clients into the
// reporter shared by `serve` and `run`.
func newReporter(cfg *config.Config, log *zap.Logger) (*reporter.Reporter, error) {
	gl, err := gitlab.New(cfg.GitLabURL, cfg.GitLabToken, log)
	if err != nil {
		return nil, err
	}
	resolver, err := correlate.NewPromResolver(cfg.PrometheusURL, cfg.ScrapeInterval, log)
	if err != nil {
		return nil, err
	}
	source, err := metrics.NewPromSource(cfg.PrometheusURL, cfg.ScrapeInterval, log)
	if err != nil {
		return nil, err
	}
	return &reporter.Reporter{
		GitLab:            gl,
		Resolver:          resolver,
		Metrics:           source,
		ThrottleWarnRatio: cfg.ThrottleWarnRatio,
		Log:               log,
	}, nil
}
