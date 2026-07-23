package chart

import (
	"bytes"
	"fmt"
	"math"
	"strings"
)

const (
	mdRows  = 8  // chart height in text rows
	mdCols  = 56 // max chart width in columns (series are downsampled to fit)
	mdGutter = 8 // width of the left-hand y-axis label column
)

// mdMarkers distinguishes overlaid series (ASCII so it renders everywhere).
var mdMarkers = []rune{'*', '+', 'x', 'o'}

// renderMarkdown draws a pure-text ASCII line chart inside a fenced code block,
// embedded directly in the reply (no upload). It renders as a monospaced block
// wherever markdown is shown.
func renderMarkdown(title string, series []Series) ([]byte, error) {
	var b bytes.Buffer
	fmt.Fprintf(&b, "**%s**\n\n```\n", title)

	cols := make([][]float64, len(series))
	width, haveData := 0, false
	for i, s := range series {
		v := make([]float64, len(s.Points))
		for j, p := range s.Points {
			v[j] = p.Y
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

	for r := 0; r < mdRows; r++ {
		switch r {
		case 0:
			fmt.Fprintf(&b, "%*.3g |", mdGutter, vmax)
		case mdRows - 1:
			fmt.Fprintf(&b, "%*.3g |", mdGutter, vmin)
		default:
			fmt.Fprintf(&b, "%*s |", mdGutter, "")
		}
		b.WriteString(string(grid[r]))
		b.WriteByte('\n')
	}
	fmt.Fprintf(&b, "%*s +%s\n", mdGutter, "", strings.Repeat("-", width))
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
