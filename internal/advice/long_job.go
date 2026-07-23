package advice

import (
	"fmt"
	"strings"
	"time"
)

// longJob advises splitting jobs that run longer than the configured budget.
// It is the only rule that works without Usage: the duration comes from
// GitLab, so it still fires when the runner pod never correlated.
type longJob struct{}

func (longJob) Name() string { return "long-job" }

func (longJob) Check(f Facts, t Thresholds) []Advice {
	if t.LongJob <= 0 || f.Duration <= t.LongJob {
		return nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "This job ran for **%s**, over the %s budget. It is the pipeline's "+
		"critical path: every push waits on it.\n\n", f.Duration.Round(time.Second), t.LongJob)
	b.WriteString("If the work splits, split it — several jobs in one stage run in parallel on separate runners:\n\n")
	b.WriteString("- Break independent steps (lint, unit, integration) into separate jobs.\n")
	b.WriteString("- Shard a long test suite with `parallel:` (or `parallel:matrix:`) and a shard flag.\n")
	b.WriteString("- Move setup that repeats every run into `cache:` or a prebuilt image.\n")

	return []Advice{{
		Job:   f.Name,
		Rule:  "long-job",
		Title: "⏱️ Long job",
		Body:  b.String(),
	}}
}
