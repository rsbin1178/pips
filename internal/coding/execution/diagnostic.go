//nolint:wsl_v5 // Diagnostic parsing is one bounded validation boundary.
package execution

import (
	"errors"
	"os"
	"strings"
	"syscall"
)

const (
	sandboxErrnoEPERM  = "EPERM"
	sandboxErrnoEACCES = "EACCES"
	sandboxErrnoENOENT = "ENOENT"
	sandboxErrnoEROFS  = "EROFS"
)

// SandboxDiagnostic is bounded machine-readable evidence for a filesystem
// denial. Path is present only when the operating system or child runtime
// reported an exact path; callers must keep it out of durable lifecycle
// projections.
type SandboxDiagnostic struct {
	Errno     string `json:"errno"`
	Operation string `json:"operation"`
	Path      string `json:"path,omitempty"`
	Backend   string `json:"backend"`
	Phase     string `json:"phase"`
}

// SandboxDiagnosticFromError classifies a wrapped OS permission error without
// exposing arbitrary error text. It is used for pre-launch sandbox failures.
func SandboxDiagnosticFromError(err error, backend, phase string) (SandboxDiagnostic, bool) {
	if err == nil {
		return SandboxDiagnostic{}, false
	}

	var pathErr *os.PathError
	if !errors.As(err, &pathErr) {
		return SandboxDiagnostic{}, false
	}

	errno := sandboxErrno(pathErr.Err)
	if errno == "" {
		return SandboxDiagnostic{}, false
	}

	return SandboxDiagnostic{
		Errno:     errno,
		Operation: boundedDiagnosticField(pathErr.Op, 64),
		Path:      boundedDiagnosticField(pathErr.Path, 4096),
		Backend:   boundedDiagnosticField(backend, 64),
		Phase:     boundedDiagnosticField(phase, 64),
	}, true
}

// SandboxDiagnosticFromOutput recognizes the common errno/operation/path form
// emitted by Node, npm, and similar runtimes, for example:
// `EPERM: operation not permitted, mkdir '/tmp/tsx-501'`.
//
//nolint:wsl_v5 // Parsing a bounded diagnostic line keeps all validation together.
func SandboxDiagnosticFromOutput(output, backend, phase string) (SandboxDiagnostic, bool) {
	for line := range strings.SplitSeq(output, "\n") {
		for _, errno := range []string{
			sandboxErrnoEPERM, sandboxErrnoEACCES, sandboxErrnoENOENT, sandboxErrnoEROFS,
		} {
			index := strings.Index(line, errno+":")
			if index < 0 {
				continue
			}

			remainder := strings.TrimSpace(line[index+len(errno)+1:])
			_, operationAndPath, found := strings.Cut(remainder, ",")
			if !found {
				continue
			}

			operationAndPath = strings.TrimSpace(operationAndPath)
			fields := strings.Fields(operationAndPath)
			if len(fields) == 0 {
				continue
			}
			operation := strings.TrimSuffix(fields[0], ":")
			path := quotedDiagnosticPath(operationAndPath)
			return SandboxDiagnostic{
				Errno:     errno,
				Operation: boundedDiagnosticField(operation, 64),
				Path:      boundedDiagnosticField(path, 4096),
				Backend:   boundedDiagnosticField(backend, 64),
				Phase:     boundedDiagnosticField(phase, 64),
			}, true
		}
	}

	return SandboxDiagnostic{}, false
}

func sandboxErrno(err error) string {
	switch {
	case errors.Is(err, syscall.EPERM):
		return sandboxErrnoEPERM
	case errors.Is(err, syscall.EACCES):
		return sandboxErrnoEACCES
	case errors.Is(err, syscall.ENOENT):
		return sandboxErrnoENOENT
	case errors.Is(err, syscall.EROFS):
		return sandboxErrnoEROFS
	default:
		return ""
	}
}

func quotedDiagnosticPath(value string) string {
	start := strings.IndexByte(value, '\'')
	if start < 0 {
		return ""
	}
	end := strings.IndexByte(value[start+1:], '\'')
	if end < 0 {
		return ""
	}

	return value[start+1 : start+1+end]
}

func boundedDiagnosticField(value string, maximum int) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	if len(value) <= maximum {
		return value
	}

	return value[:maximum-3] + "…"
}
