package api

import (
	"image"
	"image/color"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/makiuchi-d/gozxing"
	"github.com/makiuchi-d/gozxing/qrcode"
)

// The QR code a user is shown must decode to the otpauth address: draw the path the page draws and read it back.
func TestQRCodeDecodes(t *testing.T) {
	e, _ := signInEnv(t)
	e.user("quentin@example.org", operator)
	w := e.do("POST", "/api/account/2fa/begin", nil, e.as(e.cookies["quentin@example.org"]))
	var b struct {
		Secret, URI string
		QR          struct {
			Size int
			Path string
		}
	}
	decode(t, w, &b)
	const scale = 8
	img := image.NewGray(image.Rect(0, 0, b.QR.Size*scale, b.QR.Size*scale))
	for i := range img.Pix {
		img.Pix[i] = 255
	}
	for _, m := range regexp.MustCompile(`M(\d+) (\d+)h(\d+)v1h-\d+z`).FindAllStringSubmatch(b.QR.Path, -1) {
		x, _ := strconv.Atoi(m[1])
		y, _ := strconv.Atoi(m[2])
		n, _ := strconv.Atoi(m[3])
		for py := y * scale; py < (y+1)*scale; py++ {
			for px := x * scale; px < (x+n)*scale; px++ {
				img.SetGray(px, py, color.Gray{0})
			}
		}
	}
	bmp, err := gozxing.NewBinaryBitmapFromImage(img)
	if err != nil {
		t.Fatal(err)
	}
	res, err := qrcode.NewQRCodeReader().Decode(bmp, nil)
	if err != nil {
		t.Fatalf("the code cannot be read: %v", err)
	}
	if res.GetText() != b.URI {
		t.Errorf("decoded %q, want %q", res.GetText(), b.URI)
	}
	if !strings.Contains(b.URI, "secret="+b.Secret) || !strings.Contains(b.URI, "Virticle:quentin@example.org") {
		t.Errorf("uri %s", b.URI)
	}
}
