package main

import (
	"testing"

	"gitlab.com/ebuildy/gitlab-ci-resources-bot/internal/gitlab"
)

func TestFindJob(t *testing.T) {
	jobs := []gitlab.Job{
		{ID: 101, Name: "build"},
		{ID: 102, Name: "test"},
		{ID: 103, Name: "42"}, // a job literally named "42"
	}
	tests := []struct {
		name   string
		sel    string
		wantID int64
		wantOK bool
	}{
		{name: "by numeric ID", sel: "102", wantID: 102, wantOK: true},
		{name: "by name", sel: "build", wantID: 101, wantOK: true},
		{name: "numeric ID wins over same-looking name", sel: "103", wantID: 103, wantOK: true},
		{name: "falls back to name when ID absent", sel: "42", wantID: 103, wantOK: true},
		{name: "not found", sel: "deploy", wantOK: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			j, ok := findJob(jobs, tt.sel)
			if ok != tt.wantOK {
				t.Fatalf("findJob(%q) ok = %v, want %v", tt.sel, ok, tt.wantOK)
			}
			if ok && j.ID != tt.wantID {
				t.Fatalf("findJob(%q) = job %d, want %d", tt.sel, j.ID, tt.wantID)
			}
		})
	}
}
