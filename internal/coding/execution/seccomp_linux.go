//go:build linux && (amd64 || arm64)

package execution

import (
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"slices"

	"golang.org/x/sys/unix"
)

const (
	seccompDataSyscallOffset = 0
	seccompDataArchOffset    = 4
	x32SyscallBit            = uint32(0x40000000)
)

func createLinuxSeccompFile(privateDir string, networkAny bool) (*os.File, error) {
	filter, err := os.CreateTemp(privateDir, ".pips-seccomp-")
	if err != nil {
		return nil, fmt.Errorf("create seccomp filter: %w", err)
	}

	path := filter.Name()
	cleanup := func(result error) (*os.File, error) {
		return nil, closeAndRemoveSeccompFile(filter, path, result)
	}

	if err := filter.Chmod(0o600); err != nil {
		return cleanup(fmt.Errorf("restrict seccomp filter: %w", err))
	}

	program := buildLinuxSeccompProgram(networkAny)

	content := marshalLinuxSockFilters(program)
	if _, err := filter.Write(content); err != nil {
		return cleanup(fmt.Errorf("write seccomp filter: %w", err))
	}

	if _, err := filter.Seek(0, 0); err != nil {
		return cleanup(fmt.Errorf("rewind seccomp filter: %w", err))
	}

	if err := os.Remove(path); err != nil { //nolint:gosec // The path is returned by the owned file.
		return cleanup(fmt.Errorf("unlink seccomp filter: %w", err))
	}

	return filter, nil
}

func closeAndRemoveSeccompFile(filter *os.File, path string, result error) error {
	closeErr := filter.Close()

	removeErr := os.Remove(path) //nolint:gosec // The path is returned by the owned file.
	if os.IsNotExist(removeErr) {
		removeErr = nil
	}

	return errors.Join(result, closeErr, removeErr)
}

func buildLinuxSeccompProgram(networkAny bool) []unix.SockFilter {
	program := []unix.SockFilter{
		bpfStatement(unix.BPF_LD|unix.BPF_W|unix.BPF_ABS, seccompDataArchOffset),
		bpfJump(unix.BPF_JMP|unix.BPF_JEQ|unix.BPF_K, nativeLinuxAuditArch, 1, 0),
		bpfStatement(unix.BPF_RET|unix.BPF_K, unix.SECCOMP_RET_KILL_PROCESS),
		bpfStatement(unix.BPF_LD|unix.BPF_W|unix.BPF_ABS, seccompDataSyscallOffset),
	}

	program = append(program, linuxABICompatibilityGuard()...)
	for _, syscallNumber := range blockedLinuxSyscalls(networkAny) {
		program = append(program,
			bpfJump(unix.BPF_JMP|unix.BPF_JEQ|unix.BPF_K, syscallNumber, 0, 1),
			bpfStatement(
				unix.BPF_RET|unix.BPF_K,
				unix.SECCOMP_RET_ERRNO|uint32(unix.EPERM),
			),
		)
	}

	return append(program, bpfStatement(unix.BPF_RET|unix.BPF_K, unix.SECCOMP_RET_ALLOW))
}

func blockedLinuxSyscalls(networkAny bool) []uint32 {
	blocked := []uint32{
		unix.SYS_ADD_KEY,
		unix.SYS_BPF,
		unix.SYS_DELETE_MODULE,
		unix.SYS_FANOTIFY_INIT,
		unix.SYS_FINIT_MODULE,
		unix.SYS_FSCONFIG,
		unix.SYS_FSMOUNT,
		unix.SYS_FSOPEN,
		unix.SYS_FSPICK,
		unix.SYS_INIT_MODULE,
		unix.SYS_IO_URING_ENTER,
		unix.SYS_IO_URING_REGISTER,
		unix.SYS_IO_URING_SETUP,
		unix.SYS_KCMP,
		unix.SYS_KEXEC_FILE_LOAD,
		unix.SYS_KEXEC_LOAD,
		unix.SYS_KEYCTL,
		unix.SYS_MOUNT,
		unix.SYS_MOUNT_SETATTR,
		unix.SYS_MOVE_MOUNT,
		unix.SYS_OPEN_BY_HANDLE_AT,
		unix.SYS_OPEN_TREE,
		unix.SYS_PERF_EVENT_OPEN,
		unix.SYS_PIDFD_GETFD,
		unix.SYS_PIVOT_ROOT,
		unix.SYS_PROCESS_VM_READV,
		unix.SYS_PROCESS_VM_WRITEV,
		unix.SYS_PROCESS_MADVISE,
		unix.SYS_PTRACE,
		unix.SYS_REBOOT,
		unix.SYS_REQUEST_KEY,
		unix.SYS_SETNS,
		unix.SYS_SWAPOFF,
		unix.SYS_SWAPON,
		unix.SYS_UMOUNT2,
		unix.SYS_UNSHARE,
		unix.SYS_USERFAULTFD,
	}
	if !networkAny {
		blocked = append(blocked, unix.SYS_SOCKET, unix.SYS_SOCKETPAIR)
	}

	slices.Sort(blocked)

	return slices.Compact(blocked)
}

func marshalLinuxSockFilters(filters []unix.SockFilter) []byte {
	content := make([]byte, len(filters)*unix.SizeofSockFilter)
	for index, instruction := range filters {
		offset := index * unix.SizeofSockFilter
		binary.LittleEndian.PutUint16(content[offset:], instruction.Code)
		content[offset+2] = instruction.Jt
		content[offset+3] = instruction.Jf
		binary.LittleEndian.PutUint32(content[offset+4:], instruction.K)
	}

	return content
}

func bpfStatement(code uint16, value uint32) unix.SockFilter {
	return unix.SockFilter{Code: code, K: value}
}

func bpfJump(code uint16, value uint32, jumpTrue, jumpFalse uint8) unix.SockFilter {
	return unix.SockFilter{Code: code, Jt: jumpTrue, Jf: jumpFalse, K: value}
}
