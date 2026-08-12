//nolint:wsl_v5 // The opt-in provider transaction stays linear for auditability.
package tui_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/rsbin1178/pips/internal/coding"
	"github.com/rsbin1178/pips/internal/coding/cli"
	"github.com/rsbin1178/pips/internal/coding/paths"
	"github.com/stretchr/testify/require"
)

//nolint:paralleltest // An opt-in real-provider smoke must not compete for rate limits.
func TestProviderSmoke(t *testing.T) {
	if os.Getenv("PIPS_PROVIDER_SMOKE") != "1" {
		t.Skip("set PIPS_PROVIDER_SMOKE=1 with API_KEY and canonical PIPS_MODEL")
	}

	modelID := requiredSmokeEnvironment(t, "PIPS_MODEL")
	apiKey := requiredSmokeEnvironment(t, "API_KEY")
	workspace := t.TempDir()
	layout, err := paths.New(t.TempDir())
	require.NoError(t, err)

	command, err := cli.New(cli.Dependencies{
		Paths:      layout,
		LookupEnv:  os.LookupEnv,
		WorkingDir: func() (string, error) { return workspace, nil },
	})
	require.NoError(t, err)

	arguments := []string{
		"--workspace", workspace,
		"--model", modelID,
		"--sandbox", "full-access",
		"--approval", "never",
	}
	arguments = append(arguments, "exec", "--output", "jsonl", "Reply with a brief acknowledgement.")

	stdout := new(bytes.Buffer)
	stderr := new(bytes.Buffer)
	command.SetOut(stdout)
	command.SetErr(stderr)
	command.SetArgs(arguments)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	if err := command.ExecuteContext(ctx); err != nil {
		t.Fatalf("provider smoke failed without disclosing provider output: %v", err)
	}
	if strings.Contains(stdout.String(), apiKey) || strings.Contains(stderr.String(), apiKey) {
		t.Fatal("provider smoke output disclosed API_KEY")
	}

	var assistantMessage, interactionCompleted bool
	scanner := bufio.NewScanner(bytes.NewReader(stdout.Bytes()))
	scanner.Buffer(make([]byte, 64<<10), 1<<20)
	for scanner.Scan() {
		var envelope struct {
			Schema  string           `json:"schema"`
			Type    coding.EventType `json:"type"`
			Payload json.RawMessage  `json:"payload"`
		}
		require.NoError(t, json.Unmarshal(scanner.Bytes(), &envelope))
		require.Equal(t, coding.EventSchema, envelope.Schema)

		switch envelope.Type {
		case coding.EventMessageCommitted:
			var payload struct {
				Message struct {
					Role string `json:"role"`
				} `json:"message"`
			}
			require.NoError(t, json.Unmarshal(envelope.Payload, &payload))
			assistantMessage = assistantMessage || payload.Message.Role == "assistant"
		case coding.EventInteractionCompleted:
			interactionCompleted = true
		default:
			// Other lifecycle events are valid but are not smoke-test success signals.
		}
	}
	require.NoError(t, scanner.Err())
	require.True(t, assistantMessage, "provider smoke returned no assistant message")
	require.True(t, interactionCompleted, "provider smoke did not complete the interaction")
}

//nolint:paralleltest // An opt-in real-provider conformance run must not compete for rate limits.
func TestProviderPlanConformance(t *testing.T) {
	if os.Getenv("PIPS_PLAN_CONFORMANCE") != "1" {
		t.Skip("set PIPS_PLAN_CONFORMANCE=1 with API_KEY and canonical PIPS_MODEL")
	}

	modelID := requiredSmokeEnvironment(t, "PIPS_MODEL")
	apiKey := requiredSmokeEnvironment(t, "API_KEY")
	workspace := t.TempDir()
	layout, err := paths.New(t.TempDir())
	require.NoError(t, err)

	command, err := cli.New(cli.Dependencies{
		Paths:      layout,
		LookupEnv:  os.LookupEnv,
		WorkingDir: func() (string, error) { return workspace, nil },
	})
	require.NoError(t, err)

	stdout := new(bytes.Buffer)
	stderr := new(bytes.Buffer)
	command.SetOut(stdout)
	command.SetErr(stderr)
	command.SetArgs([]string{
		"exec", "--output", "jsonl",
		"--model", modelID,
		"--mode", "plan",
		"--sandbox", "workspace-write",
		"--approval", "never",
		"Help me plan and design a student management system using full-stack TypeScript.",
	})

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	runErr := command.ExecuteContext(ctx)
	if !errors.Is(runErr, coding.ErrInputRequired) {
		t.Fatalf("Plan conformance must pause for a material decision: %v", runErr)
	}
	if strings.Contains(stdout.String(), apiKey) || strings.Contains(stderr.String(), apiKey) {
		t.Fatal("Plan conformance output disclosed API_KEY")
	}

	questionRequired := false
	planWriteStarted := false
	scanner := bufio.NewScanner(bytes.NewReader(stdout.Bytes()))
	scanner.Buffer(make([]byte, 64<<10), 1<<20)
	for scanner.Scan() {
		var envelope struct {
			Type    coding.EventType `json:"type"`
			Payload json.RawMessage  `json:"payload"`
		}
		require.NoError(t, json.Unmarshal(scanner.Bytes(), &envelope))

		switch envelope.Type {
		case coding.EventQuestionRequired:
			questionRequired = true
		case coding.EventToolStarted:
			var payload struct {
				Call struct {
					Name string `json:"name"`
				} `json:"call"`
			}
			require.NoError(t, json.Unmarshal(envelope.Payload, &payload))
			planWriteStarted = planWriteStarted || payload.Call.Name == "write_plan"
		default:
		}
	}
	require.NoError(t, scanner.Err())
	require.True(t, questionRequired, "broad product request reached no structured question")
	require.False(t, planWriteStarted, "Plan write started before material product decisions")
}

func requiredSmokeEnvironment(t *testing.T, name string) string {
	t.Helper()

	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		t.Fatalf("%s is required when PIPS_PROVIDER_SMOKE=1", name)
	}

	return value
}
