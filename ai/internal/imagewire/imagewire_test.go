package imagewire_test

import (
	"testing"

	"github.com/rsbin1178/pips/ai/internal/imagewire"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestImageMapsPayloads(t *testing.T) {
	t.Parallel()

	t.Run("inline bytes decode with the png default", func(t *testing.T) {
		t.Parallel()

		image, err := imagewire.Image(imagewire.Datum{
			B64JSON:       "cGl4ZWxz",
			RevisedPrompt: "a sea otter",
		}, "", 0)
		require.NoError(t, err)
		assert.Equal(t, []byte("pixels"), image.Data)
		assert.Equal(t, "image/png", image.MIMEType)
		assert.Equal(t, "a sea otter", image.RevisedPrompt)
		assert.Empty(t, image.URL)
	})

	t.Run("url stays a link and takes its extension", func(t *testing.T) {
		t.Parallel()

		image, err := imagewire.Image(imagewire.Datum{URL: "https://images.example.test/otter.webp?expires=60"}, "", 0)
		require.NoError(t, err)
		assert.Empty(t, image.Data)
		assert.Equal(t, "https://images.example.test/otter.webp?expires=60", image.URL)
		assert.Equal(t, "image/webp", image.MIMEType)
	})

	t.Run("reported mime type wins over the url extension", func(t *testing.T) {
		t.Parallel()

		image, err := imagewire.Image(imagewire.Datum{URL: "https://images.example.test/otter.png"}, "image/jpeg", 0)
		require.NoError(t, err)
		assert.Equal(t, "image/jpeg", image.MIMEType)
	})

	t.Run("empty datum is an error naming the index", func(t *testing.T) {
		t.Parallel()

		_, err := imagewire.Image(imagewire.Datum{RevisedPrompt: "a cat"}, "", 2)
		require.ErrorContains(t, err, "data item 2 carries neither b64_json nor url")
	})

	t.Run("undecodable payload is an error naming the index", func(t *testing.T) {
		t.Parallel()

		_, err := imagewire.Image(imagewire.Datum{B64JSON: "not-base64!"}, "", 1)
		require.ErrorContains(t, err, "decoding image 1")
	})
}

func TestMIME(t *testing.T) {
	t.Parallel()

	cases := []struct {
		format string
		want   string
	}{
		{format: "png", want: "image/png"},
		{format: "PNG", want: "image/png"},
		{format: "jpeg", want: "image/jpeg"},
		{format: "jpg", want: "image/jpeg"},
		{format: "webp", want: "image/webp"},
		{format: "gif", want: ""},
		{format: "", want: ""},
	}

	for _, tc := range cases {
		assert.Equal(t, tc.want, imagewire.MIME(tc.format), "format %q", tc.format)
	}
}

func TestMIMEForPrefersReportedFormat(t *testing.T) {
	t.Parallel()

	assert.Equal(t, "image/jpeg", imagewire.MIMEFor("jpeg", "webp"))
	assert.Equal(t, "image/webp", imagewire.MIMEFor("", "webp"))
	assert.Empty(t, imagewire.MIMEFor("gif", ""))
}

func TestMIMEFromURL(t *testing.T) {
	t.Parallel()

	cases := []struct {
		url  string
		want string
	}{
		{url: "https://images.example.test/otter.webp", want: "image/webp"},
		{url: "https://images.example.test/otter.PNG?expires=60", want: "image/png"},
		{url: "https://images.example.test/otter", want: ""},
		{url: "://bad", want: ""},
	}

	for _, tc := range cases {
		assert.Equal(t, tc.want, imagewire.MIMEFromURL(tc.url), "url %q", tc.url)
	}
}
