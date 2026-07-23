package chart

import (
	"bytes"
	"image/png"
	"os"
	"strings"
	"testing"
	"time"
)

func sampleSeries() []Series {
	base := time.Unix(1752912000, 0)
	return []Series{{
		Label: "cpu",
		Points: []Point{
			{X: base, Y: 0.1},
			{X: base.Add(30 * time.Second), Y: 0.2},
			{X: base.Add(60 * time.Second), Y: 0.3},
		},
	}}
}

func TestRenderIsSanitizerSafe(t *testing.T) {
	svg, err := Render(SVG, "CPU (cores)", sampleSeries())
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	s := string(svg)
	if !strings.HasPrefix(s, "<svg") {
		t.Fatalf("output is not an <svg>: %.40q", s)
	}
	// Guard the real external-reference / script vectors. A namespace URI in
	// xmlns is not a fetch, so it is allowed (and required for standalone SVG).
	for _, bad := range []string{"<script", "javascript:", "onload=", "onerror=", "xlink:href", "<image", "<foreignObject", "url("} {
		if strings.Contains(s, bad) {
			t.Errorf("SVG contains disallowed token %q", bad)
		}
	}
	if !strings.Contains(s, `xmlns="http://www.w3.org/2000/svg"`) {
		t.Error("SVG missing xmlns; standalone SVG will not render in GitLab")
	}
	if !strings.Contains(s, "<polyline") {
		t.Error("SVG has no polyline")
	}
}

func TestRenderEmptySeries(t *testing.T) {
	svg, err := Render(SVG, "Empty", []Series{{Label: "x"}})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if !strings.HasPrefix(string(svg), "<svg") {
		t.Fatal("empty series did not produce an <svg>")
	}
}

func TestRenderMatchesGolden(t *testing.T) {
	svg, err := Render(SVG, "CPU (cores)", sampleSeries())
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	want, err := os.ReadFile("testdata/cpu.svg")
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}
	if string(svg) != string(want) {
		t.Errorf("SVG != golden.\n--- got ---\n%s", svg)
	}
}

func TestParseFormat(t *testing.T) {
	for in, want := range map[string]Format{
		"": PNG, "png": PNG, "PNG": PNG, " svg ": SVG, "SVG": SVG,
		"markdown": Markdown, "MD": Markdown,
	} {
		got, err := ParseFormat(in)
		if err != nil || got != want {
			t.Fatalf("ParseFormat(%q) = (%v, %v), want %v", in, got, err, want)
		}
	}
	if _, err := ParseFormat("gif"); err == nil {
		t.Fatal("ParseFormat(gif): want error")
	}
	if PNG.Ext() != "png" || SVG.Ext() != "svg" || Markdown.Ext() != "md" {
		t.Fatalf("Ext: png=%q svg=%q md=%q", PNG.Ext(), SVG.Ext(), Markdown.Ext())
	}
	if PNG.Inline() || SVG.Inline() || !Markdown.Inline() {
		t.Fatal("Inline: only Markdown should be inline")
	}
}

func TestRenderPNGDecodes(t *testing.T) {
	data, err := Render(PNG, "CPU (cores)", sampleSeries())
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	im, err := png.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("decode png: %v", err)
	}
	if im.Bounds().Dx() != width || im.Bounds().Dy() != height {
		t.Fatalf("png size = %v, want %dx%d", im.Bounds(), width, height)
	}
}

func TestRenderPNGEmptySeries(t *testing.T) {
	data, err := Render(PNG, "Empty", []Series{{Label: "x"}})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if _, err := png.Decode(bytes.NewReader(data)); err != nil {
		t.Fatalf("empty-series png invalid: %v", err)
	}
}

func TestRenderMarkdown(t *testing.T) {
	md, err := Render(Markdown, "CPU (cores)", sampleSeries())
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	s := string(md)
	if !strings.Contains(s, "**CPU (cores)**") {
		t.Errorf("markdown missing bold title:\n%s", s)
	}
	if strings.Count(s, "```") != 2 {
		t.Errorf("markdown missing a fenced code block:\n%s", s)
	}
	if !strings.ContainsRune(s, '*') { // the plotted line marker
		t.Errorf("markdown chart has no plotted points:\n%s", s)
	}
}

func TestRenderMarkdownMultiSeriesLegend(t *testing.T) {
	base := time.Unix(1752912000, 0)
	two := []Series{
		{Label: "rx", Points: []Point{{X: base, Y: 1}, {X: base.Add(time.Minute), Y: 3}}},
		{Label: "tx", Points: []Point{{X: base, Y: 2}, {X: base.Add(time.Minute), Y: 1}}},
	}
	md, err := Render(Markdown, "Network", two)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	s := string(md)
	if !strings.Contains(s, "rx") || !strings.Contains(s, "tx") {
		t.Errorf("multi-series markdown missing a legend:\n%s", s)
	}
}

func TestRenderMarkdownEmpty(t *testing.T) {
	md, err := Render(Markdown, "Empty", []Series{{Label: "x"}})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if !strings.Contains(string(md), "no data") {
		t.Errorf("empty markdown chart should note no data:\n%s", md)
	}
}
