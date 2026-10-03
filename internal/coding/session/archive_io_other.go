//go:build !darwin && !linux

package session

import "os"

func openArchiveDirectory(string, string, bool) (*os.File, error) {
	return nil, ErrUnsupportedPlatform
}

func openArchiveFile(*os.File, string) (*os.File, error) {
	return nil, ErrUnsupportedPlatform
}

func createArchiveTemp(*os.File) (*os.File, string, error) {
	return nil, "", ErrUnsupportedPlatform
}

func linkArchiveFile(*os.File, string, string) error {
	return ErrUnsupportedPlatform
}

func removeArchiveFile(*os.File, string) error {
	return ErrUnsupportedPlatform
}
