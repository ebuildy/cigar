package metrics

import (
	"cmp"
	"math"
	"slices"
	"strconv"
	"strings"
)

// ContainerUsage is one container's slice of a runner pod's usage.
//
// A GitLab Kubernetes runner pod runs `build` (the job's script), `helper`
// (git clone, artifacts, cache) and one `svc-N` per CI `services:` entry.
//
// Network is absent by construction: cadvisor's container_network_* series are
// pod-level and carry no `container` label.
type ContainerUsage struct {
	Name string // cadvisor `container` label: build, helper, svc-0…

	CPUSeconds      float64
	PeakMemoryBytes uint64
	DiskReadBytes   uint64
	DiskWriteBytes  uint64

	// Raw CFS counters rather than a ratio: the job-level ratio must stay
	// sum(throttled)/sum(periods), which a mean of per-container ratios would
	// not reproduce.
	ThrottledPeriods float64
	Periods          float64

	CPURequestCores    float64
	CPULimitCores      float64
	MemoryRequestBytes uint64
	MemoryLimitBytes   uint64
}

// ThrottledRatio is this container's throttled fraction of CFS periods. ok is
// false when the period series was absent: absent is not 0%.
func (c ContainerUsage) ThrottledRatio() (float64, bool) {
	if c.Periods <= 0 {
		return 0, false
	}
	return c.ThrottledPeriods / c.Periods, true
}

// runnerInitContainers are the init containers the Kubernetes executor injects
// into a build pod. They are runner plumbing, not the job's workload: they run
// before the job starts, their usage is negligible, and no CI variable tunes
// them — so they are dropped from the breakdown rather than shown as a row the
// reader can act on.
var runnerInitContainers = map[string]bool{
	"init-permissions": true,
	"permissions":      true,
	"svc-0-init":       true,
}

// IsRunnerInitContainer reports whether name is a runner-injected init
// container. Exported for the advice package, which must never suggest tuning
// one.
func IsRunnerInitContainer(name string) bool { return runnerInitContainers[name] }

// containerRank orders the breakdown: build first, then helper, then the job's
// service containers, then anything else. Prometheus returns vector samples in
// no guaranteed order and golden files need a stable one.
//
// A service is named after its `alias:` when the job sets one and only falls
// back to positional `svc-N` when it does not — a job with `alias: db` produces
// a container literally called `db`. Un-aliased `svc-N` sorts numerically so
// svc-2 precedes svc-10; everything else sorts by name.
func containerRank(name string) (group, index int) {
	switch {
	case name == "build":
		return 0, 0
	case name == "helper":
		return 1, 0
	case strings.HasPrefix(name, "svc-"):
		n, err := strconv.Atoi(strings.TrimPrefix(name, "svc-"))
		if err != nil {
			// A non-numeric svc-* suffix sorts after every numbered one.
			return 2, math.MaxInt
		}
		return 2, n
	default:
		return 3, 0
	}
}

// sortedContainers flattens the accumulator into the stable report order.
func sortedContainers(acc map[string]*ContainerUsage) []ContainerUsage {
	out := make([]ContainerUsage, 0, len(acc))
	for _, c := range acc {
		out = append(out, *c)
	}
	slices.SortFunc(out, func(a, b ContainerUsage) int {
		ga, ia := containerRank(a.Name)
		gb, ib := containerRank(b.Name)
		if ga != gb {
			return cmp.Compare(ga, gb)
		}
		if ia != ib {
			return cmp.Compare(ia, ib)
		}
		return strings.Compare(a.Name, b.Name)
	})
	return out
}

// sumContainers derives the pod-level totals from the per-container breakdown.
// It is the only writer of those fields, which is what guarantees the job row
// equals the sum of the container rows beneath it.
//
// ThrottledRatio stays unset when no container reported CFS periods: absent
// series are never rendered as a measured 0%.
func (u *JobUsage) sumContainers() {
	var throttled, periods float64
	for _, c := range u.Containers {
		u.CPUSeconds += c.CPUSeconds
		u.PeakMemoryBytes += c.PeakMemoryBytes
		u.DiskReadBytes += c.DiskReadBytes
		u.DiskWriteBytes += c.DiskWriteBytes
		u.CPURequestCores += c.CPURequestCores
		u.CPULimitCores += c.CPULimitCores
		u.MemoryRequestBytes += c.MemoryRequestBytes
		u.MemoryLimitBytes += c.MemoryLimitBytes
		throttled += c.ThrottledPeriods
		periods += c.Periods
	}
	if periods > 0 {
		u.ThrottledRatio = throttled / periods
	}
}

// SumForTest derives the pod-level totals from Containers. It exists so tests
// in other packages can build a JobUsage fixture whose totals agree with its
// rows, the same way PodUsage does. Production code must not call it: PodUsage
// already sums, and calling it twice double-counts.
func (u *JobUsage) SumForTest() { u.sumContainers() }
