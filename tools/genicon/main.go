// genicon рисует иконки PWA (восход над горизонтом) в internal/web/static.
//
//	go run ./tools/genicon
package main

import (
	"image"
	"image/color"
	"image/png"
	"log"
	"math"
	"os"
	"path/filepath"
)

var (
	bg  = color.RGBA{0x3c, 0x6f, 0xd8, 0xff}
	fg  = color.RGBA{0xff, 0xff, 0xff, 0xff}
	dim = color.RGBA{0xc8, 0xd6, 0xf5, 0xff}
)

// shade возвращает цвет точки (x, y) в координатах 0..1.
func shade(x, y float64) color.RGBA {
	const horizon = 0.62
	// линия горизонта
	if math.Abs(y-horizon) < 0.022 && x > 0.18 && x < 0.82 {
		return fg
	}
	// солнце: верхняя половина круга над горизонтом
	if y < horizon-0.05 && math.Hypot(x-0.5, y-horizon) < 0.24 {
		return fg
	}
	// короткие полосы под горизонтом
	if math.Abs(y-(horizon+0.09)) < 0.015 && x > 0.32 && x < 0.68 {
		return dim
	}
	return bg
}

func render(size int) image.Image {
	img := image.NewRGBA(image.Rect(0, 0, size, size))
	const ss = 4 // суперсэмплинг для сглаживания
	for py := 0; py < size; py++ {
		for px := 0; px < size; px++ {
			var r, g, b int
			for sy := 0; sy < ss; sy++ {
				for sx := 0; sx < ss; sx++ {
					c := shade((float64(px)+(float64(sx)+.5)/ss)/float64(size), (float64(py)+(float64(sy)+.5)/ss)/float64(size))
					r, g, b = r+int(c.R), g+int(c.G), b+int(c.B)
				}
			}
			n := ss * ss
			img.Set(px, py, color.RGBA{uint8(r / n), uint8(g / n), uint8(b / n), 0xff})
		}
	}
	return img
}

func main() {
	dir := filepath.Join("internal", "web", "static")
	for _, size := range []int{180, 192, 512} {
		f, err := os.Create(filepath.Join(dir, "icon-"+itoa(size)+".png"))
		if err != nil {
			log.Fatal(err)
		}
		if err := png.Encode(f, render(size)); err != nil {
			log.Fatal(err)
		}
		f.Close()
	}
}

func itoa(n int) string {
	s := ""
	for ; n > 0; n /= 10 {
		s = string(rune('0'+n%10)) + s
	}
	return s
}
