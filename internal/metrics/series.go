package metrics

import (
	"context"
	"time"
)

// Point is one sample of a time series.
type Point struct {
	T time.Time
	V float64
}

// Line is a labeled time series (one chart line).
type Line struct {
	Label  string
	Points []Point
}

// PodSeries holds the time series backing the three `details` charts. Only the
// transmit side of the network is charted — a CI runner's receive traffic
// (image/dependency pulls) is rarely the interesting signal.
type PodSeries struct {
	CPU    Line // cores
	Memory Line // bytes (working set)
	NetTx  Line // bytes/s (transmit)
}

// Empty reports whether no series produced any sample (absent ≠ zero).
func (s PodSeries) Empty() bool {
	return len(s.CPU.Points) == 0 && len(s.Memory.Points) == 0 &&
		len(s.NetTx.Points) == 0
}

// SeriesSource is the range-query capability consumed by the command handler;
// tests stub it. *PromSource implements it.
type SeriesSource interface {
	PodSeries(ctx context.Context, pod string, start, end time.Time) (PodSeries, error)
	PodActiveSpan(ctx context.Context, pod string) (start, end time.Time, ok bool, err error)
}
