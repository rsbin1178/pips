// Package pluginv1 defines the experimental pips executable-plugin MVP
// protocol.
//
// This package is generated from the sibling plugin.proto schema. It is a
// semantic wire contract, not a stable Go binary ABI: plugin authors may use
// any language or Go toolchain that implements the protocol.
//
// Version axes are intentionally independent:
//
//   - the bootstrap protocol belongs to the future local-channel handshake;
//   - the application protocol is negotiated by ProtocolVersion and
//     ProtocolRange;
//   - each capability family has its own CapabilityVersion;
//   - manifest schema and artifact target belong to installation metadata;
//   - PluginIdentity.SemanticVersion is the plugin's product version;
//   - StateSchema identifies host-owned durable state.
//
// This MVP includes core description/configuration/readiness/health/shutdown
// and tools.v1 declarations/invocation. Tool hints are untrusted annotations;
// catalog risk, authorization, approval, and sandbox policy remain owned by
// the application process.
package pluginv1
