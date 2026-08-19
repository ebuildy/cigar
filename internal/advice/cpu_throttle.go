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
	if f.Usage == nil || t.ThrottleWarnRatio <= 0 {
		return nil
	}
	// No breakdown (container-level series absent): fall back to the pod-level
	// ratio and the build container's variables — what this rule did before
	// per-container usage existed.
	if len(f.Usage.Containers) == 0 {
		if !throttled(f, t) {
			return nil
		}
		return []Advice{throttleAdvice(f.Name, "", f.Usage.ThrottledRatio,
			f.Usage.CPURequestCores, f.Usage.CPULimitCores)}
	}
	var out []Advice
	for _, c := range f.Usage.Containers {
		r, ok := c.ThrottledRatio()
		if !ok || r < t.ThrottleWarnRatio {
			continue
		}
		out = append(out, throttleAdvice(f.Name, c.Name, r, c.CPURequestCores, c.CPULimitCores))
	}
	return out
}

// containerVars maps a runner-pod container to the GitLab CI variables that
// govern its CPU. An empty or unrecognized container gets the build variables.
func containerVars(container string) (request, limit string) {
	switch {
	case container == "helper":
		return "KUBERNETES_HELPER_CPU_REQUEST", "KUBERNETES_HELPER_CPU_LIMIT"
	case strings.HasPrefix(container, "svc-"):
		return "KUBERNETES_SERVICE_CPU_REQUEST", "KUBERNETES_SERVICE_CPU_LIMIT"
	default:
		return "KUBERNETES_CPU_REQUEST", "KUBERNETES_CPU_LIMIT"
	}
}

// throttleAdvice renders one throttling finding. container is empty when no
// breakdown was available and the finding is about the pod as a whole.
func throttleAdvice(job, container string, ratio, requestCores, limitCores float64) Advice {
	var b strings.Builder

	subject, title := "This job", "⚠️ CPU throttling"
	if container != "" {
		subject = fmt.Sprintf("The `%s` container", container)
		title = fmt.Sprintf("⚠️ CPU throttling — %s", container)
	}
	fmt.Fprintf(&b, "%s spent **%.0f%%** of its CPU periods throttled", subject, ratio*100)
	if limitCores > 0 {
		fmt.Fprintf(&b, ", against a limit of %s", millicores(limitCores))
	} else {
		b.WriteString(" (no CPU limit series was found for this pod)")
	}
	b.WriteString(". The runner had less CPU than the job asked for, so wall-clock time is inflated.\n\n")

	if container == "helper" {
		b.WriteString("The helper container runs `git clone`, artifact upload/download and the cache. Throttling it stretches every job's setup and teardown without ever showing up in the job's own script time.\n\n")
	}
	if strings.HasPrefix(container, "svc-") {
		b.WriteString("GitLab has no per-service variable: the settings below apply to **every** `services:` container of the job.\n\n")
	}

	request, limit := containerVars(container)
	b.WriteString("Raise the allowance with GitLab CI variables, on the job or on the project:\n\n")
	b.WriteString("```yaml\nvariables:\n")
	fmt.Fprintf(&b, "  %s: %q\n", request, suggestedCPURequest(requestCores, limitCores))
	fmt.Fprintf(&b, "  %s: %q\n", limit, suggestedCPULimit(limitCores))
	b.WriteString("```\n")

	return Advice{Job: job, Rule: "cpu-throttle", Title: title, Body: b.String()}
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
