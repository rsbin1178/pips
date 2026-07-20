//go:build linux && arm64

package execution

import "golang.org/x/sys/unix"

const nativeLinuxAuditArch = uint32(unix.AUDIT_ARCH_AARCH64)

func linuxABICompatibilityGuard() []unix.SockFilter { return nil }
