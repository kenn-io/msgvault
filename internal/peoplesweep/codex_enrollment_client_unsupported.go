//go:build !linux

package peoplesweep

import (
	"errors"
	"time"
)

func NewCodexEnrollmentClient(string, string, time.Duration) (CodexEnrollment, error) {
	return nil, errors.New("codex enrollment containment is unavailable on this platform")
}
