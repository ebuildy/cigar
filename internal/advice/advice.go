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

// New builds an engine. A nil or empty enabled list selects every registered
// rule in registration order; otherwise only the named rules are selected, in
// registration order, and an unknown name is an error. This is the seam a
// future ADVICE_RULES environment variable plugs into.
func New(th Thresholds, enabled []string) (*Engine, error) {
	// A nil or empty selection means every registered rule: an engine with no
	// rules would silently produce no advice at all.
	if len(enabled) == 0 {
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
	// Report the first unknown name in the caller's own order: ranging the map
	// would pick a different one run to run.
	for _, name := range enabled {
		if want[name] {
			return nil, fmt.Errorf("unknown advice rule %q", name)
		}
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
