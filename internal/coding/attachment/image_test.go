package attachment

import (
	"bytes"
	"context"
	"encoding/binary"
	"hash/crc32"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"image/png"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNormalizeImagePreservesBoundedPNGAndJPEG(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		mimeType string
		encode   func(*testing.T, image.Image) []byte
	}{
		{name: "png", mimeType: "image/png", encode: encodePNGFixture},
		{name: "jpeg", mimeType: "image/jpeg", encode: encodeJPEGFixture},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			encoded := test.encode(t, image.NewRGBA(image.Rect(0, 0, 3, 2)))
			normalized, err := NormalizeImage(test.name, encoded)
			require.NoError(t, err)

			assert.True(t, normalized.Valid())
			assert.Equal(t, test.mimeType, normalized.MIMEType())
			assert.Equal(t, 3, normalized.Width())
			assert.Equal(t, 2, normalized.Height())
			assert.Equal(t, encoded, normalized.Bytes())

			detached := normalized.Bytes()
			detached[0] ^= 0xff
			assert.NotEqual(t, detached, normalized.Bytes())
			assert.Equal(t, normalized.Digest(), normalized.Clone().Digest())
		})
	}
}

func TestNormalizeImageUsesDeterministicBoundedJPEG(t *testing.T) {
	t.Parallel()

	source := image.NewNRGBA(image.Rect(0, 0, 2, 2))
	source.SetNRGBA(0, 0, color.NRGBA{R: 255, A: 0})
	source.SetNRGBA(1, 0, color.NRGBA{R: 255, G: 255, B: 255, A: 255})
	source.SetNRGBA(0, 1, color.NRGBA{R: 255, G: 255, B: 255, A: 255})
	source.SetNRGBA(1, 1, color.NRGBA{B: 255, A: 0})
	encoded := encodePNGFixture(t, source)
	encoded = append(encoded, make([]byte, MaxImageBytes)...)

	first, err := NormalizeImage("transparent.png", encoded)
	require.NoError(t, err)
	second, err := NormalizeImage("transparent.png", encoded)
	require.NoError(t, err)

	assert.Equal(t, "image/jpeg", first.MIMEType())
	assert.LessOrEqual(t, first.Size(), MaxImageBytes)
	assert.True(t, first.Equal(second))
	assert.Equal(t, first.Digest(), second.Digest())
	assert.Equal(t, first.Bytes(), second.Bytes())

	decoded, err := jpeg.Decode(bytes.NewReader(first.Bytes()))
	require.NoError(t, err)

	red, green, blue, _ := decoded.At(0, 0).RGBA()
	assert.Greater(t, red, uint32(50_000))
	assert.Greater(t, green, uint32(50_000))
	assert.Greater(t, blue, uint32(50_000))
}

func TestNormalizeImageResizesLargeDimensions(t *testing.T) {
	t.Parallel()

	source := image.NewRGBA(image.Rect(0, 0, 3000, 1000))

	for y := range 1000 {
		for x := range 3000 {
			source.SetRGBA(x, y, color.RGBA{
				R: uint8(x % 251),
				G: uint8(y % 241),
				B: uint8((x + y) % 239),
				A: 255,
			})
		}
	}

	encoded := encodePNGFixture(t, source)
	if len(encoded) <= MaxImageBytes {
		encoded = append(encoded, make([]byte, MaxImageBytes-len(encoded)+1)...)
	}

	normalized, err := NormalizeImage("large.png", encoded)
	require.NoError(t, err)
	assert.Equal(t, "image/jpeg", normalized.MIMEType())
	assert.Equal(t, 2048, normalized.Width())
	assert.Equal(t, 682, normalized.Height())
	assert.LessOrEqual(t, normalized.Size(), MaxImageBytes)
}

func TestNormalizeImageUsesFirstGIFFrame(t *testing.T) {
	t.Parallel()

	palette := color.Palette{color.RGBA{R: 255, A: 255}, color.RGBA{B: 255, A: 255}}
	first := image.NewPaletted(image.Rect(0, 0, 4, 4), palette)
	second := image.NewPaletted(image.Rect(0, 0, 4, 4), palette)

	for index := range second.Pix {
		second.Pix[index] = 1
	}

	var encoded bytes.Buffer
	require.NoError(t, gif.EncodeAll(&encoded, &gif.GIF{
		Image: []*image.Paletted{first, second},
		Delay: []int{0, 0},
	}))

	normalized, err := NormalizeImage("animation.gif", encoded.Bytes())
	require.NoError(t, err)
	assert.Equal(t, "image/jpeg", normalized.MIMEType())

	decoded, err := jpeg.Decode(bytes.NewReader(normalized.Bytes()))
	require.NoError(t, err)

	red, _, blue, _ := decoded.At(0, 0).RGBA()
	assert.Greater(t, red, blue)
}

func TestNormalizeImageRejectsInvalidAndUnsupportedInput(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		encoded []byte
		err     error
	}{
		{name: "empty", err: ErrLimit},
		{name: "encoded limit", encoded: make([]byte, MaxEncodedImageBytes+1), err: ErrLimit},
		{name: "malformed", encoded: []byte("not an image"), err: ErrInvalidImage},
		{name: "truncated png", encoded: []byte("\x89PNG\r\n\x1a\n"), err: ErrInvalidImage},
		{name: "webp", encoded: []byte("RIFF\x04\x00\x00\x00WEBP"), err: ErrUnsupportedImage},
		{name: "heic", encoded: []byte("\x00\x00\x00\x18ftypheic"), err: ErrUnsupportedImage},
		{name: "too many pixels", encoded: pngConfiguration(10_000, 5_000), err: ErrLimit},
		{name: "zero width", encoded: pngConfiguration(0, 1), err: ErrInvalidImage},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			_, err := NormalizeImage("fixture.png", test.encoded)
			require.ErrorIs(t, err, test.err)
		})
	}

	_, err := NormalizeImage("bad\nname.png", encodePNGFixture(t, image.NewRGBA(image.Rect(0, 0, 1, 1))))
	require.ErrorIs(t, err, ErrInvalidImage)
}

func TestImageIngressLimitsAcceptExactBoundaries(t *testing.T) {
	t.Parallel()

	require.NoError(t, validateImageInput("boundary.png", make([]byte, MaxEncodedImageBytes)))
	require.NoError(t, validateImageDimensions(8_000, 5_000))
	require.ErrorIs(t, validateImageDimensions(8_001, 5_000), ErrLimit)
}

func TestNormalizeImageContextHonorsCancellation(t *testing.T) {
	t.Parallel()

	canceled, cancel := context.WithCancel(t.Context())
	cancel()

	_, err := NormalizeImageContext(canceled, "image.png", []byte("ignored"))
	require.ErrorIs(t, err, context.Canceled)
}

func encodePNGFixture(t *testing.T, source image.Image) []byte {
	t.Helper()

	var output bytes.Buffer
	require.NoError(t, png.Encode(&output, source))

	return output.Bytes()
}

func encodeJPEGFixture(t *testing.T, source image.Image) []byte {
	t.Helper()

	var output bytes.Buffer
	require.NoError(t, jpeg.Encode(&output, source, &jpeg.Options{Quality: 90}))

	return output.Bytes()
}

func pngConfiguration(width, height uint32) []byte {
	data := make([]byte, 13)
	binary.BigEndian.PutUint32(data[0:4], width)
	binary.BigEndian.PutUint32(data[4:8], height)
	data[8] = 8
	data[9] = 6

	chunk := append([]byte("IHDR"), data...)
	result := append([]byte("\x89PNG\r\n\x1a\n\x00\x00\x00\x0d"), chunk...)
	checksum := make([]byte, 4)
	binary.BigEndian.PutUint32(checksum, crc32.ChecksumIEEE(chunk))

	return append(result, checksum...)
}
