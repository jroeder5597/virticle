package api

import (
	"bytes"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"strconv"
)

// decodeScreenshot turns what libvirt's DomainScreenshot returned into an
// image. QEMU sends PPM (P6) in most versions and PNG in newer ones.
func decodeScreenshot(b []byte) (image.Image, error) {
	switch {
	case bytes.HasPrefix(b, []byte("\x89PNG")):
		return png.Decode(bytes.NewReader(b))
	case bytes.HasPrefix(b, []byte("P6")):
		return readPPM(b)
	}
	return nil, errors.New("the hypervisor returned a screenshot in an unknown format")
}

// readPPM reads a binary PPM (P6) with 8 or 16 bit samples.
func readPPM(b []byte) (image.Image, error) {
	pos := 2
	// token returns the next whitespace separated header number, skipping comments.
	token := func() (int, error) {
		for pos < len(b) {
			c := b[pos]
			if c == '#' {
				for pos < len(b) && b[pos] != '\n' {
					pos++
				}
			} else if c == ' ' || c == '\t' || c == '\n' || c == '\r' {
				pos++
			} else {
				break
			}
		}
		start := pos
		for pos < len(b) && b[pos] >= '0' && b[pos] <= '9' {
			pos++
		}
		if start == pos {
			return 0, errors.New("bad PPM header")
		}
		return strconv.Atoi(string(b[start:pos]))
	}
	w, err := token()
	if err != nil {
		return nil, err
	}
	h, err := token()
	if err != nil {
		return nil, err
	}
	max, err := token()
	if err != nil {
		return nil, err
	}
	if w <= 0 || h <= 0 || w > 16384 || h > 16384 || max <= 0 || max > 65535 {
		return nil, fmt.Errorf("unsupported PPM size %dx%d, maxval %d", w, h, max)
	}
	pos++ // exactly one whitespace byte separates the header from the pixels
	bps := 1
	if max > 255 {
		bps = 2
	}
	if len(b)-pos < w*h*3*bps {
		return nil, errors.New("truncated PPM data")
	}
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	sample := func(i int) uint8 {
		if bps == 1 {
			return uint8(int(b[pos+i]) * 255 / max)
		}
		return uint8((int(b[pos+2*i])<<8 | int(b[pos+2*i+1])) * 255 / max)
	}
	for i := 0; i < w*h; i++ {
		o := i * 4
		img.Pix[o], img.Pix[o+1], img.Pix[o+2], img.Pix[o+3] = sample(3*i), sample(3*i+1), sample(3*i+2), 255
	}
	return img, nil
}

// boxScale shrinks src to the given width, keeping the aspect ratio, by
// averaging the source pixels that fall into each target pixel. It never
// enlarges.
func boxScale(src image.Image, width int) image.Image {
	sb := src.Bounds()
	sw, sh := sb.Dx(), sb.Dy()
	if width <= 0 || width >= sw {
		return src
	}
	height := max(1, sh*width/sw)
	dst := image.NewRGBA(image.Rect(0, 0, width, height))
	for y := 0; y < height; y++ {
		y0, y1 := y*sh/height, max(y*sh/height+1, (y+1)*sh/height)
		for x := 0; x < width; x++ {
			x0, x1 := x*sw/width, max(x*sw/width+1, (x+1)*sw/width)
			var r, g, b, n uint32
			for yy := y0; yy < y1; yy++ {
				for xx := x0; xx < x1; xx++ {
					c := color.RGBAModel.Convert(src.At(sb.Min.X+xx, sb.Min.Y+yy)).(color.RGBA)
					r, g, b, n = r+uint32(c.R), g+uint32(c.G), b+uint32(c.B), n+1
				}
			}
			dst.SetRGBA(x, y, color.RGBA{uint8(r / n), uint8(g / n), uint8(b / n), 255})
		}
	}
	return dst
}
