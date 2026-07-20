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
| Linux | Rootless Bubblewrap at `/usr/bin/bwrap` or `/bin/bwrap`, plus supported seccomp architecture | A real namespace, mount, PID, network, and seccomp probe must pass. Pips does not install Bubblewrap or change AppArmor/user-namespace settings. |
| WSL2 | Same Bubblewrap contract as Linux | Supported only when WSL2 and the complete native probe pass. Cross-compilation is not evidence of runtime support. |
| WSL1 / native Windows | No P0 backend | Commands fail as unsupported. Use WSL2 after verifying it with `pips doctor`, or run the whole application in a container/VM. |

`pips doctor` validates configuration and the provider-neutral `API_KEY`. In
`workspace-write` mode it then runs the real native Sandbox probe and reports
filesystem, network, and process isolation. In `full-access` mode it reports an
explicit warning instead of presenting an unsandboxed runner as a successful
Sandbox probe.

## Files, credentials, and HOME

The Sandbox keeps the host filesystem read-only except for the Workspace,
private execution temp, and exact directories approved for one Operation. The
Workspace `.git` metadata and known pips configuration/session/credential paths
receive stronger deny/read-only treatment. Child environments are rebuilt from
a small allowlist; `API_KEY`, token/password/secret variables, proxy settings,
agent sockets, dynamic-loader controls, and Shell startup injection variables
are removed.

`HOME` is deliberately preserved and host files remain readable to support Go
and other local toolchains. Known credential paths are denied, but this is not
a complete secrecy boundary for arbitrary files a user stored under HOME. Do
not keep a highly sensitive working environment exposed to an untrusted build
and assume the command Sandbox hides every secret.

Structured file tools remain Workspace-only. The Git Inspector runs only fixed
read-only `rev-parse`, `ls-files`, and private-tree `diff --no-index` operations;
it disables repository-controlled hooks, filters, fsmonitor, textconv, external
diff, pager, prompts, and index writes. Its report attributes only changes made
after its single-use snapshot, preserving pre-existing dirty work.

## Full Access and stronger isolation

`full-access` runs the command directly with host permissions. It must come
from a user config file, environment value, or CLI flag; project configuration,
Bundles, Extensions, and model Tool arguments cannot enable it. Full Access
removes filesystem and network isolation, and process-group cleanup remains
best effort. Use it only for a Workspace and command you trust.

The command Sandbox does not isolate the pips process itself, trusted compiled
Extensions, the model Provider client, or other same-user processes. It also
does not impose hard CPU, memory, PID, or disk quotas. For hostile repositories,
valuable host credentials, remote unattended execution, or stronger resource
governance, run the entire pips process inside a minimally privileged container,
VM, or disposable development environment. Keep `workspace-write` enabled
inside that outer boundary as defense in depth.
