package main

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/spf13/cobra"
	"go.uber.org/zap"

	"gitlab.com/ebuildy/gitlab-ci-resources-bot/internal/config"
	"gitlab.com/ebuildy/gitlab-ci-resources-bot/internal/reporter"
	"gitlab.com/ebuildy/gitlab-ci-resources-bot/internal/webhook"
)

// processTimeout bounds one pipeline's report build + MR note upsert.
const processTimeout = 2 * time.Minute

func newServeCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "serve",
		Short: "Run the webhook server",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return serve(cmd.Context())
		},
	}
}

// queue is a bounded in-memory queue between the webhook handler and the
// worker; Enqueue never blocks.
type queue chan webhook.PipelineEvent

func (q queue) Enqueue(ev webhook.PipelineEvent) bool {
	select {
	case q <- ev:
		return true
	default:
		return false
	}
}

func serve(ctx context.Context) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	log := logger
	log.Debug("configuration loaded",
		zap.String("gitlab_url", cfg.GitLabURL),
		zap.String("prometheus_url", cfg.PrometheusURL),
		zap.Strings("auth_methods", cfg.AuthMethods),
		zap.Float64("throttle_warn_ratio", cfg.ThrottleWarnRatio),
		zap.Duration("scrape_interval", cfg.ScrapeInterval))
	rep, err := newReporter(cfg, log)
	if err != nil {
		return err
	}

	q := make(queue, 128)
	go worker(ctx, q, rep, log)
	log.Debug("worker started")

	auths, err := buildAuthenticators(cfg)
	if err != nil {
		return err
	}
	app := webhook.NewApp(auths, q, log)

	ops := fiber.New(fiber.Config{ReadTimeout: 5 * time.Second})
	ops.Get("/healthz", func(c fiber.Ctx) error { return c.SendStatus(fiber.StatusOK) })
	ops.Get("/readyz", func(c fiber.Ctx) error { return c.SendStatus(fiber.StatusOK) })
	// TODO: promhttp handler on /metrics once own metrics are added.

	listenCfg := fiber.ListenConfig{
		DisableStartupMessage: true,
		GracefulContext:       ctx,
		ShutdownTimeout:       15 * time.Second,
	}

	errCh := make(chan error, 2)
	go func() {
		log.Info("webhook server listening", zap.String("addr", cfg.ListenAddr))
		errCh <- app.Listen(cfg.ListenAddr, listenCfg)
	}()
	go func() {
		log.Info("ops server listening", zap.String("addr", cfg.OpsAddr))
		errCh <- ops.Listen(cfg.OpsAddr, listenCfg)
	}()

	// Listen returns after graceful shutdown (GracefulContext); collect both.
	for range 2 {
		if err := <-errCh; err != nil {
			return fmt.Errorf("server error: %w", err)
		}
	}
	log.Info("shut down cleanly")
	return nil
}

// worker consumes validated pipeline events and posts MR comments.
func worker(ctx context.Context, q queue, rep *reporter.Reporter, log *zap.Logger) {
	for {
		select {
		case <-ctx.Done():
			log.Debug("worker stopping", zap.Error(ctx.Err()))
			return
		case ev := <-q:
			process(ctx, rep, ev, log)
		}
	}
}

func process(ctx context.Context, rep *reporter.Reporter, ev webhook.PipelineEvent, log *zap.Logger) {
	ctx, cancel := context.WithTimeout(ctx, processTimeout)
	defer cancel()

	// merge_request may be absent (branch pushed before the MR was created);
	// ProcessPipeline resolves the MR from the pipeline's branch ref.
	var mrIID int64
	if ev.MergeRequest != nil {
		mrIID = ev.MergeRequest.IID
	}
	ref := ev.ObjectAttributes.Ref
	log = log.With(
		zap.Int64("pipeline_id", ev.ObjectAttributes.ID),
		zap.Int64("project_id", ev.Project.ID),
		zap.Int64("mr_iid", mrIID),
		zap.String("ref", ref),
	)
	log.Debug("processing pipeline event", zap.String("status", ev.ObjectAttributes.Status))

	posted, err := rep.ProcessPipeline(ctx, ev.Project.ID, ev.ObjectAttributes.ID, mrIID, ref, ev.ObjectAttributes.Status)
	if err != nil {
		log.Error("process pipeline failed", zap.Error(err))
		return
	}
	if !posted {
		log.Info("no open merge request for pipeline yet, nothing posted")
		return
	}
	log.Info("report posted")
}

// buildAuthenticators turns the ordered cfg.AuthMethods into webhook
// authenticators, failing fast when an enabled method's credential is absent
// or malformed, or when no method is configured at all.
func buildAuthenticators(cfg *config.Config) ([]webhook.Authenticator, error) {
	var auths []webhook.Authenticator
	for _, m := range cfg.AuthMethods {
		switch m {
		case "secret":
			if cfg.WebhookSecret == "" {
				return nil, errors.New(`AUTH_METHODS includes "secret" but WEBHOOK_SECRET is not set`)
			}
			auths = append(auths, webhook.NewSecretAuth(cfg.WebhookSecret))
		case "signature":
			if cfg.WebhookSigningToken == "" {
				return nil, errors.New(`AUTH_METHODS includes "signature" but WEBHOOK_SIGNING_TOKEN is not set`)
			}
			a, err := webhook.NewSignatureAuth(cfg.WebhookSigningToken, webhook.DefaultTimestampTolerance)
			if err != nil {
				return nil, fmt.Errorf("signature auth: %w", err)
			}
			auths = append(auths, a)
		default:
			return nil, fmt.Errorf("unknown auth method %q", m)
		}
	}
	if len(auths) == 0 {
		return nil, errors.New("no authentication method configured")
	}
	return auths, nil
}
