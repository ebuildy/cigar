package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/spf13/cobra"

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
	if cfg.WebhookSecret == "" {
		return errors.New("missing required environment variable WEBHOOK_SECRET")
	}
	log, err := newLogger(cfg)
	if err != nil {
		return err
	}
	rep, err := newReporter(cfg, log)
	if err != nil {
		return err
	}

	q := make(queue, 128)
	go worker(ctx, q, rep, log)

	app := webhook.NewApp(cfg.WebhookSecret, q, log)

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
		log.Info("webhook server listening", "addr", cfg.ListenAddr)
		errCh <- app.Listen(cfg.ListenAddr, listenCfg)
	}()
	go func() {
		log.Info("ops server listening", "addr", cfg.OpsAddr)
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
func worker(ctx context.Context, q queue, rep *reporter.Reporter, log *slog.Logger) {
	for {
		select {
		case <-ctx.Done():
			return
		case ev := <-q:
			process(ctx, rep, ev, log)
		}
	}
}

func process(ctx context.Context, rep *reporter.Reporter, ev webhook.PipelineEvent, log *slog.Logger) {
	ctx, cancel := context.WithTimeout(ctx, processTimeout)
	defer cancel()

	// The handler only enqueues events with a merge request attached.
	mrIID := ev.MergeRequest.IID
	log = log.With("pipeline_id", ev.ObjectAttributes.ID, "project_id", ev.Project.ID, "mr_iid", mrIID)

	if err := rep.ProcessPipeline(ctx, ev.Project.ID, ev.ObjectAttributes.ID, mrIID, ev.ObjectAttributes.Status); err != nil {
		log.Error("process pipeline failed", "err", err)
		return
	}
	log.Info("report posted")
}
