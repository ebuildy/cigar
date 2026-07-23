package chart

import (
	"bytes"
	"fmt"
	"math"
	"strings"
	"time"
)

const (
	mdRows = 8  // chart height in text rows
	mdCols = 67 // max chart width in columns (series are downsampled to fit)
)

// mdMarkers distinguishes overlaid series (ASCII so it renders everywhere).
var mdMarkers = []rune{'*', '+', 'x', 'o'}

// renderMarkdown draws a pure-text ASCII line chart inside a fenced code block,
// embedded directly in the reply (no upload). It renders as a monospaced block
// wherever markdown is shown.
func renderMarkdown(title string, unit Unit, series []Series) ([]byte, error) {
	var b bytes.Buffer
	fmt.Fprintf(&b, "**%s**\n\n```\n", title)

	cols := make([][]float64, len(series))
	width, haveData := 0, false
	var minT, maxT time.Time
	for i, s := range series {
		v := make([]float64, len(s.Points))
		for j, p := range s.Points {
			v[j] = p.Y
			if !haveData && j == 0 || p.X.Before(minT) {
				minT = p.X
			}
			if p.X.After(maxT) {
				maxT = p.X
			}
		}
		cols[i] = downsample(v, mdCols)
		if len(cols[i]) > 0 {
			haveData = true
		}
		if len(cols[i]) > width {
			width = len(cols[i])
		}
	}
	if !haveData {
		b.WriteString("(no data)\n```\n")
		return b.Bytes(), nil
	}

	vmin, vmax := math.Inf(1), math.Inf(-1)
	for _, c := range cols {
		for _, v := range c {
			vmin, vmax = math.Min(vmin, v), math.Max(vmax, v)
		}
	}
	if vmin == vmax { // flat line: give the axis some height
		vmax = vmin + 1
	}

	grid := make([][]rune, mdRows)
	for r := range grid {
		grid[r] = make([]rune, width)
		for c := range grid[r] {
			grid[r][c] = ' '
		}
	}
	rowOf := func(v float64) int {
		r := int(math.Round((vmax - v) / (vmax - vmin) * float64(mdRows-1)))
		return min(max(r, 0), mdRows-1)
	}
	for i, c := range cols {
		m := mdMarkers[i%len(mdMarkers)]
		prev := -1
		for x, v := range c {
			r := rowOf(v)
			grid[r][x] = m
			if prev >= 0 { // connect adjacent points with a vertical run
				for rr := min(r, prev) + 1; rr < max(r, prev); rr++ {
					if grid[rr][x] == ' ' {
						grid[rr][x] = '|'
					}
				}
			}
			prev = r
		}
	}

	// Human-readable y-axis labels (top=max, bottom=min); gutter sized to fit.
	topLabel, botLabel := formatValue(vmax, unit), formatValue(vmin, unit)
	gutter := max(len(topLabel), len(botLabel))
	for r := 0; r < mdRows; r++ {
		label := ""
		switch r {
		case 0:
			label = topLabel
		case mdRows - 1:
			label = botLabel
		}
		fmt.Fprintf(&b, "%*s |", gutter, label)
		b.WriteString(string(grid[r]))
		b.WriteByte('\n')
	}
	fmt.Fprintf(&b, "%*s +%s\n", gutter, "", strings.Repeat("-", width))

	// X-axis labels: start time on the left, window duration on the right,
	// aligned under the plot area (which begins gutter+2 chars in).
	start := minT.Format("15:04 02/01")
	dur := humanDuration(maxT.Sub(minT))
	gap := width - len(start) - len(dur)
	if gap < 1 {
		gap = 1
	}
	fmt.Fprintf(&b, "%*s%s%s%s\n", gutter+2, "", start, strings.Repeat(" ", gap), dur)
	b.WriteString("```\n")

	if len(series) > 1 { // legend so overlaid markers are readable
		parts := make([]string, len(series))
		for i, s := range series {
			parts[i] = fmt.Sprintf("`%c` %s", mdMarkers[i%len(mdMarkers)], s.Label)
		}
		b.WriteString(strings.Join(parts, " · ") + "\n")
	}
	return b.Bytes(), nil
}

// formatValue renders a y-axis label human-readably for the given unit,
// avoiding scientific notation.
func formatValue(v float64, unit Unit) string {
	switch unit {
	case UnitBytes:
		return humanBytesF(v)
	case UnitBytesPerSec:
		return humanBytesF(v) + "/s"
	case UnitCores:
		return formatCores(v)
	default:
		return fmt.Sprintf("%.3g", v)
	}
}

// humanDuration formats a duration compactly and human-readably, rounded to the
// second (e.g. "45s", "2m5s", "1h5m").
func humanDuration(d time.Duration) string {
	d = d.Round(time.Second)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		m, s := int(d/time.Minute), int((d%time.Minute)/time.Second)
		if s == 0 {
			return fmt.Sprintf("%dm", m)
		}
		return fmt.Sprintf("%dm%ds", m, s)
	default:
		h, m := int(d/time.Hour), int((d%time.Hour)/time.Minute)
		if m == 0 {
			return fmt.Sprintf("%dh", h)
		}
		return fmt.Sprintf("%dh%dm", h, m)
	}
}

// formatCores renders a CPU core count as millicores (e.g. 229m) below one core,
// and as plain cores at or above one — matching the report's convention.
func formatCores(v float64) string {
	if math.Abs(v) < 1 {
		return fmt.Sprintf("%dm", int(math.Round(v*1000)))
	}
	return fmt.Sprintf("%.3g", v)
}

// humanBytesF formats a byte count with IEC units (KiB, MiB, …), one decimal.
func humanBytesF(v float64) string {
	const unit = 1024.0
	if math.Abs(v) < unit {
		return fmt.Sprintf("%.0f B", v)
	}
	div, exp := unit, 0
	for n := math.Abs(v) / unit; n >= unit && exp < 4; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", v/div, "KMGTP"[exp])
}

// downsample reduces v to at most n points by averaging contiguous buckets, so
// long series still fit the fixed chart width.
func downsample(v []float64, n int) []float64 {
	if len(v) <= n {
		return v
	}
	out := make([]float64, n)
	for i := 0; i < n; i++ {
		lo := i * len(v) / n
		hi := (i + 1) * len(v) / n
		if hi <= lo {
			hi = lo + 1
		}
		sum, count := 0.0, 0
		for j := lo; j < hi && j < len(v); j++ {
			sum += v[j]
			count++
		}
		out[i] = sum / float64(count)
	}
	return out
}
