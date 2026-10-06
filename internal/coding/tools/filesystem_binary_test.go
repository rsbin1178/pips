package tools

import (
	"strings"
	"testing"
)

func TestControlHeavySample(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		data string
		want bool
	}{
		{name: "plain text", data: strings.Repeat("hello world\n", 400), want: false},
		{name: "empty", data: "", want: false},
		{name: "a few escapes", data: strings.Repeat("ok", 200) + "\x1b[31mred\x1b[0m", want: false},
		{name: "terminal capture", data: strings.Repeat("\x01\x1bxy", 200), want: true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			if got := controlHeavySample([]byte(test.data)); got != test.want {
				t.Fatalf("controlHeavySample(%s) = %v, want %v", test.name, got, test.want)
			}
		})
	}
}
