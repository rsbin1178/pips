package cli

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestDoctorCapabilityValue(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		value string
		want  string
	}{
		{name: "runtime", value: "bubblewrap", want: "bubblewrap"},
		{name: "version", value: "0.8.0", want: "0.8.0"},
		{name: "empty", want: "unknown"},
		{name: "line injection", value: "bubblewrap\ncredential=secret", want: "unknown"},
		{name: "oversize", value: string(make([]byte, 65)), want: "unknown"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			assert.Equal(t, test.want, doctorCapabilityValue(test.value))
		})
	}
}
