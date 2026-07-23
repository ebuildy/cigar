package metrics

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"
)

// rangeServer serves the query_range snapshot and records the queries it saw.
func rangeServer(t *testing.T, seen *[]string) *httptest.Server {
	t.Helper()
	body, err := os.ReadFile("testdata/query_range.json")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		*seen = append(*seen, r.FormValue("query"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestPodSeries(t *testing.T) {
	var seen []string
	srv := rangeServer(t, &seen)
	src, err := NewPromSource(srv.URL, 30*time.Second, zap.NewNop())
	if err != nil {
		t.Fatalf("NewPromSource: %v", err)
	}
	start := time.Unix(1752912000, 0)
	end := start.Add(60 * time.Second)

	s, err := src.PodSeries(context.Background(), "runner-x", start, end)
	if err != nil {
		t.Fatalf("PodSeries: %v", err)
	}
	if len(s.CPU.Points) != 3 {
		t.Fatalf("CPU points = %d, want 3", len(s.CPU.Points))
	}
	if s.CPU.Points[2].V != 0.30 {
		t.Fatalf("last CPU value = %v, want 0.30", s.CPU.Points[2].V)
	}
	sawExclude := false
	for _, q := range seen {
		if strings.Contains(q, `container!="POD"`) && strings.Contains(q, "container_cpu_usage_seconds_total") {
			sawExclude = true
		}
	}
	if !sawExclude {
		t.Errorf("CPU query did not exclude the POD container; queries=%v", seen)
	}
}

func TestPodActiveSpan(t *testing.T) {
	var seen []string
	srv := rangeServer(t, &seen)
	src, err := NewPromSource(srv.URL, 30*time.Second, zap.NewNop())
	if err != nil {
		t.Fatalf("NewPromSource: %v", err)
	}
	start, end, ok, err := src.PodActiveSpan(context.Background(), "runner-x")
	if err != nil {
		t.Fatalf("PodActiveSpan: %v", err)
	}
	if !ok {
		t.Fatal("PodActiveSpan ok = false, want true")
	}
	if !start.Equal(time.Unix(1752912000, 0)) || !end.Equal(time.Unix(1752912060, 0)) {
		t.Fatalf("span = [%v,%v], want [1752912000,1752912060]", start.Unix(), end.Unix())
	}
}
