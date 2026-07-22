package correlate

import (
	"bufio"
	"context"
	"fmt"
	"regexp"
	"strings"
	"time"

	"go.uber.org/zap"
)

// traceFetcher is the slice of the GitLab client the trace resolver needs;
// gitlab.Client satisfies it.
type traceFetcher interface {
	JobTrace(ctx context.Context, projectID, jobID int64) (string, error)
}

// ansiRE strips ANSI CSI escape sequences (color codes, erase-line, etc.) GitLab wraps trace lines in.
var ansiRE = regexp.MustCompile("\x1b\\[[0-9;]*[a-zA-Z]")

// runnerLineRE captures the pod hostname from the runner's "Running on <pod>
// via <manager>" trace line (Kubernetes executor: <pod> is the build pod).
var runnerLineRE = regexp.MustCompile(`Running on (\S+) via `)

// NewTraceResolver returns a Resolver that finds the runner pod by parsing the
// job's GitLab trace log instead of querying Prometheus pod labels.
func NewTraceResolver(f traceFetcher, log *zap.Logger) Resolver {
	log.Debug("gitlab trace pod resolver created")
	return &traceResolver{fetch: f, log: log}
}

type traceResolver struct {
	fetch traceFetcher
	log   *zap.Logger
}

func (r *traceResolver) PodForJob(ctx context.Context, projectID, jobID int64, _, _ time.Time) (string, bool, error) {
	r.log.Debug("resolving pod from job trace", zap.Int64("job_id", jobID))
	trace, err := r.fetch.JobTrace(ctx, projectID, jobID)
	if err != nil {
		return "", false, fmt.Errorf("fetch trace for job %d: %w", jobID, err)
	}
	scanner := bufio.NewScanner(strings.NewReader(trace))
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := ansiRE.ReplaceAllString(scanner.Text(), "")
		if m := runnerLineRE.FindStringSubmatch(line); m != nil {
			r.log.Debug("resolved pod from trace", zap.Int64("job_id", jobID), zap.String("pod", m[1]))
			return m[1], true, nil
		}
	}
	if err := scanner.Err(); err != nil {
		return "", false, fmt.Errorf("scan trace for job %d: %w", jobID, err)
	}
	r.log.Debug("no runner line in job trace", zap.Int64("job_id", jobID))
	return "", false, nil
}
