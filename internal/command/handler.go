package command

import (
	"context"
	"fmt"

	"go.uber.org/zap"

	"gitlab.com/ebuildy/gitlab-ci-resources-bot/internal/correlate"
	"gitlab.com/ebuildy/gitlab-ci-resources-bot/internal/gitlab"
	"gitlab.com/ebuildy/gitlab-ci-resources-bot/internal/metrics"
	"gitlab.com/ebuildy/gitlab-ci-resources-bot/internal/report"
)

// Handler authorizes and executes command notes.
type Handler struct {
	GitLab     gitlab.Client
	Resolver   correlate.Resolver
	Series     metrics.SeriesSource
	SigningKey []byte
	BotUserID  int64
	Log        *zap.Logger
}

// Handle authorizes and runs a single command note. Unauthorized or unparseable
// notes are dropped silently (return nil). It performs at most one reply.
func (h *Handler) Handle(ctx context.Context, ev NoteEvent) error {
	cmd, ok := Parse(ev.Body)
	if !ok {
		return nil
	}
	if ev.AuthorID == h.BotUserID {
		return nil // loop guard: never react to our own notes
	}
	disc, err := h.GitLab.MergeRequestDiscussion(ctx, ev.ProjectID, ev.MRIID, ev.DiscussionID)
	if err != nil {
		return fmt.Errorf("authorize command note %d: %w", ev.NoteID, err)
	}
	if disc.RootNoteAuthorID != h.BotUserID {
		h.Log.Debug("command note not in a bot report thread", zap.Int64("note_id", ev.NoteID))
		return nil
	}
	pipelineID, markerMR, ok := report.ParseSignedMarker(disc.RootNoteBody, h.SigningKey)
	if !ok || markerMR != ev.MRIID {
		h.Log.Warn("command note root marker missing/invalid/mismatched",
			zap.Int64("note_id", ev.NoteID), zap.Bool("marker_ok", ok))
		return nil
	}

	switch cmd.Kind {
	case KindHelp:
		return h.reply(ctx, ev, HelpText)
	case KindDetails:
		return h.details(ctx, ev, pipelineID, cmd)
	}
	return nil
}

func (h *Handler) reply(ctx context.Context, ev NoteEvent, body string) error {
	if err := h.GitLab.CreateDiscussionReply(ctx, ev.ProjectID, ev.MRIID, ev.DiscussionID, body); err != nil {
		return fmt.Errorf("reply to command note %d: %w", ev.NoteID, err)
	}
	return nil
}

// details is implemented in Task 10.
func (h *Handler) details(ctx context.Context, ev NoteEvent, pipelineID int64, cmd Command) error {
	return h.reply(ctx, ev, "details is not implemented yet")
}
