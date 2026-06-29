package calsync

import "errors"

// ErrSyncTokenExpired is the Calendar analogue of sync.ErrHistoryExpired: the
// stored syncToken is no longer valid (HTTP 410) and a full re-sync is required
// for that calendar. Incremental sync handles it internally (clear cursor + full
// resync); the CLI surfaces it as guidance when a manual incremental run hits it.
var ErrSyncTokenExpired = errors.New("calendar sync token expired - run a full sync")
