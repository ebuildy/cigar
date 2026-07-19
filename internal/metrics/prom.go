package metrics

import (
	"context"
	"fmt"
	"time"

	"github.com/prometheus/client_golang/api"
	promv1 "github.com/prometheus/client_golang/api/prometheus/v1"
	"github.com/prometheus/common/model"
)

// NewPromSource returns the Prometheus-backed Source (cadvisor +
// kube-state-metrics queries). Windows are padded by one scrapeInterval so
// short jobs still cover at least one sample.
func NewPromSource(promURL string, scrapeInterval time.Duration) (Source, error) {
	c, err := api.NewClient(api.Config{Address: promURL})
	if err != nil {
		return nil, fmt.Errorf("create prometheus client: %w", err)
	}
	return &promSource{api: promv1.NewAPI(c), scrape: scrapeInterval}, nil
}

type promSource struct {
	api    promv1.API
	scrape time.Duration
}

func (s *promSource) PodUsage(ctx context.Context, pod string, start, end time.Time) (*JobUsage, error) {
	dur := end.Sub(start)
	if dur <= 0 {
		return nil, fmt.Errorf("invalid job window %s..%s", start, end)
	}
	window := fmt.Sprintf("%dms", (dur + s.scrape).Milliseconds())
	// Container-level series: always exclude the pause container.
	csel := fmt.Sprintf(`pod=%q,container!="",container!="POD"`, pod)
	// Pod-level series (network) and kube-state-metrics.
	psel := fmt.Sprintf(`pod=%q`, pod)

	u := &JobUsage{LowConfidence: dur < 2*s.scrape}

	for _, q := range []struct {
		query string
		set   func(v float64)
	}{
		{fmt.Sprintf(`sum(max_over_time(container_memory_working_set_bytes{%s}[%s]))`, csel, window),
			func(v float64) { u.PeakMemoryBytes = uint64(v) }},
		{fmt.Sprintf(`sum(increase(container_cpu_usage_seconds_total{%s}[%s]))`, csel, window),
			func(v float64) { u.CPUSeconds = v }},
		{fmt.Sprintf(`sum(increase(container_network_receive_bytes_total{%s}[%s]))`, psel, window),
			func(v float64) { u.NetworkRxBytes = uint64(v) }},
		{fmt.Sprintf(`sum(increase(container_network_transmit_bytes_total{%s}[%s]))`, psel, window),
			func(v float64) { u.NetworkTxBytes = uint64(v) }},
		{fmt.Sprintf(`sum(max_over_time(kube_pod_container_resource_requests{%s,resource="cpu"}[%s]))`, psel, window),
			func(v float64) { u.CPURequestCores = v }},
		{fmt.Sprintf(`sum(max_over_time(kube_pod_container_resource_limits{%s,resource="cpu"}[%s]))`, psel, window),
			func(v float64) { u.CPULimitCores = v }},
		{fmt.Sprintf(`sum(max_over_time(kube_pod_container_resource_requests{%s,resource="memory"}[%s]))`, psel, window),
			func(v float64) { u.MemoryRequestBytes = uint64(v) }},
		{fmt.Sprintf(`sum(max_over_time(kube_pod_container_resource_limits{%s,resource="memory"}[%s]))`, psel, window),
			func(v float64) { u.MemoryLimitBytes = uint64(v) }},
	} {
		v, ok, err := s.scalar(ctx, q.query, end)
		if err != nil {
			return nil, err
		}
		if ok {
			q.set(v)
		}
	}

	throttled, _, err := s.scalar(ctx,
		fmt.Sprintf(`sum(increase(container_cpu_cfs_throttled_periods_total{%s}[%s]))`, csel, window), end)
	if err != nil {
		return nil, err
	}
	periods, ok, err := s.scalar(ctx,
		fmt.Sprintf(`sum(increase(container_cpu_cfs_periods_total{%s}[%s]))`, csel, window), end)
	if err != nil {
		return nil, err
	}
	if ok && periods > 0 {
		u.ThrottledRatio = throttled / periods
	}
	return u, nil
}

// scalar runs an instant query expected to yield at most one sample.
// ok is false when the query matched no series (metric absent ≠ zero).
func (s *promSource) scalar(ctx context.Context, query string, ts time.Time) (float64, bool, error) {
	val, _, err := s.api.Query(ctx, query, ts)
	if err != nil {
		return 0, false, fmt.Errorf("prometheus query %q: %w", query, err)
	}
	vec, ok := val.(model.Vector)
	if !ok {
		return 0, false, fmt.Errorf("prometheus query %q: unexpected result type %s", query, val.Type())
	}
	if len(vec) == 0 {
		return 0, false, nil
	}
	return float64(vec[0].Value), true, nil
}
