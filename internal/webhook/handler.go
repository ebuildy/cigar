// Package webhook exposes the GitLab Pipeline-event Fiber app.
//
// The handler only validates, filters and enqueues — it must never talk to
// Prometheus or the GitLab API (GitLab's webhook timeout is 10s; metric
// queries can be slow). A worker consumes the queue.
package webhook

import (
	"encoding/json"
	"time"

	"github.com/gofiber/fiber/v3"
	"go.uber.org/zap"

	"gitlab.com/ebuildy/gitlab-ci-resources-bot/internal/command"
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
		Ref    string `json:"ref"` // branch (or tag) the pipeline ran on
	} `json:"object_attributes"`
	Project struct {
		ID int64 `json:"id"`
	} `json:"project"`
	MergeRequest *struct {
		IID int64 `json:"iid"`
	} `json:"merge_request"`
}

// notePayload is the subset of GitLab's Note Hook payload the bot needs.
type notePayload struct {
	ObjectAttributes struct {
		ID           int64  `json:"id"`
		Note         string `json:"note"`
		NoteableType string `json:"noteable_type"`
		DiscussionID string `json:"discussion_id"`
		AuthorID     int64  `json:"author_id"`
	} `json:"object_attributes"`
	Project struct {
		ID int64 `json:"id"`
	} `json:"project"`
	MergeRequest *struct {
		IID int64 `json:"iid"`
	} `json:"merge_request"`
}

// Event is a unit of work for the worker: exactly one field is non-nil.
type Event struct {
	Pipeline *PipelineEvent
	Note     *command.NoteEvent
}

// Enqueuer hands validated work to the worker. Must not block; false = full.
type Enqueuer interface {
	Enqueue(ev Event) bool
}

// NewApp builds the webhook Fiber app: POST /webhook authenticated by the
// given authenticators (tried in order, first success wins), with event
// filtering and a 1 MiB body limit. commandsEnabled gates Note Hook routing.
func NewApp(auths []Authenticator, queue Enqueuer, log *zap.Logger, commandsEnabled bool) *fiber.App {
	app := fiber.New(fiber.Config{
		BodyLimit:    maxBodyBytes,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 10 * time.Second,
	})
	h := &handler{auths: auths, queue: queue, log: log, commandsEnabled: commandsEnabled}
	app.Post("/webhook", h.handle)
	return app
}

type handler struct {
	auths           []Authenticator
	queue           Enqueuer
	log             *zap.Logger
	commandsEnabled bool
}

func (h *handler) authenticate(c fiber.Ctx) bool {
	for _, a := range h.auths {
		if a.Authenticate(c) {
			return true
		}
	}
	return false
}

func (h *handler) handle(c fiber.Ctx) error {
	if !h.authenticate(c) {
		h.log.Debug("webhook authentication failed", zap.String("event", c.Get("X-Gitlab-Event")))
		return c.SendStatus(fiber.StatusUnauthorized) // deliberately no body detail
	}

	switch c.Get("X-Gitlab-Event") {
	case "Pipeline Hook":
		return h.handlePipeline(c)
	case "Note Hook":
		if !h.commandsEnabled {
			return c.SendStatus(fiber.StatusOK)
		}
		return h.handleNote(c)
	default:
		// Ignore other event types with 200 so GitLab doesn't disable the hook.
		h.log.Debug("ignoring unsupported event", zap.String("event", c.Get("X-Gitlab-Event")))
		return c.SendStatus(fiber.StatusOK)
	}
}

func (h *handler) handlePipeline(c fiber.Ctx) error {
	var ev PipelineEvent
	if err := json.Unmarshal(c.Body(), &ev); err != nil {
		h.log.Warn("malformed pipeline payload", zap.Error(err))
		return c.SendStatus(fiber.StatusBadRequest)
	}

	// merge_request may be nil when the branch was pushed before the MR was
	// created; the worker resolves the MR from object_attributes.ref.
	if !terminalStatuses[ev.ObjectAttributes.Status] {
		h.log.Debug("ignoring non-terminal pipeline status",
			zap.Int64("pipeline_id", ev.ObjectAttributes.ID),
			zap.String("status", ev.ObjectAttributes.Status))
		return c.SendStatus(fiber.StatusOK)
	}

	if !h.queue.Enqueue(Event{Pipeline: &ev}) {
		h.log.Warn("queue full, dropping event",
			zap.Int64("pipeline_id", ev.ObjectAttributes.ID), zap.Int64("project_id", ev.Project.ID))
		return c.SendStatus(fiber.StatusServiceUnavailable)
	}
	h.log.Debug("enqueued pipeline event",
		zap.Int64("pipeline_id", ev.ObjectAttributes.ID),
		zap.Int64("project_id", ev.Project.ID),
		zap.String("status", ev.ObjectAttributes.Status))
	return c.SendStatus(fiber.StatusOK)
}

func (h *handler) handleNote(c fiber.Ctx) error {
	var p notePayload
	if err := json.Unmarshal(c.Body(), &p); err != nil {
		h.log.Warn("malformed note payload", zap.Error(err))
		return c.SendStatus(fiber.StatusBadRequest)
	}

	if p.ObjectAttributes.NoteableType != "MergeRequest" || p.MergeRequest == nil {
		return c.SendStatus(fiber.StatusOK)
	}

	if _, ok := command.Parse(p.ObjectAttributes.Note); !ok {
		return c.SendStatus(fiber.StatusOK)
	}

	ne := &command.NoteEvent{
		ProjectID:    p.Project.ID,
		MRIID:        p.MergeRequest.IID,
		NoteID:       p.ObjectAttributes.ID,
		DiscussionID: p.ObjectAttributes.DiscussionID,
		AuthorID:     p.ObjectAttributes.AuthorID,
		Body:         p.ObjectAttributes.Note,
	}

	if !h.queue.Enqueue(Event{Note: ne}) {
		h.log.Warn("queue full, dropping note command",
			zap.Int64("note_id", ne.NoteID), zap.Int64("project_id", ne.ProjectID))
		return c.SendStatus(fiber.StatusServiceUnavailable)
	}
	h.log.Debug("enqueued note command", zap.Int64("note_id", ne.NoteID), zap.Int64("mr_iid", ne.MRIID))
	return c.SendStatus(fiber.StatusOK)
}
