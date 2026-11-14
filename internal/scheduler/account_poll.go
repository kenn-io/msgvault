package scheduler

import "errors"

// ErrAcquisitionIncomplete leaves the previous completion time intact.
var ErrAcquisitionIncomplete = errors.New("scheduled acquisition incomplete")
