package report

import (
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gitlab.com/ebuildy/gitlab-ci-resources-bot/internal/metrics"
)

var update = flag.Bool("update", false, "update golden files")

func mustRender(t *testing.T, d Data) string {
	t.Helper()
	out, err := Render(d)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	return out
}

func TestRenderGolden(t *testing.T) {
	// A pipeline running from 10:00:00 to 10:04:12 wall-clock: build 10:00–10:02:30,
	// test 10:01:42–10:04:12 (overlapping, so the span is 4m 12s, not their sum).
	base := time.Date(2026, 7, 25, 10, 0, 0, 0, time.UTC)
	d := Data{
		PipelineID:        12345,
		Status:            "success",
		ThrottleWarnRatio: 0.25,
		Jobs: []JobReport{
			{Stage: "build", Name: "compile",
				StartedAt:  base,
				FinishedAt: base.Add(2*time.Minute + 30*time.Second),
				Usage: &metrics.JobUsage{
					CPUSeconds:         42.5,
					PeakMemoryBytes:    412 * 1024 * 1024,
					ThrottledRatio:     0.41,
					NetworkRxBytes:     8 * 1024 * 1024,
					NetworkTxBytes:     3 * 1024 * 1024,
					DiskReadBytes:      600 * 1024 * 1024,
					DiskWriteBytes:     220 * 1024 * 1024,
					CPURequestCores:    0.25,
					CPULimitCores:      0.5,
					MemoryRequestBytes: 256 * 1024 * 1024,
					MemoryLimitBytes:   512 * 1024 * 1024,
				}},
			{Stage: "test", Name: "unit",
				StartedAt:  base.Add(102 * time.Second),
				FinishedAt: base.Add(4*time.Minute + 12*time.Second),
				Usage: &metrics.JobUsage{
					CPUSeconds:         18,
					PeakMemoryBytes:    150 * 1024 * 1024,
					ThrottledRatio:     0.02,
					NetworkRxBytes:     1024 * 1024,
					NetworkTxBytes:     256 * 1024,
					DiskReadBytes:      120 * 1024 * 1024,
					DiskWriteBytes:     40 * 1024 * 1024,
					CPURequestCores:    0.1,
					CPULimitCores:      1,
					MemoryRequestBytes: 128 * 1024 * 1024,
					MemoryLimitBytes:   256 * 1024 * 1024,
				}},
			{Stage: "deploy", Name: "staging", Usage: nil},
		},
	}
	got := mustRender(t, d)

	golden := filepath.Join("testdata", "report.md")
	if *update {
		if err := os.WriteFile(golden, []byte(got), 0o644); err != nil {
			t.Fatalf("write golden: %v", err)
		}
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatalf("read golden (run with -update to create): %v", err)
	}
	if got != string(want) {
		t.Errorf("report does not match %s (run with -update to refresh):\n%s", golden, got)
	}
}

func TestRenderErrorWhenNoPodsResolved(t *testing.T) {
	// Jobs ran but none could be correlated to a runner pod: the report must
	// surface an error notice, not an empty summary of zeros.
	d := Data{
		PipelineID: 55,
		Status:     "success",
		RanJobs:    2,
		Jobs: []JobReport{
			{Stage: "build", Name: "compile", Usage: nil},
			{Stage: "test", Name: "unit", Usage: nil},
		},
	}
	out := mustRender(t, d)

	if !strings.HasPrefix(out, Marker) {
		t.Fatalf("report must start with marker, got:\n%s", out)
	}
	if !strings.Contains(out, "No resource data") {
		t.Errorf("expected an error notice about missing resource data, got:\n%s", out)
	}
	// The empty summary/details tables must not be rendered.
	if strings.Contains(out, "### Summary") || strings.Contains(out, "### Details") {
		t.Errorf("empty report must not render summary/details tables, got:\n%s", out)
	}
	// Never fabricate zeros.
	if strings.Contains(out, "0.0 s") || strings.Contains(out, "0 B") {
		t.Errorf("error report must not show fabricated zeros, got:\n%s", out)
	}
}

func TestRenderNormalWhenSomeUsageDespiteRanJobs(t *testing.T) {
	// At least one job has usage: render the normal report even though another
	// ran without a pod (partial data is not an error).
	d := Data{
		PipelineID: 56,
		Status:     "success",
		RanJobs:    2,
		Jobs: []JobReport{
			{Stage: "build", Name: "compile", Usage: &metrics.JobUsage{CPUSeconds: 3, PeakMemoryBytes: 64 * 1024 * 1024}},
			{Stage: "test", Name: "unit", Usage: nil},
		},
	}
	out := mustRender(t, d)
	if !strings.Contains(out, "### Summary") {
		t.Errorf("partial report should still render the summary, got:\n%s", out)
	}
	if strings.Contains(out, "No resource data") {
		t.Errorf("partial report must not show the no-data error, got:\n%s", out)
	}
}

func TestRenderStartsWithMarker(t *testing.T) {
	out := mustRender(t, Data{PipelineID: 1, Status: "success"})
	if !strings.HasPrefix(out, Marker) {
		t.Fatalf("report must start with marker %q, got:\n%s", Marker, out)
	}
}

func TestRenderSummaryTotalsAcrossJobs(t *testing.T) {
	d := Data{
		PipelineID: 42,
		Status:     "success",
		Jobs: []JobReport{
			{Stage: "build", Name: "compile", Usage: &metrics.JobUsage{
				CPUSeconds:      10,
				PeakMemoryBytes: 100 * 1024 * 1024,
				NetworkRxBytes:  1 * 1024 * 1024,
				NetworkTxBytes:  512 * 1024,
			}},
			{Stage: "test", Name: "unit", Usage: &metrics.JobUsage{
				CPUSeconds:      5,
				PeakMemoryBytes: 200 * 1024 * 1024,
				NetworkRxBytes:  2 * 1024 * 1024,
				NetworkTxBytes:  512 * 1024,
			}},
		},
	}
	out := mustRender(t, d)

	// CPU time: 10 + 5 = 15.0 s
	// Total memory (sum of peaks): 300.0 MiB
	// Peak memory (max working set): 200.0 MiB
	// Network RX: 3.0 MiB, TX: 1.0 MiB
	for _, want := range []string{
		"15.0 s",
		"300.0 MiB",
		"200.0 MiB",
		"3.0 MiB",
		"1.0 MiB",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("summary missing %q in:\n%s", want, out)
		}
	}
}

func TestRenderPipelineDurationIsWallClock(t *testing.T) {
	// Two overlapping jobs: span is earliest start → latest finish (3m 0s),
	// never the sum of the two run windows.
	base := time.Date(2026, 7, 25, 10, 0, 0, 0, time.UTC)
	d := Data{
		PipelineID: 1,
		Status:     "success",
		Jobs: []JobReport{
			{Stage: "build", Name: "a", StartedAt: base, FinishedAt: base.Add(2 * time.Minute),
				Usage: &metrics.JobUsage{CPUSeconds: 1}},
			{Stage: "test", Name: "b", StartedAt: base.Add(time.Minute), FinishedAt: base.Add(3 * time.Minute),
				Usage: &metrics.JobUsage{CPUSeconds: 1}},
		},
	}
	out := mustRender(t, d)
	if !strings.Contains(out, "| Pipeline duration | 3m 00s |") {
		t.Errorf("expected wall-clock duration row of 3m 00s, got:\n%s", out)
	}
}

func TestRenderNoDurationRowWithoutTimings(t *testing.T) {
	// Jobs with usage but no run window (zero times): the duration row must be
	// omitted, never rendered as a fabricated 0s.
	d := Data{
		PipelineID: 1,
		Status:     "success",
		Jobs:       []JobReport{{Stage: "build", Name: "a", Usage: &metrics.JobUsage{CPUSeconds: 1}}},
	}
	out := mustRender(t, d)
	if strings.Contains(out, "Pipeline duration") {
		t.Errorf("duration row must be omitted when no job carries timing, got:\n%s", out)
	}
}

func TestRenderSummaryDiskTotals(t *testing.T) {
	d := Data{
		PipelineID: 1,
		Status:     "success",
		Jobs: []JobReport{
			{Stage: "build", Name: "a", Usage: &metrics.JobUsage{DiskReadBytes: 1 * 1024 * 1024, DiskWriteBytes: 512 * 1024}},
			{Stage: "test", Name: "b", Usage: &metrics.JobUsage{DiskReadBytes: 3 * 1024 * 1024, DiskWriteBytes: 512 * 1024}},
		},
	}
	out := mustRender(t, d)
	if !strings.Contains(out, "| Disk read | 4.0 MiB |") {
		t.Errorf("expected summed disk read of 4.0 MiB, got:\n%s", out)
	}
	if !strings.Contains(out, "| Disk write | 1.0 MiB |") {
		t.Errorf("expected summed disk write of 1.0 MiB, got:\n%s", out)
	}
}

func TestRenderDetailsRowPerJob(t *testing.T) {
	d := Data{
		PipelineID:        7,
		Status:            "success",
		ThrottleWarnRatio: 0.25,
		Jobs: []JobReport{
			{Stage: "build", Name: "compile", Usage: &metrics.JobUsage{
				CPUSeconds:         12,
				PeakMemoryBytes:    256 * 1024 * 1024,
				ThrottledRatio:     0.05,
				NetworkRxBytes:     1024 * 1024,
				NetworkTxBytes:     2 * 1024 * 1024,
				DiskReadBytes:      4 * 1024 * 1024,
				DiskWriteBytes:     512 * 1024,
				CPURequestCores:    0.1,
				CPULimitCores:      0.5,
				MemoryRequestBytes: 128 * 1024 * 1024,
				MemoryLimitBytes:   512 * 1024 * 1024,
			}},
		},
	}
	out := mustRender(t, d)

	line := jobLine(t, out, "build : compile")
	for _, want := range []string{
		"12.0 s",                // CPU time
		"256.0 MiB",             // peak memory
		"128.0 MiB / 512.0 MiB", // mem request/limit
		"100m / 500m",           // CPU request/limit as millicores (exact)
		"5%",                    // throttled percent
		"1.0 MiB / 2.0 MiB",     // network rx/tx
		"4.0 MiB / 512.0 KiB",   // disk read/write
	} {
		if !strings.Contains(line, want) {
			t.Errorf("job row missing %q in:\n%s", want, line)
		}
	}
}

func TestRenderMillicoresAreExact(t *testing.T) {
	// 0.25 cores must render as 250m, not lose precision to "0.2".
	if got := cores(0.25); got != "250m" {
		t.Errorf("cores(0.25) = %q, want %q", got, "250m")
	}
	if got := cores(2); got != "2000m" {
		t.Errorf("cores(2) = %q, want %q", got, "2000m")
	}
}

func TestRenderUnsetRequestsShownAsDash(t *testing.T) {
	d := Data{
		PipelineID: 1,
		Status:     "success",
		Jobs: []JobReport{{Stage: "build", Name: "x", Usage: &metrics.JobUsage{
			CPUSeconds:      1,
			PeakMemoryBytes: 64 * 1024 * 1024,
			NetworkRxBytes:  1024,
			NetworkTxBytes:  1024,
			// Requests/limits absent (kube-state-metrics series missing).
		}}},
	}
	line := jobLine(t, mustRender(t, d), "build : x")
	// req/limit columns: absent series must render as — (mem "—/—", cpu "—/—"),
	// never as a measured "0 B" or "0m".
	if strings.Contains(line, "0m") || strings.Contains(line, "0 B") {
		t.Errorf("unset requests must not show as zero, got:\n%s", line)
	}
	if !strings.Contains(line, "— / —") {
		t.Errorf("unset requests should render as —, got:\n%s", line)
	}
}

func TestRenderBoldsThrottleWhenOverThreshold(t *testing.T) {
	d := Data{
		PipelineID:        3,
		Status:            "success",
		ThrottleWarnRatio: 0.25,
		Jobs: []JobReport{
			{Stage: "build", Name: "hot", Usage: &metrics.JobUsage{ThrottledRatio: 0.42}},
			{Stage: "test", Name: "cool", Usage: &metrics.JobUsage{ThrottledRatio: 0.10}},
		},
	}
	out := mustRender(t, d)

	hot := jobLine(t, out, "build : hot")
	if !strings.Contains(hot, "**42%**") {
		t.Errorf("throttled job should bold its percentage, got:\n%s", hot)
	}

	cool := jobLine(t, out, "test : cool")
	if strings.Contains(cool, "**") {
		t.Errorf("under-threshold job must not bold throttle, got:\n%s", cool)
	}
	if !strings.Contains(cool, "10%") {
		t.Errorf("under-threshold job should still show percentage, got:\n%s", cool)
	}
}

func TestRenderMarksJobWithoutUsageUnavailable(t *testing.T) {
	d := Data{
		PipelineID: 9,
		Status:     "failed",
		Jobs:       []JobReport{{Stage: "deploy", Name: "prod", Usage: nil}},
	}
	out := mustRender(t, d)

	line := jobLine(t, out, "deploy : prod")
	if !strings.Contains(line, "_no data_") {
		t.Errorf("job without usage should be marked unavailable, got:\n%s", line)
	}
	if strings.Contains(line, "0.0 s") || strings.Contains(line, "0 B") {
		t.Errorf("job without usage must not show fabricated zeros, got:\n%s", line)
	}
}

// jobLine returns the single markdown table row containing key (the
// "stage : job" cell), failing the test if it is absent.
func jobLine(t *testing.T, out, key string) string {
	t.Helper()
	for l := range strings.SplitSeq(out, "\n") {
		if strings.Contains(l, key) {
			return l
		}
	}
	t.Fatalf("no row for %q in:\n%s", key, out)
	return ""
}

func TestRenderUsesNoteMarkerOverride(t *testing.T) {
	d := Data{PipelineID: 42, Status: "success", NoteMarker: "<!-- ci-resources-bot p=42 m=3 sig=deadbeef -->"}
	body, err := Render(d)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if !contains(body, d.NoteMarker) {
		t.Fatalf("body missing the override marker:\n%s", body)
	}
}

func TestRenderDefaultsToPlainMarker(t *testing.T) {
	body, err := Render(Data{PipelineID: 1, Status: "success"})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if !contains(body, Marker) {
		t.Fatalf("body missing plain Marker:\n%s", body)
	}
}
