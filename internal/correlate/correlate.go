// Package correlate maps a GitLab job to the Kubernetes runner pod that
// executed it.
//
// Two interchangeable strategies implement Resolver, selected by POD_RESOLVER:
//   - trace (default): parse the job's GitLab trace for the runner's
//     "Running on <pod> via <manager>" line (see trace.go).
//   - prometheus: kube_pod_labels{label_job_id="<id>"} join, then filter
//     cadvisor series by pod (see prom.go).
package correlate

import (
	"context"
	"time"
)

// Resolver finds the runner pod for a job; tests stub it.
type Resolver interface {
	// PodForJob returns the pod name that executed the job, or ok=false when
	// no pod could be correlated.
	PodForJob(ctx context.Context, projectID, jobID int64, start, end time.Time) (pod string, ok bool, err error)
}
