# Advise Command Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add an `advise` command — as a reply to the bot's MR report note and as a `bot advise` CLI subcommand — that runs a pluggable set of rules over a pipeline's jobs and prints actionable fixes, or `You are all good dude!` when there is nothing to fix.

**Architecture:** A new pure package `internal/advice` holds the rules (`Rule` interface), a registry, an `Engine`, and the markdown renderer — no I/O, no `context`, no `error` in `Check`. `reporter.Advise` gathers `advice.Facts` per job (usage, duration, and the job trace only when a rule asks for it) and runs the engine. `internal/command` and `cmd/bot/advise.go` are two thin surfaces over that one path.

**Tech Stack:** Go ≥ 1.26, `spf13/cobra`, `go.uber.org/zap`, standard-library `regexp`/`text` formatting. Tests are table-driven with golden files; `mise r test` runs everything with `-race`.

**Spec:** `docs/superpowers/specs/2026-07-23-advise-command-design.md`

**Branch:** `feat/interactive-report-commands` (the spec builds on `internal/command`, which exists only there).

**Deviation from the spec (deliberate):** the spec said the job matcher would move to `cmd/bot/deps.go`. It cannot — `reporter.Advise` needs the same matcher, and `reporter` must not import `main`. Task 7 moves it to `internal/gitlab` instead, where both callers already import it, along with a shared `gitlab.ErrJobNotFound` sentinel.

---

## File Structure

**Created:**

| File | Responsibility |
| --- | --- |
| `internal/advice/advice.go` | `Facts`, `Thresholds`, `Advice`, `Rule`, `TraceConsumer`, `Engine`, `New`, shared rule helpers |
| `internal/advice/rules.go` | The ordered registry of built-in rules + `Register` |
| `internal/advice/cpu_throttle.go` | The `cpu-throttle` rule |
| `internal/advice/java_threads.go` | The `java-threads` rule + build-tool trace detection |
| `internal/advice/long_job.go` | The `long-job` rule |
| `internal/advice/memory_pressure.go` | The `memory-pressure` rule |
| `internal/advice/render.go` | `Render` — advice slice → markdown |
| `internal/advice/*_test.go`, `internal/advice/testdata/*.md` | One test file per rule, engine tests, golden files |
| `internal/gitlab/find.go` | `FindJob` matcher + `ErrJobNotFound`, shared by `reporter` and `cmd/bot` |
| `internal/reporter/advise.go` | `Reporter.Advise` — jobs → Facts → engine |
| `cmd/bot/advise.go` | `bot advise --project <id> <pipeline-id> [--job <name>]` |

**Modified:** `internal/command/command.go` (parse `advise`), `internal/command/handler.go` (`Advisor` + reply), `internal/config/config.go` (two env vars), `cmd/bot/deps.go` (engine + advisor wiring), `cmd/bot/serve.go` (pass the reporter into the handler), `cmd/bot/main.go` (register the subcommand), `cmd/bot/run.go` (use `gitlab.FindJob`), `internal/e2e/e2e_test.go`, `deploy/chart/cigar/{values.yaml,templates/deployment.yaml}`, `docs/usage.md`, `README.md`.

---

## Task 1: Advice core — types, Rule interface, Engine

**Files:**
- Create: `internal/advice/advice.go`
- Create: `internal/advice/rules.go`
- Test: `internal/advice/advice_test.go`

- [ ] **Step 1: Write the failing test**

`internal/advice/advice_test.go`:

```go
package advice

import (
	"testing"
	"time"

	"gitlab.com/ebuildy/gitlab-ci-resources-bot/internal/metrics"
)

// stubRule is a rule that always fires, for engine-level tests.
type stubRule struct {
	name       string
	wantsTrace bool
}

func (s stubRule) Name() string { return s.name }

func (s stubRule) Check(f Facts, _ Thresholds) []Advice {
	return []Advice{{Job: f.Name, Rule: s.name, Title: s.name, Body: "body of " + s.name}}
}

// traceRule additionally implements TraceConsumer.
type traceRule struct{ stubRule }

func (t traceRule) NeedsTrace(Facts, Thresholds) bool { return t.wantsTrace }

func TestEngineAnalyzeRunsRulesInOrder(t *testing.T) {
	e := newEngine(Thresholds{}, []Rule{stubRule{name: "first"}, stubRule{name: "second"}})
	got := e.Analyze(Facts{Name: "build"})
	if len(got) != 2 {
		t.Fatalf("Analyze returned %d advice, want 2", len(got))
	}
	if got[0].Rule != "first" || got[1].Rule != "second" {
		t.Fatalf("rules ran out of order: %q then %q", got[0].Rule, got[1].Rule)
	}
	if got[0].Job != "build" {
		t.Fatalf("Job = %q, want %q", got[0].Job, "build")
	}
}

func TestEngineNeedsTrace(t *testing.T) {
	tests := []struct {
		name  string
		rules []Rule
		want  bool
	}{
		{name: "no trace consumers", rules: []Rule{stubRule{name: "plain"}}, want: false},
		{name: "consumer declines", rules: []Rule{traceRule{stubRule{name: "t", wantsTrace: false}}}, want: false},
		{name: "consumer asks", rules: []Rule{stubRule{name: "plain"}, traceRule{stubRule{name: "t", wantsTrace: true}}}, want: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newEngine(Thresholds{}, tt.rules)
			if got := e.NeedsTrace(Facts{Name: "build"}); got != tt.want {
				t.Fatalf("NeedsTrace = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestNewSelectsRules(t *testing.T) {
	th := Thresholds{ThrottleWarnRatio: 0.25, LongJob: 10 * time.Minute, MemoryPressureRatio: 0.9}

	all, err := New(th, nil)
	if err != nil {
		t.Fatalf("New(nil): %v", err)
	}
	if len(all.rules) != len(registry) {
		t.Fatalf("New(nil) selected %d rules, want all %d", len(all.rules), len(registry))
	}

	one, err := New(th, []string{"long-job"})
	if err != nil {
		t.Fatalf("New([long-job]): %v", err)
	}
	if len(one.rules) != 1 || one.rules[0].Name() != "long-job" {
		t.Fatalf("New([long-job]) selected %v", ruleNames(one.rules))
	}

	if _, err := New(th, []string{"nope"}); err == nil {
		t.Fatal("New with an unknown rule name must fail")
	}
}

// TestThrottledHelper pins the shared predicate both CPU rules use.
func TestThrottledHelper(t *testing.T) {
	th := Thresholds{ThrottleWarnRatio: 0.25}
	tests := []struct {
		name  string
		facts Facts
		want  bool
	}{
		{name: "no usage", facts: Facts{}, want: false},
		{name: "below threshold", facts: Facts{Usage: &metrics.JobUsage{ThrottledRatio: 0.1}}, want: false},
		{name: "at threshold", facts: Facts{Usage: &metrics.JobUsage{ThrottledRatio: 0.25}}, want: true},
		{name: "above threshold", facts: Facts{Usage: &metrics.JobUsage{ThrottledRatio: 0.9}}, want: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := throttled(tt.facts, th); got != tt.want {
				t.Fatalf("throttled = %v, want %v", got, tt.want)
			}
		})
	}
}

func ruleNames(rules []Rule) []string {
	out := make([]string, len(rules))
	for i, r := range rules {
		out[i] = r.Name()
	}
	return out
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/advice/ -run 'TestEngine|TestNew|TestThrottled' -v`
Expected: FAIL — the package does not compile (`undefined: Facts`, `undefined: newEngine`, …).

- [ ] **Step 3: Write the implementation**

`internal/advice/advice.go`:

```go
// Package advice turns one pipeline's measured job usage into actionable
// recommendations. Rules are pluggable: each implements Rule in its own file
// and is listed once in rules.go. Rules are pure — all I/O happens before the
// engine runs, which is what makes them golden-testable.
package advice

import (
	"fmt"
	"time"

	"gitlab.com/ebuildy/gitlab-ci-resources-bot/internal/metrics"
)

// Facts is everything the rules know about one job. Trace is populated only
// when some rule asked for it (see TraceConsumer); it is empty otherwise.
type Facts struct {
	Stage    string
	Name     string
	Duration time.Duration
	Usage    *metrics.JobUsage // nil when the pod or its metrics were unavailable
	Trace    string
}

// Thresholds are the tunables shared by the rules, sourced from config.
type Thresholds struct {
	ThrottleWarnRatio   float64       // THROTTLE_WARN_RATIO, default 0.25
	LongJob             time.Duration // LONG_JOB_DURATION, default 10m
	MemoryPressureRatio float64       // MEMORY_PRESSURE_RATIO, default 0.9
}

// Advice is one actionable finding about one job. Job is empty for
// pipeline-wide advice.
type Advice struct {
	Job   string
	Rule  string // stable id, matches the Rule's Name()
	Title string // one line, rendered as a bold heading
	Body  string // markdown, may be multi-line and contain links
}

// Rule is one advice check: stateless, pure, independently testable. Check
// returns nil when the rule does not fire.
type Rule interface {
	Name() string
	Check(f Facts, t Thresholds) []Advice
}

// TraceConsumer is the optional capability a rule implements when it needs the
// job's trace log. The engine asks every rule before any fetch happens; the
// trace is pulled only when at least one rule says yes, and Check then runs
// with Facts.Trace populated.
type TraceConsumer interface {
	NeedsTrace(f Facts, t Thresholds) bool
}

// Engine runs a fixed set of rules over a job's facts.
type Engine struct {
	rules []Rule
	th    Thresholds
}

// New builds an engine. A nil enabled list selects every registered rule in
// registration order; otherwise only the named rules are selected, in
// registration order, and an unknown name is an error. This is the seam a
// future ADVICE_RULES environment variable plugs into.
func New(th Thresholds, enabled []string) (*Engine, error) {
	if enabled == nil {
		return newEngine(th, registry), nil
	}
	want := make(map[string]bool, len(enabled))
	for _, name := range enabled {
		want[name] = true
	}
	var rules []Rule
	for _, r := range registry {
		if want[r.Name()] {
			rules = append(rules, r)
			delete(want, r.Name())
		}
	}
	for name := range want {
		return nil, fmt.Errorf("unknown advice rule %q", name)
	}
	return newEngine(th, rules), nil
}

// newEngine builds an engine over an explicit rule list. Tests use it to drive
// stub rules without touching the global registry.
func newEngine(th Thresholds, rules []Rule) *Engine {
	return &Engine{rules: rules, th: th}
}

// NeedsTrace reports whether any rule wants the job's trace log for these
// facts. Callers fetch the trace only when it returns true.
func (e *Engine) NeedsTrace(f Facts) bool {
	for _, r := range e.rules {
		if tc, ok := r.(TraceConsumer); ok && tc.NeedsTrace(f, e.th) {
			return true
		}
	}
	return false
}

// Analyze runs every rule over one job's facts, in registration order.
func (e *Engine) Analyze(f Facts) []Advice {
	var out []Advice
	for _, r := range e.rules {
		out = append(out, r.Check(f, e.th)...)
	}
	return out
}

// throttled is the shared CPU-throttling predicate. Two rules act on the same
// condition; sharing a helper keeps them independent of each other's firing.
func throttled(f Facts, t Thresholds) bool {
	return f.Usage != nil && t.ThrottleWarnRatio > 0 && f.Usage.ThrottledRatio >= t.ThrottleWarnRatio
}

// millicores renders a Kubernetes CPU quantity as millicores (e.g. 250m).
func millicores(cores float64) string {
	return fmt.Sprintf("%dm", int64(cores*1000+0.5))
}
```

`internal/advice/rules.go`:

```go
package advice

// registry is every known rule, in report order: the built-ins first, then
// anything added by Register.
//
// Adding an adviser is one new file implementing Rule plus one line here.
var registry = []Rule{
	cpuThrottle{},
	javaThreads{},
	longJob{},
	memoryPressure{},
}

// Register appends a rule to the registry so New can select it. It exists so
// rules can be added without editing this file — call it from an init in the
// rule's own file, or from wiring code.
func Register(r Rule) {
	registry = append(registry, r)
}
```

- [ ] **Step 4: Add empty rule stubs so the package compiles**

The four rule types are implemented in Tasks 2–5. Create the files now with a
minimal non-firing implementation so `registry` compiles:

`internal/advice/cpu_throttle.go`:

```go
package advice

// cpuThrottle advises raising the job's CPU allowance when the container spent
// too many CFS periods throttled.
type cpuThrottle struct{}

func (cpuThrottle) Name() string { return "cpu-throttle" }

func (cpuThrottle) Check(Facts, Thresholds) []Advice { return nil }
```

`internal/advice/java_threads.go`:

```go
package advice

// javaThreads advises pinning Maven/Gradle/JVM parallelism to the pod's CPU
// limit when a throttled job's trace shows a Java build.
type javaThreads struct{}

func (javaThreads) Name() string { return "java-threads" }

func (javaThreads) NeedsTrace(Facts, Thresholds) bool { return false }

func (javaThreads) Check(Facts, Thresholds) []Advice { return nil }
```

`internal/advice/long_job.go`:

```go
package advice

// longJob advises splitting jobs that run longer than the configured budget.
type longJob struct{}

func (longJob) Name() string { return "long-job" }

func (longJob) Check(Facts, Thresholds) []Advice { return nil }
```

`internal/advice/memory_pressure.go`:

```go
package advice

// memoryPressure advises raising the memory limit when peak working set came
// close to it (OOMKill risk).
type memoryPressure struct{}

func (memoryPressure) Name() string { return "memory-pressure" }

func (memoryPressure) Check(Facts, Thresholds) []Advice { return nil }
```

- [ ] **Step 5: Run the tests to verify they pass**

Run: `go test ./internal/advice/ -v`
Expected: PASS — all five tests.

- [ ] **Step 6: Commit**

```bash
git add internal/advice/
git commit -m "feat(advice): rule interface, registry and engine"
```

---

## Task 2: The `cpu-throttle` rule

**Files:**
- Modify: `internal/advice/cpu_throttle.go` (replace the stub from Task 1)
- Test: `internal/advice/cpu_throttle_test.go`

- [ ] **Step 1: Write the failing test**

`internal/advice/cpu_throttle_test.go`:

```go
package advice

import (
	"strings"
	"testing"

	"gitlab.com/ebuildy/gitlab-ci-resources-bot/internal/metrics"
)

func TestCPUThrottleFires(t *testing.T) {
	th := Thresholds{ThrottleWarnRatio: 0.25}
	f := Facts{Name: "compile", Usage: &metrics.JobUsage{
		ThrottledRatio:  0.41,
		CPURequestCores: 0.25,
		CPULimitCores:   0.5,
	}}
	got := cpuThrottle{}.Check(f, th)
	if len(got) != 1 {
		t.Fatalf("Check returned %d advice, want 1", len(got))
	}
	a := got[0]
	if a.Rule != "cpu-throttle" || a.Job != "compile" {
		t.Fatalf("advice = %+v, want rule cpu-throttle for job compile", a)
	}
	for _, want := range []string{"41%", "500m", "KUBERNETES_CPU_REQUEST", "KUBERNETES_CPU_LIMIT", "1000m"} {
		if !strings.Contains(a.Body, want) {
			t.Errorf("body missing %q:\n%s", want, a.Body)
		}
	}
}

func TestCPUThrottleUnsetLimit(t *testing.T) {
	th := Thresholds{ThrottleWarnRatio: 0.25}
	f := Facts{Name: "compile", Usage: &metrics.JobUsage{ThrottledRatio: 0.5}}
	got := cpuThrottle{}.Check(f, th)
	if len(got) != 1 {
		t.Fatalf("Check returned %d advice, want 1", len(got))
	}
	// An absent limit series is reported as absent, never as a measured 0.
	if !strings.Contains(got[0].Body, "no CPU limit") {
		t.Errorf("body should say the limit series was absent:\n%s", got[0].Body)
	}
	if !strings.Contains(got[0].Body, `KUBERNETES_CPU_LIMIT: "1"`) {
		t.Errorf("body should suggest a 1-core limit when none is set:\n%s", got[0].Body)
	}
}

func TestCPUThrottleQuiet(t *testing.T) {
	th := Thresholds{ThrottleWarnRatio: 0.25}
	tests := []struct {
		name  string
		facts Facts
	}{
		{name: "no usage", facts: Facts{Name: "compile"}},
		{name: "below threshold", facts: Facts{Name: "compile", Usage: &metrics.JobUsage{ThrottledRatio: 0.2}}},
		{name: "zero throttling", facts: Facts{Name: "compile", Usage: &metrics.JobUsage{}}},
	}
	rule := cpuThrottle{} // bound to a variable: a composite literal cannot
	// appear directly in an if/for header in Go.
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := rule.Check(tt.facts, th); got != nil {
				t.Fatalf("Check fired when it should not: %+v", got)
			}
		})
	}
}

func TestSuggestedCPULimit(t *testing.T) {
	tests := []struct {
		limit float64
		want  string
	}{
		{limit: 0, want: "1"},
		{limit: 0.5, want: "1000m"},
		{limit: 0.25, want: "500m"},
		{limit: 0.35, want: "700m"},
		{limit: 2, want: "4000m"},
	}
	for _, tt := range tests {
		if got := suggestedCPULimit(tt.limit); got != tt.want {
			t.Errorf("suggestedCPULimit(%v) = %q, want %q", tt.limit, got, tt.want)
		}
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/advice/ -run TestCPUThrottle -v`
Expected: FAIL — `undefined: suggestedCPULimit`, and `Check returned 0 advice, want 1`.

- [ ] **Step 3: Write the implementation**

Replace `internal/advice/cpu_throttle.go` with:

```go
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

// suggestedCPULimit doubles the current limit, rounded up to the next 100m.
// An absent limit series (0) means no limit was set: propose one full core.
func suggestedCPULimit(limitCores float64) string {
	if limitCores <= 0 {
		return "1"
	}
	m := int64(limitCores*2*1000 + 0.5)
	m = ((m + 99) / 100) * 100
	return fmt.Sprintf("%dm", m)
}

// suggestedCPURequest keeps the current request when one is set — the request
// is what the scheduler reserves, and the throttling comes from the limit.
// With no request measured, it mirrors the suggested limit.
func suggestedCPURequest(requestCores, limitCores float64) string {
	if requestCores > 0 {
		return millicores(requestCores)
	}
	return suggestedCPULimit(limitCores)
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/advice/ -run TestCPUThrottle -v`
Expected: PASS — 3 tests plus their subtests.

- [ ] **Step 5: Commit**

```bash
git add internal/advice/cpu_throttle.go internal/advice/cpu_throttle_test.go
git commit -m "feat(advice): cpu-throttle rule"
```

---

## Task 3: The `java-threads` rule

**Files:**
- Modify: `internal/advice/java_threads.go` (replace the stub from Task 1)
- Test: `internal/advice/java_threads_test.go`

- [ ] **Step 1: Write the failing test**

`internal/advice/java_threads_test.go`:

```go
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
		"--max-workers=2",
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
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/advice/ -run 'TestDetectBuildTool|TestJavaThreads|TestThreadHint' -v`
Expected: FAIL — `undefined: detectBuildTool`, `undefined: threadHint`.

- [ ] **Step 3: Write the implementation**

Replace `internal/advice/java_threads.go` with:

```go
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
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/advice/ -run 'TestDetectBuildTool|TestJavaThreads|TestThreadHint' -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/advice/java_threads.go internal/advice/java_threads_test.go
git commit -m "feat(advice): java-threads rule with trace-based build-tool detection"
```

---

## Task 4: The `long-job` rule

**Files:**
- Modify: `internal/advice/long_job.go` (replace the stub from Task 1)
- Test: `internal/advice/long_job_test.go`

- [ ] **Step 1: Write the failing test**

`internal/advice/long_job_test.go`:

```go
package advice

import (
	"strings"
	"testing"
	"time"
)

func TestLongJobFires(t *testing.T) {
	th := Thresholds{LongJob: 10 * time.Minute}
	f := Facts{Name: "integration", Duration: 23*time.Minute + 30*time.Second}
	got := longJob{}.Check(f, th)
	if len(got) != 1 {
		t.Fatalf("Check returned %d advice, want 1", len(got))
	}
	a := got[0]
	if a.Rule != "long-job" || a.Job != "integration" {
		t.Fatalf("advice = %+v, want rule long-job for job integration", a)
	}
	for _, want := range []string{"23m30s", "10m0s", "parallel"} {
		if !strings.Contains(a.Body, want) {
			t.Errorf("body missing %q:\n%s", want, a.Body)
		}
	}
}

// TestLongJobFiresWithoutUsage proves duration-based advice survives a job
// whose runner pod never correlated — the only rule that works without metrics.
func TestLongJobFiresWithoutUsage(t *testing.T) {
	th := Thresholds{LongJob: 10 * time.Minute}
	got := longJob{}.Check(Facts{Name: "integration", Duration: 30 * time.Minute}, th)
	if len(got) != 1 {
		t.Fatalf("Check returned %d advice, want 1 (no Usage required)", len(got))
	}
}

func TestLongJobQuiet(t *testing.T) {
	tests := []struct {
		name  string
		th    Thresholds
		facts Facts
	}{
		{name: "under budget", th: Thresholds{LongJob: 10 * time.Minute}, facts: Facts{Duration: 3 * time.Minute}},
		{name: "exactly at budget", th: Thresholds{LongJob: 10 * time.Minute}, facts: Facts{Duration: 10 * time.Minute}},
		{name: "threshold disabled", th: Thresholds{LongJob: 0}, facts: Facts{Duration: 3 * time.Hour}},
	}
	rule := longJob{}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := rule.Check(tt.facts, tt.th); got != nil {
				t.Fatalf("Check fired when it should not: %+v", got)
			}
		})
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/advice/ -run TestLongJob -v`
Expected: FAIL — `Check returned 0 advice, want 1` (the stub never fires).

- [ ] **Step 3: Write the implementation**

Replace `internal/advice/long_job.go` with:

```go
package advice

import (
	"fmt"
	"strings"
	"time"
)

// longJob advises splitting jobs that run longer than the configured budget.
// It is the only rule that works without Usage: the duration comes from
// GitLab, so it still fires when the runner pod never correlated.
type longJob struct{}

func (longJob) Name() string { return "long-job" }

func (longJob) Check(f Facts, t Thresholds) []Advice {
	if t.LongJob <= 0 || f.Duration <= t.LongJob {
		return nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "This job ran for **%s**, over the %s budget. It is the pipeline's "+
		"critical path: every push waits on it.\n\n", f.Duration.Round(time.Second), t.LongJob)
	b.WriteString("If the work splits, split it — several jobs in one stage run in parallel on separate runners:\n\n")
	b.WriteString("- Break independent steps (lint, unit, integration) into separate jobs.\n")
	b.WriteString("- Shard a long test suite with `parallel:` (or `parallel:matrix:`) and a shard flag.\n")
	b.WriteString("- Move setup that repeats every run into `cache:` or a prebuilt image.\n")

	return []Advice{{
		Job:   f.Name,
		Rule:  "long-job",
		Title: "⏱️ Long job",
		Body:  b.String(),
	}}
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/advice/ -run TestLongJob -v`
Expected: PASS — 3 tests plus subtests.

- [ ] **Step 5: Commit**

```bash
git add internal/advice/long_job.go internal/advice/long_job_test.go
git commit -m "feat(advice): long-job rule"
```

---

## Task 5: The `memory-pressure` rule

**Files:**
- Modify: `internal/advice/memory_pressure.go` (replace the stub from Task 1)
- Test: `internal/advice/memory_pressure_test.go`

- [ ] **Step 1: Write the failing test**

`internal/advice/memory_pressure_test.go`:

```go
package advice

import (
	"strings"
	"testing"

	"gitlab.com/ebuildy/gitlab-ci-resources-bot/internal/metrics"
)

const mib = 1024 * 1024

func TestMemoryPressureFires(t *testing.T) {
	th := Thresholds{MemoryPressureRatio: 0.9}
	f := Facts{Name: "unit", Usage: &metrics.JobUsage{
		PeakMemoryBytes:  480 * mib,
		MemoryLimitBytes: 512 * mib,
	}}
	got := memoryPressure{}.Check(f, th)
	if len(got) != 1 {
		t.Fatalf("Check returned %d advice, want 1", len(got))
	}
	a := got[0]
	if a.Rule != "memory-pressure" || a.Job != "unit" {
		t.Fatalf("advice = %+v, want rule memory-pressure for job unit", a)
	}
	for _, want := range []string{"94%", "480Mi", "512Mi", "KUBERNETES_MEMORY_LIMIT", "OOMKill"} {
		if !strings.Contains(a.Body, want) {
			t.Errorf("body missing %q:\n%s", want, a.Body)
		}
	}
}

// TestMemoryPressureAbsentLimit pins the "absent != zero" rule: with no limit
// series there is no denominator, so the rule must stay silent rather than
// treat 0 as a limit the job exceeded.
func TestMemoryPressureAbsentLimit(t *testing.T) {
	th := Thresholds{MemoryPressureRatio: 0.9}
	f := Facts{Name: "unit", Usage: &metrics.JobUsage{PeakMemoryBytes: 900 * mib}}
	rule := memoryPressure{}
	if got := rule.Check(f, th); got != nil {
		t.Fatalf("Check fired without a limit series: %+v", got)
	}
}

func TestMemoryPressureQuiet(t *testing.T) {
	th := Thresholds{MemoryPressureRatio: 0.9}
	tests := []struct {
		name  string
		facts Facts
	}{
		{name: "no usage", facts: Facts{Name: "unit"}},
		{name: "well under limit", facts: Facts{Name: "unit", Usage: &metrics.JobUsage{
			PeakMemoryBytes: 100 * mib, MemoryLimitBytes: 512 * mib,
		}}},
	}
	rule := memoryPressure{}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := rule.Check(tt.facts, th); got != nil {
				t.Fatalf("Check fired when it should not: %+v", got)
			}
		})
	}
}

func TestSuggestedMemory(t *testing.T) {
	tests := []struct {
		peak uint64
		want string
	}{
		{peak: 480 * mib, want: "768Mi"},  // 720Mi rounded up to the next 128Mi
		{peak: 100 * mib, want: "256Mi"},  // 150Mi rounded up
		{peak: 1024 * mib, want: "1536Mi"},
	}
	for _, tt := range tests {
		if got := suggestedMemory(tt.peak); got != tt.want {
			t.Errorf("suggestedMemory(%d) = %q, want %q", tt.peak, got, tt.want)
		}
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/advice/ -run 'TestMemoryPressure|TestSuggestedMemory' -v`
Expected: FAIL — `undefined: suggestedMemory`.

- [ ] **Step 3: Write the implementation**

Replace `internal/advice/memory_pressure.go` with:

```go
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

	var b strings.Builder
	fmt.Fprintf(&b, "Peak working set reached **%s of the %s limit (%.0f%%)**. A job this close to its "+
		"limit gets OOMKilled the moment a build step allocates a little more — and an OOMKill "+
		"looks like a flaky failure, not a resource problem.\n\n",
		mebibytes(u.PeakMemoryBytes), mebibytes(u.MemoryLimitBytes), ratio*100)
	b.WriteString("Raise the ceiling with GitLab CI variables:\n\n")
	b.WriteString("```yaml\nvariables:\n")
	if u.MemoryRequestBytes > 0 {
		fmt.Fprintf(&b, "  KUBERNETES_MEMORY_REQUEST: %q\n", mebibytes(u.MemoryRequestBytes))
	} else {
		fmt.Fprintf(&b, "  KUBERNETES_MEMORY_REQUEST: %q\n", mebibytes(u.PeakMemoryBytes))
	}
	fmt.Fprintf(&b, "  KUBERNETES_MEMORY_LIMIT: %q\n", suggestedMemory(u.PeakMemoryBytes))
	b.WriteString("```\n")

	return []Advice{{
		Job:   f.Name,
		Rule:  "memory-pressure",
		Title: "🧠 Memory near the limit",
		Body:  b.String(),
	}}
}

// suggestedMemory proposes 1.5x the measured peak, rounded up to the next
// 128 MiB — headroom for growth without wasting a whole node's worth.
func suggestedMemory(peakBytes uint64) string {
	const step = 128 * 1024 * 1024
	want := peakBytes * 3 / 2
	want = ((want + step - 1) / step) * step
	return mebibytes(want)
}

// mebibytes renders a byte count as whole MiB, the unit Kubernetes quantities
// are usually written in.
func mebibytes(n uint64) string {
	return fmt.Sprintf("%dMi", n/(1024*1024))
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/advice/ -run 'TestMemoryPressure|TestSuggestedMemory' -v`
Expected: PASS.

- [ ] **Step 5: Run the whole package with the race detector**

Run: `go test -race ./internal/advice/`
Expected: `ok  gitlab.com/ebuildy/gitlab-ci-resources-bot/internal/advice`

- [ ] **Step 6: Commit**

```bash
git add internal/advice/memory_pressure.go internal/advice/memory_pressure_test.go
git commit -m "feat(advice): memory-pressure rule"
```

---

## Task 6: `Render` — advice to markdown, with golden files

**Files:**
- Create: `internal/advice/render.go`
- Test: `internal/advice/render_test.go`
- Create: `internal/advice/testdata/advise.md`, `internal/advice/testdata/advise-clean.md` (generated in Step 4)

- [ ] **Step 1: Write the failing test**

`internal/advice/render_test.go`:

```go
package advice

import (
	"flag"
	"os"
	"path/filepath"
	"testing"
	"time"

	"gitlab.com/ebuildy/gitlab-ci-resources-bot/internal/metrics"
)

var update = flag.Bool("update", false, "update golden files")

func checkGolden(t *testing.T, name, got string) {
	t.Helper()
	golden := filepath.Join("testdata", name)
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
		t.Errorf("output does not match %s (run with -update to refresh):\n%s", golden, got)
	}
}

// TestRenderGolden runs every rule across two jobs, so the golden file is the
// readable proof of what a user actually receives.
func TestRenderGolden(t *testing.T) {
	th := Thresholds{ThrottleWarnRatio: 0.25, LongJob: 10 * time.Minute, MemoryPressureRatio: 0.9}
	e, err := New(th, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	var all []Advice
	all = append(all, e.Analyze(Facts{
		Stage:    "build",
		Name:     "compile",
		Duration: 22 * time.Minute,
		Trace:    "$ mvn -T 1C clean verify",
		Usage: &metrics.JobUsage{
			ThrottledRatio:     0.41,
			CPURequestCores:    0.25,
			CPULimitCores:      0.5,
			PeakMemoryBytes:    480 * 1024 * 1024,
			MemoryRequestBytes: 256 * 1024 * 1024,
			MemoryLimitBytes:   512 * 1024 * 1024,
		},
	})...)
	all = append(all, e.Analyze(Facts{
		Stage:    "test",
		Name:     "unit",
		Duration: 45 * time.Second,
		Usage: &metrics.JobUsage{
			ThrottledRatio:   0.02,
			CPULimitCores:    1,
			PeakMemoryBytes:  100 * 1024 * 1024,
			MemoryLimitBytes: 512 * 1024 * 1024,
		},
	})...)

	checkGolden(t, "advise.md", Render(12345, all))
}

func TestRenderCleanGolden(t *testing.T) {
	checkGolden(t, "advise-clean.md", Render(12345, nil))
}

func TestRenderCleanExactMessage(t *testing.T) {
	if got := Render(1, nil); got != "You are all good dude!" {
		t.Fatalf("Render with no advice = %q, want %q", got, "You are all good dude!")
	}
}

func TestRenderGroupsByJobInOrder(t *testing.T) {
	all := []Advice{
		{Job: "compile", Rule: "a", Title: "A", Body: "body a"},
		{Job: "unit", Rule: "b", Title: "B", Body: "body b"},
		{Job: "compile", Rule: "c", Title: "C", Body: "body c"},
	}
	got := Render(7, all)
	// Both of compile's findings must appear under one heading, before unit's.
	iCompile := indexOf(got, "#### `compile`")
	iUnit := indexOf(got, "#### `unit`")
	iA, iC := indexOf(got, "body a"), indexOf(got, "body c")
	if iCompile < 0 || iUnit < 0 {
		t.Fatalf("missing job headings in:\n%s", got)
	}
	if !(iCompile < iA && iA < iC && iC < iUnit) {
		t.Fatalf("compile's findings are not grouped before unit's:\n%s", got)
	}
	if n := countOf(got, "#### `compile`"); n != 1 {
		t.Fatalf("compile heading appears %d times, want 1", n)
	}
}

func indexOf(s, sub string) int { return strings.Index(s, sub) }

func countOf(s, sub string) int { return strings.Count(s, sub) }
```

Add `"strings"` to that file's imports.

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/advice/ -run TestRender -v`
Expected: FAIL — `undefined: Render`.

- [ ] **Step 3: Write the implementation**

`internal/advice/render.go`:

```go
package advice

import (
	"fmt"
	"strings"
)

// CleanMessage is what a pipeline with nothing to fix gets back.
const CleanMessage = "You are all good dude!"

// Render turns a pipeline's advice into the markdown body the note reply posts
// and the CLI prints. With no advice it returns exactly CleanMessage.
//
// Pipeline-wide advice (Advice.Job == "") comes first; per-job advice follows,
// grouped under one heading per job, in the order the jobs first appear.
func Render(pipelineID int64, all []Advice) string {
	if len(all) == 0 {
		return CleanMessage
	}
	var b strings.Builder
	fmt.Fprintf(&b, "### Advice for pipeline #%d\n\n", pipelineID)

	for _, a := range all {
		if a.Job == "" {
			writeAdvice(&b, a)
		}
	}
	for _, job := range jobOrder(all) {
		fmt.Fprintf(&b, "#### `%s`\n\n", job)
		for _, a := range all {
			if a.Job == job {
				writeAdvice(&b, a)
			}
		}
	}
	return b.String()
}

func writeAdvice(b *strings.Builder, a Advice) {
	fmt.Fprintf(b, "**%s**\n\n%s\n\n", a.Title, strings.TrimRight(a.Body, "\n"))
}

// jobOrder lists the distinct job names in first-appearance order.
func jobOrder(all []Advice) []string {
	seen := make(map[string]bool, len(all))
	var order []string
	for _, a := range all {
		if a.Job == "" || seen[a.Job] {
			continue
		}
		seen[a.Job] = true
		order = append(order, a.Job)
	}
	return order
}
```

- [ ] **Step 4: Generate the golden files and read them**

Run: `go test ./internal/advice/ -run TestRender -update`
Then read both files and confirm they are sane markdown:

```bash
cat internal/advice/testdata/advise.md
cat internal/advice/testdata/advise-clean.md
```

Expected in `advise.md`: an `### Advice for pipeline #12345` heading, a
``#### `compile` `` section carrying the CPU-throttling, Java-parallelism and
long-job findings, and **no** `#### unit` section (the unit job's numbers are
healthy, so no rule fires for it). Expected in `advise-clean.md`: exactly
`You are all good dude!` with no trailing newline.

If `advise.md` contains a `#### unit` section, a rule is firing when it should
not — fix the rule, do not edit the golden file.

- [ ] **Step 5: Run the tests to verify they pass**

Run: `go test -race ./internal/advice/ -v`
Expected: PASS — every test in the package.

- [ ] **Step 6: Commit**

```bash
git add internal/advice/render.go internal/advice/render_test.go internal/advice/testdata/
git commit -m "feat(advice): markdown rendering with golden files"
```

---

## Task 7: Share the job matcher in `internal/gitlab`

`reporter.Advise` needs the same "by ID, then by name" matcher `cmd/bot/run.go`
already has, and `reporter` cannot import `main`. Move it to `internal/gitlab`,
which both already import, together with the `ErrJobNotFound` sentinel the
command handler needs to phrase its refusal.

**Files:**
- Create: `internal/gitlab/find.go`
- Create: `internal/gitlab/find_test.go`
- Modify: `cmd/bot/run.go` (delete `findJob`, call `gitlab.FindJob`)
- Delete: `cmd/bot/run_test.go` (its only test moves to `find_test.go`)

- [ ] **Step 1: Write the failing test**

`internal/gitlab/find_test.go`:

```go
package gitlab

import "testing"

func TestFindJob(t *testing.T) {
	jobs := []Job{
		{ID: 101, Name: "build"},
		{ID: 102, Name: "test"},
		{ID: 103, Name: "42"}, // a job literally named "42"
	}
	tests := []struct {
		name   string
		sel    string
		wantID int64
		wantOK bool
	}{
		{name: "by numeric ID", sel: "102", wantID: 102, wantOK: true},
		{name: "by name", sel: "build", wantID: 101, wantOK: true},
		{name: "numeric ID wins over same-looking name", sel: "103", wantID: 103, wantOK: true},
		{name: "falls back to name when ID absent", sel: "42", wantID: 103, wantOK: true},
		{name: "not found", sel: "deploy", wantOK: false},
		{name: "empty selector", sel: "", wantOK: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			j, ok := FindJob(jobs, tt.sel)
			if ok != tt.wantOK {
				t.Fatalf("FindJob(%q) ok = %v, want %v", tt.sel, ok, tt.wantOK)
			}
			if ok && j.ID != tt.wantID {
				t.Fatalf("FindJob(%q) = job %d, want %d", tt.sel, j.ID, tt.wantID)
			}
		})
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/gitlab/ -run TestFindJob -v`
Expected: FAIL — `undefined: FindJob`.

- [ ] **Step 3: Write the implementation**

`internal/gitlab/find.go`:

```go
package gitlab

import (
	"errors"
	"strconv"
)

// ErrJobNotFound is returned by callers that resolve a job selector against a
// pipeline and find nothing. Surfaces wrap it and phrase their own refusal.
var ErrJobNotFound = errors.New("job not found in pipeline")

// FindJob matches a job by numeric ID first (when sel parses as an int), then
// by exact name. An empty selector matches nothing.
func FindJob(jobs []Job, sel string) (Job, bool) {
	if sel == "" {
		return Job{}, false
	}
	if id, err := strconv.ParseInt(sel, 10, 64); err == nil {
		for _, j := range jobs {
			if j.ID == id {
				return j, true
			}
		}
	}
	for _, j := range jobs {
		if j.Name == sel {
			return j, true
		}
	}
	return Job{}, false
}
```

- [ ] **Step 4: Point `run.go` at it and delete the duplicate**

In `cmd/bot/run.go`, delete the whole `findJob` function (the last ~15 lines,
from `// findJob matches a job by numeric ID first` through its closing brace)
and change its single call site inside `printJobDetails`:

```go
	j, ok := gitlab.FindJob(jobs, sel)
```

`cmd/bot/run.go` already imports `internal/gitlab`, so no import change is
needed. Then delete the now-duplicated test:

```bash
rm cmd/bot/run_test.go
```

- [ ] **Step 5: Run the tests to verify they pass**

Run: `go test ./internal/gitlab/ ./cmd/bot/ && go build ./...`
Expected: both packages `ok`, build silent.

- [ ] **Step 6: Commit**

```bash
git add -A internal/gitlab/ cmd/bot/
git commit -m "refactor(gitlab): share the job matcher and ErrJobNotFound"
```

---

## Task 8: Config — `LONG_JOB_DURATION` and `MEMORY_PRESSURE_RATIO`

**Files:**
- Modify: `internal/config/config.go`
- Test: `internal/config/config_test.go`

- [ ] **Step 1: Write the failing test**

Append to `internal/config/config_test.go`:

```go
func TestLoadAdviceThresholds(t *testing.T) {
	t.Setenv("GITLAB_TOKEN", "tok")
	t.Setenv("PROMETHEUS_URL", "http://prom")

	t.Run("defaults", func(t *testing.T) {
		cfg, err := Load()
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if cfg.LongJobDuration != 10*time.Minute {
			t.Errorf("LongJobDuration = %v, want 10m", cfg.LongJobDuration)
		}
		if cfg.MemoryPressureRatio != 0.9 {
			t.Errorf("MemoryPressureRatio = %v, want 0.9", cfg.MemoryPressureRatio)
		}
	})

	t.Run("overrides", func(t *testing.T) {
		t.Setenv("LONG_JOB_DURATION", "25m")
		t.Setenv("MEMORY_PRESSURE_RATIO", "0.75")
		cfg, err := Load()
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if cfg.LongJobDuration != 25*time.Minute {
			t.Errorf("LongJobDuration = %v, want 25m", cfg.LongJobDuration)
		}
		if cfg.MemoryPressureRatio != 0.75 {
			t.Errorf("MemoryPressureRatio = %v, want 0.75", cfg.MemoryPressureRatio)
		}
	})

	t.Run("invalid values are rejected", func(t *testing.T) {
		for _, tc := range []struct{ key, val string }{
			{"LONG_JOB_DURATION", "soon"},
			{"LONG_JOB_DURATION", "0s"},
			{"LONG_JOB_DURATION", "-5m"},
			{"MEMORY_PRESSURE_RATIO", "high"},
			{"MEMORY_PRESSURE_RATIO", "0"},
			{"MEMORY_PRESSURE_RATIO", "1.5"},
		} {
			t.Run(tc.key+"="+tc.val, func(t *testing.T) {
				t.Setenv(tc.key, tc.val)
				if _, err := Load(); err == nil {
					t.Fatalf("Load accepted %s=%q", tc.key, tc.val)
				}
			})
		}
	})
}
```

Ensure the file imports `"time"`.

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/config/ -run TestLoadAdviceThresholds -v`
Expected: FAIL — `cfg.LongJobDuration undefined`.

- [ ] **Step 3: Write the implementation**

In `internal/config/config.go`, add two fields to `Config` after
`ThrottleWarnRatio`:

```go
	ThrottleWarnRatio   float64
	LongJobDuration     time.Duration
	MemoryPressureRatio float64
```

Add the defaults inside the `cfg := &Config{...}` literal, next to
`ThrottleWarnRatio: 0.25,`:

```go
		ThrottleWarnRatio:   0.25,
		LongJobDuration:     10 * time.Minute,
		MemoryPressureRatio: 0.9,
```

And add the two parse blocks after the existing `SCRAPE_INTERVAL` block:

```go
	if v := os.Getenv("LONG_JOB_DURATION"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d <= 0 {
			return nil, fmt.Errorf("LONG_JOB_DURATION must be a positive duration, got %q", v)
		}
		cfg.LongJobDuration = d
	}

	if v := os.Getenv("MEMORY_PRESSURE_RATIO"); v != "" {
		r, err := strconv.ParseFloat(v, 64)
		if err != nil || r <= 0 || r > 1 {
			return nil, fmt.Errorf("MEMORY_PRESSURE_RATIO must be a float in (0,1], got %q", v)
		}
		cfg.MemoryPressureRatio = r
	}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/config/ -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/config/
git commit -m "feat(config): LONG_JOB_DURATION and MEMORY_PRESSURE_RATIO"
```

---

## Task 9: `Reporter.Advise` — gather facts, fetch traces lazily

**Files:**
- Create: `internal/reporter/advise.go`
- Test: `internal/reporter/advise_test.go`

Read `internal/reporter/reporter_test.go` first: it already defines fakes for
`gitlab.Client`, `correlate.Resolver` and `metrics.Source`. Reuse them if their
shape fits; the test below defines its own trace-counting fakes with distinct
names so both files can coexist in the package.

- [ ] **Step 1: Write the failing test**

`internal/reporter/advise_test.go`:

```go
package reporter

import (
	"context"
	"errors"
	"testing"
	"time"

	"go.uber.org/zap"

	"gitlab.com/ebuildy/gitlab-ci-resources-bot/internal/advice"
	"gitlab.com/ebuildy/gitlab-ci-resources-bot/internal/gitlab"
	"gitlab.com/ebuildy/gitlab-ci-resources-bot/internal/metrics"
)

// adviseGitLab is a gitlab.Client that serves a fixed job list and counts
// JobTrace calls, which is how we prove traces are fetched lazily.
type adviseGitLab struct {
	jobs        []gitlab.Job
	trace       string
	traceCalls  int
	tracedJobID int64
}

func (f *adviseGitLab) PipelineJobs(context.Context, int64, int64) ([]gitlab.Job, error) {
	return f.jobs, nil
}
func (f *adviseGitLab) MergeRequestForBranch(context.Context, int64, string) (int64, bool, error) {
	return 0, false, nil
}
func (f *adviseGitLab) UpsertNote(context.Context, int64, int64, string, string) error { return nil }
func (f *adviseGitLab) JobTrace(_ context.Context, _, jobID int64) (string, error) {
	f.traceCalls++
	f.tracedJobID = jobID
	return f.trace, nil
}
func (f *adviseGitLab) CurrentUser(context.Context) (int64, error) { return 555, nil }
func (f *adviseGitLab) MergeRequestDiscussion(context.Context, int64, int64, string) (gitlab.Discussion, error) {
	return gitlab.Discussion{}, nil
}
func (f *adviseGitLab) UploadFile(context.Context, int64, string, []byte) (string, error) {
	return "", nil
}
func (f *adviseGitLab) CreateDiscussionReply(context.Context, int64, int64, string, string) error {
	return nil
}

type advisePods struct{ pods map[int64]string }

func (f *advisePods) PodForJob(_ context.Context, _, jobID int64, _, _ time.Time) (string, bool, error) {
	p, ok := f.pods[jobID]
	return p, ok, nil
}

type adviseMetrics struct{ usage map[string]*metrics.JobUsage }

func (f *adviseMetrics) PodUsage(_ context.Context, pod string, _, _ time.Time) (*metrics.JobUsage, error) {
	return f.usage[pod], nil
}

func adviseFixture(t *testing.T) (*Reporter, *adviseGitLab, *advice.Engine) {
	t.Helper()
	start := time.Date(2026, 7, 23, 10, 0, 0, 0, time.UTC)
	gl := &adviseGitLab{
		trace: "$ mvn -T 1C verify",
		jobs: []gitlab.Job{
			// Throttled: a trace consumer will ask for this one's trace.
			{ID: 1, Stage: "build", Name: "compile", StartedAt: start, FinishedAt: start.Add(20 * time.Minute)},
			// Healthy: no rule needs its trace.
			{ID: 2, Stage: "test", Name: "unit", StartedAt: start, FinishedAt: start.Add(time.Minute)},
			// Never ran: skipped entirely.
			{ID: 3, Stage: "deploy", Name: "staging"},
		},
	}
	r := &Reporter{
		GitLab:   gl,
		Resolver: &advisePods{pods: map[int64]string{1: "pod-1", 2: "pod-2"}},
		Metrics: &adviseMetrics{usage: map[string]*metrics.JobUsage{
			"pod-1": {ThrottledRatio: 0.8, CPULimitCores: 1, PeakMemoryBytes: 10, MemoryLimitBytes: 1000},
			"pod-2": {ThrottledRatio: 0.0, CPULimitCores: 1, PeakMemoryBytes: 10, MemoryLimitBytes: 1000},
		}},
		Log: zap.NewNop(),
	}
	eng, err := advice.New(advice.Thresholds{
		ThrottleWarnRatio:   0.25,
		LongJob:             10 * time.Minute,
		MemoryPressureRatio: 0.9,
	}, nil)
	if err != nil {
		t.Fatalf("advice.New: %v", err)
	}
	return r, gl, eng
}

func TestAdviseFetchesTraceOnlyForThrottledJobs(t *testing.T) {
	r, gl, eng := adviseFixture(t)

	all, err := r.Advise(context.Background(), 7, 42, "", eng)
	if err != nil {
		t.Fatalf("Advise: %v", err)
	}
	if gl.traceCalls != 1 {
		t.Fatalf("JobTrace called %d times, want exactly 1 (only the throttled job)", gl.traceCalls)
	}
	if gl.tracedJobID != 1 {
		t.Fatalf("traced job %d, want the throttled job 1", gl.tracedJobID)
	}

	rules := map[string]bool{}
	for _, a := range all {
		rules[a.Rule] = true
		if a.Job == "staging" {
			t.Fatal("a job that never ran produced advice")
		}
	}
	for _, want := range []string{"cpu-throttle", "java-threads", "long-job"} {
		if !rules[want] {
			t.Errorf("missing %s advice; got %v", want, rules)
		}
	}
}

func TestAdviseJobFilter(t *testing.T) {
	r, _, eng := adviseFixture(t)

	all, err := r.Advise(context.Background(), 7, 42, "unit", eng)
	if err != nil {
		t.Fatalf("Advise: %v", err)
	}
	for _, a := range all {
		if a.Job != "unit" {
			t.Fatalf("filter returned advice for %q", a.Job)
		}
	}

	if _, err := r.Advise(context.Background(), 7, 42, "nope", eng); !errors.Is(err, gitlab.ErrJobNotFound) {
		t.Fatalf("Advise with an unknown job = %v, want gitlab.ErrJobNotFound", err)
	}
}

// TestAdviseSurvivesMissingPod proves a job whose pod never correlated still
// gets duration-based advice instead of aborting the whole run.
func TestAdviseSurvivesMissingPod(t *testing.T) {
	r, _, eng := adviseFixture(t)
	r.Resolver = &advisePods{pods: map[int64]string{}} // nothing correlates

	all, err := r.Advise(context.Background(), 7, 42, "compile", eng)
	if err != nil {
		t.Fatalf("Advise: %v", err)
	}
	if len(all) != 1 || all[0].Rule != "long-job" {
		t.Fatalf("advice = %+v, want exactly the long-job finding", all)
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/reporter/ -run TestAdvise -v`
Expected: FAIL — `r.Advise undefined`.

- [ ] **Step 3: Write the implementation**

`internal/reporter/advise.go`:

```go
package reporter

import (
	"context"
	"fmt"

	"go.uber.org/zap"

	"gitlab.com/ebuildy/gitlab-ci-resources-bot/internal/advice"
	"gitlab.com/ebuildy/gitlab-ci-resources-bot/internal/gitlab"
)

// Advise builds advice.Facts for every job of the pipeline — or the single job
// matching jobFilter (numeric ID or exact name) — and runs the engine over
// them. Jobs that never ran are skipped. Per-job metric failures leave Usage
// nil, so duration-based advice still applies; only the jobs listing failing
// aborts. An unmatched jobFilter returns an error wrapping
// gitlab.ErrJobNotFound.
func (r *Reporter) Advise(ctx context.Context, projectID, pipelineID int64, jobFilter string, eng *advice.Engine) ([]advice.Advice, error) {
	jobs, err := r.GitLab.PipelineJobs(ctx, projectID, pipelineID)
	if err != nil {
		return nil, fmt.Errorf("list pipeline jobs: %w", err)
	}
	if jobFilter != "" {
		j, ok := gitlab.FindJob(jobs, jobFilter)
		if !ok {
			return nil, fmt.Errorf("%q: %w", jobFilter, gitlab.ErrJobNotFound)
		}
		jobs = []gitlab.Job{j}
	}

	var out []advice.Advice
	for _, job := range jobs {
		if job.StartedAt.IsZero() || job.FinishedAt.IsZero() {
			continue // never ran (skipped/canceled/manual)
		}
		f := advice.Facts{
			Stage:    job.Stage,
			Name:     job.Name,
			Duration: job.FinishedAt.Sub(job.StartedAt),
			Usage:    r.jobUsage(ctx, projectID, job),
		}
		// Lazy: the trace costs a GitLab call, so fetch it only when a rule
		// actually wants it for this job.
		if eng.NeedsTrace(f) {
			trace, err := r.GitLab.JobTrace(ctx, projectID, job.ID)
			if err != nil {
				r.Log.Warn("fetch job trace for advice failed",
					zap.String("job", job.Name), zap.Int64("job_id", job.ID), zap.Error(err))
			} else {
				f.Trace = trace
			}
		}
		out = append(out, eng.Analyze(f)...)
	}
	r.Log.Debug("advice built",
		zap.Int64("pipeline_id", pipelineID), zap.Int("jobs", len(jobs)), zap.Int("advice", len(out)))
	return out, nil
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test -race ./internal/reporter/ -v`
Expected: PASS — the three new tests plus the existing reporter tests.

- [ ] **Step 5: Commit**

```bash
git add internal/reporter/advise.go internal/reporter/advise_test.go
git commit -m "feat(reporter): Advise gathers facts and runs the advice engine"
```

---

## Task 10: Parse the `advise` note command

**Files:**
- Modify: `internal/command/command.go`
- Test: `internal/command/command_test.go`

- [ ] **Step 1: Write the failing test**

In `internal/command/command_test.go`, add these cases to the `tests` slice in
`TestParse`, after the existing `details` cases:

```go
		{name: "advise all", body: "advise", wantOK: true, wantKind: KindAdvise},
		{name: "advise case-insensitive", body: "ADVISE", wantOK: true, wantKind: KindAdvise},
		{name: "advise one job", body: "advise build", wantOK: true, wantKind: KindAdvise, wantName: "build"},
		{name: "advise explicit job", body: "advise job build", wantOK: true, wantKind: KindAdvise, wantName: "build"},
		{name: "advise extra args ignored", body: "advise job build extra", wantOK: false},
```

And add this test at the end of the file:

```go
// TestHelpTextListsAdvise keeps the help reply honest about what exists.
func TestHelpTextListsAdvise(t *testing.T) {
	for _, want := range []string{"`advise`", "`advise <job>`"} {
		if !strings.Contains(HelpText, want) {
			t.Errorf("HelpText missing %q:\n%s", want, HelpText)
		}
	}
}
```

Add `"strings"` to the file's imports.

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/command/ -run 'TestParse|TestHelpText' -v`
Expected: FAIL — `undefined: KindAdvise`.

- [ ] **Step 3: Write the implementation**

In `internal/command/command.go`:

Add the kind:

```go
const (
	KindHelp Kind = iota
	KindDetails
	KindAdvise
)
```

Add the regex next to the others:

```go
var (
	helpRE    = regexp.MustCompile(`(?i)^help$`)
	detailsRE = regexp.MustCompile(`(?i)^details\s+(?:(job|pod)\s+)?(\S+)$`)
	adviseRE  = regexp.MustCompile(`(?i)^advise(?:\s+(?:job\s+)?(\S+))?$`)
	runnerRE  = regexp.MustCompile(`^runner-`)
)
```

Add the branch in `Parse`, after the `detailsRE` branch and before the final
`return`:

```go
	if m := adviseRE.FindStringSubmatch(line); m != nil {
		// Advice is always about a CI job — there is no pod target.
		return Command{Kind: KindAdvise, Target: TargetJob, Name: m[1]}, true
	}
```

Extend `HelpText`:

```go
// HelpText is the reply for the help command. Extend it as commands are added.
const HelpText = "**cigar commands**\n\n" +
	"- `help` — show this message\n" +
	"- `details job <name>` — CPU / memory / network charts for a job in this report\n" +
	"- `details pod <runner-...>` — same, for a runner pod in this report\n" +
	"- `details <name>` — auto-detects job vs pod\n" +
	"- `advise` — recommendations for every job in this report\n" +
	"- `advise <job>` — recommendations for one job\n"
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/command/ -run 'TestParse|TestHelpText' -v`
Expected: PASS — including the five new `TestParse` subtests.

- [ ] **Step 5: Commit**

```bash
git add internal/command/command.go internal/command/command_test.go
git commit -m "feat(command): parse the advise command"
```

---

## Task 11: Execute `advise` in the note handler

**Files:**
- Modify: `internal/command/handler.go`
- Test: `internal/command/handler_test.go`

- [ ] **Step 1: Write the failing test**

Append to `internal/command/handler_test.go`:

```go
// fakeAdvisor records what the handler asked for and returns canned advice.
type fakeAdvisor struct {
	calls     int
	gotFilter string
	out       []advice.Advice
	err       error
}

func (f *fakeAdvisor) Advise(_ context.Context, _, _ int64, jobFilter string) ([]advice.Advice, error) {
	f.calls++
	f.gotFilter = jobFilter
	return f.out, f.err
}

func TestHandleAdviseAllJobs(t *testing.T) {
	gl := &fakeGitLab{discussion: signedRoot(42, 3)}
	adv := &fakeAdvisor{out: []advice.Advice{
		{Job: "build", Rule: "cpu-throttle", Title: "⚠️ CPU throttling", Body: "raise the limit"},
	}}
	h := newHandler(gl, &fakeResolver{}, &fakeSeries{})
	h.Advisor = adv

	if err := h.Handle(context.Background(), NoteEvent{ProjectID: 7, MRIID: 3, DiscussionID: "abc", AuthorID: 9, Body: "advise"}); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if adv.calls != 1 || adv.gotFilter != "" {
		t.Fatalf("Advise called %d times with filter %q, want 1 and \"\"", adv.calls, adv.gotFilter)
	}
	if len(gl.replies) != 1 {
		t.Fatalf("replies = %d, want 1", len(gl.replies))
	}
	if !strings.Contains(gl.replies[0], "raise the limit") {
		t.Fatalf("reply does not carry the advice:\n%s", gl.replies[0])
	}
	if !strings.Contains(gl.replies[0], report.MarkerPrefix) {
		t.Fatalf("reply not tagged with the marker: %q", gl.replies[0])
	}
}

func TestHandleAdviseOneJob(t *testing.T) {
	gl := &fakeGitLab{discussion: signedRoot(42, 3)}
	adv := &fakeAdvisor{}
	h := newHandler(gl, &fakeResolver{}, &fakeSeries{})
	h.Advisor = adv

	if err := h.Handle(context.Background(), NoteEvent{ProjectID: 7, MRIID: 3, DiscussionID: "abc", AuthorID: 9, Body: "advise build"}); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if adv.gotFilter != "build" {
		t.Fatalf("filter = %q, want %q", adv.gotFilter, "build")
	}
	// No advice at all must produce the clean message, not an empty reply.
	if len(gl.replies) != 1 || !strings.Contains(gl.replies[0], advice.CleanMessage) {
		t.Fatalf("replies = %v, want one containing %q", gl.replies, advice.CleanMessage)
	}
}

func TestHandleAdviseUnknownJob(t *testing.T) {
	gl := &fakeGitLab{discussion: signedRoot(42, 3)}
	adv := &fakeAdvisor{err: fmt.Errorf("%q: %w", "nope", gitlab.ErrJobNotFound)}
	h := newHandler(gl, &fakeResolver{}, &fakeSeries{})
	h.Advisor = adv

	if err := h.Handle(context.Background(), NoteEvent{ProjectID: 7, MRIID: 3, DiscussionID: "abc", AuthorID: 9, Body: "advise nope"}); err != nil {
		t.Fatalf("Handle must not fail on an unknown job: %v", err)
	}
	if len(gl.replies) != 1 || !strings.Contains(gl.replies[0], "not part of pipeline") {
		t.Fatalf("replies = %v, want one refusal notice", gl.replies)
	}
}

func TestHandleAdviseWithoutAdvisor(t *testing.T) {
	gl := &fakeGitLab{discussion: signedRoot(42, 3)}
	h := newHandler(gl, &fakeResolver{}, &fakeSeries{}) // Advisor left nil

	if err := h.Handle(context.Background(), NoteEvent{ProjectID: 7, MRIID: 3, DiscussionID: "abc", AuthorID: 9, Body: "advise"}); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if len(gl.replies) != 1 || !strings.Contains(gl.replies[0], "not available") {
		t.Fatalf("replies = %v, want one 'not available' notice", gl.replies)
	}
}
```

Add `"fmt"`, `"gitlab.com/ebuildy/gitlab-ci-resources-bot/internal/advice"` and
`"gitlab.com/ebuildy/gitlab-ci-resources-bot/internal/gitlab"` to that file's
imports (`gitlab` is already imported).

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/command/ -run TestHandleAdvise -v`
Expected: FAIL — `h.Advisor undefined`.

- [ ] **Step 3: Write the implementation**

In `internal/command/handler.go`, add the interface above `Handler`:

```go
// Advisor produces recommendations for a pipeline, optionally narrowed to one
// job. It is an interface so the handler stays independent of the reporter and
// stubbable in tests. An unmatched jobFilter must return an error wrapping
// gitlab.ErrJobNotFound.
type Advisor interface {
	Advise(ctx context.Context, projectID, pipelineID int64, jobFilter string) ([]advice.Advice, error)
}
```

Add the field to `Handler`:

```go
type Handler struct {
	GitLab      gitlab.Client
	Resolver    correlate.Resolver
	Series      metrics.SeriesSource
	Advisor     Advisor // nil disables the advise command
	SigningKey  []byte
	BotUserID   int64
	ChartFormat chart.Format // PNG (default) or SVG
	Log         *zap.Logger
}
```

Add the dispatch branch in `Handle`'s switch:

```go
	switch cmd.Kind {
	case KindHelp:
		return h.reply(ctx, ev, HelpText)
	case KindDetails:
		return h.details(ctx, ev, pipelineID, cmd)
	case KindAdvise:
		return h.advise(ctx, ev, pipelineID, cmd)
	}
```

And add the executor next to `details`:

```go
// advise runs the advice engine over the report's pipeline (or one job of it)
// and posts the rendered recommendations as one reply.
func (h *Handler) advise(ctx context.Context, ev NoteEvent, pipelineID int64, cmd Command) error {
	if h.Advisor == nil {
		return h.reply(ctx, ev, "Advice is not available on this instance.")
	}
	all, err := h.Advisor.Advise(ctx, ev.ProjectID, pipelineID, cmd.Name)
	if errors.Is(err, gitlab.ErrJobNotFound) {
		return h.reply(ctx, ev, fmt.Sprintf("`%s` is not part of pipeline #%d's report.", cmd.Name, pipelineID))
	}
	if err != nil {
		return fmt.Errorf("build advice for pipeline %d: %w", pipelineID, err)
	}
	return h.reply(ctx, ev, advice.Render(pipelineID, all))
}
```

Add `"errors"` and the `internal/advice` import to the file.

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test -race ./internal/command/ -v`
Expected: PASS — the four new tests plus every existing handler test.

- [ ] **Step 5: Commit**

```bash
git add internal/command/handler.go internal/command/handler_test.go
git commit -m "feat(command): execute advise via the Advisor interface"
```

---

## Task 12: Wire it up — `bot advise` and the serve path

**Files:**
- Create: `cmd/bot/advise.go`
- Modify: `cmd/bot/deps.go`, `cmd/bot/serve.go`, `cmd/bot/main.go`

- [ ] **Step 1: Write the CLI command**

`cmd/bot/advise.go`:

```go
package main

import (
	"context"
	"fmt"
	"strconv"

	"github.com/spf13/cobra"
	"go.uber.org/zap"

	"gitlab.com/ebuildy/gitlab-ci-resources-bot/internal/advice"
	"gitlab.com/ebuildy/gitlab-ci-resources-bot/internal/config"
	"gitlab.com/ebuildy/gitlab-ci-resources-bot/internal/reporter"
)

func newAdviseCmd() *cobra.Command {
	var projectID int64
	var job string
	cmd := &cobra.Command{
		Use:   "advise <pipeline-id>",
		Short: "Print resource-usage recommendations for one pipeline",
		Long: "Runs the same advice rules the `advise` note command uses and prints " +
			"the recommendations to stdout. With --job, only that job is analyzed. " +
			"JSON logs are also written to stdout; use --log-level error to keep the " +
			"output to just the advice.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			pipelineID, err := strconv.ParseInt(args[0], 10, 64)
			if err != nil {
				return fmt.Errorf("invalid pipeline ID %q", args[0])
			}
			cfg, err := config.Load()
			if err != nil {
				return err
			}
			log := logger
			rep, err := newReporter(cfg, log)
			if err != nil {
				return err
			}
			eng, err := newAdviceEngine(cfg)
			if err != nil {
				return err
			}
			log.Debug("building advice",
				zap.Int64("project_id", projectID),
				zap.Int64("pipeline_id", pipelineID),
				zap.String("job", job))
			all, err := rep.Advise(cmd.Context(), projectID, pipelineID, job, eng)
			if err != nil {
				return err
			}
			_, err = fmt.Fprintln(cmd.OutOrStdout(), advice.Render(pipelineID, all))
			return err
		},
	}
	cmd.Flags().Int64Var(&projectID, "project", 0, "GitLab project ID the pipeline belongs to (required)")
	cmd.Flags().StringVar(&job, "job", "", "Only analyze this job (numeric ID or exact name)")
	_ = cmd.MarkFlagRequired("project")
	return cmd
}

// reporterAdvisor adapts a Reporter plus its engine to command.Advisor, so the
// note handler depends on the interface rather than on the reporter.
type reporterAdvisor struct {
	rep *reporter.Reporter
	eng *advice.Engine
}

func (a reporterAdvisor) Advise(ctx context.Context, projectID, pipelineID int64, jobFilter string) ([]advice.Advice, error) {
	return a.rep.Advise(ctx, projectID, pipelineID, jobFilter, a.eng)
}
```

- [ ] **Step 2: Add the engine builder and wire the handler**

In `cmd/bot/deps.go`, add the engine builder:

```go
// newAdviceEngine builds the advice engine from config. A nil rule list selects
// every registered rule; this is where a future ADVICE_RULES would plug in.
func newAdviceEngine(cfg *config.Config) (*advice.Engine, error) {
	return advice.New(advice.Thresholds{
		ThrottleWarnRatio:   cfg.ThrottleWarnRatio,
		LongJob:             cfg.LongJobDuration,
		MemoryPressureRatio: cfg.MemoryPressureRatio,
	}, nil)
}
```

Change `newCommandHandler` to take the already-built reporter and set the
advisor. Its signature becomes:

```go
func newCommandHandler(ctx context.Context, cfg *config.Config, log *zap.Logger, rep *reporter.Reporter) (*command.Handler, error) {
```

and its `return` becomes:

```go
	eng, err := newAdviceEngine(cfg)
	if err != nil {
		return nil, err
	}
	return &command.Handler{
		GitLab:      gl,
		Resolver:    resolver,
		Series:      source,
		Advisor:     reporterAdvisor{rep: rep, eng: eng},
		SigningKey:  []byte(cfg.CommandsSigningKey),
		BotUserID:   botID,
		ChartFormat: format,
		Log:         log,
	}, nil
```

Add the `internal/advice` import to `deps.go`.

In `cmd/bot/serve.go`, pass the reporter at the single call site:

```go
		cmdHandler, err = newCommandHandler(ctx, cfg, log, rep)
```

- [ ] **Step 3: Register the subcommand**

In `cmd/bot/main.go`:

```go
	root.AddCommand(newServeCmd(), newRunCmd(), newAdviseCmd())
```

- [ ] **Step 4: Verify it builds and the help text is right**

```bash
go build ./... && go run ./cmd/bot advise --help
```

Expected: the build is silent and the help shows `bot advise <pipeline-id>`
with both `--project` and `--job` flags.

- [ ] **Step 5: Run the full suite**

Run: `mise r test`
Expected: every package `ok`, no race warnings.

- [ ] **Step 6: Commit**

```bash
git add cmd/bot/
git commit -m "feat(bot): bot advise subcommand and serve-path advisor wiring"
```

---

## Task 13: End-to-end coverage for the `advise` note command

**Files:**
- Modify: `internal/e2e/e2e_test.go`

The e2e mock Prometheus answers every instant query with `123.45`, so the
throttled ratio computes to 1.0 and peak memory equals the limit — both the
`cpu-throttle` and `memory-pressure` rules fire, which makes the assertion
below deterministic.

- [ ] **Step 1: Give the e2e handler an advisor**

In `internal/e2e/e2e_test.go`, inside `harness`, extend the `command.Handler`
literal (currently around line 247) — `rep` is already built just above it:

```go
	eng, err := advice.New(advice.Thresholds{
		ThrottleWarnRatio:   0.25,
		LongJob:             10 * time.Minute,
		MemoryPressureRatio: 0.9,
	}, nil)
	if err != nil {
		t.Fatalf("advice engine: %v", err)
	}
	cmdHandler := &command.Handler{
		GitLab:     glClient,
		Resolver:   resolver,
		Series:     source, // *metrics.PromSource satisfies metrics.SeriesSource
		Advisor:    e2eAdvisor{rep: rep, eng: eng},
		SigningKey: []byte(commandsKey),
		BotUserID:  555,
		Log:        log,
	}
```

Add the adapter at the end of the file:

```go
// e2eAdvisor adapts the reporter to command.Advisor, mirroring the adapter
// `bot serve` wires in cmd/bot/advise.go.
type e2eAdvisor struct {
	rep *reporter.Reporter
	eng *advice.Engine
}

func (a e2eAdvisor) Advise(ctx context.Context, projectID, pipelineID int64, jobFilter string) ([]advice.Advice, error) {
	return a.rep.Advise(ctx, projectID, pipelineID, jobFilter, a.eng)
}
```

Add the `internal/advice` import (and `"context"` if the file lacks it).

- [ ] **Step 2: Write the failing test**

Add next to `TestNoteCommandDetailsJob`:

```go
// TestNoteCommandAdvise drives the whole chain — Note Hook webhook, queue,
// worker, command handler, reporter, advice engine — and asserts the reply
// carries real recommendations.
func TestNoteCommandAdvise(t *testing.T) {
	app, glMock, _ := harness(t, "trace")
	payload := fmt.Sprintf(`{
		"object_kind":"note",
		"object_attributes":{"id":79,"note":"advise","noteable_type":"MergeRequest","discussion_id":"disc1","author_id":9},
		"project":{"id":%d},
		"merge_request":{"iid":%d}
	}`, projectID, mrIID)

	postNoteWebhook(t, app, payload)
	waitFor(t, "advice reply posted", func() bool {
		glMock.mu.Lock()
		defer glMock.mu.Unlock()
		return len(glMock.replies) == 1
	})

	glMock.mu.Lock()
	defer glMock.mu.Unlock()
	reply := glMock.replies[0]
	if glMock.uploads != 0 {
		t.Fatalf("uploads = %d, want 0 (advice posts no charts)", glMock.uploads)
	}
	for _, want := range []string{"Advice for pipeline", "KUBERNETES_CPU_LIMIT", report.MarkerPrefix} {
		if !strings.Contains(reply, want) {
			t.Fatalf("reply missing %q:\n%s", want, reply)
		}
	}
}
```

Confirm `internal/report` is imported in the file (it is, for the marker
constants); add it if not.

- [ ] **Step 3: Run the test to verify it fails**

Run: `go test ./internal/e2e/ -run TestNoteCommandAdvise -count=1 -v`
Expected: FAIL first at compile time if Step 1 was skipped, otherwise on the
missing reply.

- [ ] **Step 4: Run it to verify it passes**

Run: `mise r test:e2e`
Expected: PASS — every e2e test, including the existing details and loop-guard
cases.

- [ ] **Step 5: Commit**

```bash
git add internal/e2e/e2e_test.go
git commit -m "test(e2e): advise note command end to end"
```

---

## Task 14: Chart and documentation

**Files:**
- Modify: `deploy/chart/cigar/values.yaml`, `deploy/chart/cigar/templates/deployment.yaml`
- Modify: `docs/usage.md`, `README.md`

- [ ] **Step 1: Expose the thresholds in the chart**

In `deploy/chart/cigar/values.yaml`, under `config:` after `scrapeInterval`:

```yaml
  # Job duration above which the advise command suggests splitting the job.
  longJobDuration: "10m"
  # Peak-memory-to-limit ratio above which the advise command warns about
  # OOMKill risk.
  memoryPressureRatio: "0.9"
```

In `deploy/chart/cigar/templates/deployment.yaml`, after the `SCRAPE_INTERVAL`
entry:

```yaml
            - name: LONG_JOB_DURATION
              value: {{ .Values.config.longJobDuration | quote }}
            - name: MEMORY_PRESSURE_RATIO
              value: {{ .Values.config.memoryPressureRatio | quote }}
```

- [ ] **Step 2: Validate the chart**

```bash
helm lint deploy/chart/cigar
helm template deploy/chart/cigar | grep -A1 'LONG_JOB_DURATION'
```

Expected: `1 chart(s) linted, 0 chart(s) failed`, and the rendered env var
showing `value: "10m"`.

- [ ] **Step 3: Document the env vars and the command**

In `docs/usage.md`, add two rows to the configuration table (after
`SCRAPE_INTERVAL`):

```markdown
| `LONG_JOB_DURATION` | no | `10m` | Job duration above which `advise` suggests splitting the job |
| `MEMORY_PRESSURE_RATIO` | no | `0.9` | Peak-memory-to-limit ratio above which `advise` warns about OOMKill risk |
```

Add two rows to the commands table (after the `details <name>` row):

```markdown
| `advise` | Reply with recommendations for every job in this report |
| `advise <job>` | Same, for one job — accepts a job name or numeric ID |
```

Then add this subsection right after the commands table:

```markdown
#### How advice is generated

Each job is checked by an independent rule; a rule that does not apply stays
silent, and a pipeline with nothing to fix gets `You are all good dude!`.

| Rule | Fires when |
| --- | --- |
| `cpu-throttle` | The job was throttled at or above `THROTTLE_WARN_RATIO` |
| `java-threads` | Throttled **and** the job trace shows a Maven/Gradle/Java build |
| `long-job` | The job ran longer than `LONG_JOB_DURATION` |
| `memory-pressure` | Peak memory reached `MEMORY_PRESSURE_RATIO` of the memory limit |

The `java-threads` rule is the only one that reads the job's trace, and it is
fetched only for jobs that were actually throttled — one extra API call per
throttled job, none for the rest.

The same rules back the CLI:

```sh
bot advise --project 42 987654            # every job
bot advise --project 42 987654 --job test # one job
```
```

In `README.md`, add `advise` to the command list next to `details`.

- [ ] **Step 4: Final verification**

Run: `mise r lint test`
Expected: golangci-lint clean, every package `ok` with the race detector on.

- [ ] **Step 5: Commit**

```bash
git add deploy/chart/cigar/ docs/usage.md README.md
git commit -m "docs,chart: document and expose the advise command"
```

---

## Definition of Done

- [ ] `mise r lint test` clean, race detector on.
- [ ] `bot advise --project <id> <pipeline-id>` prints advice or `You are all good dude!`.
- [ ] Replying `advise` under the bot's report note posts one reply carrying the marker.
- [ ] `internal/advice/testdata/*.md` golden files reviewed by a human, not just regenerated.
- [ ] Adding a fifth rule requires one new file and one line in `rules.go` — verify by reading `rules.go`.
