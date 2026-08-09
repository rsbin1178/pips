package hooks

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/rsbin/pips/internal/coding/paths"
	"github.com/rsbin/pips/internal/coding/workspace"
	"github.com/rsbin/pips/internal/jsonx"
)

// LoadOptions select user and optional trusted-project lifecycle hook files.
type LoadOptions struct {
	Paths          paths.Layout
	Tree           *workspace.Tree
	ProjectTrusted bool
	Limits         Limits
}

// Load reads strict user and trusted-project lifecycle hook definitions. An
// untrusted project hook file is never inspected.
func Load(ctx context.Context, options LoadOptions) (Definitions, error) {
	if options.Paths.Root() == "" || !options.Limits.valid() {
		return Definitions{}, fmt.Errorf("%w: invalid load options", ErrInvalid)
	}
	if options.ProjectTrusted && options.Tree == nil {
		return Definitions{}, fmt.Errorf("%w: trusted project requires a workspace tree", ErrInvalid)
	}
	if err := ctx.Err(); err != nil {
		return Definitions{}, err
	}

	values := make([]Definition, 0)
	userData, userExists, err := readUserFile(options.Paths.HooksFile(), options.Limits.MaxFileBytes)
	if err != nil {
		return Definitions{}, err
	}
	if userExists {
		decoded, decodeErr := decodeFile(
			userData, ScopeUser, options.Paths.HooksFile(), options.Limits,
		)
		if decodeErr != nil {
			return Definitions{}, fmt.Errorf("coding hooks: decode user definitions: %w", decodeErr)
		}
		values = append(values, decoded...)
	}

	if options.ProjectTrusted {
		projectData, projectExists, readErr := readProjectFile(
			options.Tree, paths.ProjectHooksFile(), options.Limits.MaxFileBytes,
		)
		if readErr != nil {
			return Definitions{}, readErr
		}
		if projectExists {
			decoded, decodeErr := decodeFile(
				projectData, ScopeProject, paths.ProjectHooksFile(), options.Limits,
			)
			if decodeErr != nil {
				return Definitions{}, fmt.Errorf("coding hooks: decode project definitions: %w", decodeErr)
			}
			values = append(values, decoded...)
		}
	}

	if len(values) > options.Limits.MaxHandlers {
		return Definitions{}, ErrLimitExceeded
	}

	privateIDs := make(map[string]struct{})

	for _, definition := range values {
		if definition.effectiveVisibility() != VisibilityAgentPrivate {
			continue
		}

		if _, duplicate := privateIDs[definition.ID]; duplicate {
			return Definitions{}, fmt.Errorf("%w: duplicate agent-private hook ID %q", ErrInvalid, definition.ID)
		}

		privateIDs[definition.ID] = struct{}{}
	}

	return Definitions{values: values}, nil
}

type hookFile struct {
	Schema string                 `json:"schema"`
	Hooks  map[string][]hookGroup `json:"hooks"`
}

type hookGroup struct {
	Matcher string        `json:"matcher,omitempty"`
	Hooks   []hookCommand `json:"hooks"`
}

type hookCommand struct {
	ID         string          `json:"id,omitempty"`
	Type       string          `json:"type"`
	Visibility string          `json:"visibility,omitempty"`
	Command    string          `json:"command"`
	Timeout    json.RawMessage `json:"timeout"`
}

func decodeFile(data []byte, scope Scope, source string, limits Limits) ([]Definition, error) {
	var file hookFile
	if err := jsonx.Decode(data, &file); err != nil {
		return nil, err
	}
	if file.Schema != Schema {
		return nil, fmt.Errorf("%w: unsupported schema %q", ErrInvalid, file.Schema)
	}
	if file.Hooks == nil {
		return nil, fmt.Errorf("%w: hooks must be an object", ErrInvalid)
	}
	for eventName := range file.Hooks {
		if !Event(eventName).valid() {
			return nil, fmt.Errorf("%w: unsupported event %q", ErrInvalid, eventName)
		}
	}

	values := make([]Definition, 0)
	for _, event := range eventOrder {
		groups := file.Hooks[string(event)]
		for groupIndex, group := range groups {
			if len(group.Matcher) > limits.MaxMatcher || !utf8.ValidString(group.Matcher) ||
				strings.ContainsRune(group.Matcher, '\x00') {
				return nil, fmt.Errorf("%w: %s matcher %d", ErrInvalid, event, groupIndex)
			}
			if len(group.Hooks) == 0 {
				return nil, fmt.Errorf("%w: %s group %d has no handlers", ErrInvalid, event, groupIndex)
			}

			for handlerIndex, handler := range group.Hooks {
				if len(values) >= limits.MaxHandlers {
					return nil, ErrLimitExceeded
				}
				if handler.Type != "command" || !utf8.ValidString(handler.Command) ||
					strings.ContainsRune(handler.Command, '\x00') {
					return nil, fmt.Errorf("%w: %s handler %d", ErrInvalid, event, handlerIndex)
				}
				timeout, err := decodeTimeout(handler.Timeout, event)
				if err != nil {
					return nil, fmt.Errorf("%w: %s handler %d timeout", ErrInvalid, event, handlerIndex)
				}

				reference := fmt.Sprintf("%s/%s/%d/%d", scope, event, groupIndex, handlerIndex)
				if handler.Visibility == string(VisibilityAgentPrivate) && handler.ID != "" {
					reference = fmt.Sprintf("%s/private/%s", scope, handler.ID)
				}
				definition, compileErr := compileDefinition(Definition{
					ID: handler.ID, Reference: reference,
					Scope: scope, Source: source, Visibility: Visibility(handler.Visibility),
					Event: event, Matcher: group.Matcher, Command: handler.Command, Timeout: timeout,
				}, limits)
				if compileErr != nil {
					return nil, fmt.Errorf("%w: %s handler %d: %w", ErrInvalid, event, handlerIndex, compileErr)
				}
				values = append(values, definition)
			}
		}
	}

	return values, nil
}

func decodeTimeout(raw json.RawMessage, event Event) (time.Duration, error) {
	if len(raw) == 0 {
		if event == EventSessionEnd {
			return defaultEndTimeout, nil
		}

		return defaultTimeout, nil
	}
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return 0, errors.New("timeout cannot be null")
	}

	var seconds int64
	if err := jsonx.Decode(raw, &seconds); err != nil || seconds <= 0 {
		return 0, errors.New("timeout must be a positive integer")
	}
	if seconds > int64(maximumTimeout/time.Second) ||
		event == EventSessionEnd && seconds > int64(maximumEndTimeout/time.Second) {
		return 0, ErrLimitExceeded
	}

	return time.Duration(seconds) * time.Second, nil
}

func readUserFile(filePath string, maximum int64) ([]byte, bool, error) {
	info, err := os.Lstat(filePath)
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("coding hooks: inspect user definitions: %w", err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
		return nil, false, fmt.Errorf("%w: user hooks file", ErrUnsafeFile)
	}

	file, err := os.Open(filePath) //nolint:gosec // Lstat and SameFile bind this user-owned path.
	if err != nil {
		return nil, false, fmt.Errorf("coding hooks: open user definitions: %w", err)
	}
	defer func() { _ = file.Close() }()

	opened, err := file.Stat()
	if err != nil || !os.SameFile(info, opened) {
		return nil, false, fmt.Errorf("%w: user hooks file changed while opening", ErrUnsafeFile)
	}
	data, err := readBounded(file, maximum)
	if err != nil {
		return nil, false, fmt.Errorf("coding hooks: read user definitions: %w", err)
	}

	return data, true, nil
}

func readProjectFile(tree *workspace.Tree, filePath string, maximum int64) ([]byte, bool, error) {
	info, err := tree.Lstat(filePath)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("coding hooks: inspect project file %q: %w", filePath, err)
	}
	if !info.Mode().IsRegular() {
		return nil, false, fmt.Errorf("%w: project hooks file %q is not regular", ErrUnsafeFile, filePath)
	}

	file, err := tree.Open(filePath)
	if err != nil {
		return nil, false, fmt.Errorf("coding hooks: open project file %q: %w", filePath, err)
	}
	defer func() { _ = file.Close() }()

	opened, err := file.Stat()
	if err != nil || !os.SameFile(info, opened) {
		return nil, false, fmt.Errorf("%w: project hooks file changed while opening", ErrUnsafeFile)
	}
	data, err := readBounded(file, maximum)
	if err != nil {
		return nil, false, fmt.Errorf("coding hooks: read project file %q: %w", filePath, err)
	}

	return data, true, nil
}

func readBounded(reader io.Reader, maximum int64) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(reader, maximum+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > maximum {
		return nil, ErrLimitExceeded
	}

	return data, nil
}
