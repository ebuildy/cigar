// Package chart renders a labeled time series as a line chart, either as a PNG
// raster (default — GitLab renders uploaded PNGs inline reliably) or as a
// self-contained, sanitizer-safe SVG (no scripts, no external references), both
// pure Go so the distroless image stays cgo-free.
package chart

import (
	"bytes"
	"fmt"
	"strings"
	"text/template"
	"time"
)

// Point is one sample.
type Point struct {
	X time.Time
	Y float64
}

// Series is a labeled line.
type Series struct {
	Label  string
	Points []Point
}

// Format is the chart image encoding.
type Format int

const (
	// PNG is the default: GitLab renders uploaded PNGs inline reliably.
	PNG Format = iota
	// SVG is vector/smaller but GitLab's rendering of uploaded SVG is unreliable.
	SVG
	// Markdown is a pure-text ASCII line chart embedded directly in the reply
	// (no upload) — renders as a monospaced code block anywhere markdown does.
	Markdown
)

// ParseFormat maps "png"/"svg"/"markdown" (case-insensitive; "md" aliases
// markdown; empty defaults to png) to a Format, erroring on anything else.
func ParseFormat(s string) (Format, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "png":
		return PNG, nil
	case "svg":
		return SVG, nil
	case "markdown", "md":
		return Markdown, nil
	default:
		return PNG, fmt.Errorf("unknown chart format %q (want png, svg or markdown)", s)
	}
}

// Ext is the upload file extension for the format (no leading dot). Unused for
// Markdown, which is inlined rather than uploaded.
func (f Format) Ext() string {
	switch f {
	case SVG:
		return "svg"
	case Markdown:
		return "md"
	default:
		return "png"
	}
}

// Inline reports whether the rendered output is embedded directly in the reply
// body (Markdown) rather than uploaded as an image and referenced.
func (f Format) Inline() bool { return f == Markdown }

// Render draws one chart with the given title and series in the given format.
func Render(format Format, title string, series []Series) ([]byte, error) {
	switch format {
	case SVG:
		return renderSVG(title, series)
	case Markdown:
		return renderMarkdown(title, series)
	default:
		return renderPNG(title, series)
	}
}

const (
	width  = 600
	height = 200
	padX   = 40
	padY   = 20
	plotW  = width - 2*padX
	plotH  = height - 2*padY
)

// palette are fixed, colorblind-safe line colors (no external refs).
var palette = []string{"#1f77b4", "#d62728", "#2ca02c", "#9467bd"}

type lineVM struct {
	Color  string
	Points string // "x,y x,y ..."
	Label  string
	LabelY int
}

type chartVM struct {
	Width, Height int
	Title         string
	Lines         []lineVM
}

var tmpl = template.Must(template.New("svg").Parse(
	`<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 {{.Width}} {{.Height}}" width="{{.Width}}" height="{{.Height}}" font-family="sans-serif" font-size="11">` +
		`<rect x="0" y="0" width="{{.Width}}" height="{{.Height}}" fill="#ffffff"/>` +
		`<text x="8" y="14" fill="#111111">{{.Title}}</text>` +
		`{{range .Lines}}<polyline fill="none" stroke="{{.Color}}" stroke-width="1.5" points="{{.Points}}"/>` +
		`<text x="8" y="{{.LabelY}}" fill="{{.Color}}">{{.Label}}</text>{{end}}` +
		`</svg>`))

// renderSVG draws one chart as a sanitizer-safe SVG.
func renderSVG(title string, series []Series) ([]byte, error) {
	minX, maxX, minY, maxY := bounds(series)
	vm := chartVM{Width: width, Height: height, Title: title}
	for i, s := range series {
		vm.Lines = append(vm.Lines, lineVM{
			Color:  palette[i%len(palette)],
			Points: project(s.Points, minX, maxX, minY, maxY),
			Label:  s.Label,
			LabelY: 28 + i*14, // legend labels stacked under the title
		})
	}
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, vm); err != nil {
		return nil, fmt.Errorf("render svg: %w", err)
	}
	return buf.Bytes(), nil
}

func bounds(series []Series) (minX, maxX, minY, maxY float64) {
	first := true
	for _, s := range series {
		for _, p := range s.Points {
			x := float64(p.X.Unix())
			if first {
				minX, maxX, minY, maxY = x, x, p.Y, p.Y
				first = false
				continue
			}
			minX, maxX = minf(minX, x), maxf(maxX, x)
			minY, maxY = minf(minY, p.Y), maxf(maxY, p.Y)
		}
	}
	if minY == maxY { // flat or single value: give the axis height
		maxY = minY + 1
	}
	if minX == maxX {
		maxX = minX + 1
	}
	return
}

func project(pts []Point, minX, maxX, minY, maxY float64) string {
	var b bytes.Buffer
	for i, p := range pts {
		x := padX + (float64(p.X.Unix())-minX)/(maxX-minX)*plotW
		y := padY + (1-(p.Y-minY)/(maxY-minY))*plotH
		if i > 0 {
			b.WriteByte(' ')
		}
		fmt.Fprintf(&b, "%.1f,%.1f", x, y)
	}
	return b.String()
}

func minf(a, b float64) float64 {
	if a < b {
		return a
	}
	return b
}
func maxf(a, b float64) float64 {
	if a > b {
		return a
	}
	return b
}
