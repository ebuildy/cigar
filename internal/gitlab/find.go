package gitlab

import (
	"errors"
	"strconv"
)

// ErrJobNotFound is returned by callers that resolve a job selector against a
// pipeline and find nothing. Surfaces wrap it and phrase their own refusal.
var ErrJobNotFound = errors.New("job not found in pipeline")

// FindJob matches a job by numeric ID first (when sel parses as an int), then
// by exact name. An empty selector matches nothing.
func FindJob(jobs []Job, sel string) (Job, bool) {
	if sel == "" {
		return Job{}, false
	}
	if id, err := strconv.ParseInt(sel, 10, 64); err == nil {
		for _, j := range jobs {
			if j.ID == id {
				return j, true
			}
		}
	}
	for _, j := range jobs {
		if j.Name == sel {
			return j, true
		}
	}
	return Job{}, false
}
