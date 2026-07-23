package advice

import (
	"fmt"
	"strings"
)

// cpuThrottle advises raising the job's CPU allowance when the container spent
// too many CFS periods throttled.
type cpuThrottle struct{}

func (cpuThrottle) Name() string { return "cpu-throttle" }

func (cpuThrottle) Check(f Facts, t Thresholds) []Advice {
	if !throttled(f, t) {
		return nil
	}
	u := f.Usage

	var b strings.Builder
	fmt.Fprintf(&b, "This job spent **%.0f%%** of its CPU periods throttled", u.ThrottledRatio*100)
	if u.CPULimitCores > 0 {
		fmt.Fprintf(&b, ", against a limit of %s", millicores(u.CPULimitCores))
	} else {
		b.WriteString(" (no CPU limit series was found for this pod)")
	}
	b.WriteString(". The runner had less CPU than the job asked for, so wall-clock time is inflated.\n\n")
	b.WriteString("Raise the allowance with GitLab CI variables, on the job or on the project:\n\n")
	b.WriteString("```yaml\nvariables:\n")
	fmt.Fprintf(&b, "  KUBERNETES_CPU_REQUEST: %q\n", suggestedCPURequest(u.CPURequestCores, u.CPULimitCores))
	fmt.Fprintf(&b, "  KUBERNETES_CPU_LIMIT: %q\n", suggestedCPULimit(u.CPULimitCores))
	b.WriteString("```\n")

	return []Advice{{
		Job:   f.Name,
		Rule:  "cpu-throttle",
		Title: "⚠️ CPU throttling",
		Body:  b.String(),
	}}
}

// suggestedCPULimitMillis doubles the current limit, rounded up to the next
// 100m. An absent limit series (0) means no limit was set: propose one core.
func suggestedCPULimitMillis(limitCores float64) int64 {
	if limitCores <= 0 {
		return 1000
	}
	m := int64(limitCores*2*1000 + 0.5)
	return ((m + 99) / 100) * 100
}

// suggestedCPULimit renders the suggested limit. With no limit series at all we
// say "1" rather than "1000m" — a whole core is how people write that.
func suggestedCPULimit(limitCores float64) string {
	if limitCores <= 0 {
		return "1"
	}
	return fmt.Sprintf("%dm", suggestedCPULimitMillis(limitCores))
}

// suggestedCPURequest keeps the current request when one is set — the request
// is what the scheduler reserves, and the throttling comes from the limit. It
// is clamped to the suggested limit: Kubernetes rejects a pod whose request
// exceeds its limit, and requests and limits are summed per pod, so the two
// measured sums are not guaranteed to be coherent with each other.
func suggestedCPURequest(requestCores, limitCores float64) string {
	if requestCores <= 0 {
		return suggestedCPULimit(limitCores)
	}
	limitMillis := suggestedCPULimitMillis(limitCores)
	if reqMillis := int64(requestCores*1000 + 0.5); reqMillis > limitMillis {
		return fmt.Sprintf("%dm", limitMillis)
	}
	return millicores(requestCores)
}
