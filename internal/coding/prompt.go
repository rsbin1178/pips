package coding

import (
	"errors"
	"fmt"
	"strings"

	"github.com/rsbin/pips/ai"
	"github.com/rsbin/pips/internal/coding/attachment"
)

const (
	// MaxPromptTextBytes is the largest UTF-8 text prompt accepted by Coding
	// frontends and Runtime.
	MaxPromptTextBytes = maxEventTextBytes
)

// ErrInvalidPrompt means prompt text is empty, malformed, or exceeds the
// bounded Runtime input contract.
var ErrInvalidPrompt = errors.New("coding prompt: invalid text")

// ValidatePromptText validates a text-only prompt without normalizing it.
func ValidatePromptText(text string) error {
	if !validBoundedText(text, MaxPromptTextBytes, false) {
		return ErrInvalidPrompt
	}

	return nil
}

func validatePromptMessages(messages []ai.Message) error {
	if len(messages) == 0 {
		return fmt.Errorf("%w: messages are required", ErrInvalidPrompt)
	}

	textBytes := 0
	imageCount := 0
	imageBytes := int64(0)
	hasUserContent := false

	for index, message := range messages {
		if err := validateMessage(message); err != nil {
			return fmt.Errorf("%w: message %d: %w", ErrInvalidPrompt, index, err)
		}

		user, ok := message.(ai.UserMessage)
		if !ok {
			continue
		}

		for _, part := range user.Parts {
			switch value := part.(type) {
			case ai.TextPart:
				textBytes += len(value.Text)
				if strings.TrimSpace(value.Text) != "" {
					hasUserContent = true
				}
			case ai.ImagePart:
				if len(value.Source.Data) > attachment.MaxImageBytes {
					return ErrInvalidPrompt
				}

				imageCount++
				imageBytes += int64(len(value.Source.Data))
				hasUserContent = true
			default:
				hasUserContent = true
			}
		}
	}

	if !hasUserContent || textBytes > MaxPromptTextBytes ||
		imageCount > attachment.MaxImagesPerMessage ||
		imageBytes > int64(attachment.MaxImageBytesPerMessage) {
		return ErrInvalidPrompt
	}

	return nil
}
