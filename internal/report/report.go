// Package report renders the MR comment (markdown via text/template) and
// applies the advice engine: throttling warnings, over-provisioning and
// OOM-risk hints. Rendering is covered by golden-file tests (testdata/*.md).
package report

import (
	"gitlab.com/ebuildy/gitlab-ci-resources-bot/internal/metrics"
)

// Marker identifies the bot's note on an MR so it can be updated in place.
const Marker = "<!-- ci-resources-bot -->"

// JobReport is one row of the per-job table. Usage is nil when the job's pod
// could not be correlated or its metrics could not be queried — the report
// marks it unavailable instead of showing fabricated numbers.
type JobReport struct {
	Name  string
	Usage *metrics.JobUsage
}

// Data is everything the template needs to render one pipeline report.
type Data struct {
	PipelineID int64
	Status     string
	Jobs       []JobReport

	// ThrottleWarnRatio is the threshold above which a job gets a ⚠️ CPU
	// throttling warning with KUBERNETES_CPU_REQUEST/LIMIT advice.
	ThrottleWarnRatio float64
}

// Render produces the markdown body of the MR note, starting with Marker.
func Render(d Data) (string, error) {
	// TODO: text/template rendering + advice engine, golden-file tested.
	return Marker + "\n\n_report rendering not implemented yet_\n", nil
}
