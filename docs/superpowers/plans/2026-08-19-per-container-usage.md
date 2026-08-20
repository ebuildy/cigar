# Per-container usage Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Break each job's runner-pod usage down into its `build`, `helper` and `svc-N` containers, show that breakdown in the MR report, and make CPU-throttling advice name the container it is about with the GitLab CI variable that actually governs it.

**Architecture:** `PromSource.PodUsage` switches its twelve instant queries from `sum(...)` to `sum by (container)(...)` and assembles a `[]metrics.ContainerUsage`; every pod-level total on `JobUsage` is then *derived* by summing that slice, so a total can never disagree with the rows beneath it. `internal/report` nests container rows under each job when the pipeline is small enough (`report.container_detail_max_jobs`, default 10); `internal/advice` fires `cpu-throttle` once per throttled container; the `details` command reply gains the same table so the breakdown stays reachable on large pipelines.

**Tech Stack:** Go ≥ 1.26, `prometheus/client_golang` (PromQL), `spf13/viper` + `spf13/cobra` (config), `text/template`-free string building in `internal/report`, golden-file tests, `helm-unittest` for the chart.

**Spec:** [docs/superpowers/specs/2026-08-19-per-container-usage-design.md](../specs/2026-08-19-per-container-usage-design.md)

---

## Background you need before starting

Read the spec first. Two rules in this codebase are non-negotiable and this plan depends on both:

1. **Absent ≠ zero.** A PromQL query that matches no series leaves the field *unset*. It is never written as a measured `0`. That is why `ContainerUsage` stores raw CFS counters instead of a ratio, and why the report renders `—` rather than `0%`.
2. **Totals must equal their parts.** The job row is the sum of its container rows, computed in Go from one query pass — never a second, independent query.

Commands (use mise, not raw `go`):

```sh
mise r test     # go test -race ./...
mise r lint     # golangci-lint run
mise r helm:test
```

Golden files are refreshed with `go test ./internal/report -run TestRenderGolden -update`. **Never** run `-update` before you have read the diff and confirmed the change is the one you intended.

---

## File structure

| File | Responsibility | Task |
|---|---|---|
| `internal/metrics/container.go` (new) | `ContainerUsage`, its `ThrottledRatio`, ordering, total derivation | 1 |
| `internal/metrics/container_test.go` (new) | unit tests for the above | 1 |
| `internal/metrics/source.go` (modify) | `JobUsage.Containers` field | 1 |
| `internal/metrics/prom.go` (modify) | `sum by (container)` queries, `vector` helper | 2 |
| `internal/metrics/prom_usage_test.go` (new) | `PodUsage` against a stub Prometheus | 2 |
| `internal/metrics/testdata/query_containers.json` (new) | three-container vector fixture | 2 |
| `internal/config/config.go` (modify) | `report.container_detail_max_jobs`, `parseInt` | 3 |
| `internal/report/report.go` (modify) | nested rows, `ContainerTable`, `Data.ContainerDetailMaxJobs` | 4 |
| `internal/report/testdata/*.md` | goldens | 4 |
| `internal/advice/cpu_throttle.go` (modify) | per-container advice + variable mapping | 5 |
| `internal/reporter/reporter.go`, `cmd/bot/deps.go`, `cmd/bot/run.go` (modify) | wiring | 6 |
| `internal/command/handler.go`, `command.go` (modify) | container table in `details`, help line | 7 |
| `internal/e2e/e2e_test.go` (modify) | two-container end-to-end proof | 8 |
| `deploy/chart/cigar/*` (modify) | chart value + configmap + unittest | 9 |
| `README.md`, `docs/usage.md`, `docs/deploy.md` (modify) | docs | 9 |

---

## Task 1: `ContainerUsage` — the data model

**Files:**
- Create: `internal/metrics/container.go`
- Create: `internal/metrics/container_test.go`
- Modify: `internal/metrics/source.go`

- [ ] **Step 1: Write the failing test**

Create `internal/metrics/container_test.go`:

```go
package metrics

import "testing"

func TestContainerThrottledRatio(t *testing.T) {
	tests := []struct {
		name      string
		container ContainerUsage
		want      float64
		wantOK    bool
	}{
		{name: "measured", container: ContainerUsage{ThrottledPeriods: 25, Periods: 100}, want: 0.25, wantOK: true},
		{name: "no throttling measured", container: ContainerUsage{ThrottledPeriods: 0, Periods: 100}, want: 0, wantOK: true},
		// Absent ≠ 0%: with no CFS period series there is nothing to divide by.
		{name: "absent series", container: ContainerUsage{}, wantOK: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := tt.container.ThrottledRatio()
			if ok != tt.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tt.wantOK)
			}
			if ok && got != tt.want {
				t.Errorf("ratio = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestSortedContainers(t *testing.T) {
	acc := map[string]*ContainerUsage{
		"svc-10": {Name: "svc-10"},
		"helper": {Name: "helper"},
		"svc-2":  {Name: "svc-2"},
		"build":  {Name: "build"},
		"zebra":  {Name: "zebra"},
	}
	got := sortedContainers(acc)
	want := []string{"build", "helper", "svc-2", "svc-10", "zebra"}
	if len(got) != len(want) {
		t.Fatalf("got %d containers, want %d", len(got), len(want))
	}
	for i, name := range want {
		if got[i].Name != name {
			t.Errorf("position %d = %q, want %q", i, got[i].Name, name)
		}
	}
}

func TestSumContainersDerivesTotals(t *testing.T) {
	u := &JobUsage{Containers: []ContainerUsage{
		{Name: "build", CPUSeconds: 39.8, PeakMemoryBytes: 400, DiskReadBytes: 500, DiskWriteBytes: 200,
			ThrottledPeriods: 12, Periods: 100,
			CPURequestCores: 0.15, CPULimitCores: 0.25, MemoryRequestBytes: 128, MemoryLimitBytes: 256},
		{Name: "helper", CPUSeconds: 2.2, PeakMemoryBytes: 12, DiskReadBytes: 20, DiskWriteBytes: 20,
			ThrottledPeriods: 58, Periods: 100,
			CPURequestCores: 0.1, CPULimitCores: 0.25, MemoryRequestBytes: 128, MemoryLimitBytes: 256},
	}}
	u.sumContainers()

	if u.CPUSeconds != 42.0 {
		t.Errorf("CPUSeconds = %v, want 42", u.CPUSeconds)
	}
	if u.PeakMemoryBytes != 412 {
		t.Errorf("PeakMemoryBytes = %d, want 412", u.PeakMemoryBytes)
	}
	if u.DiskReadBytes != 520 || u.DiskWriteBytes != 220 {
		t.Errorf("disk = %d/%d, want 520/220", u.DiskReadBytes, u.DiskWriteBytes)
	}
	if u.CPURequestCores != 0.25 || u.CPULimitCores != 0.5 {
		t.Errorf("cpu req/limit = %v/%v, want 0.25/0.5", u.CPURequestCores, u.CPULimitCores)
	}
	if u.MemoryRequestBytes != 256 || u.MemoryLimitBytes != 512 {
		t.Errorf("mem req/limit = %d/%d, want 256/512", u.MemoryRequestBytes, u.MemoryLimitBytes)
	}
	// sum(throttled)/sum(periods) — not the mean of the two ratios (0.35).
	if u.ThrottledRatio != 0.35 {
		t.Errorf("ThrottledRatio = %v, want 0.35", u.ThrottledRatio)
	}
}

func TestSumContainersLeavesRatioUnsetWithoutPeriods(t *testing.T) {
	u := &JobUsage{Containers: []ContainerUsage{{Name: "build", CPUSeconds: 1}}}
	u.sumContainers()
	if u.ThrottledRatio != 0 {
		t.Errorf("ThrottledRatio = %v, want 0 (absent, not measured)", u.ThrottledRatio)
	}
}
```

> Note on `TestSumContainersDerivesTotals`: 12+58 = 70 throttled over 200 periods = 0.35, which here happens to equal the mean of 0.12 and 0.58. That is a coincidence of the round numbers; the assertion still pins the sum/sum formula because `sumContainers` has no other way to reach it. Do not "simplify" the implementation to a mean.

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/metrics -run 'TestContainer|TestSorted|TestSum' -v`
Expected: FAIL — `undefined: ContainerUsage`, `undefined: sortedContainers`.

- [ ] **Step 3: Add the `Containers` field to `JobUsage`**

In `internal/metrics/source.go`, inside the `JobUsage` struct, after the `LowConfidence` field:

```go
	// Containers is the per-container breakdown of this pod, ordered build,
	// helper, svc-N ascending, then anything else alphabetically. It is empty
	// when the container-level series were absent — the totals above are then
	// all that is known. Every total above is derived from this slice by
	// sumContainers, so a total can never disagree with its rows.
	Containers []ContainerUsage
```

- [ ] **Step 4: Write `internal/metrics/container.go`**

```go
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

// containerRank orders the breakdown: build first, then helper, then svc-N by
// ascending index, then anything else. Prometheus returns vector samples in no
// guaranteed order and golden files need a stable one.
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
```

- [ ] **Step 5: Run the test to verify it passes**

Run: `go test ./internal/metrics -run 'TestContainer|TestSorted|TestSum' -v`
Expected: PASS, four tests.

- [ ] **Step 6: Commit**

```bash
git add internal/metrics/container.go internal/metrics/container_test.go internal/metrics/source.go
git commit -m "feat(metrics): add per-container usage model"
```

---

## Task 2: `PodUsage` queries per container

**Files:**
- Modify: `internal/metrics/prom.go:44-113`
- Create: `internal/metrics/prom_usage_test.go`
- Create: `internal/metrics/testdata/query_containers.json`

- [ ] **Step 1: Write the fixture**

Create `internal/metrics/testdata/query_containers.json`. This is the vector the stub returns for **every** container-level query, so each container gets the same value per metric — the test asserts structure and derivation, not per-metric realism.

```json
{
  "status": "success",
  "data": {
    "resultType": "vector",
    "result": [
      {"metric": {"container": "helper", "pod": "runner-x"}, "value": [1752912000, "10"]},
      {"metric": {"container": "build", "pod": "runner-x"}, "value": [1752912000, "100"]},
      {"metric": {"container": "svc-0", "pod": "runner-x"}, "value": [1752912000, "1"]}
    ]
  }
}
```

- [ ] **Step 2: Write the failing test**

Create `internal/metrics/prom_usage_test.go`:

```go
package metrics

import (
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"
)

// usageServer serves the container vector for container-level queries and a
// label-less vector for the pod-level (network) ones, recording every query.
func usageServer(t *testing.T, seen *[]string) *httptest.Server {
	t.Helper()
	containers, err := os.ReadFile("testdata/query_containers.json")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	const podLevel = `{"status":"success","data":{"resultType":"vector","result":[{"metric":{},"value":[1752912000,"7"]}]}}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		q := r.FormValue("query")
		*seen = append(*seen, q)
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(q, "container_network_") {
			_, _ = w.Write([]byte(podLevel))
			return
		}
		_, _ = w.Write(containers)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestPodUsageGroupsByContainer(t *testing.T) {
	var seen []string
	srv := usageServer(t, &seen)
	src, err := NewPromSource(srv.URL, 30*time.Second, zap.NewNop(), nil)
	if err != nil {
		t.Fatalf("NewPromSource: %v", err)
	}
	start := time.Unix(1752912000, 0)
	u, err := src.PodUsage(t.Context(), "runner-x", start, start.Add(5*time.Minute))
	if err != nil {
		t.Fatalf("PodUsage: %v", err)
	}

	// Stable order: build, helper, svc-0.
	want := []string{"build", "helper", "svc-0"}
	if len(u.Containers) != len(want) {
		t.Fatalf("got %d containers, want %d: %+v", len(u.Containers), len(want), u.Containers)
	}
	for i, name := range want {
		if u.Containers[i].Name != name {
			t.Errorf("container %d = %q, want %q", i, u.Containers[i].Name, name)
		}
	}
	// Totals are the sum of the parts: 100 + 10 + 1.
	if u.CPUSeconds != 111 {
		t.Errorf("CPUSeconds = %v, want 111", u.CPUSeconds)
	}
	if u.PeakMemoryBytes != 111 {
		t.Errorf("PeakMemoryBytes = %d, want 111", u.PeakMemoryBytes)
	}
	// Every container reports 100 throttled of 100 periods in this fixture.
	if u.ThrottledRatio != 1 {
		t.Errorf("ThrottledRatio = %v, want 1", u.ThrottledRatio)
	}
	// Network stays pod-level and never lands on a container.
	if u.NetworkRxBytes != 7 || u.NetworkTxBytes != 7 {
		t.Errorf("network = %d/%d, want 7/7", u.NetworkRxBytes, u.NetworkTxBytes)
	}
}

func TestPodUsageQueriesGroupByContainer(t *testing.T) {
	var seen []string
	srv := usageServer(t, &seen)
	src, err := NewPromSource(srv.URL, 30*time.Second, zap.NewNop(), nil)
	if err != nil {
		t.Fatalf("NewPromSource: %v", err)
	}
	start := time.Unix(1752912000, 0)
	if _, err := src.PodUsage(t.Context(), "runner-x", start, start.Add(time.Minute)); err != nil {
		t.Fatalf("PodUsage: %v", err)
	}

	// Every container-level query groups by container and excludes the pause
	// container; the two network queries stay pod-level.
	var network int
	for _, q := range seen {
		if strings.Contains(q, "container_network_") {
			network++
			if strings.Contains(q, "by (container)") {
				t.Errorf("network query must stay pod-level: %s", q)
			}
			continue
		}
		if !strings.Contains(q, "by (container)") {
			t.Errorf("query is not grouped by container: %s", q)
		}
		if !strings.Contains(q, `container!="POD"`) {
			t.Errorf("query does not exclude the pause container: %s", q)
		}
	}
	if network != 2 {
		t.Errorf("saw %d network queries, want 2", network)
	}
	if len(seen) != 12 {
		t.Errorf("saw %d queries, want 12 (query count must not grow)", len(seen))
	}
}

func TestPodUsagePartialContainerCoverage(t *testing.T) {
	// A container that reports memory but no disk still gets a row, with the
	// missing field unset rather than zeroed.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		q := r.FormValue("query")
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.Contains(q, "container_memory_working_set_bytes"):
			_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[` +
				`{"metric":{"container":"build"},"value":[1752912000,"200"]},` +
				`{"metric":{"container":"helper"},"value":[1752912000,"50"]}]}}`))
		case strings.Contains(q, "container_fs_reads_bytes_total"):
			_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[` +
				`{"metric":{"container":"build"},"value":[1752912000,"900"]}]}}`))
		default:
			_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[]}}`))
		}
	}))
	t.Cleanup(srv.Close)
	src, err := NewPromSource(srv.URL, 30*time.Second, zap.NewNop(), nil)
	if err != nil {
		t.Fatalf("NewPromSource: %v", err)
	}
	start := time.Unix(1752912000, 0)
	u, err := src.PodUsage(t.Context(), "runner-x", start, start.Add(time.Minute))
	if err != nil {
		t.Fatalf("PodUsage: %v", err)
	}
	if len(u.Containers) != 2 {
		t.Fatalf("got %d containers, want 2: %+v", len(u.Containers), u.Containers)
	}
	if u.Containers[0].Name != "build" || u.Containers[0].DiskReadBytes != 900 {
		t.Errorf("build = %+v, want DiskReadBytes 900", u.Containers[0])
	}
	if u.Containers[1].Name != "helper" || u.Containers[1].DiskReadBytes != 0 {
		t.Errorf("helper = %+v, want DiskReadBytes unset", u.Containers[1])
	}
	if u.PeakMemoryBytes != 250 || u.DiskReadBytes != 900 {
		t.Errorf("totals = mem %d / disk %d, want 250 / 900", u.PeakMemoryBytes, u.DiskReadBytes)
	}
}

func TestPodUsageAbsentSeriesLeavesTotalsUnset(t *testing.T) {
	empty := `{"status":"success","data":{"resultType":"vector","result":[]}}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(empty))
	}))
	t.Cleanup(srv.Close)
	src, err := NewPromSource(srv.URL, 30*time.Second, zap.NewNop(), nil)
	if err != nil {
		t.Fatalf("NewPromSource: %v", err)
	}
	start := time.Unix(1752912000, 0)
	u, err := src.PodUsage(t.Context(), "runner-x", start, start.Add(time.Minute))
	if err != nil {
		t.Fatalf("PodUsage: %v", err)
	}
	if len(u.Containers) != 0 {
		t.Errorf("Containers = %+v, want empty", u.Containers)
	}
	if u.CPUSeconds != 0 || u.PeakMemoryBytes != 0 || u.ThrottledRatio != 0 {
		t.Errorf("absent series must leave totals unset, got %+v", u)
	}
}
```

- [ ] **Step 3: Run the test to verify it fails**

Run: `go test ./internal/metrics -run TestPodUsage -v`
Expected: FAIL — `PodUsage` still issues ungrouped `sum(...)` queries, so `u.Containers` is empty and the grouping assertions fail.

- [ ] **Step 4: Add the `vector` helper to `internal/metrics/prom.go`**

Insert directly after the existing `scalar` method (which stays — the network queries still use it):

```go
// vector runs an instant query grouped by container and returns one value per
// container name. An empty result yields a nil map: a query that matched no
// series leaves the field unset, it is never a measured zero.
func (s *PromSource) vector(ctx context.Context, query string, ts time.Time) (map[string]float64, error) {
	defer s.observe(time.Now())
	val, _, err := s.api.Query(ctx, query, ts)
	if err != nil {
		return nil, fmt.Errorf("prometheus query %q: %w", query, err)
	}
	vec, ok := val.(model.Vector)
	if !ok {
		return nil, fmt.Errorf("prometheus query %q: unexpected result type %s", query, val.Type())
	}
	if len(vec) == 0 {
		s.log.Debug("prometheus query matched no series", zap.String("query", query))
		return nil, nil
	}
	out := make(map[string]float64, len(vec))
	for _, sm := range vec {
		name := string(sm.Metric["container"])
		if name == "" {
			// No container label: not a per-container sample, nothing to
			// attribute it to.
			continue
		}
		out[name] = float64(sm.Value)
	}
	return out, nil
}
```

- [ ] **Step 5: Rewrite the body of `PodUsage`**

Replace everything in `internal/metrics/prom.go` between the `u := &JobUsage{...}` line and the closing `return u, nil` (i.e. the whole scalar loop plus the two throttling queries) with:

```go
	u := &JobUsage{LowConfidence: dur < 2*s.scrape}

	// One pass of container-grouped queries, accumulated per container name.
	// The pod-level totals are derived from the result below, never queried
	// separately: two query sets drift whenever a container enters or leaves
	// the window.
	acc := map[string]*ContainerUsage{}
	for _, q := range []struct {
		query string
		set   func(c *ContainerUsage, v float64)
	}{
		{fmt.Sprintf(`sum by (container) (max_over_time(container_memory_working_set_bytes{%s}[%s]))`, csel, window),
			func(c *ContainerUsage, v float64) { c.PeakMemoryBytes = uint64(v) }},
		{fmt.Sprintf(`sum by (container) (increase(container_cpu_usage_seconds_total{%s}[%s]))`, csel, window),
			func(c *ContainerUsage, v float64) { c.CPUSeconds = v }},
		{fmt.Sprintf(`sum by (container) (increase(container_fs_reads_bytes_total{%s}[%s]))`, csel, window),
			func(c *ContainerUsage, v float64) { c.DiskReadBytes = uint64(v) }},
		{fmt.Sprintf(`sum by (container) (increase(container_fs_writes_bytes_total{%s}[%s]))`, csel, window),
			func(c *ContainerUsage, v float64) { c.DiskWriteBytes = uint64(v) }},
		{fmt.Sprintf(`sum by (container) (increase(container_cpu_cfs_throttled_periods_total{%s}[%s]))`, csel, window),
			func(c *ContainerUsage, v float64) { c.ThrottledPeriods = v }},
		{fmt.Sprintf(`sum by (container) (increase(container_cpu_cfs_periods_total{%s}[%s]))`, csel, window),
			func(c *ContainerUsage, v float64) { c.Periods = v }},
		{fmt.Sprintf(`sum by (container) (max_over_time(kube_pod_container_resource_requests{%s,resource="cpu"}[%s]))`, csel, window),
			func(c *ContainerUsage, v float64) { c.CPURequestCores = v }},
		{fmt.Sprintf(`sum by (container) (max_over_time(kube_pod_container_resource_limits{%s,resource="cpu"}[%s]))`, csel, window),
			func(c *ContainerUsage, v float64) { c.CPULimitCores = v }},
		{fmt.Sprintf(`sum by (container) (max_over_time(kube_pod_container_resource_requests{%s,resource="memory"}[%s]))`, csel, window),
			func(c *ContainerUsage, v float64) { c.MemoryRequestBytes = uint64(v) }},
		{fmt.Sprintf(`sum by (container) (max_over_time(kube_pod_container_resource_limits{%s,resource="memory"}[%s]))`, csel, window),
			func(c *ContainerUsage, v float64) { c.MemoryLimitBytes = uint64(v) }},
	} {
		samples, err := s.vector(ctx, q.query, end)
		if err != nil {
			return nil, err
		}
		for name, v := range samples {
			c, ok := acc[name]
			if !ok {
				c = &ContainerUsage{Name: name}
				acc[name] = c
			}
			q.set(c, v)
		}
	}
	u.Containers = sortedContainers(acc)
	u.sumContainers()

	// Network is pod-level: cadvisor's container_network_* series carry no
	// container label, so it cannot be attributed to a container.
	for _, q := range []struct {
		query string
		set   func(v float64)
	}{
		{fmt.Sprintf(`sum(increase(container_network_receive_bytes_total{%s}[%s]))`, psel, window),
			func(v float64) { u.NetworkRxBytes = uint64(v) }},
		{fmt.Sprintf(`sum(increase(container_network_transmit_bytes_total{%s}[%s]))`, psel, window),
			func(v float64) { u.NetworkTxBytes = uint64(v) }},
	} {
		v, ok, err := s.scalar(ctx, q.query, end)
		if err != nil {
			return nil, err
		}
		if ok {
			q.set(v)
		}
	}

	s.log.Debug("pod usage computed",
		zap.String("pod", pod),
		zap.Int("containers", len(u.Containers)),
		zap.Uint64("peak_memory_bytes", u.PeakMemoryBytes),
		zap.Float64("cpu_seconds", u.CPUSeconds),
		zap.Float64("throttled_ratio", u.ThrottledRatio),
		zap.Bool("low_confidence", u.LowConfidence))
	return u, nil
```

Note the request/limit queries moved from `psel` to `csel`: `kube_pod_container_resource_*` carries a `container` label, and the container filter keeps the sum identical to what the pod-wide query returned before.

- [ ] **Step 6: Run the tests to verify they pass**

Run: `go test ./internal/metrics -v`
Expected: PASS, including the pre-existing `TestPodSeries` and `TestPodActiveSpan`.

- [ ] **Step 7: Verify the new PromQL against the snapshot rule**

The project forbids merging queries verified only "by eye". Confirm each new expression parses and is grouped as intended:

```bash
go test ./internal/metrics -run TestPodUsageQueriesGroupByContainer -v
```

Expected: PASS — this test is the mechanical check that all ten container queries carry `by (container)` and `container!="POD"`, and that the count stayed at twelve.

- [ ] **Step 8: Commit**

```bash
git add internal/metrics/prom.go internal/metrics/prom_usage_test.go internal/metrics/testdata/query_containers.json
git commit -m "feat(metrics): query pod usage per container"
```

---

## Task 3: `report.container_detail_max_jobs` config

**Files:**
- Modify: `internal/config/config.go`
- Modify: `internal/config/config_test.go`

- [ ] **Step 1: Write the failing test**

Append to `internal/config/config_test.go`:

```go
func TestContainerDetailMaxJobsDefault(t *testing.T) {
	t.Setenv("GITLAB_TOKEN", "t")
	t.Setenv("PROMETHEUS_URL", "http://prom")
	t.Setenv("WEBHOOK_SECRET", "s")

	cfg, err := Load(New())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.ContainerDetailMaxJobs != 10 {
		t.Errorf("ContainerDetailMaxJobs = %d, want 10", cfg.ContainerDetailMaxJobs)
	}
}

func TestContainerDetailMaxJobsInvalid(t *testing.T) {
	t.Setenv("GITLAB_TOKEN", "t")
	t.Setenv("PROMETHEUS_URL", "http://prom")
	t.Setenv("WEBHOOK_SECRET", "s")

	for _, raw := range []string{"-1", "many", ""} {
		t.Run(raw, func(t *testing.T) {
			t.Setenv("REPORT_CONTAINER_DETAIL_MAX_JOBS", raw)
			if _, err := Load(New()); err == nil {
				t.Errorf("Load accepted %q, want an error", raw)
			}
		})
	}
}

func TestContainerDetailMaxJobsZeroDisables(t *testing.T) {
	t.Setenv("GITLAB_TOKEN", "t")
	t.Setenv("PROMETHEUS_URL", "http://prom")
	t.Setenv("WEBHOOK_SECRET", "s")
	t.Setenv("REPORT_CONTAINER_DETAIL_MAX_JOBS", "0")

	cfg, err := Load(New())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.ContainerDetailMaxJobs != 0 {
		t.Errorf("ContainerDetailMaxJobs = %d, want 0", cfg.ContainerDetailMaxJobs)
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/config -run TestContainerDetailMaxJobs -v`
Expected: FAIL — `cfg.ContainerDetailMaxJobs undefined`.

- [ ] **Step 3: Add the setting, field and parser**

In `internal/config/config.go`, add to the `settings` slice immediately after the `report.memory_pressure_ratio` line:

```go
	{"report.container_detail_max_jobs", "10", "Nest per-container rows in the report when the pipeline has at most this many jobs (0 disables)"},
```

Add to the `Config` struct, after `MemoryPressureRatio`:

```go
	ContainerDetailMaxJobs int
```

In `Load`, after the `MemoryPressureRatio` block:

```go
	if cfg.ContainerDetailMaxJobs, err = parseInt(v.GetString("report.container_detail_max_jobs"), "REPORT_CONTAINER_DETAIL_MAX_JOBS"); err != nil {
		return nil, err
	}
```

Add the parser next to `parseBool`:

```go
// parseInt parses a non-negative integer setting.
func parseInt(raw, label string) (int, error) {
	n, err := strconv.Atoi(raw)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("%s must be a non-negative integer, got %q", label, raw)
	}
	return n, nil
}
```

- [ ] **Step 4: Cover the new key in the precedence test**

Read `internal/config/resolve_test.go` and find the test that proves flag > env > file > default. Add the new key to it in the same shape the existing keys use — a config file entry, an env override and a changed flag, asserting the flag wins:

```go
func TestResolveContainerDetailMaxJobsPrecedence(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte("report:\n  container_detail_max_jobs: 3\n"), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	t.Setenv("CIGAR_CONFIG", path)
	t.Setenv("REPORT_CONTAINER_DETAIL_MAX_JOBS", "7")

	cmd := &cobra.Command{}
	BindFlags(cmd)
	if err := cmd.PersistentFlags().Set("report-container-detail-max-jobs", "9"); err != nil {
		t.Fatalf("set flag: %v", err)
	}
	v, err := Resolve(cmd)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got := v.GetString("report.container_detail_max_jobs"); got != "9" {
		t.Errorf("value = %q, want %q (changed flag beats env and file)", got, "9")
	}
}
```

Adjust the cobra/flag plumbing to match whatever `resolve_test.go` already does — if it has a helper for building the command and setting flags, use that instead of the inline version above.

- [ ] **Step 5: Run the tests to verify they pass**

Run: `go test ./internal/config -v`
Expected: PASS, including the existing precedence tests in `resolve_test.go`.

- [ ] **Step 6: Commit**

```bash
git add internal/config/config.go internal/config/config_test.go internal/config/resolve_test.go
git commit -m "feat(config): add report.container_detail_max_jobs"
```

---

## Task 4: Nested container rows in the report

**Files:**
- Modify: `internal/report/report.go`
- Modify: `internal/report/report_test.go`
- Create: `internal/report/testdata/report-containers.md`
- Modify: `internal/report/testdata/report.md` (must stay byte-identical — see Step 5)

- [ ] **Step 1: Write the failing test**

Append to `internal/report/report_test.go`:

```go
// containerJob is one job with a three-container breakdown, shared by the
// tests below.
func containerJob() JobReport {
	base := time.Date(2026, 8, 19, 10, 0, 0, 0, time.UTC)
	u := &metrics.JobUsage{
		NetworkRxBytes: 8 * 1024 * 1024,
		NetworkTxBytes: 3 * 1024 * 1024,
		Containers: []metrics.ContainerUsage{
			{Name: "build", CPUSeconds: 39.8, PeakMemoryBytes: 380 * 1024 * 1024,
				DiskReadBytes: 580 * 1024 * 1024, DiskWriteBytes: 200 * 1024 * 1024,
				ThrottledPeriods: 12, Periods: 100,
				CPURequestCores: 0.15, CPULimitCores: 0.25,
				MemoryRequestBytes: 128 * 1024 * 1024, MemoryLimitBytes: 256 * 1024 * 1024},
			{Name: "helper", CPUSeconds: 2.3, PeakMemoryBytes: 24 * 1024 * 1024,
				DiskReadBytes: 20 * 1024 * 1024, DiskWriteBytes: 20 * 1024 * 1024,
				ThrottledPeriods: 58, Periods: 100,
				CPURequestCores: 0.1, CPULimitCores: 0.25,
				MemoryRequestBytes: 128 * 1024 * 1024, MemoryLimitBytes: 256 * 1024 * 1024},
			// No CFS series and no requests/limits: everything must render as
			// an em dash, never as a measured 0%.
			{Name: "svc-0", CPUSeconds: 0.4, PeakMemoryBytes: 8 * 1024 * 1024},
		},
	}
	u.SumForTest()
	return JobReport{Stage: "build", Name: "compile",
		StartedAt: base, FinishedAt: base.Add(2 * time.Minute), Usage: u}
}

func TestRenderContainersGolden(t *testing.T) {
	d := Data{
		PipelineID:             777,
		Status:                 "success",
		ThrottleWarnRatio:      0.25,
		ContainerDetailMaxJobs: 10,
		RanJobs:                1,
		Jobs:                   []JobReport{containerJob()},
	}
	got := mustRender(t, d)

	golden := filepath.Join("testdata", "report-containers.md")
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
		t.Errorf("report does not match %s (run with -update to refresh):\n%s", golden, got)
	}
}

func TestRenderContainersSuppressedAboveThreshold(t *testing.T) {
	d := Data{
		PipelineID:             778,
		Status:                 "success",
		ThrottleWarnRatio:      0.25,
		ContainerDetailMaxJobs: 1,
		RanJobs:                2,
		Jobs:                   []JobReport{containerJob(), {Stage: "test", Name: "unit"}},
	}
	out := mustRender(t, d)
	if strings.Contains(out, "↳") {
		t.Errorf("breakdown must be suppressed above the threshold, got:\n%s", out)
	}
}

func TestRenderContainersDisabledByZero(t *testing.T) {
	d := Data{
		PipelineID:             779,
		Status:                 "success",
		ContainerDetailMaxJobs: 0,
		RanJobs:                1,
		Jobs:                   []JobReport{containerJob()},
	}
	if out := mustRender(t, d); strings.Contains(out, "↳") {
		t.Errorf("0 must disable the breakdown entirely, got:\n%s", out)
	}
}

func TestRenderSingleContainerNotNested(t *testing.T) {
	// A one-container breakdown would just repeat the job row.
	u := &metrics.JobUsage{Containers: []metrics.ContainerUsage{
		{Name: "build", CPUSeconds: 3, PeakMemoryBytes: 64 * 1024 * 1024},
	}}
	u.SumForTest()
	d := Data{
		PipelineID:             780,
		Status:                 "success",
		ContainerDetailMaxJobs: 10,
		RanJobs:                1,
		Jobs:                   []JobReport{{Stage: "build", Name: "compile", Usage: u}},
	}
	if out := mustRender(t, d); strings.Contains(out, "↳") {
		t.Errorf("single-container job must not be nested, got:\n%s", out)
	}
}
```

- [ ] **Step 2: Export a test-only total derivation from `internal/metrics`**

`sumContainers` is unexported and `internal/report`'s tests need it to build fixtures whose totals match their parts. Add to `internal/metrics/container.go`:

```go
// SumForTest derives the pod-level totals from Containers. It exists so tests
// in other packages can build a JobUsage fixture whose totals agree with its
// rows, the same way PodUsage does. Production code must not call it: PodUsage
// already sums, and calling it twice double-counts.
func (u *JobUsage) SumForTest() { u.sumContainers() }
```

- [ ] **Step 3: Run the test to verify it fails**

Run: `go test ./internal/report -run TestRenderContainers -v`
Expected: FAIL — `unknown field ContainerDetailMaxJobs in struct literal`.

- [ ] **Step 4: Implement nesting in `internal/report/report.go`**

Add to the `Data` struct, after `ThrottleWarnRatio`:

```go
	// ContainerDetailMaxJobs nests one row per runner-pod container (build,
	// helper, svc-N) under each job, but only while the pipeline has at most
	// this many jobs — the breakdown triples a large table's height. 0 disables
	// it; the `details` command still serves the breakdown on demand.
	ContainerDetailMaxJobs int
```

Add next to `hasUsage`:

```go
// showContainers reports whether the Details table should nest per-container
// rows: the breakdown is enabled and the table is short enough to stay readable.
func (d Data) showContainers() bool {
	return d.ContainerDetailMaxJobs > 0 && len(d.Jobs) <= d.ContainerDetailMaxJobs
}
```

Replace the job loop in `Render`:

```go
	for _, j := range d.Jobs {
		row(&b, j, d.ThrottleWarnRatio)
		// A single-container breakdown only repeats the job row above it.
		if d.showContainers() && j.Usage != nil && len(j.Usage.Containers) > 1 {
			for _, c := range j.Usage.Containers {
				containerRow(&b, c, d.ThrottleWarnRatio, "↳ ")
			}
		}
	}
```

Add after `row`:

```go
// containerRow renders one container. prefix marks it as a child row in the
// job table ("↳ ") and is empty in the standalone ContainerTable. Network is
// always dashed: cadvisor's network series carry no container label, so it
// cannot be attributed here — absent, not zero.
func containerRow(b *strings.Builder, c metrics.ContainerUsage, warnRatio float64, prefix string) {
	fmt.Fprintf(b, "| %s%s | %s | %s | %s / %s | %s / %s | %s | %s / %s | %s / %s |\n",
		prefix, c.Name,
		cpuTime(c.CPUSeconds),
		humanBytes(c.PeakMemoryBytes),
		optBytes(c.MemoryRequestBytes), optBytes(c.MemoryLimitBytes),
		cores(c.CPURequestCores), cores(c.CPULimitCores),
		containerThrottle(c, warnRatio),
		dash, dash,
		optBytes(c.DiskReadBytes), optBytes(c.DiskWriteBytes),
	)
}

// containerThrottle renders a container's throttled percentage, or a dash when
// its CFS period series was absent (absent ≠ 0%).
func containerThrottle(c metrics.ContainerUsage, warnRatio float64) string {
	r, ok := c.ThrottledRatio()
	if !ok {
		return dash
	}
	return throttle(r, warnRatio)
}

// ContainerTable renders a standalone per-container table for one job — the
// `details` command reply. It shares containerRow with the report's nested
// rows so the two renderings cannot drift. Returns "" when there is nothing to
// show.
func ContainerTable(containers []metrics.ContainerUsage, warnRatio float64) string {
	if len(containers) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("| Container | CPU time | Peak memory | Mem req / limit | CPU req / limit | Throttled | Network RX / TX | Disk R / W |\n")
	b.WriteString("|---|---|---|---|---|---|---|---|\n")
	for _, c := range containers {
		containerRow(&b, c, warnRatio, "")
	}
	return b.String()
}
```

- [ ] **Step 5: Create the new golden and confirm the old one did not move**

```bash
go test ./internal/report -run TestRenderContainersGolden -update
git diff --stat internal/report/testdata/
```

Expected: `report-containers.md` is created; **`report.md` must not appear in the diff.** The existing golden's `Data` has no `ContainerDetailMaxJobs`, so it is 0 and nesting is off — if `report.md` changed, the gate in `showContainers` is wrong. Fix it before continuing.

Read `report-containers.md` and confirm:
- three `↳` rows under `build : compile`, in order `build`, `helper`, `svc-0`;
- the helper row shows `**58%** ⚠️` and the build row `12%`;
- the `svc-0` row shows `—` in the Throttled column, not `0%`;
- every container row shows `— / —` for Network.

- [ ] **Step 6: Run the tests to verify they pass**

Run: `go test ./internal/report -v`
Expected: PASS, all tests.

- [ ] **Step 7: Commit**

```bash
git add internal/report/report.go internal/report/report_test.go internal/report/testdata/report-containers.md internal/metrics/container.go
git commit -m "feat(report): nest per-container rows under each job"
```

---

## Task 5: Per-container CPU-throttling advice

**Files:**
- Modify: `internal/advice/cpu_throttle.go`
- Modify: `internal/advice/cpu_throttle_test.go`

- [ ] **Step 1: Write the failing test**

Append to `internal/advice/cpu_throttle_test.go`:

```go
func TestCPUThrottlePerContainer(t *testing.T) {
	th := Thresholds{ThrottleWarnRatio: 0.25}
	u := &metrics.JobUsage{Containers: []metrics.ContainerUsage{
		{Name: "build", ThrottledPeriods: 41, Periods: 100, CPURequestCores: 0.25, CPULimitCores: 0.5},
		{Name: "helper", ThrottledPeriods: 58, Periods: 100, CPURequestCores: 0.1, CPULimitCores: 0.25},
		{Name: "svc-0", ThrottledPeriods: 60, Periods: 100},
		{Name: "svc-1", ThrottledPeriods: 1, Periods: 100}, // below threshold, quiet
	}}
	u.SumForTest()
	got := cpuThrottle{}.Check(Facts{Name: "compile", Usage: u}, th)

	if len(got) != 3 {
		t.Fatalf("Check returned %d advice, want 3: %+v", len(got), got)
	}
	// Stable order, following the container order.
	wantTitles := []string{
		"⚠️ CPU throttling — build",
		"⚠️ CPU throttling — helper",
		"⚠️ CPU throttling — svc-0",
	}
	for i, want := range wantTitles {
		if got[i].Title != want {
			t.Errorf("advice %d title = %q, want %q", i, got[i].Title, want)
		}
		if got[i].Rule != "cpu-throttle" || got[i].Job != "compile" {
			t.Errorf("advice %d = %+v, want rule cpu-throttle for job compile", i, got[i])
		}
	}
	// Each container gets the variables that actually govern it.
	if !strings.Contains(got[0].Body, "KUBERNETES_CPU_LIMIT") ||
		strings.Contains(got[0].Body, "KUBERNETES_HELPER") {
		t.Errorf("build advice must use the build variables:\n%s", got[0].Body)
	}
	for _, want := range []string{"KUBERNETES_HELPER_CPU_REQUEST", "KUBERNETES_HELPER_CPU_LIMIT", "58%", "git clone"} {
		if !strings.Contains(got[1].Body, want) {
			t.Errorf("helper advice missing %q:\n%s", want, got[1].Body)
		}
	}
	for _, want := range []string{"KUBERNETES_SERVICE_CPU_REQUEST", "KUBERNETES_SERVICE_CPU_LIMIT", "every"} {
		if !strings.Contains(got[2].Body, want) {
			t.Errorf("service advice missing %q:\n%s", want, got[2].Body)
		}
	}
}

func TestCPUThrottleIgnoresContainersWithoutCFSSeries(t *testing.T) {
	th := Thresholds{ThrottleWarnRatio: 0.25}
	u := &metrics.JobUsage{Containers: []metrics.ContainerUsage{{Name: "build", CPUSeconds: 5}}}
	u.SumForTest()
	if got := cpuThrottle{}.Check(Facts{Name: "compile", Usage: u}, th); len(got) != 0 {
		t.Errorf("absent CFS series must not fire advice, got %+v", got)
	}
}

func TestCPUThrottleFallsBackWithoutBreakdown(t *testing.T) {
	// A Prometheus that cannot answer the container breakdown must produce
	// exactly the pre-breakdown advice.
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
	if got[0].Title != "⚠️ CPU throttling" {
		t.Errorf("title = %q, want the un-suffixed fallback title", got[0].Title)
	}
	if !strings.HasPrefix(got[0].Body, "This job spent **41%** of its CPU periods throttled, against a limit of 500m.") {
		t.Errorf("fallback body changed:\n%s", got[0].Body)
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/advice -run TestCPUThrottle -v`
Expected: FAIL — `TestCPUThrottlePerContainer` gets 1 advice (the rule still reads only the pod-level ratio), not 3.

- [ ] **Step 3: Rewrite `internal/advice/cpu_throttle.go`**

Replace the `Check` method and add the two helpers; `suggestedCPULimitMillis`, `suggestedCPULimit` and `suggestedCPURequest` stay exactly as they are.

```go
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
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/advice -v`
Expected: PASS. The pre-existing `TestCPUThrottleFires`, `TestCPUThrottleUnsetLimit` and `TestCPUThrottleQuiet` all use a `JobUsage` with no `Containers`, so they exercise the fallback and must still pass unchanged. If `advise.md` golden fails, the fallback text drifted — fix the code, do not refresh the golden.

- [ ] **Step 5: Commit**

```bash
git add internal/advice/cpu_throttle.go internal/advice/cpu_throttle_test.go
git commit -m "feat(advice): report CPU throttling per container"
```

---

## Task 6: Wire the threshold through reporter and CLI

**Files:**
- Modify: `internal/reporter/reporter.go:19-30,87`
- Modify: `cmd/bot/deps.go:68-75`
- Modify: `cmd/bot/run.go:108-115`
- Modify: `cmd/bot/serve.go:106`

- [ ] **Step 1: Write the failing test**

Append to `internal/reporter/reporter_test.go`, which already defines the `fakeGitLab` stub this test uses:

```go
func TestBuildPassesContainerDetailMaxJobs(t *testing.T) {
	r := &Reporter{
		GitLab:                 &fakeGitLab{},
		ContainerDetailMaxJobs: 4,
		Log:                    zap.NewNop(),
	}
	data, err := r.Build(t.Context(), 1, 2)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if data.ContainerDetailMaxJobs != 4 {
		t.Errorf("ContainerDetailMaxJobs = %d, want 4", data.ContainerDetailMaxJobs)
	}
}
```

A zero-value `fakeGitLab` returns no jobs and no error from `PipelineJobs`, which is all this test needs.

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/reporter -run TestBuildPassesContainerDetailMaxJobs -v`
Expected: FAIL — `unknown field ContainerDetailMaxJobs`.

- [ ] **Step 3: Add the field and pass it through**

In `internal/reporter/reporter.go`, add to the `Reporter` struct after `ThrottleWarnRatio`:

```go
	// ContainerDetailMaxJobs is forwarded to report.Data: the job-count ceiling
	// under which the report nests per-container rows.
	ContainerDetailMaxJobs int
```

In `Build`, replace the `data := report.Data{...}` line with:

```go
	data := report.Data{
		PipelineID:             pipelineID,
		ThrottleWarnRatio:      r.ThrottleWarnRatio,
		ContainerDetailMaxJobs: r.ContainerDetailMaxJobs,
	}
```

In `cmd/bot/deps.go`, in the `&reporter.Reporter{...}` literal, after `ThrottleWarnRatio: cfg.ThrottleWarnRatio,`:

```go
		ContainerDetailMaxJobs: cfg.ContainerDetailMaxJobs,
```

In `cmd/bot/run.go`, in the `report.Data{...}` literal, after `ThrottleWarnRatio: cfg.ThrottleWarnRatio,`:

```go
		ContainerDetailMaxJobs: cfg.ContainerDetailMaxJobs,
```

`bot run` reports a single job, so the breakdown is always shown there — which is the point of the command.

In `cmd/bot/serve.go`, add to the startup log fields next to `zap.Float64("throttle_warn_ratio", ...)`:

```go
		zap.Int("container_detail_max_jobs", cfg.ContainerDetailMaxJobs),
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/reporter ./cmd/... -v`
Expected: PASS.

- [ ] **Step 5: Verify `bot run` shows the breakdown**

Run: `mise r build && ./bot run --help`
Expected: the flag `--report-container-detail-max-jobs` is listed among the persistent flags.

- [ ] **Step 6: Commit**

```bash
git add internal/reporter cmd/bot
git commit -m "feat: wire container-detail threshold through reporter and CLI"
```

---

## Task 7: Container table in the `details` command

**Files:**
- Modify: `internal/command/handler.go:33-44,119-165`
- Modify: `internal/command/command.go:106-113`
- Modify: `internal/command/handler_test.go`
- Modify: `internal/command/command_test.go`
- Modify: `cmd/bot/deps.go:122-135`

- [ ] **Step 1: Write the failing test**

Append to `internal/command/handler_test.go`. It already defines `fakeGitLab`, `fakeResolver`, `fakeSeries`, `signedRoot`, `nonEmptySeries` and `newHandler(gl, res, se)` — reuse them, do not add duplicates. Add `"errors"` to the file's imports if it is not already there.

```go
// fakeUsage serves one pod usage with a two-container breakdown.
type fakeUsage struct{ err error }

func (f *fakeUsage) PodUsage(context.Context, string, time.Time, time.Time) (*metrics.JobUsage, error) {
	if f.err != nil {
		return nil, f.err
	}
	u := &metrics.JobUsage{Containers: []metrics.ContainerUsage{
		{Name: "build", CPUSeconds: 39.8, PeakMemoryBytes: 380 * 1024 * 1024,
			ThrottledPeriods: 12, Periods: 100},
		{Name: "helper", CPUSeconds: 2.3, PeakMemoryBytes: 24 * 1024 * 1024,
			ThrottledPeriods: 58, Periods: 100},
	}}
	u.SumForTest()
	return u, nil
}

// detailsHandler builds a handler whose `details job build` resolves to a pod
// with a non-empty series, matching TestHandleDetailsJob's fixture.
func detailsHandler(t *testing.T, usage *fakeUsage) (*Handler, *fakeGitLab) {
	t.Helper()
	start := time.Now().Add(-5 * time.Minute)
	end := time.Now()
	gl := &fakeGitLab{
		discussion: signedRoot(42, 3),
		jobs:       []gitlab.Job{{ID: 1, Name: "build", StartedAt: start, FinishedAt: end}},
	}
	res := &fakeResolver{pods: map[int64]string{1: "runner-abc-project-7-concurrent-0"}}
	h := newHandler(gl, res, &fakeSeries{series: nonEmptySeries()})
	h.Usage = usage
	h.ThrottleWarnRatio = 0.25
	return h, gl
}

func detailsEvent() NoteEvent {
	return NoteEvent{ProjectID: 7, MRIID: 3, DiscussionID: "abc", AuthorID: 9, Body: "details job build"}
}

func TestHandleDetailsIncludesContainerTable(t *testing.T) {
	h, gl := detailsHandler(t, &fakeUsage{})

	if err := h.Handle(t.Context(), detailsEvent()); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if len(gl.replies) != 1 {
		t.Fatalf("replies = %d, want 1", len(gl.replies))
	}
	for _, want := range []string{"| Container |", "| build |", "| helper |", "**58%** ⚠️"} {
		if !strings.Contains(gl.replies[0], want) {
			t.Errorf("reply missing %q:\n%s", want, gl.replies[0])
		}
	}
}

func TestHandleDetailsStillRepliesWhenUsageFails(t *testing.T) {
	// A chart is worth more than an error: the breakdown is best-effort.
	h, gl := detailsHandler(t, &fakeUsage{err: errors.New("prometheus down")})

	if err := h.Handle(t.Context(), detailsEvent()); err != nil {
		t.Fatalf("Handle must not fail when the breakdown is unavailable: %v", err)
	}
	if len(gl.replies) != 1 {
		t.Fatalf("replies = %d, want 1", len(gl.replies))
	}
	if strings.Contains(gl.replies[0], "| Container |") {
		t.Errorf("failed breakdown must be omitted, not rendered:\n%s", gl.replies[0])
	}
}

func TestHandleDetailsWithoutUsageSource(t *testing.T) {
	// A nil source is legal: charts only, no table, no error.
	h, gl := detailsHandler(t, nil)
	h.Usage = nil

	if err := h.Handle(t.Context(), detailsEvent()); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if len(gl.replies) != 1 || strings.Contains(gl.replies[0], "| Container |") {
		t.Errorf("nil usage source must omit the table:\n%v", gl.replies)
	}
}
```

Note `detailsHandler(t, nil)` passes a typed nil `*fakeUsage` into the `metrics.Source` interface field, which is **not** a nil interface — that is why the last test reassigns `h.Usage = nil` explicitly.

Also append to `internal/command/command_test.go`:

```go
func TestHelpTextMentionsContainers(t *testing.T) {
	if !strings.Contains(HelpText, "container") {
		t.Errorf("HelpText should say details includes the container breakdown:\n%s", HelpText)
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/command -run 'TestDetails|TestHelpText' -v`
Expected: FAIL — `h.Usage undefined`.

- [ ] **Step 3: Add the fields to `Handler`**

In `internal/command/handler.go`, add to the `Handler` struct after `Series`:

```go
	// Usage supplies the per-container breakdown shown by `details`. Optional:
	// a nil source, or a failing query, simply omits the table.
	Usage             metrics.Source
	ThrottleWarnRatio float64
```

- [ ] **Step 4: Render the table in `details`**

In `internal/command/handler.go`, inside `details`, replace:

```go
	var body strings.Builder
	fmt.Fprintf(&body, "### Resource usage for `%s`\n\n", cmd.Name)
```

with:

```go
	var body strings.Builder
	fmt.Fprintf(&body, "### Resource usage for `%s`\n\n", cmd.Name)
	h.writeContainerTable(ctx, &body, pod, start, end)
```

and add after `details`:

```go
// writeContainerTable appends the per-container breakdown for pod. It is
// best-effort: a nil source, a failed query or a pod with no container series
// leaves the reply to its charts rather than turning into an error.
func (h *Handler) writeContainerTable(ctx context.Context, body *strings.Builder, pod string, start, end time.Time) {
	if h.Usage == nil {
		return
	}
	u, err := h.Usage.PodUsage(ctx, pod, start, end)
	if err != nil {
		h.Log.Warn("container breakdown unavailable",
			zap.String("pod", pod), zap.Error(err))
		return
	}
	table := report.ContainerTable(u.Containers, h.ThrottleWarnRatio)
	if table == "" {
		return
	}
	body.WriteString(table)
	body.WriteString("\n")
}
```

- [ ] **Step 5: Update `HelpText`**

In `internal/command/command.go`, change the two `details` lines to:

```go
	"- `details job <name>` — per-container breakdown plus CPU / memory / network charts for a job in this report\n" +
	"- `details pod <runner-...>` — same, for a runner pod in this report\n" +
```

- [ ] **Step 6: Wire it in `cmd/bot/deps.go`**

In `newCommandHandler`, in the `&command.Handler{...}` literal, after `Series: source,`:

```go
		Usage:             source,
		ThrottleWarnRatio: cfg.ThrottleWarnRatio,
```

`source` is the same `*metrics.PromSource` already built in that function; it satisfies both `metrics.SeriesSource` and `metrics.Source`.

- [ ] **Step 7: Run the tests to verify they pass**

Run: `go test ./internal/command ./cmd/... -v`
Expected: PASS.

- [ ] **Step 8: Commit**

```bash
git add internal/command cmd/bot/deps.go
git commit -m "feat(command): show the container breakdown in details replies"
```

---

## Task 8: End-to-end proof

**Files:**
- Modify: `internal/e2e/e2e_test.go:166-205`

- [ ] **Step 1: Write the failing assertion**

In `internal/e2e/e2e_test.go`, replace the fallback response in `mockProm.server` — the final `fmt.Fprint` that returns a label-less vector — with a container-aware one:

```go
		// Container-grouped queries get a two-container pod; pod-level queries
		// (network) keep the label-less sample.
		if strings.Contains(query, "by (container)") {
			_, _ = fmt.Fprint(w,
				`{"status":"success","data":{"resultType":"vector","result":[`+
					`{"metric":{"container":"build"},"value":[1752912000,"100"]},`+
					`{"metric":{"container":"helper"},"value":[1752912000,"23.45"]}]}}`)
			return
		}
		_, _ = fmt.Fprint(w,
			`{"status":"success","data":{"resultType":"vector","result":[{"metric":{},"value":[1752912000,"123.45"]}]}}`)
```

Then add to the existing end-to-end test's assertions on the posted note body (find it by searching for `sawQuery` and the note-body assertions):

```go
	// The note carries the per-container breakdown and names the helper.
	for _, want := range []string{"↳ build", "↳ helper"} {
		if !strings.Contains(body, want) {
			t.Errorf("note body missing %q:\n%s", want, body)
		}
	}
	if !m.sawQuery("by (container)") {
		t.Errorf("no container-grouped query was issued; queries=%v", m.queries)
	}
```

- [ ] **Step 2: Run the test to verify it fails or passes**

Run: `mise r test:e2e`
Expected: PASS if Tasks 1-6 are complete — this task is the integration proof, not new behavior. If it FAILS, the wiring from Task 6 is incomplete: check that `reporter.Reporter.ContainerDetailMaxJobs` is set in the e2e's own reporter construction. The e2e builds its `Reporter` directly, so it needs `ContainerDetailMaxJobs: 10` added to that literal.

- [ ] **Step 3: Set the threshold in the e2e reporter if needed**

In `internal/e2e/e2e_test.go`, in the `reporter.Reporter{...}` literal:

```go
		ContainerDetailMaxJobs: 10,
```

- [ ] **Step 4: Run the full suite**

Run: `mise r test`
Expected: PASS, race detector clean.

- [ ] **Step 5: Commit**

```bash
git add internal/e2e/e2e_test.go
git commit -m "test(e2e): assert the container breakdown survives the whole chain"
```

---

## Task 9: Chart, docs and the definition of done

**Files:**
- Modify: `deploy/chart/cigar/values.yaml:29-32`
- Modify: `deploy/chart/cigar/templates/configmap.yaml:21-24`
- Modify: `deploy/chart/cigar/tests/config_test.yaml`
- Modify: `README.md`, `docs/usage.md`, `docs/deploy.md`

- [ ] **Step 1: Write the failing helm-unittest case**

In `deploy/chart/cigar/tests/config_test.yaml`, add `container_detail_max_jobs: "10"` to the default-render assertion, immediately after the `memory_pressure_ratio` line:

```yaml
            report:
              throttle_warn_ratio: "0.25"
              long_job_duration: "10m"
              memory_pressure_ratio: "0.9"
              container_detail_max_jobs: "10"
```

and add a new test at the end of the `tests:` list:

```yaml
  - it: propagates containerDetailMaxJobs
    set:
      config:
        report:
          containerDetailMaxJobs: 25
    asserts:
      - matchRegex:
          path: data["config.yaml"]
          pattern: 'container_detail_max_jobs: "25"'
```

- [ ] **Step 2: Run it to verify it fails**

Run: `mise r helm:test`
Expected: FAIL — the rendered `config.yaml` has no `container_detail_max_jobs` key.

- [ ] **Step 3: Add the value and the configmap line**

In `deploy/chart/cigar/values.yaml`, under `config.report`:

```yaml
  report:
    throttleWarnRatio: "0.25"
    longJobDuration: "10m"
    memoryPressureRatio: "0.9"
    # Nest per-container rows (build / helper / svc-N) under each job while the
    # pipeline has at most this many jobs. 0 disables; `details <job>` always
    # serves the breakdown on demand.
    containerDetailMaxJobs: 10
```

In `deploy/chart/cigar/templates/configmap.yaml`, after the `memory_pressure_ratio` line:

```yaml
      container_detail_max_jobs: {{ .Values.config.report.containerDetailMaxJobs | quote }}
```

- [ ] **Step 4: Run it to verify it passes**

Run: `mise r helm:test`
Expected: PASS — `helm lint` clean and every unittest suite green.

- [ ] **Step 5: Update the docs**

In `docs/usage.md`, add `REPORT_CONTAINER_DETAIL_MAX_JOBS` to the settings table (yaml `report.container_detail_max_jobs`, flag `--report-container-detail-max-jobs`, default `10`) with the description: *"Nest per-container rows under each job while the pipeline has at most this many jobs; 0 disables the breakdown."* Add a short subsection explaining the three container kinds and which CI variables govern each:

```md
### Containers in a runner pod

A GitLab Kubernetes runner pod runs more than the job's script:

| Container | What it does | CPU variables |
|---|---|---|
| `build` | the job's `script:` | `KUBERNETES_CPU_REQUEST` / `KUBERNETES_CPU_LIMIT` |
| `helper` | git clone, artifacts, cache | `KUBERNETES_HELPER_CPU_REQUEST` / `KUBERNETES_HELPER_CPU_LIMIT` |
| `svc-N` | one per CI `services:` entry | `KUBERNETES_SERVICE_CPU_REQUEST` / `KUBERNETES_SERVICE_CPU_LIMIT` (all services) |

A job row in the report is the pod total — every container summed. The `↳` rows
beneath it show the split. A throttled `helper` is worth acting on even when the
job's own script is not throttled: it stalls clone, cache and artifact upload,
which inflates wall-clock time without appearing in the build container's numbers.
```

In `docs/deploy.md`, add `config.report.containerDetailMaxJobs` to the chart reference table alongside the other `config.report.*` values.

In `README.md`, refresh the report screenshot/sample so it shows the nested rows (the definition of done requires the screenshot to match the comment format), and mention the breakdown in the feature list.

- [ ] **Step 6: Update CLAUDE.md's PromQL section**

The queries documented in `CLAUDE.md` under "PromQL queries" now group by container. Update those bullets to say `sum by (container)(...)` for the container-level metrics, and add a line noting that requests/limits are now container-filtered.

- [ ] **Step 7: Run the full definition of done**

```bash
mise r lint test
mise r helm:test
```

Expected: both clean, race detector on.

- [ ] **Step 8: Commit**

```bash
git add deploy/chart docs README.md CLAUDE.md
git commit -m "docs: document the per-container breakdown and its threshold"
```

---

## Final verification

- [ ] `mise r lint test` clean, race detector on
- [ ] `mise r helm:test` clean
- [ ] `internal/report/testdata/report.md` unchanged from `main` (`git diff main -- internal/report/testdata/report.md` is empty) — the breakdown must be off by default in that fixture
- [ ] `internal/advice/testdata/advise.md` unchanged from `main` — the no-breakdown fallback is byte-identical to the old advice
- [ ] `git log --oneline main..HEAD` shows nine focused commits
