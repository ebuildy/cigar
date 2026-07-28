package history

import (
	"testing"
	"time"
)

func TestMedian(t *testing.T) {
	tests := []struct {
		name string
		in   []time.Duration
		want time.Duration
		ok   bool
	}{
		{"odd count picks the middle", []time.Duration{
			3 * time.Minute, time.Minute, 2 * time.Minute,
		}, 2 * time.Minute, true},
		{"even count averages the two middles", []time.Duration{
			4 * time.Minute, time.Minute, 3 * time.Minute, 2 * time.Minute,
		}, 150 * time.Second, true},
		{"below minSamples yields nothing", []time.Duration{
			time.Minute, 2 * time.Minute,
		}, 0, false},
		{"empty yields nothing", nil, 0, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := newStat(tt.in)
			if ok != tt.ok {
				t.Fatalf("ok = %v, want %v", ok, tt.ok)
			}
			if !ok {
				return
			}
			if got.Median != tt.want {
				t.Errorf("median = %s, want %s", got.Median, tt.want)
			}
			if got.Samples != len(tt.in) {
				t.Errorf("samples = %d, want %d", got.Samples, len(tt.in))
			}
		})
	}
}

func TestNewStatDoesNotMutateInput(t *testing.T) {
	in := []time.Duration{3 * time.Minute, time.Minute, 2 * time.Minute}
	newStat(in)
	if in[0] != 3*time.Minute {
		t.Errorf("input was sorted in place: %v", in)
	}
}
