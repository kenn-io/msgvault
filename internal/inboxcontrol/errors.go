package inboxcontrol

import "errors"

// ErrInternal reports unavailable daemon-owned control state without exposing it.
var ErrInternal = errors.New("inbox controller failed")
