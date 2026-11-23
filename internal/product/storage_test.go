package product

import (
	"bytes"
	"context"
	"image"
	"image/jpeg"
	"image/png"
	"testing"
)

func TestImageSanitization(t *testing.T) {
	im := image.NewNRGBA(image.Rect(0, 0, 8, 8))
	var pngData, jpegData bytes.Buffer
	_ = png.Encode(&pngData, im)
	_ = jpeg.Encode(&jpegData, im, nil)
	for _, tc := range []struct {
		name, mime string
		data       []byte
	}{{"../../image.png", "image/png", pngData.Bytes()}, {"image.jpg", "image/jpeg", jpegData.Bytes()}} {
		t.Run(tc.mime, func(t *testing.T) {
			raw := append(append([]byte{}, tc.data...), []byte("secret trailing metadata")...)
			p, err := prepareImage(context.Background(), Upload{Filename: tc.name, MIME: tc.mime, Data: raw})
			if err != nil {
				t.Fatal(err)
			}
			if bytes.Contains(p.Data, []byte("secret trailing metadata")) || p.Filename == tc.name && tc.name[0] == '.' {
				t.Fatal("unsafe metadata retained")
			}
			if len(p.Hash) != 32 || !keyPattern.MatchString(p.Key) {
				t.Fatal("unsafe generated identity")
			}
		})
	}
	for _, bad := range []Upload{{Filename: "x.png", MIME: "image/png", Data: []byte("fake")}, {Filename: "x.jpg", MIME: "image/jpeg", Data: pngData.Bytes()}, {Filename: "x.png", MIME: "image/jpeg", Data: pngData.Bytes()}, {Filename: "x.svg", MIME: "image/svg+xml", Data: []byte("<svg/>")}, {Filename: "x.gif", MIME: "image/gif", Data: []byte("GIF89a")}, {Filename: "x.webp", MIME: "image/webp", Data: []byte("RIFF0000WEBP")}, {Filename: "x.png", MIME: "image/png", Data: make([]byte, MaxImageBytes+1)}} {
		if _, err := prepareImage(context.Background(), bad); err == nil {
			t.Fatal("invalid image accepted")
		}
	}
	oversized := image.NewGray(image.Rect(0, 0, 8193, 1))
	var tooWide bytes.Buffer
	_ = png.Encode(&tooWide, oversized)
	if _, err := prepareImage(context.Background(), Upload{Filename: "huge.png", MIME: "image/png", Data: tooWide.Bytes()}); err == nil {
		t.Fatal("oversized dimensions accepted")
	}
}
