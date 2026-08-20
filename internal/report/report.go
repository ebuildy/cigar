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
}

// Data is everything the template needs to render one pipeline report.
type Data struct {
	PipelineID int64
	Status     string
	Jobs       []JobReport

	// ThrottleWarnRatio is the threshold above which a job gets a ⚠️ CPU
	// throttling warning with KUBERNETES_CPU_REQUEST/LIMIT advice.
	ThrottleWarnRatio float64

	// ContainerDetailMaxJobs nests one row per runner-pod container (build,
	// helper, svc-N) under each job, but only while the pipeline has at most
	// this many jobs — the breakdown multiplies a large table's height. 0
	// disables it; the `details` command still serves the breakdown on demand.
	ContainerDetailMaxJobs int

	// RanJobs is how many jobs actually executed (had start and finish times).
	// When it is > 0 but no job produced usage — every runner pod failed to
	// correlate — Render emits a "no resource data" error notice instead of an
	// empty report of zeros.
	RanJobs int

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

// showContainers reports whether the Details table should nest per-container
// rows: the breakdown is enabled and the table is short enough to stay readable.
func (d Data) showContainers() bool {
	return d.ContainerDetailMaxJobs > 0 && len(d.Jobs) <= d.ContainerDetailMaxJobs
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
		fmt.Fprintf(&b, "| Pipeline duration | %s |\n", humanDuration(dur))
	}
	fmt.Fprintf(&b, "| CPU time | %s |\n", cpuTime(t.CPUSeconds))
	fmt.Fprintf(&b, "| Total memory (sum of peaks) | %s |\n", humanBytes(t.TotalMemBytes))
	fmt.Fprintf(&b, "| Peak memory (max working set) | %s |\n", humanBytes(t.PeakMemBytes))
	fmt.Fprintf(&b, "| Network RX | %s |\n", humanBytes(t.NetRxBytes))
	fmt.Fprintf(&b, "| Network TX | %s |\n", humanBytes(t.NetTxBytes))
	fmt.Fprintf(&b, "| Disk read | %s |\n", humanBytes(t.DiskReadBytes))
	fmt.Fprintf(&b, "| Disk write | %s |\n", humanBytes(t.DiskWriteBytes))

	b.WriteString("\n### Details\n\n")
	b.WriteString("| Stage : Job | CPU time | Peak memory | Mem req / limit | CPU req / limit | Throttled | Network RX / TX | Disk R / W |\n")
	b.WriteString("|---|---|---|---|---|---|---|---|\n")
	for _, j := range d.Jobs {
		row(&b, j, d.ThrottleWarnRatio)
		// A single-container breakdown only repeats the job row above it.
		if d.showContainers() && j.Usage != nil && len(j.Usage.Containers) > 1 {
			for _, c := range j.Usage.Containers {
				containerRow(&b, c, d.ThrottleWarnRatio, "↳ ")
			}
		}
	}

	return b.String(), nil
}

// row renders one job's detail row. A nil Usage job is marked unavailable
// rather than shown with fabricated zeros.
func row(b *strings.Builder, j JobReport, warnRatio float64) {
	name := fmt.Sprintf("%s : %s", j.Stage, j.Name)
	if j.Usage == nil {
		fmt.Fprintf(b, "| %s | _no data_ | | | | | | |\n", name)
		return
	}
	u := j.Usage
	fmt.Fprintf(b, "| %s | %s | %s | %s / %s | %s / %s | %s | %s / %s | %s / %s |\n",
		name,
		cpuTime(u.CPUSeconds),
		humanBytes(u.PeakMemoryBytes),
		optBytes(u.MemoryRequestBytes), optBytes(u.MemoryLimitBytes),
		cores(u.CPURequestCores), cores(u.CPULimitCores),
		throttle(u.ThrottledRatio, warnRatio),
		humanBytes(u.NetworkRxBytes), humanBytes(u.NetworkTxBytes),
		optBytes(u.DiskReadBytes), optBytes(u.DiskWriteBytes),
	)
}

// containerRow renders one container. prefix marks it as a child row in the
// job table ("↳ ") and is empty in the standalone ContainerTable. Network is
// always dashed: cadvisor's network series carry no container label, so it
// cannot be attributed here — absent, not zero.
func containerRow(b *strings.Builder, c metrics.ContainerUsage, warnRatio float64, prefix string) {
	fmt.Fprintf(b, "| %s%s | %s | %s | %s / %s | %s / %s | %s | %s / %s | %s / %s |\n",
		prefix, c.Name,
		cpuTime(c.CPUSeconds),
		humanBytes(c.PeakMemoryBytes),
		optBytes(c.MemoryRequestBytes), optBytes(c.MemoryLimitBytes),
		cores(c.CPURequestCores), cores(c.CPULimitCores),
		containerThrottle(c, warnRatio),
		dash, dash,
		optBytes(c.DiskReadBytes), optBytes(c.DiskWriteBytes),
	)
}

// containerThrottle renders a container's throttled percentage, or a dash when
// its CFS period series was absent (absent ≠ 0%).
func containerThrottle(c metrics.ContainerUsage, warnRatio float64) string {
	r, ok := c.ThrottledRatio()
	if !ok {
		return dash
	}
	return throttle(r, warnRatio)
}

// ContainerTable renders a standalone per-container table for one job — the
// `details` command reply. It shares containerRow with the report's nested
// rows so the two renderings cannot drift. Returns "" when there is nothing to
// show.
func ContainerTable(containers []metrics.ContainerUsage, warnRatio float64) string {
	if len(containers) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("| Container | CPU time | Peak memory | Mem req / limit | CPU req / limit | Throttled | Network RX / TX | Disk R / W |\n")
	b.WriteString("|---|---|---|---|---|---|---|---|\n")
	for _, c := range containers {
		containerRow(&b, c, warnRatio, "")
	}
	return b.String()
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
