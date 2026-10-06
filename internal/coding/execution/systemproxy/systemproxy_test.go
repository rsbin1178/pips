package systemproxy

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestParseScutilHTTPSProxy(t *testing.T) {
	t.Parallel()

	enabled := "<dictionary> {\n  HTTPEnable : 1\n  HTTPPort : 8080\n  HTTPProxy : 10.0.0.2\n" +
		"  HTTPSEnable : 1\n  HTTPSPort : 7890\n  HTTPSProxy : 127.0.0.1\n}\n"
	assert.Equal(t, "127.0.0.1:7890", parseScutilHTTPSProxy(enabled))
	assert.Equal(t, "[::1]:7890", parseScutilHTTPSProxy("HTTPSEnable : 1\nHTTPSPort : 7890\nHTTPSProxy : ::1\n"))
	assert.Empty(t, parseScutilHTTPSProxy("HTTPSEnable : 0\nHTTPSPort : 7890\nHTTPSProxy : 127.0.0.1\n"))
	assert.Empty(t, parseScutilHTTPSProxy("HTTPSEnable : 1\nHTTPSProxy : 127.0.0.1\n"))
	assert.Empty(t, parseScutilHTTPSProxy(""))
}
