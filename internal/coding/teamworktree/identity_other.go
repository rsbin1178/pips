//go:build !darwin && !linux

package teamworktree

func canonicalDirectory(string) (FileIdentity, error) { return FileIdentity{}, ErrUnsupported }
func fileIdentity(string) (FileIdentity, error)       { return FileIdentity{}, ErrUnsupported }
func sameFileIdentity(FileIdentity) error             { return ErrUnsupported }
