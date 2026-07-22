package chart

import (
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
	svg, err := Render("CPU (cores)", sampleSeries())
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	s := string(svg)
	if !strings.HasPrefix(s, "<svg") {
		t.Fatalf("output is not an <svg>: %.40q", s)
	}
	for _, bad := range []string{"<script", "javascript:", "onload=", "http://", "https://"} {
		if strings.Contains(s, bad) {
			t.Errorf("SVG contains disallowed token %q", bad)
		}
	}
	if !strings.Contains(s, "<polyline") {
		t.Error("SVG has no polyline")
	}
}

func TestRenderEmptySeries(t *testing.T) {
	svg, err := Render("Empty", []Series{{Label: "x"}})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if !strings.HasPrefix(string(svg), "<svg") {
		t.Fatal("empty series did not produce an <svg>")
	}
}

func TestRenderMatchesGolden(t *testing.T) {
	svg, err := Render("CPU (cores)", sampleSeries())
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
