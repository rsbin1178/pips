//go:build !darwin && !linux

package gitcontrol

func resolveExecutable(string) (executableIdentity, error) {
	return executableIdentity{}, ErrUnsupported
}

func (executableIdentity) validate() error { return ErrUnsupported }
