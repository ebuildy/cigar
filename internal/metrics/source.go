// Package metrics queries Prometheus (cadvisor + kube-state-metrics) for the
// resource usage of a runner pod over a job's time window, and aggregates it
// per job.
package metrics

import (
	"context"
	"time"
)

// JobUsage is the aggregated resource usage of one job's runner pod.
// Pause/POD containers are always excluded from container-level aggregations.
type JobUsage struct {
	CPUSeconds      float64 // increase(container_cpu_usage_seconds_total)
	PeakMemoryBytes uint64  // max_over_time(container_memory_working_set_bytes), summed per container
	ThrottledRatio  float64 // throttled_periods / periods over the window
	NetworkRxBytes  uint64
	NetworkTxBytes  uint64
	DiskReadBytes   uint64 // increase(container_fs_reads_bytes_total)
	DiskWriteBytes  uint64 // increase(container_fs_writes_bytes_total)

	CPURequestCores    float64
	CPULimitCores      float64
	MemoryRequestBytes uint64
	MemoryLimitBytes   uint64

	// LowConfidence marks jobs shorter than two scrape intervals: numbers are
	// reported as-is with a marker, never fabricated.
	LowConfidence bool

	// Containers is the per-container breakdown of this pod, ordered build,
	// helper, then the job's service containers; runner-injected init
	// containers are excluded. It is empty when the container-level series were
	// absent — the totals above are then all that is known. Every total above
	// is derived from this slice by sumContainers, so a total can never
	// disagree with its rows.
	Containers []ContainerUsage
}

// Source is the boundary interface consumed by the worker; tests stub it.
type Source interface {
	// PodUsage aggregates usage for pod over [start, end], padded by one
	// scrape interval.
	PodUsage(ctx context.Context, pod string, start, end time.Time) (*JobUsage, error)
}
