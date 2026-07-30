# CentOS 7 SSH image bridge checklist

This is the native real-host acceptance checklist for `pips ssh`. Run it on
the actual CentOS 7 target; a Linux cross-build or an upstream Bubblewrap test
does not replace these checks.

## 1. Verify both installations

On the local and remote hosts, verify that Pips and the fixed OpenSSH binary
are available:

```sh
pips version
test -x /usr/bin/ssh -o -x /bin/ssh
stat -c '%A %U:%G %n' /usr/bin/ssh 2>/dev/null || \
  stat -c '%A %U:%G %n' /bin/ssh
```

The complete `pips version` output must match on both hosts. The SSH executable
must be a root-owned regular executable and must not be group/world writable.
Confirm the remote non-interactive command path and version from the local
host:

```sh
ssh dev-host 'command -v pips && pips version'
```

If this cannot find `pips`, configure the remote SSH command PATH; do not add a
client-provided remote command override.

## 2. Verify remote Pips ownership

The remote account must have its own mode-0700 `~/.pips`, model configuration,
and provider-neutral `API_KEY` available to non-interactive SSH commands:

```sh
ssh dev-host 'stat -c "%a %U:%G %n" "$HOME/.pips"; pips doctor'
```

Use the remote host's normal secret manager or protected shell/session setup.
Do not place the remote provider key in `~/.ssh/config` `SetEnv`, a Pips SSH
argument, or a command line. A local `API_KEY` is intentionally not forwarded.

For `workspace-write`, the remote CentOS 7 host must also pass the real native
Sandbox probe. The previously installed non-setuid Bubblewrap 0.11.2 is valid
only when all namespace, mount, PID, network, filesystem, and seccomp checks in
`pips doctor` pass. An isolated upstream overlay test may fail with
`Stale file handle` on the host filesystem without invalidating unrelated
Bubblewrap functions, but Pips' own complete probe is the support gate.

## 3. Exercise the interactive bridge

Start from a local real terminal:

```sh
pips ssh dev-host --workspace /root/server/temp
```

Verify all of the following:

1. OpenSSH performs the expected host-key/authentication flow, then the remote
   Pips trust prompt names `/root/server/temp`.
2. Resizing the local terminal immediately resizes and redraws the remote TUI.
3. Copy a PNG or JPEG locally and press Ctrl+V once. The remote draft gains an
   `[Image #1 · clipboard.png]` reference without creating a remote image file.
4. Submit a message that uses the image and confirm the remote model answers.
5. Press Ctrl+V repeatedly while one read/upload is pending. Work remains
   bounded and excess requests show only the fixed pending/failure notice.
6. Paste ordinary multiline text. It remains bracketed text and does not
   trigger clipboard image reads for embedded byte `0x16`.
7. Exit the remote TUI normally, then repeat and interrupt once with SIGINT.
   The local terminal echo/mode and remote exit status are restored correctly.

## 4. Check ephemeral cleanup

After all Pips SSH sessions for the account have exited, inspect—not blindly
delete—the private paths:

```sh
find /tmp -maxdepth 1 -user "$(id -u)" -name 'pips-ssh-*' -ls
ssh dev-host 'find "/tmp/pips-bridge-$(id -u)" -maxdepth 1 -user "$(id -u)" -ls 2>/dev/null || true'
```

There should be no socket from the completed invocation. A shared empty remote
UID directory may be absent or may remain owned mode 0700 while another live
Pips SSH session uses it. If an unexpected object remains, record its exact
type, owner, mode, and inode before changing anything.

## 5. Ordinary fallback

Confirm that normal SSH remains available independently of the bridge:

```sh
ssh -tt dev-host 'cd /root/server/temp && exec pips'
```

This fallback validates remote Pips/TUI operation but intentionally has no
local clipboard image bridge.
