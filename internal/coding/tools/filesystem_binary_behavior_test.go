package tools_test

import (
	"strings"
	"testing"

	"github.com/rsbin1178/pips/internal/coding/tools"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestReadRejectsControlHeavyText(t *testing.T) {
	t.Parallel()

	fixture := newToolFixture(t, tools.DefaultLimits())
	fixture.write("capture.log", strings.Repeat("\x01\x1bxy", 200))

	_, err := fixture.exec(t.Context(), "read", `{"path":"capture.log"}`)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "binary file")
}

func TestReadKeepsOrdinaryText(t *testing.T) {
	t.Parallel()

	fixture := newToolFixture(t, tools.DefaultLimits())
	fixture.write("notes.txt", "hello\nworld\n")

	text, err := fixture.exec(t.Context(), "read", `{"path":"notes.txt"}`)
	require.NoError(t, err)
	assert.Contains(t, text, "hello")
}
