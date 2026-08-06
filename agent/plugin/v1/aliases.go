package pluginv1

// ToolEvent is the semantic name used by validation helpers for one streamed
// tools.v1 event. The wire message is named InvokeToolResponse so generated
// gRPC APIs retain the conventional RPC request/response naming.
type ToolEvent = InvokeToolResponse
