package chart

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"math"

	"golang.org/x/image/font"
	"golang.org/x/image/font/basicfont"
	"golang.org/x/image/math/fixed"
)

// renderPNG rasterizes the chart with the standard library (no cgo): a white
// background, a bitmap-font title and legend, and one polyline per series.
func renderPNG(title string, series []Series) ([]byte, error) {
	img := image.NewRGBA(image.Rect(0, 0, width, height))
	draw.Draw(img, img.Bounds(), image.NewUniform(color.White), image.Point{}, draw.Src)

	drawText(img, 8, 14, title, color.RGBA{R: 0x11, G: 0x11, B: 0x11, A: 0xff})

	minX, maxX, minY, maxY := bounds(series)
	for i, s := range series {
		col := hexColor(palette[i%len(palette)])
		var px, py float64
		for j, p := range s.Points {
			x := padX + (float64(p.X.Unix())-minX)/(maxX-minX)*plotW
			y := padY + (1-(p.Y-minY)/(maxY-minY))*plotH
			if j > 0 {
				drawLine(img, px, py, x, y, col)
			}
			px, py = x, y
		}
		drawText(img, 8, 28+i*14, s.Label, col)
	}

	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return nil, fmt.Errorf("encode png: %w", err)
	}
	return buf.Bytes(), nil
}

// drawText draws a string with the bundled 7x13 bitmap font (no external font
// file, pure Go). x,y is the text baseline.
func drawText(img *image.RGBA, x, y int, s string, c color.RGBA) {
	d := &font.Drawer{
		Dst:  img,
		Src:  image.NewUniform(c),
		Face: basicfont.Face7x13,
		Dot:  fixed.P(x, y),
	}
	d.DrawString(s)
}

// drawLine plots a 2px-thick line via DDA (no anti-aliasing — adequate for a
// small resource chart, and keeps the renderer dependency-free).
func drawLine(img *image.RGBA, x0, y0, x1, y1 float64, c color.RGBA) {
	dx, dy := x1-x0, y1-y0
	steps := math.Max(math.Abs(dx), math.Abs(dy))
	if steps == 0 {
		setPixel(img, x0, y0, c)
		return
	}
	xi, yi := dx/steps, dy/steps
	x, y := x0, y0
	for i := 0.0; i <= steps; i++ {
		setPixel(img, x, y, c)
		setPixel(img, x, y+1, c) // thickness
		x += xi
		y += yi
	}
}

func setPixel(img *image.RGBA, x, y float64, c color.RGBA) {
	ix, iy := int(x+0.5), int(y+0.5)
	if image.Pt(ix, iy).In(img.Bounds()) {
		img.SetRGBA(ix, iy, c)
	}
}

// hexColor parses "#rrggbb" to an opaque RGBA.
func hexColor(s string) color.RGBA {
	var r, g, b uint8
	_, _ = fmt.Sscanf(s, "#%02x%02x%02x", &r, &g, &b)
	return color.RGBA{R: r, G: g, B: b, A: 0xff}
}
