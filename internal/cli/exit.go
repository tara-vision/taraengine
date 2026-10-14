package cli

import "errors"

// ExitError ends a command with a status other than 1. An empty Message adds nothing to what the command printed.
type ExitError struct {
	Code    int
	Message string
}

func (e *ExitError) Error() string { return e.Message }

// ExitCode is the status the process ends with after err: 0 for none, the Code of an ExitError, and 1 for any other
// error.
func ExitCode(err error) int {
	var exit *ExitError
	switch {
	case err == nil:
		return 0
	case errors.As(err, &exit):
		return exit.Code
	}
	return 1
}
