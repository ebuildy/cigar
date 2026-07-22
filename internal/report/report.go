// Package report renders the MR comment (markdown via text/template) and
// applies the advice engine: throttling warnings, over-provisioning and
// OOM-risk hints. Rendering is covered by golden-file tests (testdata/*.md).
package report

import (
	"fmt"
	"strings"

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

// totals aggregates resource usage across every job that has usage data.
type totals struct {
	CPUSeconds    float64
	TotalMemBytes uint64 // sum of per-job peaks
	PeakMemBytes  uint64 // max working set across jobs
	NetRxBytes    uint64
	NetTxBytes    uint64
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
	}
	return t
}

// Render produces the markdown body of the MR note, starting with Marker.
func Render(d Data) (string, error) {
	var b strings.Builder
	b.WriteString(Marker)
	b.WriteString("\n\n")

	t := d.totals()
	fmt.Fprintf(&b, "## Pipeline #%d resource report — %s\n\n", d.PipelineID, d.Status)

	b.WriteString("### Summary\n\n")
	b.WriteString("| Resource | Total |\n|---|---|\n")
	fmt.Fprintf(&b, "| CPU time | %s |\n", cpuTime(t.CPUSeconds))
	fmt.Fprintf(&b, "| Total memory (sum of peaks) | %s |\n", humanBytes(t.TotalMemBytes))
	fmt.Fprintf(&b, "| Peak memory (max working set) | %s |\n", humanBytes(t.PeakMemBytes))
	fmt.Fprintf(&b, "| Network RX | %s |\n", humanBytes(t.NetRxBytes))
	fmt.Fprintf(&b, "| Network TX | %s |\n", humanBytes(t.NetTxBytes))

	b.WriteString("\n### Details\n\n")
	b.WriteString("| Stage : Job | CPU time | Peak memory | Mem req / limit | CPU req / limit | Throttled | Network RX / TX |\n")
	b.WriteString("|---|---|---|---|---|---|---|\n")
	for _, j := range d.Jobs {
		row(&b, j, d.ThrottleWarnRatio)
	}

	return b.String(), nil
}

// row renders one job's detail row. A nil Usage job is marked unavailable
// rather than shown with fabricated zeros.
func row(b *strings.Builder, j JobReport, warnRatio float64) {
	name := fmt.Sprintf("%s : %s", j.Stage, j.Name)
	if j.Usage == nil {
		fmt.Fprintf(b, "| %s | _no data_ | | | | | |\n", name)
		return
	}
	u := j.Usage
	fmt.Fprintf(b, "| %s | %s | %s | %s / %s | %s / %s | %s | %s / %s |\n",
		name,
		cpuTime(u.CPUSeconds),
		humanBytes(u.PeakMemoryBytes),
		optBytes(u.MemoryRequestBytes), optBytes(u.MemoryLimitBytes),
		cores(u.CPURequestCores), cores(u.CPULimitCores),
		throttle(u.ThrottledRatio, warnRatio),
		humanBytes(u.NetworkRxBytes), humanBytes(u.NetworkTxBytes),
	)
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
