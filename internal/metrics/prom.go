package metrics

import (
	"context"
	"fmt"
	"time"
)

// NewPromSource returns the Prometheus-backed Source (cadvisor +
// kube-state-metrics queries).
func NewPromSource(promURL string) Source {
	return &promSource{url: promURL}
}

type promSource struct {
	url string
}

func (s *promSource) PodUsage(_ context.Context, pod string, _, _ time.Time) (*JobUsage, error) {
	// TODO: PromQL queries from CLAUDE.md (working set peak, CPU increase,
	// throttling ratio, network, requests/limits), padded by one scrape
	// interval. Queries must be verified against a real Prometheus snapshot
	// in testdata/ before landing.
	return nil, fmt.Errorf("prometheus queries via %s not implemented yet (pod %s)", s.url, pod)
}
