package history

import (
	"slices"
	"time"

	"gitlab.com/ebuildy/gitlab-ci-resources-bot/internal/gitlab"
)

// scanFactor widens the pipeline listing: GitLab cannot express "ref != X", so
// refs are filtered client-side and the scan needs slack to still yield a full
// sample set after the reported refs drop out.
const scanFactor = 3

// pipelineWallClock is a sample pipeline's duration under the same definition
// the report prints: max finish - min start across jobs that ran. ok is false
// when no job carries a run window, so such a pipeline is never counted as 0.
func pipelineWallClock(jobs []gitlab.Job) (time.Duration, bool) {
	var start, end time.Time
	for _, j := range jobs {
		if j.StartedAt.IsZero() || j.FinishedAt.IsZero() {
			continue
		}
		if start.IsZero() || j.StartedAt.Before(start) {
			start = j.StartedAt
		}
		if j.FinishedAt.After(end) {
			end = j.FinishedAt
		}
	}
	if start.IsZero() || !end.After(start) {
		return 0, false
	}
	return end.Sub(start), true
}

// jobDurations maps each job identity of one pipeline to its duration. A retried
// job appears twice in the listing; the last-finishing attempt wins, since that
// is the one whose duration the pipeline actually paid for.
func jobDurations(jobs []gitlab.Job) map[JobKey]time.Duration {
	out := make(map[JobKey]time.Duration, len(jobs))
	finish := make(map[JobKey]time.Time, len(jobs))
	for _, j := range jobs {
		if j.StartedAt.IsZero() || j.FinishedAt.IsZero() {
			continue
		}
		k := JobKey{Stage: j.Stage, Name: j.Name}
		if prev, seen := finish[k]; seen && !j.FinishedAt.After(prev) {
			continue
		}
		finish[k] = j.FinishedAt
		out[k] = j.FinishedAt.Sub(j.StartedAt)
	}
	return out
}

// selectSamples drops pipelines on the excluded refs and keeps the newest limit
// survivors, preserving the newest-first order of the listing.
func selectSamples(all []gitlab.Pipeline, excludeRefs []string, limit int) []gitlab.Pipeline {
	out := make([]gitlab.Pipeline, 0, min(limit, len(all)))
	for _, p := range all {
		if slices.Contains(excludeRefs, p.Ref) {
			continue
		}
		out = append(out, p)
		if len(out) == limit {
			break
		}
	}
	return out
}

// reduce turns the sample pipelines' job listings into medians.
func reduce(pipelines [][]gitlab.Job) Baseline {
	var wall []time.Duration
	perJob := map[JobKey][]time.Duration{}
	for _, jobs := range pipelines {
		if d, ok := pipelineWallClock(jobs); ok {
			wall = append(wall, d)
		}
		for k, d := range jobDurations(jobs) {
			perJob[k] = append(perJob[k], d)
		}
	}
	b := Baseline{Jobs: make(map[JobKey]Stat, len(perJob))}
	b.Pipeline, _ = newStat(wall)
	for k, samples := range perJob {
		if s, ok := newStat(samples); ok {
			b.Jobs[k] = s
		}
	}
	return b
}
