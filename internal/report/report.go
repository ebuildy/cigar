// Package report renders the MR comment (markdown via text/template) and
// applies the advice engine: throttling warnings, over-provisioning and
// OOM-risk hints. Rendering is covered by golden-file tests (testdata/*.md).
package report

import (
	"fmt"
	"strings"
	"time"

	"gitlab.com/ebuildy/gitlab-ci-resources-bot/internal/metrics"
)

// Marker identifies the bot's note on an MR so it can be updated in place.
const Marker = "<!-- ci-resources-bot -->"

// JobReport is one row of the per-job table. Usage is nil when the job's pod
// could not be correlated or its metrics could not be queried — the report
// marks it unavailable instead of showing fabricated numbers.
type JobReport struct {
	Stage string
	Name  string
	Usage *metrics.JobUsage

	// StartedAt/FinishedAt are the job's run window, used to compute the
	// pipeline's wall-clock duration (max finish − min start). Zero when the
	// job never ran (skipped/canceled/manual).
	StartedAt  time.Time
	FinishedAt time.Time

	// BaselineDuration is this job's typical duration in recent successful
	// pipelines on other refs; 0 means no comparable baseline. BaselineSamples
	// is how many pipelines backed it — below minBaselineSamples no delta is
	// rendered.
	BaselineDuration time.Duration
	BaselineSamples  int
}

// duration is the job's run time, or 0 when it never ran.
func (j JobReport) duration() time.Duration {
	if j.StartedAt.IsZero() || j.FinishedAt.IsZero() {
		return 0
	}
	return j.FinishedAt.Sub(j.StartedAt)
}

// Data is everything the template needs to render one pipeline report.
type Data struct {
	PipelineID int64
	Status     string
	Jobs       []JobReport

	// ThrottleWarnRatio is the threshold above which a job gets a ⚠️ CPU
	// throttling warning with KUBERNETES_CPU_REQUEST/LIMIT advice.
	ThrottleWarnRatio float64

	// RanJobs is how many jobs actually executed (had start and finish times).
	// When it is > 0 but no job produced usage — every runner pod failed to
	// correlate — Render emits a "no resource data" error notice instead of an
	// empty report of zeros.
	RanJobs int

	// BaselinePipelineDuration/Samples are the same comparison for the pipeline
	// as a whole (0 = no baseline). DurationDeltaRatio is the relative change
	// above which a delta is shown at all.
	BaselinePipelineDuration time.Duration
	BaselinePipelineSamples  int
	DurationDeltaRatio       float64

	// NoteMarker, when non-empty, is written at the top of the body instead of
	// the plain Marker. serve sets it to a SignedMarker; `bot run` leaves it
	// empty (no signing key, no commands).
	NoteMarker string
}

// hasUsage reports whether any job produced resource-usage data.
func (d Data) hasUsage() bool {
	for _, j := range d.Jobs {
		if j.Usage != nil {
			return true
		}
	}
	return false
}

// baselineFootnote names the sample count when the baseline is thin (fewer than
// fullBaselineSamples), so a median backed by a handful of runs is not
// over-trusted. A full or absent baseline gets no footnote.
func (d Data) baselineFootnote() string {
	n := d.BaselinePipelineSamples
	if n < minBaselineSamples || n >= fullBaselineSamples {
		return ""
	}
	return fmt.Sprintf("_Duration deltas vs the median of %d recent successful pipelines on other refs._", n)
}

// totals aggregates resource usage across every job that has usage data.
type totals struct {
	CPUSeconds     float64
	TotalMemBytes  uint64 // sum of per-job peaks
	PeakMemBytes   uint64 // max working set across jobs
	NetRxBytes     uint64
	NetTxBytes     uint64
	DiskReadBytes  uint64
	DiskWriteBytes uint64
}

func (d Data) totals() totals {
	var t totals
	for _, j := range d.Jobs {
		if j.Usage == nil {
			continue
		}
		t.CPUSeconds += j.Usage.CPUSeconds
		t.TotalMemBytes += j.Usage.PeakMemoryBytes
		if j.Usage.PeakMemoryBytes > t.PeakMemBytes {
			t.PeakMemBytes = j.Usage.PeakMemoryBytes
		}
		t.NetRxBytes += j.Usage.NetworkRxBytes
		t.NetTxBytes += j.Usage.NetworkTxBytes
		t.DiskReadBytes += j.Usage.DiskReadBytes
		t.DiskWriteBytes += j.Usage.DiskWriteBytes
	}
	return t
}

// wallClock is the pipeline's wall-clock duration: the span from the earliest
// job start to the latest job finish. ok is false when no job carries a run
// window (e.g. every job was skipped), so it is never rendered as a fake 0s.
func (d Data) wallClock() (time.Duration, bool) {
	var start, end time.Time
	for _, j := range d.Jobs {
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

// Render produces the markdown body of the MR note, starting with Marker.
func Render(d Data) (string, error) {
	var b strings.Builder
	marker := Marker
	if d.NoteMarker != "" {
		marker = d.NoteMarker
	}
	b.WriteString(marker)
	b.WriteString("\n\n")

	fmt.Fprintf(&b, "## Pipeline #%d resource report — %s\n\n", d.PipelineID, d.Status)

	// Jobs ran but not one could be correlated to a runner pod: surface an
	// error rather than an empty table of zeros ("absent ≠ zero").
	if d.RanJobs > 0 && !d.hasUsage() {
		b.WriteString("> ⚠️ **No resource data available.** None of this pipeline's jobs could be correlated to a runner pod, so no CPU or memory metrics were collected.\n>\n")
		b.WriteString("> This usually means the jobs did not run on the Kubernetes runner, or the runner pod could not be identified from the job trace.\n")
		return b.String(), nil
	}

	t := d.totals()
	b.WriteString("### Summary\n\n")
	b.WriteString("| Resource | Total |\n|---|---|\n")
	if dur, ok := d.wallClock(); ok {
		fmt.Fprintf(&b, "| Pipeline duration | %s |\n",
			durationCell(dur, d.BaselinePipelineDuration, d.BaselinePipelineSamples, d.DurationDeltaRatio))
	}
	fmt.Fprintf(&b, "| CPU time | %s |\n", cpuTime(t.CPUSeconds))
	fmt.Fprintf(&b, "| Total memory (sum of peaks) | %s |\n", humanBytes(t.TotalMemBytes))
	fmt.Fprintf(&b, "| Peak memory (max working set) | %s |\n", humanBytes(t.PeakMemBytes))
	fmt.Fprintf(&b, "| Network RX | %s |\n", humanBytes(t.NetRxBytes))
	fmt.Fprintf(&b, "| Network TX | %s |\n", humanBytes(t.NetTxBytes))
	fmt.Fprintf(&b, "| Disk read | %s |\n", humanBytes(t.DiskReadBytes))
	fmt.Fprintf(&b, "| Disk write | %s |\n", humanBytes(t.DiskWriteBytes))

	b.WriteString("\n### Details\n\n")
	b.WriteString("| Stage : Job | Duration | CPU time | Peak memory | Mem req / limit | CPU req / limit | Throttled | Network RX / TX | Disk R / W |\n")
	b.WriteString("|---|---|---|---|---|---|---|---|---|\n")
	for _, j := range d.Jobs {
		row(&b, j, d.ThrottleWarnRatio, d.DurationDeltaRatio)
	}
	if note := d.baselineFootnote(); note != "" {
		b.WriteString("\n" + note + "\n")
	}

	return b.String(), nil
}

// row renders one job's detail row. A nil Usage job is marked unavailable
// rather than shown with fabricated zeros.
func row(b *strings.Builder, j JobReport, warnRatio, deltaRatio float64) {
	name := fmt.Sprintf("%s : %s", j.Stage, j.Name)
	dur := durationCell(j.duration(), j.BaselineDuration, j.BaselineSamples, deltaRatio)
	if j.Usage == nil {
		fmt.Fprintf(b, "| %s | %s | _no data_ | | | | | | |\n", name, dur)
		return
	}
	u := j.Usage
	fmt.Fprintf(b, "| %s | %s | %s | %s | %s / %s | %s / %s | %s | %s / %s | %s / %s |\n",
		name,
		dur,
		cpuTime(u.CPUSeconds),
		humanBytes(u.PeakMemoryBytes),
		optBytes(u.MemoryRequestBytes), optBytes(u.MemoryLimitBytes),
		cores(u.CPURequestCores), cores(u.CPULimitCores),
		throttle(u.ThrottledRatio, warnRatio),
		humanBytes(u.NetworkRxBytes), humanBytes(u.NetworkTxBytes),
		optBytes(u.DiskReadBytes), optBytes(u.DiskWriteBytes),
	)
}

// humanDuration renders a wall-clock span as compact minutes/seconds
// (e.g. "4m 12s", "45s", "1h 3m").
func humanDuration(d time.Duration) string {
	d = d.Round(time.Second)
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm %02ds", int(d/time.Minute), int(d%time.Minute/time.Second))
	}
	return fmt.Sprintf("%dh %02dm", int(d/time.Hour), int(d%time.Hour/time.Minute))
}

// dash marks a value whose Prometheus series was absent — never rendered as a
// measured zero ("absent ≠ zero").
const dash = "—"

// cores renders a Kubernetes CPU quantity as exact millicores (e.g. 250m).
// A zero quantity means the request/limit was unset (series absent).
func cores(c float64) string {
	if c == 0 {
		return dash
	}
	return fmt.Sprintf("%dm", int64(c*1000+0.5))
}

// optBytes renders a byte quantity, or dash when unset (zero → series absent).
func optBytes(n uint64) string {
	if n == 0 {
		return dash
	}
	return humanBytes(n)
}

// throttle renders the CPU throttled percentage, bolded with a ⚠️ when it
// meets or exceeds warnRatio.
func throttle(ratio, warnRatio float64) string {
	pct := fmt.Sprintf("%.0f%%", ratio*100)
	if warnRatio > 0 && ratio >= warnRatio {
		return "**" + pct + "** ⚠️"
	}
	return pct
}

// cpuTime renders core-seconds of CPU time consumed.
func cpuTime(seconds float64) string {
	return fmt.Sprintf("%.1f s", seconds)
}

// humanBytes formats a byte count with IEC units (KiB, MiB, …), one decimal.
func humanBytes(n uint64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := uint64(unit), 0
	for v := n / unit; v >= unit; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}
