package correlate

import (
	"context"
	"fmt"
	"time"
)

// NewPromResolver returns the Prometheus-backed Resolver: kube_pod_labels
// join on label_job_id, with the runner pod-name-pattern fallback.
func NewPromResolver(promURL string) Resolver {
	return &promResolver{url: promURL}
}

type promResolver struct {
	url string
}

func (r *promResolver) PodForJob(_ context.Context, _, jobID int64, _, _ time.Time) (string, bool, error) {
	// TODO: kube_pod_labels{label_job_id="<id>"} join, then pod-name-pattern
	// fallback within the job window. Queries must be verified against a real
	// Prometheus snapshot in testdata/ before landing.
	return "", false, fmt.Errorf("pod correlation via %s not implemented yet (job %d)", r.url, jobID)
}
