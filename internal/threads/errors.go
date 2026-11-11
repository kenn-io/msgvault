package threads

import "errors"

var ErrRedirect = errors.New("threads API redirects are not allowed")

type PauseError struct {
	Reason string
}

func (e *PauseError) Error() string { return "threads run paused: " + e.Reason }
