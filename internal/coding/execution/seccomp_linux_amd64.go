//go:build linux && amd64

package execution

import "golang.org/x/sys/unix"

const nativeLinuxAuditArch = uint32(unix.AUDIT_ARCH_X86_64)

func linuxABICompatibilityGuard() []unix.SockFilter {
	return []unix.SockFilter{
		bpfJump(unix.BPF_JMP|unix.BPF_JGE|unix.BPF_K, x32SyscallBit, 0, 1),
		bpfStatement(unix.BPF_RET|unix.BPF_K, unix.SECCOMP_RET_ERRNO|uint32(unix.ENOSYS)),
	}
}
