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

	empty, err := New(th, []string{})
	if err != nil {
		t.Fatalf("New(empty): %v", err)
	}
	if len(empty.rules) != len(registry) {
		t.Fatalf("New(empty) selected %d rules, want all %d", len(empty.rules), len(registry))
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
