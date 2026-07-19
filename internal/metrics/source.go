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

	CPURequestCores    float64
	CPULimitCores      float64
	MemoryRequestBytes uint64
	MemoryLimitBytes   uint64

	// LowConfidence marks jobs shorter than two scrape intervals: numbers are
	// reported as-is with a marker, never fabricated.
	LowConfidence bool
}

// Source is the boundary interface consumed by the worker; tests stub it.
type Source interface {
	// PodUsage aggregates usage for pod over [start, end], padded by one
	// scrape interval.
	PodUsage(ctx context.Context, pod string, start, end time.Time) (*JobUsage, error)
}
