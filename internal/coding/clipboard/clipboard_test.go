package clipboard

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestReaderReadImageReturnsDetachedBytes(t *testing.T) {
	t.Parallel()

	source := []byte{1, 2, 3}
	initialized := 0
	reader := newReader(func() error {
		initialized++

		return nil
	}, func() []byte {
		return source
	})

	data, err := reader.ReadImage(t.Context())
	require.NoError(t, err)
	assert.Equal(t, []byte{1, 2, 3}, data)
	assert.Equal(t, 1, initialized)

	source[0] = 9

	assert.Equal(t, []byte{1, 2, 3}, data)
}

func TestReaderReadImageRejectsUnavailableEmptyAndPanic(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		reader *Reader
		err    error
	}{
		{
			name:   "zero reader",
			reader: &Reader{},
			err:    ErrUnavailable,
		},
		{
			name: "initialization failure",
			reader: newReader(
				func() error { return errors.New("display missing") },
				func() []byte { return []byte{1} },
			),
			err: ErrUnavailable,
		},
		{
			name: "empty",
			reader: newReader(
				func() error { return nil },
				func() []byte { return nil },
			),
			err: ErrEmpty,
		},
		{
			name: "initialization panic",
			reader: newReader(
				func() error { panic("private backend value") },
				func() []byte { return nil },
			),
			err: ErrUnavailable,
		},
		{
			name: "read panic",
			reader: newReader(
				func() error { return nil },
				func() []byte { panic("private clipboard bytes") },
			),
			err: ErrUnavailable,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			_, err := test.reader.ReadImage(t.Context())
			require.ErrorIs(t, err, test.err)
			assert.NotContains(t, err.Error(), "private")
		})
	}
}

func TestReaderReadImageHonorsCancellation(t *testing.T) {
	t.Parallel()

	canceled, cancel := context.WithCancel(t.Context())
	cancel()

	called := false
	reader := newReader(func() error {
		called = true

		return nil
	}, func() []byte {
		called = true

		return []byte{1}
	})

	_, err := reader.ReadImage(canceled)
	require.ErrorIs(t, err, context.Canceled)
	assert.False(t, called)
}
