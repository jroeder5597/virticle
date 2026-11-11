package api

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"testing"
)

func TestReadPPM(t *testing.T) {
	p := append([]byte("P6\n# a comment\n2 1\n255\n"), 255, 0, 0, 0, 0, 255)
	img, err := decodeScreenshot(p)
	if err != nil {
		t.Fatal(err)
	}
	if img.Bounds().Dx() != 2 || img.Bounds().Dy() != 1 {
		t.Fatalf("size %v", img.Bounds())
	}
	if c := color.RGBAModel.Convert(img.At(0, 0)).(color.RGBA); c != (color.RGBA{255, 0, 0, 255}) {
		t.Errorf("pixel 0: %v", c)
	}
	if c := color.RGBAModel.Convert(img.At(1, 0)).(color.RGBA); c != (color.RGBA{0, 0, 255, 255}) {
		t.Errorf("pixel 1: %v", c)
	}
}

func TestReadPPM16Bit(t *testing.T) {
	p := append([]byte("P6 1 1 65535 "), 0xff, 0xff, 0x80, 0x00, 0, 0)
	img, err := readPPM(p)
	if err != nil {
		t.Fatal(err)
	}
	if c := color.RGBAModel.Convert(img.At(0, 0)).(color.RGBA); c.R != 255 || c.G != 127 || c.B != 0 {
		t.Errorf("pixel: %v", c)
	}
}

func TestReadPPMBad(t *testing.T) {
	for name, in := range map[string][]byte{
		"truncated": []byte("P6\n2 2\n255\n\x00\x00\x00"),
		"no size":   []byte("P6\n"),
		"huge":      []byte("P6\n99999 99999\n255\n"),
		"unknown":   []byte("GIF89a"),
	} {
		if _, err := decodeScreenshot(in); err == nil {
			t.Errorf("%s: want error", name)
		}
	}
}

func TestDecodePNG(t *testing.T) {
	src := image.NewRGBA(image.Rect(0, 0, 3, 2))
	var buf bytes.Buffer
	png.Encode(&buf, src)
	img, err := decodeScreenshot(buf.Bytes())
	if err != nil || img.Bounds().Dx() != 3 {
		t.Fatalf("%v %v", img, err)
	}
}

func TestBoxScale(t *testing.T) {
	src := image.NewRGBA(image.Rect(0, 0, 4, 4))
	for y := 0; y < 4; y++ {
		for x := 0; x < 4; x++ {
			v := uint8(0)
			if x < 2 {
				v = 200
			}
			if x == 0 || x == 2 {
				v = 100
				if x == 0 {
					v = 100
				}
			}
			src.SetRGBA(x, y, color.RGBA{v, v, v, 255})
		}
	}
	// columns are 100,200,100,0 -> halves average to 150 and 50
	out := boxScale(src, 2)
	if out.Bounds().Dx() != 2 || out.Bounds().Dy() != 2 {
		t.Fatalf("size %v", out.Bounds())
	}
	if c := color.RGBAModel.Convert(out.At(0, 0)).(color.RGBA); c.R != 150 {
		t.Errorf("left: %v", c)
	}
	if c := color.RGBAModel.Convert(out.At(1, 1)).(color.RGBA); c.R != 50 {
		t.Errorf("right: %v", c)
	}
	if boxScale(src, 10) != image.Image(src) || boxScale(src, 0) != image.Image(src) {
		t.Error("must not enlarge")
	}
	if o := boxScale(src, 1); o.Bounds().Dx() != 1 || o.Bounds().Dy() != 1 {
		t.Errorf("1px: %v", o.Bounds())
	}
}
