package advice

import (
	"fmt"
	"strings"
)

// memoryPressure advises raising the memory limit when peak working set came
// close to it (OOMKill risk). It never fires without a limit series: an absent
// limit is not a denominator ("absent != zero").
type memoryPressure struct{}

func (memoryPressure) Name() string { return "memory-pressure" }

func (memoryPressure) Check(f Facts, t Thresholds) []Advice {
	if f.Usage == nil || f.Usage.MemoryLimitBytes == 0 || t.MemoryPressureRatio <= 0 {
		return nil
	}
	u := f.Usage
	ratio := float64(u.PeakMemoryBytes) / float64(u.MemoryLimitBytes)
	if ratio < t.MemoryPressureRatio {
		return nil
	}
	limitBytes := suggestedMemoryBytes(u.PeakMemoryBytes)

	var b strings.Builder
	fmt.Fprintf(&b, "Peak working set reached **%s of the %s limit (%.0f%%)**. A job this close to its "+
		"limit gets OOMKilled the moment a build step allocates a little more — and an OOMKill "+
		"looks like a flaky failure, not a resource problem.\n\n",
		mebibytes(u.PeakMemoryBytes), mebibytes(u.MemoryLimitBytes), ratio*100)
	b.WriteString("Raise the ceiling with GitLab CI variables:\n\n")
	b.WriteString("```yaml\nvariables:\n")
	fmt.Fprintf(&b, "  KUBERNETES_MEMORY_REQUEST: %q\n", mebibytes(suggestedRequestBytes(u.MemoryRequestBytes, u.PeakMemoryBytes, limitBytes)))
	fmt.Fprintf(&b, "  KUBERNETES_MEMORY_LIMIT: %q\n", mebibytes(limitBytes))
	b.WriteString("```\n")

	return []Advice{{
		Job:   f.Name,
		Rule:  "memory-pressure",
		Title: "🧠 Memory near the limit",
		Body:  b.String(),
	}}
}

// suggestedMemoryBytes proposes 1.5x the measured peak, rounded up to the next
// 128 MiB — headroom for growth without wasting a whole node's worth.
func suggestedMemoryBytes(peakBytes uint64) uint64 {
	const step = 128 * 1024 * 1024
	want := peakBytes * 3 / 2
	return ((want + step - 1) / step) * step
}

// suggestedMemory renders suggestedMemoryBytes.
func suggestedMemory(peakBytes uint64) string {
	return mebibytes(suggestedMemoryBytes(peakBytes))
}

// suggestedRequestBytes keeps the measured request when there is one, falling
// back to the measured peak. It is clamped to the suggested limit: Kubernetes
// rejects a pod whose request exceeds its limit, and requests and limits are
// summed per pod, so the two measured sums are not guaranteed coherent.
func suggestedRequestBytes(requestBytes, peakBytes, limitBytes uint64) uint64 {
	want := requestBytes
	if want == 0 {
		want = peakBytes
	}
	return min(want, limitBytes)
}

// mebibytes renders a byte count as whole MiB, the unit Kubernetes quantities
// are usually written in.
func mebibytes(n uint64) string {
	return fmt.Sprintf("%dMi", n/(1024*1024))
}
