package metrics

import "testing"

func TestContainerThrottledRatio(t *testing.T) {
	tests := []struct {
		name      string
		container ContainerUsage
		want      float64
		wantOK    bool
	}{
		{name: "measured", container: ContainerUsage{ThrottledPeriods: 25, Periods: 100}, want: 0.25, wantOK: true},
		{name: "no throttling measured", container: ContainerUsage{ThrottledPeriods: 0, Periods: 100}, want: 0, wantOK: true},
		// Absent ≠ 0%: with no CFS period series there is nothing to divide by.
		{name: "absent series", container: ContainerUsage{}, wantOK: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := tt.container.ThrottledRatio()
			if ok != tt.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tt.wantOK)
			}
			if ok && got != tt.want {
				t.Errorf("ratio = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestSortedContainers(t *testing.T) {
	acc := map[string]*ContainerUsage{
		"svc-10": {Name: "svc-10"},
		"helper": {Name: "helper"},
		"svc-2":  {Name: "svc-2"},
		"build":  {Name: "build"},
		"zebra":  {Name: "zebra"},
	}
	got := sortedContainers(acc)
	want := []string{"build", "helper", "svc-2", "svc-10", "zebra"}
	if len(got) != len(want) {
		t.Fatalf("got %d containers, want %d", len(got), len(want))
	}
	for i, name := range want {
		if got[i].Name != name {
			t.Errorf("position %d = %q, want %q", i, got[i].Name, name)
		}
	}
}

func TestSumContainersDerivesTotals(t *testing.T) {
	u := &JobUsage{Containers: []ContainerUsage{
		{Name: "build", CPUSeconds: 39.8, PeakMemoryBytes: 400, DiskReadBytes: 500, DiskWriteBytes: 200,
			ThrottledPeriods: 12, Periods: 100,
			CPURequestCores: 0.15, CPULimitCores: 0.25, MemoryRequestBytes: 128, MemoryLimitBytes: 256},
		{Name: "helper", CPUSeconds: 2.2, PeakMemoryBytes: 12, DiskReadBytes: 20, DiskWriteBytes: 20,
			ThrottledPeriods: 58, Periods: 100,
			CPURequestCores: 0.1, CPULimitCores: 0.25, MemoryRequestBytes: 128, MemoryLimitBytes: 256},
	}}
	u.sumContainers()

	if u.CPUSeconds != 42.0 {
		t.Errorf("CPUSeconds = %v, want 42", u.CPUSeconds)
	}
	if u.PeakMemoryBytes != 412 {
		t.Errorf("PeakMemoryBytes = %d, want 412", u.PeakMemoryBytes)
	}
	if u.DiskReadBytes != 520 || u.DiskWriteBytes != 220 {
		t.Errorf("disk = %d/%d, want 520/220", u.DiskReadBytes, u.DiskWriteBytes)
	}
	if u.CPURequestCores != 0.25 || u.CPULimitCores != 0.5 {
		t.Errorf("cpu req/limit = %v/%v, want 0.25/0.5", u.CPURequestCores, u.CPULimitCores)
	}
	if u.MemoryRequestBytes != 256 || u.MemoryLimitBytes != 512 {
		t.Errorf("mem req/limit = %d/%d, want 256/512", u.MemoryRequestBytes, u.MemoryLimitBytes)
	}
	// sum(throttled)/sum(periods) — not the mean of the two ratios.
	if u.ThrottledRatio != 0.35 {
		t.Errorf("ThrottledRatio = %v, want 0.35", u.ThrottledRatio)
	}
}

func TestSumContainersLeavesRatioUnsetWithoutPeriods(t *testing.T) {
	u := &JobUsage{Containers: []ContainerUsage{{Name: "build", CPUSeconds: 1}}}
	u.sumContainers()
	if u.ThrottledRatio != 0 {
		t.Errorf("ThrottledRatio = %v, want 0 (absent, not measured)", u.ThrottledRatio)
	}
}
