package correlate

import (
	"context"
	"fmt"
	"time"

	"github.com/prometheus/client_golang/api"
	promv1 "github.com/prometheus/client_golang/api/prometheus/v1"
	"github.com/prometheus/common/model"
	"go.uber.org/zap"
)

// NewPromResolver returns the Prometheus-backed Resolver: kube_pod_labels
// join on label_job_id, over the job window padded by one scrapeInterval.
func NewPromResolver(promURL string, scrapeInterval time.Duration, log *zap.Logger) (Resolver, error) {
	c, err := api.NewClient(api.Config{Address: promURL})
	if err != nil {
		return nil, fmt.Errorf("create prometheus client: %w", err)
	}
	log.Debug("prometheus pod resolver created", zap.String("url", promURL))
	return &promResolver{api: promv1.NewAPI(c), scrape: scrapeInterval, log: log}, nil
}

type promResolver struct {
	api    promv1.API
	scrape time.Duration
	log    *zap.Logger
}

func (r *promResolver) PodForJob(ctx context.Context, _, jobID int64, start, end time.Time) (string, bool, error) {
	window := fmt.Sprintf("%dms", (end.Sub(start) + r.scrape).Milliseconds())
	query := fmt.Sprintf(`max_over_time(kube_pod_labels{label_job_id="%d"}[%s])`, jobID, window)
	r.log.Debug("resolving pod for job", zap.Int64("job_id", jobID), zap.String("window", window))

	val, _, err := r.api.Query(ctx, query, end)
	if err != nil {
		return "", false, fmt.Errorf("prometheus query %q: %w", query, err)
	}
	vec, ok := val.(model.Vector)
	if !ok {
		return "", false, fmt.Errorf("prometheus query %q: unexpected result type %s", query, val.Type())
	}
	// TODO fallback: pod name pattern runner-<token>-project-<id>-concurrent-<n>
	// within the job window, for runners that don't inject the job_id label.
	if len(vec) == 0 {
		r.log.Debug("no pod label series for job", zap.Int64("job_id", jobID))
		return "", false, nil
	}
	pod := string(vec[0].Metric["pod"])
	if pod == "" {
		r.log.Warn("pod label present but empty", zap.Int64("job_id", jobID))
		return "", false, nil
	}
	r.log.Debug("resolved pod for job", zap.Int64("job_id", jobID), zap.String("pod", pod))
	return pod, true, nil
}
