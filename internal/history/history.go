// Package history answers "what did this normally take": it reduces the
// project's recent successful pipelines to median durations, so a report can
// say whether this pipeline and its jobs are slower than usual. Pipelines on
// the reported refs are excluded by the caller — comparing a branch against
// itself measures iteration noise, not the cost of the change.
package history

import (
	"context"
	"slices"
	"time"
)

// minSamples is how many baseline pipelines a median needs before it is worth
// showing. Below it, no Stat is produced at all.
const minSamples = 3

// JobKey identifies a job across pipelines.
type JobKey struct {
	Stage string
	Name  string
}

// Stat is a median duration and how many pipelines backed it.
type Stat struct {
	Median  time.Duration
	Samples int
}

// Baseline is the typical duration of a pipeline and of each of its jobs. A
// zero Baseline (no samples anywhere) is valid and renders no comparison.
type Baseline struct {
	Pipeline Stat
	Jobs     map[JobKey]Stat
}

// Source is the boundary the reporter depends on; tests stub it.
type Source interface {
	// Baseline returns typical durations from the project's recent successful
	// pipelines, ignoring any pipeline whose ref is in excludeRefs. An empty
	// excludeRefs filters nothing.
	Baseline(ctx context.Context, projectID int64, excludeRefs []string) (Baseline, error)
}

// newStat reduces samples to a median. ok is false below minSamples: a median
// of one or two runs is noise dressed up as a number.
func newStat(samples []time.Duration) (Stat, bool) {
	if len(samples) < minSamples {
		return Stat{}, false
	}
	sorted := slices.Clone(samples)
	slices.Sort(sorted)
	mid := len(sorted) / 2
	median := sorted[mid]
	if len(sorted)%2 == 0 {
		median = (sorted[mid-1] + sorted[mid]) / 2
	}
	return Stat{Median: median, Samples: len(sorted)}, true
}
