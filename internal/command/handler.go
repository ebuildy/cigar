package command

import (
	"context"
	"fmt"
	"strings"
	"time"

	"go.uber.org/zap"

	"gitlab.com/ebuildy/gitlab-ci-resources-bot/internal/chart"
	"gitlab.com/ebuildy/gitlab-ci-resources-bot/internal/correlate"
	"gitlab.com/ebuildy/gitlab-ci-resources-bot/internal/gitlab"
	"gitlab.com/ebuildy/gitlab-ci-resources-bot/internal/metrics"
	"gitlab.com/ebuildy/gitlab-ci-resources-bot/internal/report"
)

// Handler authorizes and executes command notes.
type Handler struct {
	GitLab      gitlab.Client
	Resolver    correlate.Resolver
	Series      metrics.SeriesSource
	SigningKey  []byte
	BotUserID   int64
	ChartFormat chart.Format // PNG (default) or SVG
	Log         *zap.Logger
}

// Handle authorizes and runs a single command note. Unauthorized or unparseable
// notes are dropped silently (return nil). It performs at most one reply.
func (h *Handler) Handle(ctx context.Context, ev NoteEvent) error {
	cmd, ok := Parse(ev.Body)
	if !ok {
		return nil
	}
	// Loop guard: every note the bot writes (report + command replies) carries
	// the marker, so we skip anything that contains it. This is identity-free on
	// purpose — the GitLab token may belong to a real user who also issues
	// commands, so an author==bot check would wrongly drop that user's replies.
	if strings.Contains(ev.Body, report.MarkerPrefix) {
		h.Log.Debug("ignoring bot-authored (marker-tagged) note", zap.Int64("note_id", ev.NoteID))
		return nil
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
	// Tag every reply with the marker so the loop guard recognizes it as the
	// bot's own note (the marker is an HTML comment — invisible when rendered).
	body += "\n\n" + report.Marker
	if err := h.GitLab.CreateDiscussionReply(ctx, ev.ProjectID, ev.MRIID, ev.DiscussionID, body); err != nil {
		return fmt.Errorf("reply to command note %d: %w", ev.NoteID, err)
	}
	return nil
}

// details resolves the target against the report's pipeline, queries its series,
// renders three charts, uploads them and posts one reply.
func (h *Handler) details(ctx context.Context, ev NoteEvent, pipelineID int64, cmd Command) error {
	pod, start, end, found, err := h.resolveTarget(ctx, ev.ProjectID, pipelineID, cmd)
	if err != nil {
		return err
	}
	if !found {
		return h.reply(ctx, ev, fmt.Sprintf("`%s` is not part of pipeline #%d's report.", cmd.Name, pipelineID))
	}
	series, err := h.Series.PodSeries(ctx, pod, start, end)
	if err != nil {
		return fmt.Errorf("query series for pod %q: %w", pod, err)
	}
	if series.Empty() {
		return h.reply(ctx, ev, fmt.Sprintf("No metrics found for `%s` in the report window.", cmd.Name))
	}

	charts := []struct {
		base  string
		title string
		unit  chart.Unit
		lines []chart.Series
	}{
		{"cpu", "CPU (cores)", chart.UnitCores, []chart.Series{toChart(series.CPU)}},
		{"memory", "Memory (bytes)", chart.UnitBytes, []chart.Series{toChart(series.Memory)}},
		{"network", "Network TX (bytes/s)", chart.UnitBytesPerSec, []chart.Series{toChart(series.NetTx)}},
	}
	var body strings.Builder
	fmt.Fprintf(&body, "### Resource usage for `%s`\n\n", cmd.Name)
	for _, c := range charts {
		data, err := chart.Render(h.ChartFormat, c.title, c.unit, c.lines)
		if err != nil {
			return fmt.Errorf("render %s: %w", c.base, err)
		}
		if h.ChartFormat.Inline() {
			// Markdown charts are embedded directly — no upload.
			body.Write(data)
		} else {
			filename := c.base + "." + h.ChartFormat.Ext()
			md, err := h.GitLab.UploadFile(ctx, ev.ProjectID, filename, data)
			if err != nil {
				return fmt.Errorf("upload %s: %w", filename, err)
			}
			body.WriteString(md)
		}
		body.WriteString("\n\n")
	}
	return h.reply(ctx, ev, body.String())
}

func toChart(l metrics.Line) chart.Series {
	pts := make([]chart.Point, len(l.Points))
	for i, p := range l.Points {
		pts[i] = chart.Point{X: p.T, Y: p.V}
	}
	return chart.Series{Label: l.Label, Points: pts}
}

// resolveTarget validates the target against the pipeline's jobs (the live
// allowlist) and returns the pod plus its chart window. found is false when the
// target is not part of the report.
func (h *Handler) resolveTarget(ctx context.Context, projectID, pipelineID int64, cmd Command) (pod string, start, end time.Time, found bool, err error) {
	jobs, err := h.GitLab.PipelineJobs(ctx, projectID, pipelineID)
	if err != nil {
		return "", time.Time{}, time.Time{}, false, fmt.Errorf("list jobs of pipeline %d: %w", pipelineID, err)
	}
	switch cmd.Target {
	case TargetJob:
		for _, j := range jobs {
			if j.Name != cmd.Name {
				continue
			}
			if j.StartedAt.IsZero() || j.FinishedAt.IsZero() {
				return "", time.Time{}, time.Time{}, false, nil // job never ran
			}
			p, ok, err := h.Resolver.PodForJob(ctx, projectID, j.ID, j.StartedAt, j.FinishedAt)
			if err != nil {
				return "", time.Time{}, time.Time{}, false, err
			}
			if !ok {
				return "", time.Time{}, time.Time{}, false, nil
			}
			return p, j.StartedAt, j.FinishedAt, true, nil
		}
		return "", time.Time{}, time.Time{}, false, nil
	case TargetPod:
		for _, j := range jobs {
			if j.StartedAt.IsZero() || j.FinishedAt.IsZero() {
				continue
			}
			p, ok, err := h.Resolver.PodForJob(ctx, projectID, j.ID, j.StartedAt, j.FinishedAt)
			if err != nil {
				return "", time.Time{}, time.Time{}, false, err
			}
			if !ok || p != cmd.Name {
				continue
			}
			s, e, ok, err := h.Series.PodActiveSpan(ctx, p)
			if err != nil {
				return "", time.Time{}, time.Time{}, false, err
			}
			if !ok {
				return p, j.StartedAt, j.FinishedAt, true, nil
			}
			return p, s, e, true, nil
		}
		return "", time.Time{}, time.Time{}, false, nil
	}
	return "", time.Time{}, time.Time{}, false, nil
}
