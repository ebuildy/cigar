package advice

import (
	"fmt"
	"strings"
)

// CleanMessage is what a pipeline with nothing to fix gets back.
const CleanMessage = "You are all good dude!"

// Render turns a pipeline's advice into the markdown body the note reply posts
// and the CLI prints. With no advice it returns exactly CleanMessage.
//
// Pipeline-wide advice (Advice.Job == "") comes first; per-job advice follows,
// grouped under one heading per job, in the order the jobs first appear.
func Render(pipelineID int64, all []Advice) string {
	if len(all) == 0 {
		return CleanMessage
	}
	var b strings.Builder
	fmt.Fprintf(&b, "### Advice for pipeline #%d\n\n", pipelineID)

	for _, a := range all {
		if a.Job == "" {
			writeAdvice(&b, a)
		}
	}
	for _, job := range jobOrder(all) {
		fmt.Fprintf(&b, "#### `%s`\n\n", job)
		for _, a := range all {
			if a.Job == job {
				writeAdvice(&b, a)
			}
		}
	}
	return b.String()
}

func writeAdvice(b *strings.Builder, a Advice) {
	fmt.Fprintf(b, "**%s**\n\n%s\n\n", a.Title, strings.TrimRight(a.Body, "\n"))
}

// jobOrder lists the distinct job names in first-appearance order.
func jobOrder(all []Advice) []string {
	seen := make(map[string]bool, len(all))
	var order []string
	for _, a := range all {
		if a.Job == "" || seen[a.Job] {
			continue
		}
		seen[a.Job] = true
		order = append(order, a.Job)
	}
	return order
}
