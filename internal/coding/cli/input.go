package cli

import (
	"context"
	"fmt"
	"io"
	"os"

	"github.com/rsbin/pips/internal/coding"
)

type outputMode string

const (
	outputPlain outputMode = "plain"
	outputJSONL outputMode = "jsonl"
)

func parseOutputMode(value string) (outputMode, error) {
	switch outputMode(value) {
	case outputPlain:
		return outputPlain, nil
	case outputJSONL:
		return outputJSONL, nil
	default:
		return "", fmt.Errorf("%w: unsupported output mode %q", ErrUsage, value)
	}
}

func readPrompt(ctx context.Context, input io.Reader, args []string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}

	if len(args) > 1 {
		return "", fmt.Errorf("%w: exec accepts at most one prompt", ErrUsage)
	}

	readInput := len(args) == 0 || args[0] == "-"
	if readInput {
		if len(args) == 0 && isCharacterDevice(input) {
			return "", fmt.Errorf("%w: provide a prompt argument or use '-' to read stdin", ErrUsage)
		}

		value, err := readPromptInput(ctx, input)
		if err != nil {
			return "", err
		}

		return validatePrompt(value)
	}

	if !isCharacterDevice(input) {
		data, err := readBounded(ctx, input, 1)
		if err != nil {
			return "", err
		}

		if len(data) != 0 {
			return "", fmt.Errorf("%w: prompt argument and stdin cannot both contain input", ErrUsage)
		}
	}

	return validatePrompt(args[0])
}

func readPromptInput(ctx context.Context, input io.Reader) (string, error) {
	data, err := readBounded(ctx, input, coding.MaxPromptTextBytes+1)
	if err != nil {
		return "", err
	}

	if len(data) > coding.MaxPromptTextBytes {
		return "", fmt.Errorf("%w: prompt exceeds %d bytes", ErrUsage, coding.MaxPromptTextBytes)
	}

	return string(data), nil
}

func validatePrompt(value string) (string, error) {
	if err := coding.ValidatePromptText(value); err != nil {
		return "", fmt.Errorf("%w: %w", ErrUsage, err)
	}

	return value, nil
}

func readBounded(ctx context.Context, input io.Reader, maximum int64) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	file, isFile := input.(*os.File)
	if isFile {
		data, err := readFileBounded(ctx, file, maximum)
		if err != nil {
			return nil, fmt.Errorf("coding cli: read stdin: %w", err)
		}

		return data, nil
	}

	data, err := io.ReadAll(io.LimitReader(input, maximum))
	if err != nil {
		return nil, fmt.Errorf("coding cli: read stdin: %w", err)
	}

	if err := ctx.Err(); err != nil {
		return nil, err
	}

	return data, nil
}

func isCharacterDevice(input io.Reader) bool {
	file, ok := input.(*os.File)
	if !ok {
		return false
	}

	info, err := file.Stat()

	return err == nil && info.Mode()&os.ModeCharDevice != 0
}
