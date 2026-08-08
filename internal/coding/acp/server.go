package acp

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"slices"
	"strconv"
	"sync"
	"time"
	"unicode/utf8"

	acpsdk "github.com/coder/acp-go-sdk"
	"github.com/rsbin/pips/internal/coding"
	"github.com/rsbin/pips/internal/coding/modelcatalog"
)

const (
	protocolVersion acpsdk.ProtocolVersion = acpsdk.ProtocolVersionNumber
	errorKindKey                           = "kind"
)

// Server owns one ACP connection and its active Runtime sessions.
type Server struct {
	config Config

	mu              sync.Mutex
	sessions        map[string]*session
	opening         int
	openWG          sync.WaitGroup
	initialized     bool
	formElicitation bool
	serving         bool
	closed          bool
	outbound        outbound
	connection      *acpsdk.Connection
	closeDone       chan struct{}
	closeErr        error
}

// New returns an idle ACP v1 server.
func New(config Config) (*Server, error) {
	if config.Limits == (Limits{}) {
		config.Limits = DefaultLimits()
	}

	if config.Logger == nil {
		config.Logger = slog.New(slog.DiscardHandler)
	}

	if err := validateConfig(config); err != nil {
		return nil, err
	}

	return &Server{
		config: config, sessions: make(map[string]*session), closeDone: make(chan struct{}),
	}, nil
}

// Serve starts line-delimited JSON-RPC over the supplied streams and blocks
// until the peer disconnects or ctx is cancelled. Stdout must be passed as
// output without sharing it with application logs.
func (s *Server) Serve(ctx context.Context, input io.Reader, output io.Writer) error {
	if input == nil || output == nil {
		return ErrInvalid
	}

	s.mu.Lock()
	if s.serving || s.closed {
		s.mu.Unlock()
		return ErrClosed
	}

	s.serving = true
	gatedInput := newGatedReader(input)
	connection := acpsdk.NewConnection(s.handle, output, gatedInput)
	// The SDK logs malformed input verbatim. Keep its diagnostics isolated so
	// protocol payloads can never cross the stderr disclosure boundary.
	connection.SetLogger(slog.New(slog.DiscardHandler))
	gatedInput.open()

	s.connection = connection
	s.outbound = connectionOutbound{connection: connection}
	s.mu.Unlock()

	select {
	case <-ctx.Done():
	case <-connection.Done():
	case <-s.closeDone:
	}

	closeErr := s.Close(context.WithoutCancel(ctx))
	if ctx.Err() != nil {
		return errors.Join(ctx.Err(), closeErr)
	}

	return closeErr
}

// Close cancels and closes every active Runtime once.
func (s *Server) Close(ctx context.Context) error {
	if s == nil {
		return nil
	}

	s.mu.Lock()
	if s.closed {
		done := s.closeDone
		closeErr := s.closeErr
		s.mu.Unlock()

		select {
		case <-done:
			return closeErr
		default:
		}

		select {
		case <-done:
			s.mu.Lock()
			closeErr = s.closeErr
			s.mu.Unlock()

			return closeErr
		case <-ctx.Done():
			return ctx.Err()
		}
	}

	s.closed = true

	sessions := make([]*session, 0, len(s.sessions))
	for _, active := range s.sessions {
		sessions = append(sessions, active)
	}

	clear(s.sessions)
	s.mu.Unlock()
	// Open increments the WaitGroup while holding the same mutex used to set
	// closed, so no Add can race with this Wait.
	s.openWG.Wait()

	errorsBySession := make(chan error, len(sessions))

	var wait sync.WaitGroup
	for _, active := range sessions {
		wait.Go(func() {
			if err := active.close(ctx); err != nil {
				errorsBySession <- err
			}
		})
	}

	wait.Wait()
	close(errorsBySession)

	var closeErr error
	for err := range errorsBySession {
		closeErr = errors.Join(closeErr, err)
	}

	s.mu.Lock()
	s.closeErr = closeErr
	close(s.closeDone)
	s.mu.Unlock()

	return closeErr
}

func (s *Server) handle(
	ctx context.Context,
	method string,
	params json.RawMessage,
) (any, *acpsdk.RequestError) {
	if method == acpsdk.AgentMethodInitialize {
		response, err := s.initialize(params)
		if errors.Is(err, errInitializationState) {
			return nil, acpsdk.NewInvalidRequest(map[string]any{errorKindKey: "already_initialized"})
		}

		return response, s.requestError(method, err)
	}

	if err := s.requireInitialized(); err != nil {
		return nil, acpsdk.NewInvalidRequest(map[string]any{errorKindKey: "not_initialized"})
	}

	response, supported, err := s.dispatch(ctx, method, params)
	if !supported {
		return nil, acpsdk.NewMethodNotFound(method)
	}

	return response, s.requestError(method, err)
}

func (s *Server) dispatch(
	ctx context.Context,
	method string,
	params json.RawMessage,
) (any, bool, error) {
	switch method {
	case acpsdk.AgentMethodSessionNew:
		response, err := s.newSession(ctx, params)

		return response, true, err
	case acpsdk.AgentMethodSessionLoad:
		response, err := s.loadSession(ctx, params, true)

		return response, true, err
	case acpsdk.AgentMethodSessionResume:
		response, err := s.loadSession(ctx, params, false)

		return response, true, err
	case acpsdk.AgentMethodSessionList:
		response, err := s.listSessions(ctx, params)

		return response, true, err
	case acpsdk.AgentMethodSessionClose:
		response, err := s.closeSession(ctx, params)

		return response, true, err
	case acpsdk.AgentMethodSessionDelete:
		response, err := s.deleteSession(ctx, params)

		return response, true, err
	case acpsdk.AgentMethodSessionPrompt:
		response, err := s.prompt(ctx, params)

		return response, true, err
	case acpsdk.AgentMethodSessionCancel:
		return nil, true, s.cancelSession(params)
	case acpsdk.AgentMethodSessionSetMode:
		response, err := s.setSessionMode(ctx, params)

		return response, true, err
	case acpsdk.AgentMethodSessionSetConfigOption:
		response, err := s.setSessionConfigOption(ctx, params)

		return response, true, err
	default:
		return nil, false, nil
	}
}

func (s *Server) initialize(params json.RawMessage) (acpsdk.InitializeResponse, error) {
	var request acpsdk.InitializeRequest
	if err := decodeParams(params, &request, "protocolVersion", "clientCapabilities", "clientInfo"); err != nil {
		return acpsdk.InitializeResponse{}, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if s.initialized || s.closed {
		return acpsdk.InitializeResponse{}, errInitializationState
	}

	s.initialized = true
	s.formElicitation = request.ClientCapabilities.Elicitation != nil &&
		request.ClientCapabilities.Elicitation.Form != nil

	configuredInfo := s.config.AgentInfo

	info := acpsdk.Implementation{
		Name: configuredInfo.Name, Version: configuredInfo.Version,
	}
	if configuredInfo.Title != "" {
		info.Title = &configuredInfo.Title
	}

	return acpsdk.InitializeResponse{
		ProtocolVersion: protocolVersion,
		AgentInfo:       &info,
		AuthMethods:     []acpsdk.AuthMethod{},
		AgentCapabilities: acpsdk.AgentCapabilities{
			LoadSession: true,
			PromptCapabilities: acpsdk.PromptCapabilities{
				Image: true, EmbeddedContext: true,
			},
			SessionCapabilities: acpsdk.SessionCapabilities{
				Close:  &acpsdk.SessionCloseCapabilities{},
				Delete: &acpsdk.SessionDeleteCapabilities{},
				List:   &acpsdk.SessionListCapabilities{},
				Resume: &acpsdk.SessionResumeCapabilities{},
			},
		},
	}, nil
}

func (s *Server) newSession(
	ctx context.Context,
	params json.RawMessage,
) (acpsdk.NewSessionResponse, error) {
	var request acpsdk.NewSessionRequest
	if err := decodeParams(params, &request, "cwd", "mcpServers", "additionalDirectories"); err != nil {
		return acpsdk.NewSessionResponse{}, err
	}

	if err := request.Validate(); err != nil {
		return acpsdk.NewSessionResponse{}, ErrInvalid
	}

	active, err := s.open(ctx, "", request.Cwd, request.McpServers, request.AdditionalDirectories)
	if err != nil {
		return acpsdk.NewSessionResponse{}, err
	}

	state := active.controller.Snapshot()

	return acpsdk.NewSessionResponse{
		SessionId:     acpsdk.SessionId(active.id),
		Modes:         sessionModes(state.Mode),
		ConfigOptions: sessionConfigOptions(state, active.controller.Models()),
	}, nil
}

func (s *Server) loadSession(
	ctx context.Context,
	params json.RawMessage,
	replay bool,
) (any, error) {
	if replay {
		var request acpsdk.LoadSessionRequest
		if err := decodeParams(
			params,
			&request,
			"sessionId",
			"cwd",
			"mcpServers",
			"additionalDirectories",
		); err != nil {
			return nil, err
		}

		if err := request.Validate(); err != nil {
			return nil, ErrInvalid
		}

		active, err := s.open(
			ctx,
			string(request.SessionId),
			request.Cwd,
			request.McpServers,
			request.AdditionalDirectories,
		)
		if err != nil {
			return nil, err
		}

		state := active.controller.Snapshot()
		if err := replayTranscript(
			ctx,
			active.outbound,
			active.id,
			state.Transcript,
			state.SyntheticMessages,
		); err != nil {
			s.detachAndClose(ctx, active.id)

			return nil, err
		}

		return acpsdk.LoadSessionResponse{
			Modes:         sessionModes(state.Mode),
			ConfigOptions: sessionConfigOptions(state, active.controller.Models()),
		}, nil
	}

	var request acpsdk.ResumeSessionRequest
	if err := decodeParams(
		params,
		&request,
		"sessionId",
		"cwd",
		"mcpServers",
		"additionalDirectories",
	); err != nil {
		return nil, err
	}

	if err := request.Validate(); err != nil {
		return nil, ErrInvalid
	}

	active, err := s.open(
		ctx,
		string(request.SessionId),
		request.Cwd,
		request.McpServers,
		request.AdditionalDirectories,
	)
	if err != nil {
		return nil, err
	}

	state := active.controller.Snapshot()

	return acpsdk.ResumeSessionResponse{
		Modes:         sessionModes(state.Mode),
		ConfigOptions: sessionConfigOptions(state, active.controller.Models()),
	}, nil
}

func (s *Server) open(
	ctx context.Context,
	id string,
	cwd string,
	servers []acpsdk.McpServer,
	additionalDirectories []string,
) (*session, error) {
	if len(additionalDirectories) != 0 || !validCWD(cwd) || !validSessionID(id, true) {
		return nil, ErrInvalid
	}

	definitions, err := convertMCPServers(servers, s.config.Limits.MaxMCPServers)
	if err != nil {
		return nil, err
	}

	out, formElicitation, err := s.reserveOpen(id)
	if err != nil {
		return nil, err
	}
	defer s.finishOpening()

	controller, err := s.config.Factory.Open(ctx, SessionSpec{
		ID: id, CWD: cwd, MCPDefinitions: definitions,
	})
	if err != nil {
		return nil, err
	}

	active, err := runtimeSession(ctx, id, controller, out, formElicitation)
	if err != nil {
		return nil, err
	}

	if err := s.installSession(active); err != nil {
		_ = active.close(context.WithoutCancel(ctx))

		return nil, err
	}

	return active, nil
}

func (s *Server) reserveOpen(id string) (outbound, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.closed || s.outbound == nil {
		return nil, false, ErrClosed
	}

	if id != "" && s.sessions[id] != nil {
		return nil, false, ErrSessionBusy
	}

	if len(s.sessions)+s.opening >= s.config.Limits.MaxSessions {
		return nil, false, fmt.Errorf("%w: session limit", ErrInvalid)
	}

	s.opening++
	s.openWG.Add(1)

	return s.outbound, s.formElicitation, nil
}

func runtimeSession(
	ctx context.Context,
	requestedID string,
	controller Controller,
	out outbound,
	formElicitation bool,
) (*session, error) {
	actualID := controller.Snapshot().SessionID
	if !validSessionID(actualID, false) || requestedID != "" && actualID != requestedID {
		_ = controller.Close(context.WithoutCancel(ctx))

		return nil, fmt.Errorf("%w: Runtime session identity", ErrInvalid)
	}

	active, err := newSession(actualID, controller, out, formElicitation)
	if err != nil {
		_ = controller.Close(context.WithoutCancel(ctx))

		return nil, err
	}

	return active, nil
}

func (s *Server) installSession(active *session) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.closed || s.sessions[active.id] != nil {
		return ErrSessionBusy
	}

	s.sessions[active.id] = active

	return nil
}

func (s *Server) finishOpening() {
	s.mu.Lock()
	s.opening--
	s.mu.Unlock()
	s.openWG.Done()
}

func (s *Server) listSessions(
	ctx context.Context,
	params json.RawMessage,
) (acpsdk.ListSessionsResponse, error) {
	var request acpsdk.ListSessionsRequest
	if err := decodeParams(params, &request, "cursor", "cwd"); err != nil {
		return acpsdk.ListSessionsResponse{}, err
	}

	if request.Cwd != nil && !validCWD(*request.Cwd) {
		return acpsdk.ListSessionsResponse{}, ErrInvalid
	}

	offset, err := decodeCursor(request.Cursor)
	if err != nil {
		return acpsdk.ListSessionsResponse{}, err
	}

	metadata, err := s.config.Factory.List(ctx)
	if err != nil {
		return acpsdk.ListSessionsResponse{}, err
	}

	filtered := filterSessionMetadata(metadata, request.Cwd)
	if offset > len(filtered) {
		return acpsdk.ListSessionsResponse{}, ErrInvalid
	}

	return s.sessionListPage(filtered, offset), nil
}

func filterSessionMetadata(metadata []SessionMetadata, cwd *string) []SessionMetadata {
	filtered := make([]SessionMetadata, 0, len(metadata))
	for _, item := range metadata {
		if !validSessionID(item.ID, false) || !validCWD(item.CWD) {
			continue
		}

		if cwd == nil || item.CWD == *cwd {
			filtered = append(filtered, item)
		}
	}

	slices.SortFunc(filtered, func(left, right SessionMetadata) int {
		if order := right.UpdatedAt.Compare(left.UpdatedAt); order != 0 {
			return order
		}

		return stringCompare(left.ID, right.ID)
	})

	return filtered
}

func (s *Server) sessionListPage(
	filtered []SessionMetadata,
	offset int,
) acpsdk.ListSessionsResponse {
	end := min(offset+s.config.Limits.ListPageSize, len(filtered))

	items := make([]acpsdk.SessionInfo, 0, end-offset)
	for _, item := range filtered[offset:end] {
		info := acpsdk.SessionInfo{SessionId: acpsdk.SessionId(item.ID), Cwd: item.CWD}
		if item.Title != "" {
			info.Title = new(item.Title)
		}

		if !item.UpdatedAt.IsZero() {
			updatedAt := item.UpdatedAt.UTC().Format(time.RFC3339Nano)
			info.UpdatedAt = &updatedAt
		}

		items = append(items, info)
	}

	response := acpsdk.ListSessionsResponse{Sessions: items}
	if end < len(filtered) {
		response.NextCursor = new(encodeCursor(end))
	}

	return response
}

func (s *Server) closeSession(
	ctx context.Context,
	params json.RawMessage,
) (acpsdk.CloseSessionResponse, error) {
	var request acpsdk.CloseSessionRequest
	if err := decodeParams(params, &request, "sessionId"); err != nil {
		return acpsdk.CloseSessionResponse{}, err
	}

	active, err := s.detach(string(request.SessionId))
	if err != nil {
		return acpsdk.CloseSessionResponse{}, err
	}

	return acpsdk.CloseSessionResponse{}, active.close(ctx)
}

type deleteSessionRequest struct {
	SessionID acpsdk.SessionId `json:"sessionId"`
}

type deleteSessionResponse struct{}

func (s *Server) deleteSession(
	ctx context.Context,
	params json.RawMessage,
) (deleteSessionResponse, error) {
	var request deleteSessionRequest
	if err := decodeParams(params, &request, "sessionId"); err != nil {
		return deleteSessionResponse{}, err
	}

	id := string(request.SessionID)
	if !validSessionID(id, false) {
		return deleteSessionResponse{}, ErrInvalid
	}

	active := s.detachIfPresent(id)
	if active != nil {
		if err := active.close(ctx); err != nil {
			return deleteSessionResponse{}, err
		}
	}

	if err := s.config.Factory.Delete(ctx, id); err != nil {
		return deleteSessionResponse{}, err
	}

	return deleteSessionResponse{}, nil
}

func (s *Server) prompt(
	ctx context.Context,
	params json.RawMessage,
) (acpsdk.PromptResponse, error) {
	var request acpsdk.PromptRequest
	if err := decodeParams(params, &request, "sessionId", "prompt", "messageId"); err != nil {
		return acpsdk.PromptResponse{}, err
	}

	if err := request.Validate(); err != nil {
		return acpsdk.PromptResponse{}, ErrInvalid
	}

	active, err := s.lookup(string(request.SessionId))
	if err != nil {
		return acpsdk.PromptResponse{}, err
	}

	message, err := convertPrompt(request.Prompt, s.config.Limits)
	if err != nil {
		return acpsdk.PromptResponse{}, err
	}

	stop, err := active.prompt(ctx, message)
	if err != nil {
		return acpsdk.PromptResponse{}, err
	}

	return acpsdk.PromptResponse{StopReason: stop, UserMessageId: request.MessageId}, nil
}

func (s *Server) cancelSession(params json.RawMessage) error {
	var request acpsdk.CancelNotification
	if err := decodeParams(params, &request, "sessionId"); err != nil {
		return err
	}

	active, err := s.lookup(string(request.SessionId))
	if err != nil {
		return err
	}

	return active.cancel()
}

func (s *Server) setSessionMode(
	ctx context.Context,
	params json.RawMessage,
) (acpsdk.SetSessionModeResponse, error) {
	var request acpsdk.SetSessionModeRequest
	if err := decodeParams(params, &request, "sessionId", "modeId"); err != nil {
		return acpsdk.SetSessionModeResponse{}, err
	}

	mode := coding.OperatingMode(request.ModeId)
	if mode != coding.ModeAgent && mode != coding.ModePlan {
		return acpsdk.SetSessionModeResponse{}, ErrInvalid
	}

	active, err := s.lookup(string(request.SessionId))
	if err != nil {
		return acpsdk.SetSessionModeResponse{}, err
	}

	if err := active.setMode(ctx, mode); err != nil {
		return acpsdk.SetSessionModeResponse{}, err
	}

	options := active.configOptions()
	for _, update := range modeUpdates(mode, options) {
		if err := active.outbound.update(ctx, request.SessionId, update); err != nil {
			return acpsdk.SetSessionModeResponse{}, err
		}
	}

	return acpsdk.SetSessionModeResponse{}, nil
}

type setSessionConfigOptionRequest struct {
	SessionID acpsdk.SessionId       `json:"sessionId"`
	ConfigID  acpsdk.SessionConfigId `json:"configId"`
	Type      string                 `json:"type,omitempty"`
	Value     json.RawMessage        `json:"value"`
}

func (s *Server) setSessionConfigOption(
	ctx context.Context,
	params json.RawMessage,
) (acpsdk.SetSessionConfigOptionResponse, error) {
	var request setSessionConfigOptionRequest
	if err := decodeParams(params, &request, "sessionId", "configId", "type", "value"); err != nil {
		return acpsdk.SetSessionConfigOptionResponse{}, err
	}

	if request.Type != "" && request.Type != "select" {
		return acpsdk.SetSessionConfigOptionResponse{}, ErrInvalid
	}

	var value string
	if len(request.Value) == 0 || json.Unmarshal(request.Value, &value) != nil {
		return acpsdk.SetSessionConfigOptionResponse{}, ErrInvalid
	}

	active, err := s.lookup(string(request.SessionID))
	if err != nil {
		return acpsdk.SetSessionConfigOptionResponse{}, err
	}

	switch request.ConfigID {
	case sessionModeConfigID:
		return s.setModeConfigOption(
			ctx,
			active,
			request.SessionID,
			value,
		)
	case sessionModelConfigID:
		return s.setModelConfigOption(
			ctx,
			active,
			request.SessionID,
			value,
		)
	default:
		return acpsdk.SetSessionConfigOptionResponse{}, ErrInvalid
	}
}

func (s *Server) setModeConfigOption(
	ctx context.Context,
	active *session,
	sessionID acpsdk.SessionId,
	value string,
) (acpsdk.SetSessionConfigOptionResponse, error) {
	mode := coding.OperatingMode(value)
	if mode != coding.ModeAgent && mode != coding.ModePlan {
		return acpsdk.SetSessionConfigOptionResponse{}, ErrInvalid
	}

	if err := active.setMode(ctx, mode); err != nil {
		return acpsdk.SetSessionConfigOptionResponse{}, err
	}

	options := active.configOptions()
	for _, update := range modeUpdates(mode, options) {
		if err := active.outbound.update(ctx, sessionID, update); err != nil {
			return acpsdk.SetSessionConfigOptionResponse{}, err
		}
	}

	return acpsdk.SetSessionConfigOptionResponse{ConfigOptions: options}, nil
}

func (s *Server) setModelConfigOption(
	ctx context.Context,
	active *session,
	sessionID acpsdk.SessionId,
	value string,
) (acpsdk.SetSessionConfigOptionResponse, error) {
	selection, err := modelSelection(value, active.controller.Models())
	if err != nil {
		return acpsdk.SetSessionConfigOptionResponse{}, err
	}

	if err := active.switchModel(ctx, selection); err != nil {
		return acpsdk.SetSessionConfigOptionResponse{}, err
	}

	options := active.configOptions()
	if err := active.outbound.update(
		ctx,
		sessionID,
		configOptionUpdate(options),
	); err != nil {
		return acpsdk.SetSessionConfigOptionResponse{}, err
	}

	return acpsdk.SetSessionConfigOptionResponse{ConfigOptions: options}, nil
}

func modelSelection(
	value string,
	models []modelcatalog.Entry,
) (modelcatalog.Selection, error) {
	for _, model := range models {
		if model.Ref.String() == value {
			return modelcatalog.Selection{Ref: model.Ref}, nil
		}
	}

	return modelcatalog.Selection{}, ErrInvalid
}

func (s *Server) lookup(id string) (*session, error) {
	if !validSessionID(id, false) {
		return nil, ErrInvalid
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	active := s.sessions[id]
	if active == nil {
		return nil, ErrSessionNotFound
	}

	return active, nil
}

func (s *Server) detach(id string) (*session, error) {
	if !validSessionID(id, false) {
		return nil, ErrInvalid
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	active := s.sessions[id]
	if active == nil {
		return nil, ErrSessionNotFound
	}

	delete(s.sessions, id)

	return active, nil
}

func (s *Server) detachIfPresent(id string) *session {
	s.mu.Lock()
	defer s.mu.Unlock()

	active := s.sessions[id]
	delete(s.sessions, id)

	return active
}

func (s *Server) detachAndClose(ctx context.Context, id string) {
	active, err := s.detach(id)
	if err == nil {
		_ = active.close(context.WithoutCancel(ctx))
	}
}

func (s *Server) requireInitialized() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if !s.initialized || s.closed {
		return ErrInvalid
	}

	return nil
}

func (s *Server) requestError(method string, err error) *acpsdk.RequestError {
	if err == nil {
		return nil
	}

	if errors.Is(err, context.Canceled) {
		return acpsdk.NewRequestCancelled(map[string]any{errorKindKey: "cancelled"})
	}

	for sentinel, kind := range map[error]string{
		ErrInvalid:         "invalid_input",
		ErrSessionNotFound: "session_not_found",
		ErrSessionBusy:     "session_busy",
		ErrClosed:          "closed",
	} {
		if errors.Is(err, sentinel) {
			return acpsdk.NewInvalidParams(map[string]any{errorKindKey: kind})
		}
	}

	s.config.Logger.Error(
		"ACP request failed",
		"method", method,
		"error_type", fmt.Sprintf("%T", err),
	)

	return acpsdk.NewInternalError(map[string]any{errorKindKey: "internal"})
}

type validates interface {
	Validate() error
}

func decodeParams(raw json.RawMessage, target any, allowed ...string) error {
	if len(raw) == 0 || string(raw) == "null" {
		return ErrInvalid
	}

	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil || fields == nil {
		return ErrInvalid
	}

	allowedSet := make(map[string]struct{}, len(allowed)+1)

	allowedSet["_meta"] = struct{}{}
	for _, field := range allowed {
		allowedSet[field] = struct{}{}
	}

	for field := range fields {
		if _, exists := allowedSet[field]; !exists {
			return ErrInvalid
		}
	}

	if err := json.Unmarshal(raw, target); err != nil {
		return ErrInvalid
	}

	if value, ok := target.(validates); ok {
		if err := value.Validate(); err != nil {
			return ErrInvalid
		}
	}

	return nil
}

func validCWD(value string) bool {
	return filepath.IsAbs(value) && filepath.Clean(value) == value &&
		validProtocolText(value, 32<<10, false)
}

func validSessionID(value string, allowEmpty bool) bool {
	return (allowEmpty && value == "") || validProtocolText(value, 512, false)
}

func encodeCursor(offset int) string {
	value := "v1:" + strconv.Itoa(offset)

	return base64.RawURLEncoding.EncodeToString([]byte(value))
}

func decodeCursor(cursor *string) (int, error) {
	if cursor == nil {
		return 0, nil
	}

	if *cursor == "" || len(*cursor) > 128 || !utf8.ValidString(*cursor) {
		return 0, ErrInvalid
	}

	decoded, err := base64.RawURLEncoding.DecodeString(*cursor)
	if err != nil || len(decoded) < 4 || string(decoded[:3]) != "v1:" {
		return 0, ErrInvalid
	}

	offset, err := strconv.Atoi(string(decoded[3:]))
	if err != nil || offset < 0 {
		return 0, ErrInvalid
	}

	return offset, nil
}

func stringCompare(left, right string) int {
	switch {
	case left < right:
		return -1
	case left > right:
		return 1
	default:
		return 0
	}
}

// gatedReader prevents the SDK's receive goroutine from observing Connection
// fields until post-construction configuration is complete. Closing ready
// establishes the required happens-before edge for SetLogger.
type gatedReader struct {
	reader io.Reader
	ready  chan struct{}
	once   sync.Once
}

func newGatedReader(reader io.Reader) *gatedReader {
	return &gatedReader{reader: reader, ready: make(chan struct{})}
}

func (r *gatedReader) Read(buffer []byte) (int, error) {
	<-r.ready

	return r.reader.Read(buffer)
}

func (r *gatedReader) open() {
	r.once.Do(func() { close(r.ready) })
}
