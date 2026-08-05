# Coding execution security

The coding application treats model-generated commands and repository content
as untrusted. Its default `workspace-write` mode combines four separate
controls:

1. a normalized Operation bound to the Workspace, executable, arguments,
   environment, paths, network request, timeout, and output limits;
2. an Approval policy that permits the Workspace-only baseline and asks for an
   exact external-write or network expansion;
3. a native OS Sandbox that enforces the resulting plan;
4. a durable approval journal that never automatically retries an operation
   whose outcome became unknown after process start.

There is no bare-command fallback. If capability probing, plan compilation,
resource validation, or journal persistence fails, the command does not start.
The Shell handler is not directly registerable as an Agent Tool; only the
approval Controller can expose its guarded wrapper.

## Platform requirements

| Host | Runtime | Support contract |
| --- | --- | --- |
| macOS | System `/usr/bin/sandbox-exec` (Seatbelt) | A real read/write/network capability probe must pass. Seatbelt is deprecated by Apple, so it remains a replaceable backend rather than an application contract. Process-tree cleanup is weaker than a PID namespace. |
| Linux | Non-setuid Bubblewrap 0.8.0 or newer at `/usr/bin/bwrap` or `/bin/bwrap`, plus supported seccomp architecture | The version gate provides an early diagnostic, then a real namespace, mount, PID, network, filesystem, and seccomp probe must pass. Pips does not install Bubblewrap or change container, SELinux/AppArmor, or user-namespace settings. |
| WSL2 | Same Bubblewrap contract as Linux | Conditional, not part of the formal P0 matrix. A release may claim WSL2 support only after the complete native smoke passes in WSL2; cross-compilation is not evidence. |
| WSL1 / native Windows | No P0 backend | Commands fail as unsupported. Use a verified WSL2 environment conditionally, or run the whole application in a container/VM. |

The release-blocking P0 matrix is macOS and native Linux on amd64/arm64. A
successful `pips doctor` proves the current machine's capability but does not by
itself replace the release's native platform-runner evidence.

Linux support is capability-based, not distribution-name-based. CentOS 7 and
other older hosts are supported only when an administrator supplies a trusted
Bubblewrap 0.8.0-or-newer system binary and the complete probe succeeds. A
stock package version, a successful `bwrap --version`, or an
`ID_LIKE=centos` value is not sufficient. In particular, an outer container
must allow the user, mount, PID, and network namespaces and the private `/proc`
mount required by the probe. Pips fails closed if any layer blocks them; it
does not fall back to a weaker Sandbox or automatically modify the host.
Pips explicitly requests the initial user namespace before disabling nested
user namespaces, so the same contract applies when Pips itself runs as root or
as an unprivileged user.

`pips doctor` validates configuration and the provider-neutral `API_KEY`. In
`workspace-write` mode it then runs the real native Sandbox probe and reports
the platform, Sandbox runtime/version, and filesystem, network, and process
isolation. In `full-access` mode it reports an explicit warning instead of
presenting an unsandboxed runner as a successful Sandbox probe.

## Files, credentials, and HOME

The Sandbox keeps the host filesystem read-only except for the Workspace,
a canonical owner-only per-runtime private execution root, and exact
directories approved for one Operation. Production scratch roots live outside
`PIPS_HOME`; legacy explicitly supplied nested roots are materialized through a
narrow backend-specific private alias and never make the protected product
root writable. The Workspace `.git` metadata and known pips
configuration/session/credential paths receive stronger deny/read-only
treatment. Child environments are rebuilt from a small allowlist; `TMP`,
`TEMP`, `TMPDIR`, `XDG_CACHE_HOME`, `GOCACHE`, `GOTMPDIR`, npm cache, and npm
log paths are forced below the Plan root. `API_KEY`, token/password/secret
variables, proxy settings, agent sockets, dynamic-loader controls, and Shell
startup injection variables are removed.

NetworkNone still denies host TCP/UDP and Unix sockets outside the private
Plan root; local Unix-domain IPC below that root is allowed for runtimes such
as tsx. `HOME` is deliberately preserved and host files remain readable to
support Go and other local toolchains. Known credential paths are denied, but
this is not
a complete secrecy boundary for arbitrary files a user stored under HOME. Do
not keep a highly sensitive working environment exposed to an untrusted build
and assume the command Sandbox hides every secret.

Structured file tools remain Workspace-only. The Git Inspector runs only fixed
read-only `rev-parse`, `ls-files`, and private-tree `diff --no-index` operations;
it disables repository-controlled hooks, filters, fsmonitor, textconv, external
diff, pager, prompts, and index writes. Its report attributes only changes made
after its single-use snapshot, preserving pre-existing dirty work.

## Full Access and stronger isolation

`full-access` runs the command directly with host permissions. At startup it
must come from a user config file, environment value, or CLI flag; during an
interactive session it may also be selected only after explicit TUI confirmation
through the Controller's one-shot `session_override` capability. Project
configuration, Bundles, Extensions, and model Tool arguments cannot enable it.
Full Access removes filesystem and network isolation, and process-group cleanup
remains best effort. `/permissions` can switch to or from Full Access at an
idle boundary without restarting Pips; the choice remains process-local. Its
normal view uses human-facing capability labels and shows Network as
unrestricted under Full Access; configuration-layer provenance remains an
internal Controller concern. Use Full Access only for a Workspace and command
you trust.

The command Sandbox does not isolate the pips process itself, trusted compiled
Extensions, the model Provider client, or other same-user processes. It also
does not impose hard CPU, memory, PID, or disk quotas. For hostile repositories,
valuable host credentials, remote unattended execution, or stronger resource
governance, run the entire pips process inside a minimally privileged container,
VM, or disposable development environment. Keep `workspace-write` enabled
inside that outer boundary as defense in depth.

## Remote SSH image bridge

`pips ssh` delegates authentication, host keys, transport, and user SSH
configuration to the fixed root-owned `/usr/bin/ssh` or `/bin/ssh`. Pips passes
separate validated argv, forces a private non-persistent ControlMaster,
disables agent/X11/port forwarding and local commands, and does not accept SSH
options from its CLI. Its child environment is a small authentication and
terminal allowlist; provider keys, Pips configuration paths, proxy variables,
and secret-shaped ambient variables are absent. `SSH_AUTH_SOCK` may reach the
local OpenSSH client, but `ForwardAgent=no` prevents forwarding it. Explicit
environment literals configured by the user with OpenSSH `SetEnv` remain the
user's SSH policy and must not contain Pips provider credentials.

Each invocation creates a fresh 256-bit nonce. Dynamic remote values use
canonical base64url or a bounded destination alphabet before OpenSSH joins the
fixed remote command. The remote side requires the exact same Pips version and
creates one mode-0600 Unix socket in a same-UID mode-0700 directory under
`/tmp`. Both connector and listener verify type, ownership, mode, inode, and
peer credentials. The sequential receiver accepts only one strict frame with
fixed magic/version/type, bounded length, absolute deadline, SHA-256 digest,
and EOF; malformed or trailing data is rejected. Its image inbox has capacity
four and never persists payload bytes.

This channel only moves a local clipboard image into a live remote draft. The
image reaches the configured model provider only if the user later submits a
message containing it. The remote cannot request a local clipboard read, and
there is no reverse forward, generic file transfer, durable service, or remote
path supplied by the image frame. A same-user adversary remains within the
same OS authority; use separate accounts or an outer container/VM when that is
not an acceptable trust boundary.
