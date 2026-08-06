# pips executable-plugin protocol v1 (experimental)

This directory contains the smallest reviewed semantic protocol surface for
pips executable plugins. It is **experimental** and is not a stable wire
compatibility promise yet. The protobuf field layout may change during the
protocol review phase.

The MVP includes:

- core identity and description;
- application protocol and capability declarations;
- configuration and host-selected capability grants;
- readiness, health, and shutdown;
- `tools.v1` declarations;
- cancellable, deadline-aware server-streaming invocation;
- progress, bounded result, and structured failure messages.

The generated Go package is not a cross-version Go ABI. Plugin authors should
use the generated schema through an SDK once the process supervisor phase is
implemented. Internal `agent/*` and `internal/coding/*` types do not cross the
wire boundary.

## Independent version axes

- bootstrap framing and local-channel authentication are not defined here;
- application protocol major/minor is negotiated by `ProtocolRange`;
- capability families have independent `CapabilityVersion` values;
- manifest schema and artifact target belong to installation metadata;
- `PluginIdentity.semantic_version` is the plugin product version;
- `StateSchema` identifies durable state, but migration is deferred.

Tool risk and concurrency fields are annotations only. The application owns
catalog risk, authorization, approval, secret delivery, filesystem/network
policy, process isolation, and OS sandboxing.

## Generation

From the repository root:

```sh
buf lint
buf generate
```

`buf.gen.yaml` uses the local `protoc-gen-go` and `protoc-gen-go-grpc` plugins
with `paths=source_relative`; generated files therefore remain beside
`plugin.proto`.

## Phase 3 bootstrap implementation note

The standalone Phase 3 supervisor currently uses a four-byte big-endian
length-prefixed strict JSON bootstrap frame (version 1) on reserved stdout. The
frame carries transport kind/endpoint, plugin ID, artifact digest, and the
application protocol range. A per-launch token is passed through a restricted
environment entry and sent as `x-pips-plugin-token` gRPC metadata over an
insecure local channel; it is not part of this semantic protobuf package and is
never logged.

The supported transport intent is explicit: literal loopback TCP, Unix-domain
sockets beneath the supervisor-owned temporary root, and Windows named pipes.
Windows named-pipe support uses `go-winio`; it is not silently downgraded to TCP.
Linux/macOS process groups and Windows Job Objects provide process-tree cleanup.
Unix process groups do not contain a child that deliberately creates a new
session, and the supervisor does not claim OS sandboxing, publisher trust, or
capability authorization. Those are application-policy concerns for later
phases.

## Deliberately unresolved decisions

- final bootstrap threat model and local-channel authentication hardening;
- final protobuf field layout and compatibility/breaking-change policy;
- manifest/install record schema and provenance/signature roots;
- exact default resource budgets and host-side enforcement;
- state storage, migration, and rollback transactions;
- whether the first transport uses loopback TCP, Unix sockets, or named pipes.
