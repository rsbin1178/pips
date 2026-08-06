//nolint:wsl_v5 // Table fixtures keep validation inputs adjacent to assertions.
package pluginv1_test

import (
	"errors"
	"testing"

	pluginv1 "github.com/rsbin/pips/agent/plugin/v1"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"
)

func TestValidatePluginDescription(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		value   *pluginv1.DescribeResponse
		wantErr bool
	}{
		{
			name:    "valid",
			value:   validDescription(),
			wantErr: false,
		},
		{
			name: "duplicate capability",
			value: func() *pluginv1.DescribeResponse {
				value := validDescription()
				value.Capabilities = append(value.Capabilities, value.Capabilities[0])
				return value
			}(),
			wantErr: true,
		},
		{
			name: "inverted protocol range",
			value: func() *pluginv1.DescribeResponse {
				value := validDescription()
				value.ApplicationProtocol.MinMinor = 3
				value.ApplicationProtocol.MaxMinor = 2
				return value
			}(),
			wantErr: true,
		},
		{
			name: "missing identity",
			value: func() *pluginv1.DescribeResponse {
				value := validDescription()
				value.Identity = nil
				return value
			}(),
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			err := pluginv1.ValidatePluginDescription(tt.value)
			if (err != nil) != tt.wantErr {
				t.Fatalf("ValidatePluginDescription() error = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr && !errors.Is(err, pluginv1.ErrInvalid) {
				t.Fatalf("error = %v, want errors.Is(ErrInvalid)", err)
			}
		})
	}
}

func TestValidateToolDeclarations(t *testing.T) {
	t.Parallel()

	validTool := &pluginv1.ToolDescriptor{
		Id:              "review",
		Name:            "review",
		Description:     "Review a change.",
		InputSchemaJson: []byte(`{"type":"object","properties":{}}`),
	}

	if err := pluginv1.ValidateToolList(&pluginv1.ListToolsResponse{Tools: []*pluginv1.ToolDescriptor{validTool}}); err != nil {
		t.Fatalf("valid tool list: %v", err)
	}

	tests := []struct {
		name  string
		value *pluginv1.ListToolsResponse
	}{
		{
			name: "duplicate id",
			value: func() *pluginv1.ListToolsResponse {
				cloned, ok := proto.Clone(validTool).(*pluginv1.ToolDescriptor)
				if !ok {
					panic("tool clone has unexpected type")
				}
				return &pluginv1.ListToolsResponse{Tools: []*pluginv1.ToolDescriptor{validTool, cloned}}
			}(),
		},
		{
			name: "non object schema",
			value: &pluginv1.ListToolsResponse{Tools: []*pluginv1.ToolDescriptor{
				{
					Id:              "review",
					Name:            "review",
					InputSchemaJson: []byte(`[]`),
				},
			}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if err := pluginv1.ValidateToolList(tt.value); err == nil {
				t.Fatal("ValidateToolList() error = nil, want error")
			}
		})
	}
}

func TestToolStreamValidator(t *testing.T) {
	t.Parallel()

	var stream pluginv1.ToolStreamValidator
	if err := stream.Validate(&pluginv1.ToolEvent{
		Sequence: 1,
		Payload: &pluginv1.InvokeToolResponse_Progress{Progress: &pluginv1.ToolProgress{
			Message:       "working",
			PercentMillis: 50000,
		}},
	}); err != nil {
		t.Fatalf("progress: %v", err)
	}
	if err := stream.Validate(&pluginv1.ToolEvent{
		Sequence: 2,
		Payload: &pluginv1.InvokeToolResponse_Result{Result: &pluginv1.ToolResult{
			Content: []*pluginv1.ToolContent{{MediaType: "text/plain", Data: []byte("done")}},
		}},
	}); err != nil {
		t.Fatalf("result: %v", err)
	}
	if err := stream.Done(); err != nil {
		t.Fatalf("Done(): %v", err)
	}
	if err := stream.Validate(&pluginv1.ToolEvent{
		Sequence: 3,
		Payload:  &pluginv1.InvokeToolResponse_Progress{Progress: &pluginv1.ToolProgress{}},
	}); err == nil {
		t.Fatal("event after terminal accepted")
	}

	var incomplete pluginv1.ToolStreamValidator
	if err := incomplete.Done(); err == nil {
		t.Fatal("incomplete stream accepted")
	}
}

func TestValidateFailureAndInvocation(t *testing.T) {
	t.Parallel()

	if err := pluginv1.ValidateToolEvent(&pluginv1.ToolEvent{
		Sequence: 1,
		Payload: &pluginv1.InvokeToolResponse_Failure{Failure: &pluginv1.ToolFailure{
			Code:    pluginv1.FailureCode_FAILURE_CODE_INVALID_ARGUMENT,
			Message: "invalid input",
		}},
	}); err != nil {
		t.Fatalf("structured failure: %v", err)
	}

	if err := pluginv1.ValidateToolEvent(&pluginv1.ToolEvent{
		Sequence: 1,
		Payload:  &pluginv1.InvokeToolResponse_Failure{Failure: &pluginv1.ToolFailure{}},
	}); err == nil {
		t.Fatal("unspecified failure code accepted")
	}
	if err := pluginv1.ValidateToolEvent(&pluginv1.ToolEvent{
		Sequence: 1,
		Payload: &pluginv1.InvokeToolResponse_Failure{Failure: &pluginv1.ToolFailure{
			Code: pluginv1.FailureCode(99),
		}},
	}); err == nil {
		t.Fatal("unknown failure code accepted")
	}

	valid := &pluginv1.InvokeToolRequest{
		ToolId:            "review",
		InvocationId:      "call-1",
		ArgumentsJson:     []byte(`{"path":"README.md"}`),
		DeadlineUnixNanos: 123,
	}
	if err := pluginv1.ValidateInvokeToolRequest(valid); err != nil {
		t.Fatalf("valid invocation: %v", err)
	}
	valid.ArgumentsJson = []byte(`[]`)
	if err := pluginv1.ValidateInvokeToolRequest(valid); err == nil {
		t.Fatal("array arguments accepted")
	}
	valid.ArgumentsJson = []byte(`{"path":"README.md","path":"other.md"}`)
	if err := pluginv1.ValidateInvokeToolRequest(valid); err == nil {
		t.Fatal("duplicate JSON key accepted")
	}
}

func TestValidateSemanticEnums(t *testing.T) {
	t.Parallel()

	if err := pluginv1.ValidateReadyResponse(&pluginv1.ReadyResponse{
		Status: pluginv1.ReadinessStatus_READINESS_STATUS_READY,
	}); err != nil {
		t.Fatalf("valid readiness: %v", err)
	}
	if err := pluginv1.ValidateReadyResponse(&pluginv1.ReadyResponse{
		Status: pluginv1.ReadinessStatus(99),
	}); err == nil {
		t.Fatal("unknown readiness status accepted")
	}
	if err := pluginv1.ValidateHealthResponse(&pluginv1.HealthResponse{
		Status: pluginv1.HealthStatus(99),
	}); err == nil {
		t.Fatal("unknown health status accepted")
	}
	if err := pluginv1.ValidateShutdownRequest(&pluginv1.ShutdownRequest{
		Mode: pluginv1.ShutdownMode(99),
	}); err == nil {
		t.Fatal("unknown shutdown mode accepted")
	}
	if err := pluginv1.ValidateToolHints("hints", &pluginv1.ToolHints{
		Risk: pluginv1.RiskHint(99),
	}); err == nil {
		t.Fatal("unknown tool risk hint accepted")
	}
}

func TestGeneratedServicesAndProtoRoundTrip(t *testing.T) {
	t.Parallel()

	type coreServer struct {
		pluginv1.UnimplementedCoreServiceServer
	}
	type toolsServer struct {
		pluginv1.UnimplementedToolsServiceServer
	}

	grpcServer := grpc.NewServer()
	pluginv1.RegisterCoreServiceServer(grpcServer, coreServer{})
	pluginv1.RegisterToolsServiceServer(grpcServer, toolsServer{})
	grpcServer.Stop()

	original := &pluginv1.InvokeToolResponse{
		Sequence: 1,
		Payload: &pluginv1.InvokeToolResponse_Result{Result: &pluginv1.ToolResult{
			StructuredJson: []byte(`{"ok":true}`),
		}},
	}
	encoded, err := proto.Marshal(original)
	if err != nil {
		t.Fatalf("proto.Marshal() error = %v", err)
	}
	var decoded pluginv1.InvokeToolResponse
	if err := proto.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("proto.Unmarshal() error = %v", err)
	}
	if !proto.Equal(original, &decoded) {
		t.Fatalf("round trip differs: got %v", &decoded)
	}

	var _ pluginv1.CoreServiceServer = coreServer{}
	var _ pluginv1.ToolsServiceServer = toolsServer{}
}

func validDescription() *pluginv1.DescribeResponse {
	return &pluginv1.DescribeResponse{
		Identity: &pluginv1.PluginIdentity{
			Id:              "example.review",
			SemanticVersion: "1.0.0",
			DisplayName:     "Review",
		},
		ApplicationProtocol: &pluginv1.ProtocolRange{Major: 1, MinMinor: 0, MaxMinor: 1},
		Capabilities: []*pluginv1.CapabilityVersion{{
			Name:     pluginv1.ToolsCapabilityName,
			Major:    pluginv1.ToolsCapabilityMajor,
			MinMinor: 0,
			MaxMinor: 1,
		}},
		StateSchemas: []*pluginv1.StateSchema{{Kind: "review", Version: 1}},
		Limits: &pluginv1.ResourceLimits{
			MaxInputBytes:      1 << 20,
			MaxOutputBytes:     8 << 20,
			MaxEventBytes:      1 << 20,
			MaxProgressEvents:  128,
			MaxConcurrentCalls: 4,
		},
	}
}
