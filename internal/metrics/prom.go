package metrics

import (
	"context"
	"fmt"
	"time"

	"github.com/prometheus/client_golang/api"
	promv1 "github.com/prometheus/client_golang/api/prometheus/v1"
	"github.com/prometheus/common/model"
	"go.uber.org/zap"
)

// NewPromSource returns the Prometheus-backed source. It satisfies both Source
// (aggregation) and SeriesSource (range queries).
func NewPromSource(promURL string, scrapeInterval time.Duration, log *zap.Logger) (*PromSource, error) {
	c, err := api.NewClient(api.Config{Address: promURL})
	if err != nil {
		return nil, fmt.Errorf("create prometheus client: %w", err)
	}
	log.Debug("prometheus metrics source created", zap.String("url", promURL))
	return &PromSource{api: promv1.NewAPI(c), scrape: scrapeInterval, log: log}, nil
}

type PromSource struct {
	api    promv1.API
	scrape time.Duration
	log    *zap.Logger
}

func (s *PromSource) PodUsage(ctx context.Context, pod string, start, end time.Time) (*JobUsage, error) {
	dur := end.Sub(start)
	if dur <= 0 {
		return nil, fmt.Errorf("invalid job window %s..%s", start, end)
	}
	window := fmt.Sprintf("%dms", (dur + s.scrape).Milliseconds())
	s.log.Debug("querying pod usage", zap.String("pod", pod), zap.String("window", window))
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
	s.log.Debug("pod usage computed",
		zap.String("pod", pod),
		zap.Uint64("peak_memory_bytes", u.PeakMemoryBytes),
		zap.Float64("cpu_seconds", u.CPUSeconds),
		zap.Float64("throttled_ratio", u.ThrottledRatio),
		zap.Bool("low_confidence", u.LowConfidence))
	return u, nil
}

// scalar runs an instant query expected to yield at most one sample.
// ok is false when the query matched no series (metric absent ≠ zero).
func (s *PromSource) scalar(ctx context.Context, query string, ts time.Time) (float64, bool, error) {
	val, _, err := s.api.Query(ctx, query, ts)
	if err != nil {
		return 0, false, fmt.Errorf("prometheus query %q: %w", query, err)
	}
	vec, ok := val.(model.Vector)
	if !ok {
		return 0, false, fmt.Errorf("prometheus query %q: unexpected result type %s", query, val.Type())
	}
	if len(vec) == 0 {
		s.log.Debug("prometheus query matched no series", zap.String("query", query))
		return 0, false, nil
	}
	return float64(vec[0].Value), true, nil
}

// activeSpanLookback bounds how far back PodActiveSpan looks for a pod's series.
const activeSpanLookback = 6 * time.Hour

// PodSeries returns aligned CPU/memory/network series over [start,end] via
// query_range. Absent series yield empty lines (never fabricated as zero).
func (s *PromSource) PodSeries(ctx context.Context, pod string, start, end time.Time) (PodSeries, error) {
	step := seriesStep(end.Sub(start))
	rng := rangeVector(step, s.scrape)
	csel := fmt.Sprintf(`pod=%q,container!="",container!="POD"`, pod)
	psel := fmt.Sprintf(`pod=%q`, pod)

	var out PodSeries
	for _, q := range []struct {
		query string
		line  *Line
		label string
	}{
		{fmt.Sprintf(`sum(rate(container_cpu_usage_seconds_total{%s}[%s]))`, csel, rng), &out.CPU, "cpu"},
		{fmt.Sprintf(`sum(container_memory_working_set_bytes{%s})`, csel), &out.Memory, "memory"},
		{fmt.Sprintf(`sum(rate(container_network_receive_bytes_total{%s}[%s]))`, psel, rng), &out.NetRx, "rx"},
		{fmt.Sprintf(`sum(rate(container_network_transmit_bytes_total{%s}[%s]))`, psel, rng), &out.NetTx, "tx"},
	} {
		pts, err := s.rangePoints(ctx, q.query, start, end, step)
		if err != nil {
			return PodSeries{}, err
		}
		q.line.Label = q.label
		q.line.Points = pts
	}
	return out, nil
}

// PodActiveSpan returns the min/max sample timestamps of the pod's memory series
// over the lookback window. ok is false when the pod has no samples.
func (s *PromSource) PodActiveSpan(ctx context.Context, pod string) (time.Time, time.Time, bool, error) {
	now := time.Now()
	pts, err := s.rangePoints(ctx,
		fmt.Sprintf(`sum(container_memory_working_set_bytes{pod=%q,container!="",container!="POD"})`, pod),
		now.Add(-activeSpanLookback), now, s.scrape)
	if err != nil {
		return time.Time{}, time.Time{}, false, err
	}
	if len(pts) == 0 {
		return time.Time{}, time.Time{}, false, nil
	}
	return pts[0].T, pts[len(pts)-1].T, true, nil
}

// rangePoints runs a query_range and flattens the single matrix stream.
func (s *PromSource) rangePoints(ctx context.Context, query string, start, end time.Time, step time.Duration) ([]Point, error) {
	val, _, err := s.api.QueryRange(ctx, query, promv1.Range{Start: start, End: end, Step: step})
	if err != nil {
		return nil, fmt.Errorf("prometheus range query %q: %w", query, err)
	}
	m, ok := val.(model.Matrix)
	if !ok {
		return nil, fmt.Errorf("prometheus range query %q: unexpected type %s", query, val.Type())
	}
	if len(m) == 0 {
		return nil, nil
	}
	pts := make([]Point, 0, len(m[0].Values))
	for _, v := range m[0].Values {
		pts = append(pts, Point{T: v.Timestamp.Time(), V: float64(v.Value)})
	}
	return pts, nil
}

// seriesStep keeps charts to ~120 points, floored at one second.
func seriesStep(window time.Duration) time.Duration {
	step := window / 120
	if step < time.Second {
		step = time.Second
	}
	return step
}

// rangeVector is the [range] inside rate(). It is at least four scrape intervals
// (mirroring Grafana's $__rate_interval) so a rate reliably spans ≥2 samples
// even for short-lived pods and slight scrape jitter — a 2×scrape window can
// straddle just one sample and yield an empty series.
func rangeVector(step, scrape time.Duration) string {
	rv := step
	if rv < 4*scrape {
		rv = 4 * scrape
	}
	return fmt.Sprintf("%dms", rv.Milliseconds())
}
