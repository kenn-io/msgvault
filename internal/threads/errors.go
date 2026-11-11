package threads

import "errors"

var ErrRedirect = errors.New("threads API redirects are not allowed")

type PauseError struct {
	Reason string
}

func (e *PauseError) Error() string {
	if e.Reason == "attempt_limit" {
		return "Threads connection stopped after too many requests; wait and rerun add-threads"
	}
	return "Threads connection timed out; rerun add-threads"
}
