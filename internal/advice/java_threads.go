package advice

import (
	"fmt"
	"regexp"
	"strings"
)

// javaThreads advises pinning Maven/Gradle/JVM parallelism to the pod's CPU
// limit when a throttled job's trace shows a Java build.
//
// The rule deliberately re-evaluates the throttling condition instead of
// keying off cpuThrottle having fired: rules must not depend on each other.
type javaThreads struct{}

func (javaThreads) Name() string { return "java-threads" }

// NeedsTrace asks for the trace only for throttled jobs — on a 30-job pipeline
// with 2 throttled jobs that is 2 extra GitLab calls, not 30.
func (javaThreads) NeedsTrace(f Facts, t Thresholds) bool { return throttled(f, t) }

func (javaThreads) Check(f Facts, t Thresholds) []Advice {
	if !throttled(f, t) {
		return nil
	}
	tool, ok := detectBuildTool(f.Trace)
	if !ok {
		return nil
	}
	n := threadHint(f.Usage.CPULimitCores)

	var b strings.Builder
	fmt.Fprintf(&b, "The trace shows a **%s** build. Maven's `-T 1C` and Gradle's default worker "+
		"count both size themselves from the *host* core count, and the JVM only derives "+
		"`Runtime.availableProcessors()` from the cgroup quota when a CPU **limit** is set. "+
		"With requests only, a JVM on a 64-core node builds 64-wide thread pools inside a "+
		"one-core slice — which is exactly what shows up as CFS throttling.\n\n", tool)
	fmt.Fprintf(&b, "Cap the parallelism to the CPU the pod actually gets (%d):\n\n", n)
	b.WriteString("```sh\n")
	fmt.Fprintf(&b, "mvn -T %d verify                 # an explicit count, not -T 1C\n", n)
	fmt.Fprintf(&b, "./gradlew build --max-workers=%d  # or org.gradle.workers.max=%d in gradle.properties\n", n, n)
	b.WriteString("```\n\n")
	fmt.Fprintf(&b, "If tests fork JVMs, cap Surefire's `forkCount` too, and pin the JVM's view of the "+
		"machine with `JAVA_TOOL_OPTIONS=-XX:ActiveProcessorCount=%d`. Never set "+
		"`-XX:-UseContainerSupport` — that disables container awareness entirely.\n\n", n)
	b.WriteString("- <https://cwiki.apache.org/confluence/display/MAVEN/Parallel+builds+in+Maven+3>\n")
	b.WriteString("- <https://docs.gradle.org/current/userguide/command_line_interface.html>\n")
	b.WriteString("- <https://kestra.io/docs/administrator-guide/jvm-cpu-limits>\n")

	return []Advice{{
		Job:   f.Name,
		Rule:  "java-threads",
		Title: "☕ Java build parallelism",
		Body:  b.String(),
	}}
}

// buildTools maps a display name to the trace patterns that identify it. Order
// matters: the most specific tool wins, so Maven and Gradle are tried before
// the generic Java match.
var buildTools = []struct {
	name string
	re   *regexp.Regexp
}{
	{"Maven", regexp.MustCompile(`(?i)\bmvn\b|\[INFO\] Scanning for projects|maven-\w+-plugin`)},
	{"Gradle", regexp.MustCompile(`(?i)\bgradlew?\b|Welcome to Gradle|Starting a Gradle Daemon`)},
	{"Java", regexp.MustCompile(`(?i)\bjava\s+-|openjdk`)},
}

// detectBuildTool reports which Java build tool the trace shows, if any.
func detectBuildTool(trace string) (string, bool) {
	if trace == "" {
		return "", false
	}
	for _, bt := range buildTools {
		if bt.re.MatchString(trace) {
			return bt.name, true
		}
	}
	return "", false
}

// threadHint is the parallelism to suggest for a pod limited to limitCores:
// the whole cores available, never below 1. With no limit measured it suggests
// a conservative explicit 2 rather than leaving the default host-wide value.
func threadHint(limitCores float64) int {
	if limitCores <= 0 {
		return 2
	}
	if n := int(limitCores); n >= 1 {
		return n
	}
	return 1
}
