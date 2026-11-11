package threads

import (
	"errors"
	"time"
)

var ErrRedirect = errors.New("threads API redirects are not allowed")

type PauseError struct {
	Reason     string
	RetryAfter time.Duration
}

func (e *PauseError) Error() string { return "threads run paused: " + e.Reason }
