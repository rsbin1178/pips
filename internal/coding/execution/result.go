package execution

import "time"

// Status classifies a started process outcome.
type Status uint8

// Supported process outcome classes.
const (
	StatusUnknown Status = iota
	StatusExited
	StatusSignaled
	StatusTimedOut
	StatusCanceled
	StatusOutputLimit
)

// Result is a bounded process outcome without absolute workspace paths.
type Result struct {
	Status   Status
	ExitCode int
	Signal   string
	Duration time.Duration
	CWD      string
	Stdout   StreamResult
	Stderr   StreamResult
}

func emptyResult(cwd string) Result {
	return Result{Status: StatusUnknown, ExitCode: -1, CWD: cwd}
}
