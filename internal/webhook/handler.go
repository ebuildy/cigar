// Package webhook exposes the GitLab Pipeline-event Fiber app.
//
// The handler only validates, filters and enqueues — it must never talk to
// Prometheus or the GitLab API (GitLab's webhook timeout is 10s; metric
// queries can be slow). A worker consumes the queue.
package webhook

import (
	"crypto/subtle"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/gofiber/fiber/v3"
)

const maxBodyBytes = 1 << 20 // 1 MiB, enforced by Fiber's BodyLimit (413 beyond)

// terminal pipeline statuses worth reporting on.
var terminalStatuses = map[string]bool{"success": true, "failed": true}

// PipelineEvent is the subset of GitLab's Pipeline Hook payload the bot needs.
type PipelineEvent struct {
	ObjectKind       string `json:"object_kind"`
	ObjectAttributes struct {
		ID     int64  `json:"id"`
		Status string `json:"status"`
	} `json:"object_attributes"`
	Project struct {
		ID int64 `json:"id"`
	} `json:"project"`
	MergeRequest *struct {
		IID int64 `json:"iid"`
	} `json:"merge_request"`
}

// Enqueuer hands a validated event to the processing pipeline. Implementations
// must not block; return false when the queue is full.
type Enqueuer interface {
	Enqueue(ev PipelineEvent) bool
}

// NewApp builds the webhook Fiber app: POST /webhook with token validation,
// event filtering and a 1 MiB body limit.
func NewApp(secret string, queue Enqueuer, log *slog.Logger) *fiber.App {
	app := fiber.New(fiber.Config{
		BodyLimit:    maxBodyBytes,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 10 * time.Second,
	})
	h := &handler{secret: []byte(secret), queue: queue, log: log}
	app.Post("/webhook", h.handle)
	return app
}

type handler struct {
	secret []byte
	queue  Enqueuer
	log    *slog.Logger
}

func (h *handler) handle(c fiber.Ctx) error {
	token := []byte(c.Get("X-Gitlab-Token"))
	if subtle.ConstantTimeCompare(token, h.secret) != 1 {
		return c.SendStatus(fiber.StatusUnauthorized) // deliberately no body detail
	}

	// Ignore other event types with 200 so GitLab doesn't disable the hook.
	if c.Get("X-Gitlab-Event") != "Pipeline Hook" {
		return c.SendStatus(fiber.StatusOK)
	}

	var ev PipelineEvent
	if err := json.Unmarshal(c.Body(), &ev); err != nil {
		return c.SendStatus(fiber.StatusBadRequest)
	}

	if !terminalStatuses[ev.ObjectAttributes.Status] || ev.MergeRequest == nil {
		return c.SendStatus(fiber.StatusOK)
	}

	if !h.queue.Enqueue(ev) {
		h.log.Warn("queue full, dropping event",
			"pipeline_id", ev.ObjectAttributes.ID, "project_id", ev.Project.ID)
		return c.SendStatus(fiber.StatusServiceUnavailable)
	}
	return c.SendStatus(fiber.StatusOK)
}
