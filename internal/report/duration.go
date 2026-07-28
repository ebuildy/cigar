package report

import (
	"fmt"
	"math"
	"time"
)

// minBaselineSamples mirrors history's minimum: a median backed by fewer runs
// is noise, so its delta is not rendered even if the data reaches us.
const minBaselineSamples = 3

// fullBaselineSamples is the sample count at or above which the baseline is
// considered solid enough to need no footnote. It matches the default
// report.compare.history_pipelines.
const fullBaselineSamples = 6

// durationCell renders a duration, followed by a delta against baseline when the
// comparison is trustworthy (enough samples) and material (beyond warnRatio).
// A zero current duration means the job never ran — never rendered as 0s.
func durationCell(current, baseline time.Duration, samples int, warnRatio float64) string {
	if current <= 0 {
		return dash
	}
	cell := humanDuration(current)
	if suffix := deltaSuffix(current, baseline, samples, warnRatio); suffix != "" {
		cell += " " + suffix
	}
	return cell
}

// deltaSuffix is the "🔺 +2m 08s (+51%)" part, or empty when there is no
// trustworthy or material change to report.
func deltaSuffix(current, baseline time.Duration, samples int, warnRatio float64) string {
	if samples < minBaselineSamples || baseline <= 0 || current <= 0 {
		return ""
	}
	delta := current - baseline
	ratio := float64(delta) / float64(baseline)
	if math.Abs(ratio) <= warnRatio {
		return ""
	}
	marker, sign := "🔺", "+"
	if delta < 0 {
		marker, sign = "🔻", "−" // U+2212 minus, not a hyphen
		delta = -delta
	}
	return fmt.Sprintf("%s %s%s (%s%.0f%%)",
		marker, sign, humanDuration(delta), sign, math.Abs(ratio)*100)
}
