// Package pluginsupervisor owns the application-side lifecycle of one external
// executable plugin process.
//
// Phase 3 deliberately stops at authenticated bootstrap, gRPC connection, and
// the core readiness check. It does not publish plugin declarations into an
// IntegrationGeneration or construct Agent tools. Later capability adapters
// own that boundary.
//
// Plugin discovery supplies an install-record identity, but Start independently
// hashes the source artifact, copies it into a supervisor-owned 0700 launch root,
// revalidates the source identity/digest, and executes only that private copy.
// It never executes a candidate to inspect metadata. The process receives an
// explicit environment, stdout is a length-prefixed bootstrap channel, and
// stderr is a bounded, sanitized, token-redacted diagnostic stream.
//
// The local channel uses loopback TCP, Unix-domain sockets, or Windows named
// pipes according to the bootstrap frame. A bootstrap token is passed through
// a reserved environment variable and sent as per-RPC metadata; it is never
// included in the bootstrap frame, status, logs, or returned errors. The token
// is local-channel authentication, not artifact provenance or an OS sandbox.
//
// Process-tree cleanup is implemented with a process group on Linux/macOS. On
// Windows the process is created suspended, assigned to a pre-created Job Object
// with KILL_ON_JOB_CLOSE, and then resumed; native Windows runtime coverage is
// required in CI. Termination returns an explicit bounded tree-quiescence result:
// Unix polls process-group liveness after TERM/KILL, while Windows polls the Job
// Object active-process count after TerminateJobObject. The direct child must be
// reaped and the tree quiescent before the controller is closed. The 0700 launch
// root is removed only after its captured filesystem identity still matches; a
// missing or replaced root is retained with ErrCleanupUncertain. The Unix
// process-group boundary does not stop a child that deliberately creates a new
// session. Windows pipe ACL/peer identity, reparse point policy, and native OS
// sandboxing remain application/CI concerns for a later phase. This package does
// not implement Phase 4 adapters or generation publication.
package pluginsupervisor
