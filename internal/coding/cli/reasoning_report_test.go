package cli_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// reasoningVisibilityConfig selects a Qwen model whose configured level can be
// checked end to end: a reviewed Chat Completions provider plus a declared
// reasoning ladder.
const reasoningVisibilityConfig = `
[providers.qwen]
protocol = "openai/chat_completions"

[providers.qwen.models."qwen3.8-max"]
default = true
reasoning_levels = ["low", "high"]
default_reasoning_level = "high"
`

// TestConfigShowReportsReasoningState proves `config show` states the reasoning
// level that will be sent and the encoding that carries it, so a profile change
// is visible without starting a session
// (.trellis/spec/backend/provider-compatibility-policy.md R5).
func TestConfigShowReportsReasoningState(t *testing.T) {
	t.Parallel()

	fixture := newCLIFixture(t)
	writeCLIFile(t, fixture.layout.ConfigFile(), reasoningVisibilityConfig)

	output, err := executeWithDependencies(t, fixture.dependencies(nil), "config", "show")
	require.NoError(t, err)
	assert.Contains(t, output, `resolved.reasoning.level = "high"`)
	assert.Contains(t, output, `resolved.reasoning.encoding = "reasoning_effort" # compatibility.chat_reasoning`)
	assert.NotContains(t, output, "resolved.reasoning.warning")

	// A level chosen on the command line resolves through the same path and
	// must not be blocked by the profile.
	output, err = executeWithDependencies(
		t,
		fixture.dependencies(nil),
		"config",
		"show",
		"--reasoning=low",
	)
	require.NoError(t, err)
	assert.Contains(t, output, `reasoning = "low" # source=flag detail="--reasoning"`)
	assert.Contains(t, output, `resolved.reasoning.level = "low"`)
	assert.NotContains(t, output, "resolved.reasoning.warning")
}

// TestConfigShowWarnsWhenProfileCannotEncodeReasoning covers the configured
// case the policy forbids from being silent: the profile declines to encode a
// level, so the selection cannot take effect and both config show and doctor
// must say so.
func TestConfigShowWarnsWhenProfileCannotEncodeReasoning(t *testing.T) {
	t.Parallel()

	fixture := newCLIFixture(t)
	writeCLIFile(t, fixture.layout.ConfigFile(), reasoningVisibilityConfig+`
[providers.qwen.compatibility]
chat_reasoning = "omit"
`)

	output, err := executeWithDependencies(t, fixture.dependencies(nil), "config", "show")
	require.NoError(t, err)
	assert.Contains(t, output, `resolved.reasoning.encoding = "omit" # compatibility.chat_reasoning`)
	assert.Contains(t, output, `resolved.reasoning.warning = "compatibility.chat_reasoning=omit on qwen/qwen3.8-max`)
	assert.Contains(t, output, "the configured reasoning selection will not be sent")
}

// TestDoctorReportsReasoningWarningWithoutFailing proves doctor surfaces the
// unencoded selection as a warning and still exits successfully: only `fail`
// changes the exit code.
func TestDoctorReportsReasoningWarningWithoutFailing(t *testing.T) {
	t.Parallel()

	fixture := newCLIFixture(t)
	writeCLIFile(t, fixture.layout.ConfigFile(), reasoningVisibilityConfig+`
[providers.qwen.compatibility]
chat_reasoning = "omit"
`)

	output, err := executeWithDependencies(
		t,
		fixture.dependencies(map[string]string{"API_KEY": "key"}),
		"doctor",
	)
	require.NoError(t, err)
	assert.Contains(t, output, "configuration ok")
	assert.Contains(t, output, "reasoning warn compatibility.chat_reasoning=omit on qwen/qwen3.8-max")
	assert.NotContains(t, output, "reasoning fail")
}

// TestDoctorStaysRunnableOnBrokenConfiguration proves the diagnostic command
// reports a configuration it cannot resolve instead of dying before it prints
// anything; a broken file is exactly when doctor is needed.
func TestDoctorStaysRunnableOnBrokenConfiguration(t *testing.T) {
	t.Parallel()

	fixture := newCLIFixture(t)
	writeCLIFile(t, fixture.layout.ConfigFile(), "this is not TOML = = =")

	output, err := executeWithDependencies(t, fixture.dependencies(nil), "doctor")
	require.Error(t, err)
	assert.Contains(t, output, "workspace ok")
	assert.Contains(t, output, "configuration fail")
	assert.Contains(t, output, "Next steps:")
	assert.NotContains(t, output, "configuration ok")
}
