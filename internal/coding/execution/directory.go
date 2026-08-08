package execution

import (
	"errors"
	"os"
)

// ValidateDirectoryWritable verifies that a subprocess running as the current
// client identity can create and remove a file in directory.
func ValidateDirectoryWritable(directory string) error {
	probe, err := os.CreateTemp(directory, ".pips-write-check-")
	if err != nil {
		return errors.New("coding execution: directory is not writable")
	}

	name := probe.Name()
	closeErr := probe.Close()

	removeErr := os.Remove(name)
	if closeErr != nil || removeErr != nil {
		return errors.New("coding execution: directory writability check could not be cleaned up")
	}

	return nil
}
