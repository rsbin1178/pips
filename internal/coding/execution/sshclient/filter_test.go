package sshclient

import (
	"bytes"
	"strings"
	"testing"

	"github.com/rsbin/pips/internal/coding/imagebridge"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestInputFilterCtrlVAndBracketedPaste(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		chunks   []string
		forward  string
		requests int
	}{
		{name: "ordinary", chunks: []string{"abc"}, forward: "abc"},
		{name: "ctrl v", chunks: []string{"a\x16b\x16"}, forward: "ab", requests: 2},
		{
			name:     "inside bracketed paste",
			chunks:   []string{"x\x1b[20", "0~a\x16b\x1b[", "201~y\x16"},
			forward:  "x\x1b[200~a\x16b\x1b[201~y",
			requests: 1,
		},
		{name: "partial marker", chunks: []string{"\x1b[20"}, forward: "\x1b[20"},
		{name: "hostile near marker", chunks: []string{"\x1b[20\x16x"}, forward: "\x1b[20x", requests: 1},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			var (
				output   bytes.Buffer
				requests int
			)

			filter := &InputFilter{}

			for _, chunk := range test.chunks {
				forward, count := filter.Push([]byte(chunk))
				output.Write(forward)

				requests += count
			}

			forward, count := filter.Flush()
			output.Write(forward)

			requests += count

			assert.Equal(t, test.forward, output.String())
			assert.Equal(t, test.requests, requests)
		})
	}
}

func FuzzInputFilterPreservesBytesExceptCtrlV(f *testing.F) {
	f.Add([]byte("text\x16more"), uint8(1))
	f.Add([]byte("\x1b[200~\x16\x1b[201~"), uint8(2))

	f.Fuzz(func(t *testing.T, input []byte, chunkSize uint8) {
		baseline := &InputFilter{}
		baselineOutput, baselineRequests := baseline.Push(input)
		baselineTail, baselineTailRequests := baseline.Flush()
		baselineOutput = append(baselineOutput, baselineTail...)
		baselineRequests += baselineTailRequests

		var (
			output   bytes.Buffer
			requests int
		)

		filter := &InputFilter{}
		step := max(1, int(chunkSize))

		for start := 0; start < len(input); start += step {
			forward, count := filter.Push(input[start:min(start+step, len(input))])
			output.Write(forward)

			requests += count
		}

		forward, count := filter.Flush()
		output.Write(forward)

		requests += count

		assert.Equal(t, baselineOutput, output.Bytes())
		assert.Equal(t, baselineRequests, requests)
	})
}

func TestValidationAndFixedArguments(t *testing.T) {
	t.Parallel()

	nonce, err := imagebridge.NewNonce()
	require.NoError(t, err)

	request := Request{
		Destination: "deploy@example.internal",
		Workspace:   "/root/server/temp",
		Version:     "1.2.3",
		Nonce:       nonce,
	}

	primary, err := primaryArguments(request, "/tmp/pips-ssh-owned/master.sock")
	require.NoError(t, err)

	upload, err := uploadArguments(request, "/tmp/pips-ssh-owned/master.sock")
	require.NoError(t, err)

	assert.Equal(t, "-M", primary[0])
	assert.Contains(t, primary, "__bridge-session")
	assert.Contains(t, upload, "__bridge-upload")
	assert.NotContains(t, strings.Join(primary, " "), request.Workspace)
	assert.NotContains(t, strings.Join(upload, " "), request.Version)
	assert.Contains(t, primary, "SendEnv=-*")
	assert.Contains(t, primary, "ForwardAgent=no")
	assert.Contains(t, upload, "BatchMode=yes")
	assert.Contains(t, upload, "PubkeyAuthentication=no")
}

func TestValidationRejectsHostileInput(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		destination string
		workspace   string
	}{
		{name: "option", destination: "-oProxyCommand=bad", workspace: "."},
		{name: "destination whitespace", destination: "host name", workspace: "."},
		{name: "destination control", destination: "host\nname", workspace: "."},
		{name: "remote shell", destination: "host;touch", workspace: "."},
		{name: "workspace relative", destination: "host", workspace: "relative/path"},
		{name: "workspace traversal", destination: "host", workspace: "/root/../etc"},
		{name: "workspace whitespace", destination: "host", workspace: "/root/server temp"},
		{name: "workspace control", destination: "host", workspace: "/root\ntemp"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			request := Request{
				Destination: test.destination,
				Workspace:   test.workspace,
				Version:     "1.2.3",
				Nonce:       strings.Repeat("A", 43),
			}
			require.ErrorIs(t, ValidateRequest(request), ErrInvalid)
		})
	}
}
