package main

import (
	"context"
	"fmt"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"

	"gitlab.com/ebuildy/gitlab-ci-resources-bot/internal/chart"
	"gitlab.com/ebuildy/gitlab-ci-resources-bot/internal/command"
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
	var resolver correlate.Resolver
	switch cfg.PodResolver {
	case "trace":
		resolver = correlate.NewTraceResolver(gl, log)
	case "prometheus":
		resolver, err = correlate.NewPromResolver(cfg.PrometheusURL, cfg.ScrapeInterval, log)
		if err != nil {
			return nil, err
		}
	default:
		return nil, fmt.Errorf("unknown pod resolver %q", cfg.PodResolver)
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
		SigningKey:        []byte(cfg.CommandsSigningKey),
		Log:               log,
	}, nil
}

// newCommandHandler builds the interactive-command handler. It resolves the bot
// user id once (for the author/loop guard) and reuses the Prometheus source's
// range-query capability.
func newCommandHandler(ctx context.Context, cfg *config.Config, log *zap.Logger) (*command.Handler, error) {
	gl, err := gitlab.New(cfg.GitLabURL, cfg.GitLabToken, log)
	if err != nil {
		return nil, err
	}
	botID, err := gl.CurrentUser(ctx)
	if err != nil {
		return nil, fmt.Errorf("resolve bot user: %w", err)
	}
	var resolver correlate.Resolver
	switch cfg.PodResolver {
	case "trace":
		resolver = correlate.NewTraceResolver(gl, log)
	case "prometheus":
		resolver, err = correlate.NewPromResolver(cfg.PrometheusURL, cfg.ScrapeInterval, log)
		if err != nil {
			return nil, err
		}
	default:
		return nil, fmt.Errorf("unknown pod resolver %q", cfg.PodResolver)
	}
	source, err := metrics.NewPromSource(cfg.PrometheusURL, cfg.ScrapeInterval, log)
	if err != nil {
		return nil, err
	}
	format, err := chart.ParseFormat(cfg.ChartFormat)
	if err != nil {
		return nil, err
	}
	return &command.Handler{
		GitLab:      gl,
		Resolver:    resolver,
		Series:      source,
		SigningKey:  []byte(cfg.CommandsSigningKey),
		BotUserID:   botID,
		ChartFormat: format,
		Log:         log,
	}, nil
}
