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
	// Every container reports as many throttled periods as periods here.
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
