package cmd

import "errors"

// ErrSyncPaused signals a sync that paused until a later run.
var ErrSyncPaused = errors.New("sync paused")

// ExitCodeSyncPaused carries a paused outcome from the CLI child to the daemon.
const ExitCodeSyncPaused = 75
