// Package mockmedia holds the fake media served by DY_MOCK=1: a real, tiny
// sample MP4 (so the built-in player can actually demux and play it) plus a
// generated cover image. It lives in its own package because both the API
// server (mock CDN routes) and the provider (variant size metadata) need it,
// and api already imports provider.
//
// Sample video: (c) Blender Foundation, CC-BY 3.0, via test-videos.co.uk
// (Big Buck Bunny, 360p/10s, ~1 MiB, web-optimized so truncated prefixes
// stay playable - the three quality tiers are byte-prefixes of it).
package mockmedia

import (
	"bytes"
	"embed"
	"image"
	"image/color"
	"image/jpeg"
)

//go:embed mockassets/sample.mp4
var sampleFS embed.FS

var sample, _ = sampleFS.ReadFile("mockassets/sample.mp4")

// SampleVideo is the full-quality mock payload; Size variants are byte
// prefixes of it (valid MP4s because the sample is faststart/moov-first).
var (
	SampleVideo     = sample
	SampleVideoSize = len(sample)
	Sample720Video  = sample[:len(sample)*2/3]
	Sample720Size   = len(Sample720Video)
	Sample540Video  = sample[:len(sample)/3]
	Sample540Size   = len(Sample540Video)
)

// CoverJPEG renders a deterministic gradient thumbnail.
func CoverJPEG() []byte {
	img := image.NewRGBA(image.Rect(0, 0, 128, 128))
	for y := 0; y < 128; y++ {
		for x := 0; x < 128; x++ {
			img.Set(x, y, color.RGBA{R: uint8(x * 2), G: uint8(y * 2), B: 128, A: 255})
		}
	}
	var buf bytes.Buffer
	_ = jpeg.Encode(&buf, img, &jpeg.Options{Quality: 80})
	return buf.Bytes()
}
