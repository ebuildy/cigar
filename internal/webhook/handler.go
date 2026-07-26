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
	User *struct {
		ID int64 `json:"id"` // pipeline triggerer, for adoption metrics
	} `json:"user"`
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

// projectPath best-effort extracts project.path_with_namespace from any GitLab
// webhook body (pipeline and note hooks both carry it). Returns "" when the
// body is absent, unparseable, or lacks the field — never errors, so it is safe
// to call on unauthenticated requests for logging only.
func projectPath(body []byte) string {
	var p struct {
		Project struct {
			PathWithNamespace string `json:"path_with_namespace"`
		} `json:"project"`
	}
	_ = json.Unmarshal(body, &p)
	return p.Project.PathWithNamespace
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

// Recorder records ops metrics from webhook deliveries. Optional (nil = no-op).
// It keeps this package ignorant of Prometheus — telemetry satisfies it.
type Recorder interface {
	RecordWebhook(projectID int64, status int)
	RecordUser(userID int64)
}

// NewApp builds the webhook Fiber app: POST /webhook authenticated by auth,
// with event filtering and a 1 MiB body limit. commandsEnabled gates Note Hook
// routing. rec may be nil (no metrics).
func NewApp(auth Authenticator, queue Enqueuer, log *zap.Logger, commandsEnabled bool, rec Recorder) *fiber.App {
	app := fiber.New(fiber.Config{
		BodyLimit:    maxBodyBytes,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 10 * time.Second,
	})
	h := &handler{auth: auth, queue: queue, log: log, commandsEnabled: commandsEnabled, rec: rec}
	app.Post("/webhook", h.handle)
	return app
}

type handler struct {
	auth            Authenticator
	queue           Enqueuer
	log             *zap.Logger
	commandsEnabled bool
	rec             Recorder
}

func (h *handler) handle(c fiber.Ctx) error {
	// Record one webhook-call metric per request, tagged with the project (0
	// until the payload is parsed) and the final HTTP status — so 401s and other
	// pre-parse failures are counted too. The defer reads the status after the
	// handler has set it via SendStatus.
	var projectID int64
	if h.rec != nil {
		defer func() { h.rec.RecordWebhook(projectID, c.Response().StatusCode()) }()
	}

	if !h.auth.Authenticate(c) {
		// Best-effort parse of the project path from the (already size-limited)
		// body so the operator can tell which project's hook has a bad token.
		h.log.Warn("webhook authentication failed",
			zap.String("auth_method", h.auth.Name()),
			zap.String("event", c.Get("X-Gitlab-Event")),
			zap.String("project", projectPath(c.Body())))
		return c.SendStatus(fiber.StatusUnauthorized) // deliberately no body detail
	}

	switch c.Get("X-Gitlab-Event") {
	case "Pipeline Hook":
		return h.handlePipeline(c, &projectID)
	case "Note Hook":
		if !h.commandsEnabled {
			return c.SendStatus(fiber.StatusOK)
		}
		return h.handleNote(c, &projectID)
	default:
		// Ignore other event types with 200 so GitLab doesn't disable the hook.
		h.log.Debug("ignoring unsupported event", zap.String("event", c.Get("X-Gitlab-Event")))
		return c.SendStatus(fiber.StatusOK)
	}
}

func (h *handler) handlePipeline(c fiber.Ctx, projectID *int64) error {
	var ev PipelineEvent
	if err := json.Unmarshal(c.Body(), &ev); err != nil {
		h.log.Warn("malformed pipeline payload", zap.Error(err))
		return c.SendStatus(fiber.StatusBadRequest)
	}
	*projectID = ev.Project.ID

	if h.rec != nil && ev.User != nil {
		h.rec.RecordUser(ev.User.ID)
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
		// Dropped for good: GitLab does not retry, so this pipeline gets no report.
		h.log.Error("queue full, dropping event",
			zap.Int64("pipeline_id", ev.ObjectAttributes.ID), zap.Int64("project_id", ev.Project.ID))
		return c.SendStatus(fiber.StatusServiceUnavailable)
	}
	h.log.Debug("enqueued pipeline event",
		zap.Int64("pipeline_id", ev.ObjectAttributes.ID),
		zap.Int64("project_id", ev.Project.ID),
		zap.String("status", ev.ObjectAttributes.Status))
	return c.SendStatus(fiber.StatusOK)
}

func (h *handler) handleNote(c fiber.Ctx, projectID *int64) error {
	var p notePayload
	if err := json.Unmarshal(c.Body(), &p); err != nil {
		h.log.Warn("malformed note payload", zap.Error(err))
		return c.SendStatus(fiber.StatusBadRequest)
	}
	*projectID = p.Project.ID

	if h.rec != nil {
		h.rec.RecordUser(p.ObjectAttributes.AuthorID)
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
		h.log.Error("queue full, dropping note command",
			zap.Int64("note_id", ne.NoteID), zap.Int64("project_id", ne.ProjectID))
		return c.SendStatus(fiber.StatusServiceUnavailable)
	}
	h.log.Debug("enqueued note command", zap.Int64("note_id", ne.NoteID), zap.Int64("mr_iid", ne.MRIID))
	return c.SendStatus(fiber.StatusOK)
}
