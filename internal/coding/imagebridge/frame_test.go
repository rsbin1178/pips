package imagebridge

import (
	"bytes"
	"context"
	"encoding/binary"
	"image"
	"image/color"
	"image/png"
	"testing"
	"time"

	"github.com/rsbin/pips/internal/coding/attachment"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestFrameRoundTripImageAndNotices(t *testing.T) {
	t.Parallel()

	now := time.Unix(1_800_000_000, 0)
	image := bridgeImageFixture(t)
	tests := []Frame{
		{Image: image, Deadline: now.Add(10 * time.Second)},
		{Notice: NoticeAccepted, Deadline: now.Add(5 * time.Second)},
		{Notice: NoticeBusy, Deadline: now.Add(5 * time.Second)},
		{Notice: NoticeRejected, Deadline: now.Add(5 * time.Second)},
	}

	for _, expected := range tests {
		var encoded bytes.Buffer
		require.NoError(t, Encode(&encoded, expected))

		decoded, err := Decode(t.Context(), bytes.NewReader(encoded.Bytes()), now)
		require.NoError(t, err)
		assert.Equal(t, expected.Notice, decoded.Notice)
		assert.Equal(t, expected.Deadline, decoded.Deadline)
		assert.True(t, expected.Image.Equal(decoded.Image))
	}
}

func TestDecodeRejectsHostileFrames(t *testing.T) {
	t.Parallel()

	now := time.Unix(1_800_000_000, 0)
	valid := encodedFrameFixture(t, Frame{
		Image: bridgeImageFixture(t), Deadline: now.Add(10 * time.Second),
	})

	tests := []struct {
		name  string
		value func([]byte) []byte
		err   error
	}{
		{name: "truncated", value: func(value []byte) []byte { return value[:20] }, err: ErrProtocol},
		{name: "magic", value: mutateFrame(0, 0xff), err: ErrProtocol},
		{name: "version", value: mutateFrame(8, 2), err: ErrProtocol},
		{name: "type", value: mutateFrame(9, 9), err: ErrProtocol},
		{name: "reserved", value: mutateFrame(10, 1), err: ErrProtocol},
		{name: "media", value: mutateFrame(11, 9), err: ErrProtocol},
		{name: "digest", value: mutateFrame(24, 0xff), err: ErrProtocol},
		{name: "trailing", value: func(value []byte) []byte { return append(value, 0) }, err: ErrProtocol},
		{name: "expired", value: replaceDeadline(now.Add(-time.Second)), err: ErrDeadline},
		{name: "excessive deadline", value: replaceDeadline(now.Add(maximumFrameAge + time.Millisecond)), err: ErrDeadline},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			_, err := Decode(t.Context(), bytes.NewReader(test.value(bytes.Clone(valid))), now)
			require.ErrorIs(t, err, test.err)
		})
	}
}

func TestDecodeHonorsCancellation(t *testing.T) {
	t.Parallel()

	canceled, cancel := context.WithCancel(t.Context())
	cancel()

	_, err := Decode(canceled, bytes.NewReader(nil), time.Now())
	require.ErrorIs(t, err, context.Canceled)
}

func TestNonceIsCanonicalAndRandom(t *testing.T) {
	t.Parallel()

	first, err := newNonce(bytes.NewReader(bytes.Repeat([]byte{1}, nonceBytes)))
	require.NoError(t, err)
	second, err := newNonce(bytes.NewReader(bytes.Repeat([]byte{2}, nonceBytes)))
	require.NoError(t, err)

	assert.Len(t, first, nonceLength)
	assert.NotEqual(t, first, second)
	require.NoError(t, ValidateNonce(first))
	require.ErrorIs(t, ValidateNonce(first+"x"), ErrProtocol)
	require.ErrorIs(t, ValidateNonce("___________________________________________"), ErrProtocol)
}

func FuzzDecodeFrame(f *testing.F) {
	now := time.Unix(1_800_000_000, 0)
	image := bridgeImageFixture(f)
	f.Add(encodedFrameFixture(f, Frame{Image: image, Deadline: now.Add(time.Second)}))
	f.Add([]byte("PIPSIB01"))

	f.Fuzz(func(t *testing.T, value []byte) {
		_, _ = Decode(t.Context(), bytes.NewReader(value), now)
	})
}

func bridgeImageFixture(tb testing.TB) attachment.Image {
	tb.Helper()

	source := image.NewRGBA(image.Rect(0, 0, 2, 2))

	for y := range 2 {
		for x := range 2 {
			source.SetRGBA(x, y, color.RGBA{R: 32, G: 64, B: 128, A: 255})
		}
	}

	var encoded bytes.Buffer
	require.NoError(tb, png.Encode(&encoded, source))

	normalized, err := attachment.NormalizeImage("clipboard.png", encoded.Bytes())
	require.NoError(tb, err)

	return normalized
}

func encodedFrameFixture(tb testing.TB, frame Frame) []byte {
	tb.Helper()

	var encoded bytes.Buffer
	require.NoError(tb, Encode(&encoded, frame))

	return encoded.Bytes()
}

func mutateFrame(offset int, replacement byte) func([]byte) []byte {
	return func(value []byte) []byte {
		value[offset] = replacement

		return value
	}
}

func replaceDeadline(deadline time.Time) func([]byte) []byte {
	return func(value []byte) []byte {
		binary.BigEndian.PutUint64(value[16:24], uint64(deadline.UnixMilli()))

		return value
	}
}
