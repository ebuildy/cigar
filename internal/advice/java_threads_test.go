package advice

import (
	"strings"
	"testing"

	"gitlab.com/ebuildy/gitlab-ci-resources-bot/internal/metrics"
)

func TestDetectBuildTool(t *testing.T) {
	tests := []struct {
		name  string
		trace string
		want  string
		wants bool
	}{
		{name: "maven command", trace: "$ mvn -T 1C clean verify", want: "Maven", wants: true},
		{name: "maven banner", trace: "[INFO] Scanning for projects...", want: "Maven", wants: true},
		{name: "gradle wrapper", trace: "$ ./gradlew build --stacktrace", want: "Gradle", wants: true},
		{name: "gradle daemon", trace: "Starting a Gradle Daemon, 1 incompatible", want: "Gradle", wants: true},
		{name: "plain java", trace: "$ java -jar app.jar", want: "Java", wants: true},
		{name: "maven wrapper", trace: "$ ./mvnw -B verify", want: "Maven", wants: true},
		{name: "ansi-coloured maven", trace: "\x1b[32m$ mvn\x1b[0m -T 1C verify", want: "Maven", wants: true},
		{name: "ansi between java and flag", trace: "$ java\x1b[0m -jar app.jar", want: "Java", wants: true},
		{name: "no java", trace: "$ npm ci\n$ npm test", wants: false},
		{name: "empty trace", trace: "", wants: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := detectBuildTool(tt.trace)
			if ok != tt.wants {
				t.Fatalf("detectBuildTool(%q) ok = %v, want %v", tt.trace, ok, tt.wants)
			}
			if ok && got != tt.want {
				t.Fatalf("detectBuildTool(%q) = %q, want %q", tt.trace, got, tt.want)
			}
		})
	}
}

func TestJavaThreadsNeedsTraceOnlyWhenThrottled(t *testing.T) {
	th := Thresholds{ThrottleWarnRatio: 0.25}
	hot := Facts{Name: "build", Usage: &metrics.JobUsage{ThrottledRatio: 0.9}}
	cold := Facts{Name: "build", Usage: &metrics.JobUsage{ThrottledRatio: 0.01}}
	// Bound to a variable: a composite literal cannot appear directly in an
	// if/for header in Go.
	rule := javaThreads{}
	if !rule.NeedsTrace(hot, th) {
		t.Error("NeedsTrace must be true for a throttled job")
	}
	if rule.NeedsTrace(cold, th) {
		t.Error("NeedsTrace must be false for a job that is not throttled — no wasted API call")
	}
	if rule.NeedsTrace(Facts{Name: "build"}, th) {
		t.Error("NeedsTrace must be false without usage data")
	}
}

func TestJavaThreadsFires(t *testing.T) {
	th := Thresholds{ThrottleWarnRatio: 0.25}
	f := Facts{
		Name:  "build",
		Usage: &metrics.JobUsage{ThrottledRatio: 0.8, CPULimitCores: 2},
		Trace: "$ mvn -T 1C clean verify",
	}
	got := javaThreads{}.Check(f, th)
	if len(got) != 1 {
		t.Fatalf("Check returned %d advice, want 1", len(got))
	}
	a := got[0]
	if a.Rule != "java-threads" || a.Job != "build" {
		t.Fatalf("advice = %+v, want rule java-threads for job build", a)
	}
	for _, want := range []string{
		"Maven",
		"mvn -T 2",
		"-XX:ActiveProcessorCount=2",
		"cwiki.apache.org",
		"docs.gradle.org",
		"kestra.io",
	} {
		if !strings.Contains(a.Body, want) {
			t.Errorf("body missing %q:\n%s", want, a.Body)
		}
	}
}

func TestJavaThreadsAdvisesOnlyTheDetectedTool(t *testing.T) {
	th := Thresholds{ThrottleWarnRatio: 0.25}
	base := metrics.JobUsage{ThrottledRatio: 0.8, CPULimitCores: 2}
	rule := javaThreads{}

	gradle := rule.Check(Facts{Name: "build", Usage: &base, Trace: "$ ./gradlew build"}, th)
	if len(gradle) != 1 {
		t.Fatalf("gradle trace: %d advice, want 1", len(gradle))
	}
	if strings.Contains(gradle[0].Body, "mvn -T") {
		t.Errorf("gradle build advised to tune Maven:\n%s", gradle[0].Body)
	}
	if !strings.Contains(gradle[0].Body, "--max-workers=2") {
		t.Errorf("gradle build not advised about --max-workers:\n%s", gradle[0].Body)
	}

	plain := rule.Check(Facts{Name: "build", Usage: &base, Trace: "$ java -jar app.jar"}, th)
	if len(plain) != 1 {
		t.Fatalf("plain java trace: %d advice, want 1", len(plain))
	}
	if strings.Contains(plain[0].Body, "mvn -T") || strings.Contains(plain[0].Body, "gradlew") {
		t.Errorf("plain JVM run advised to tune Maven/Gradle:\n%s", plain[0].Body)
	}
	if !strings.Contains(plain[0].Body, "-XX:ActiveProcessorCount=2") {
		t.Errorf("plain JVM run missing the ActiveProcessorCount advice:\n%s", plain[0].Body)
	}
}

func TestJavaThreadsQuiet(t *testing.T) {
	th := Thresholds{ThrottleWarnRatio: 0.25}
	tests := []struct {
		name  string
		facts Facts
	}{
		{name: "throttled but not java", facts: Facts{
			Name: "build", Usage: &metrics.JobUsage{ThrottledRatio: 0.8}, Trace: "$ npm ci",
		}},
		{name: "java but not throttled", facts: Facts{
			Name: "build", Usage: &metrics.JobUsage{ThrottledRatio: 0.01}, Trace: "$ mvn verify",
		}},
		{name: "throttled, trace never fetched", facts: Facts{
			Name: "build", Usage: &metrics.JobUsage{ThrottledRatio: 0.8},
		}},
	}
	rule := javaThreads{}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := rule.Check(tt.facts, th); got != nil {
				t.Fatalf("Check fired when it should not: %+v", got)
			}
		})
	}
}

func TestThreadHint(t *testing.T) {
	tests := []struct {
		limit float64
		want  int
	}{
		{limit: 0, want: 2},   // no limit measured: a safe, explicit default
		{limit: 0.5, want: 1}, // sub-core: one thread
		{limit: 1, want: 1},
		{limit: 2.5, want: 2},
		{limit: 4, want: 4},
	}
	for _, tt := range tests {
		if got := threadHint(tt.limit); got != tt.want {
			t.Errorf("threadHint(%v) = %d, want %d", tt.limit, got, tt.want)
		}
	}
}
