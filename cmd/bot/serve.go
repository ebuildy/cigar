package main

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/gofiber/fiber/v3/middleware/adaptor"
	"github.com/spf13/cobra"
	"go.uber.org/zap"

	"gitlab.com/ebuildy/gitlab-ci-resources-bot/internal/command"
	"gitlab.com/ebuildy/gitlab-ci-resources-bot/internal/config"
	"gitlab.com/ebuildy/gitlab-ci-resources-bot/internal/reporter"
	"gitlab.com/ebuildy/gitlab-ci-resources-bot/internal/telemetry"
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

const (
	// queueSize is the worker queue's buffer: events beyond it are dropped.
	queueSize = 128
	// queueWarnPercent is the fill level at which Enqueue warns the operator
	// that the worker is falling behind and drops are close.
	queueWarnPercent = 80
)

// queue is a bounded in-memory queue between the webhook handler and the
// worker; Enqueue never blocks.
type queue struct {
	ch     chan webhook.Event
	log    *zap.Logger
	warnAt int // depth at which the queue counts as nearly full

	// warned is set while a "nearly full" warning is outstanding, so a queue
	// hovering above the threshold logs once per crossing instead of once per
	// event. Racing enqueues may cost an extra line; that is cheaper than a
	// lock on the hot path.
	warned atomic.Bool
}

func newQueue(size int, log *zap.Logger) *queue {
	return &queue{
		ch:  make(chan webhook.Event, size),
		log: log,
		// Round up, so warnAt is the first depth at or above the percentage.
		warnAt: (size*queueWarnPercent + 99) / 100,
	}
}

// Enqueue hands an event to the worker, reporting false when the buffer is
// full (the caller drops the event). It never blocks.
func (q *queue) Enqueue(ev webhook.Event) bool {
	select {
	case q.ch <- ev:
		depth := len(q.ch)
		switch {
		case depth >= q.warnAt:
			if q.warned.CompareAndSwap(false, true) {
				q.log.Warn("worker queue nearly full",
					zap.Int("depth", depth),
					zap.Int("capacity", cap(q.ch)),
					zap.Int("warn_at", q.warnAt))
			}
		default:
			q.warned.Store(false)
		}
		return true
	default:
		return false
	}
}

func serve(ctx context.Context) error {
	cfg, err := config.Load(cfgViper)
	if err != nil {
		return err
	}
	// Wrap the root logger before anything else uses it, so every warn-or-worse
	// entry from here down — including those of the loggers derived below — is
	// counted into cigar_log_total.
	m := telemetry.New()
	log := logger.WithOptions(m.LogOption())
	zap.ReplaceGlobals(log)

	log.Debug("configuration loaded",
		zap.String("gitlab_url", cfg.GitLabURL),
		zap.String("prometheus_url", cfg.PrometheusURL),
		zap.Strings("auth_methods", cfg.AuthMethods),
		zap.Float64("throttle_warn_ratio", cfg.ThrottleWarnRatio),
		zap.Duration("scrape_interval", cfg.ScrapeInterval))

	rep, err := newReporter(cfg, log, m)
	if err != nil {
		return err
	}

	if cfg.CommandsEnabled && cfg.CommandsSigningKey == "" {
		return errors.New("COMMANDS_ENABLED is true but COMMANDS_SIGNING_KEY is not set")
	}
	var cmdHandler *command.Handler
	if cfg.CommandsEnabled {
		cmdHandler, err = newCommandHandler(ctx, cfg, log, rep, m)
		if err != nil {
			return err
		}
		log.Info("interactive commands enabled")
	}

	q := newQueue(queueSize, log.Named("queue"))
	go worker(ctx, q, rep, cmdHandler, log.Named("worker"))
	log.Debug("worker started")

	auths, err := buildAuthenticators(cfg)
	if err != nil {
		return err
	}
	app := webhook.NewApp(auths, q, log.Named("webhook"), cfg.CommandsEnabled, m)

	ops := fiber.New(fiber.Config{ReadTimeout: 5 * time.Second})
	ops.Get("/healthz", func(c fiber.Ctx) error { return c.SendStatus(fiber.StatusOK) })
	ops.Get("/readyz", func(c fiber.Ctx) error { return c.SendStatus(fiber.StatusOK) })
	ops.Get("/metrics", adaptor.HTTPHandler(m.Handler()))

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

// worker consumes validated pipeline and note events, posting MR comments and
// handling interactive commands respectively.
func worker(ctx context.Context, q *queue, rep *reporter.Reporter, cmd *command.Handler, log *zap.Logger) {
	seen := make(map[int64]bool) // note IDs already handled (dedup retried deliveries)
	for {
		select {
		case <-ctx.Done():
			log.Debug("worker stopping", zap.Error(ctx.Err()))
			return
		case ev := <-q.ch:
			switch {
			case ev.Pipeline != nil:
				process(ctx, rep, *ev.Pipeline, log)
			case ev.Note != nil && cmd != nil:
				processNote(ctx, cmd, seen, *ev.Note, log)
			}
		}
	}
}

func processNote(ctx context.Context, h *command.Handler, seen map[int64]bool, ev command.NoteEvent, log *zap.Logger) {
	if seen[ev.NoteID] {
		log.Debug("duplicate note delivery ignored", zap.Int64("note_id", ev.NoteID))
		return
	}
	seen[ev.NoteID] = true
	ctx, cancel := context.WithTimeout(ctx, processTimeout)
	defer cancel()
	if err := h.Handle(ctx, ev); err != nil {
		log.Error("handle command note failed", zap.Int64("note_id", ev.NoteID), zap.Error(err))
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
