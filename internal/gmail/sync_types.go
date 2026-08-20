package gmail

import "time"

// SyncProgress reports sync progress to the caller.
type SyncProgress interface {
	// OnStart is called when sync begins.
	OnStart(total int64)

	// OnProgress is called periodically during sync.
	OnProgress(processed, added, skipped int64)

	// OnComplete is called when sync finishes.
	OnComplete(summary *SyncSummary)

	// OnError is called when an error occurs.
	OnError(err error)
}

// SyncSummary contains statistics about a completed sync.
type SyncSummary struct {
	SyncRunID        int64
	StartTime        time.Time
	EndTime          time.Time
	Duration         time.Duration
	MessagesFound    int64
	MessagesAdded    int64
	MessagesUpdated  int64
	MessagesSkipped  int64
	BytesDownloaded  int64
	Errors           int64
	FinalHistoryID   uint64
	WasResumed       bool
	ResumedFromToken string
}

// SyncProgressWithDate is an optional extension of SyncProgress
// that provides message date info for better progress context.
type SyncProgressWithDate interface {
	SyncProgress
	// OnLatestDate reports the date of the most recently processed message.
	// This helps show where in the mailbox the sync is currently processing.
	OnLatestDate(date time.Time)
}

// NullProgress is a no-op progress reporter.
type NullProgress struct{}

func (NullProgress) OnStart(total int64)                        {}
func (NullProgress) OnProgress(processed, added, skipped int64) {}
func (NullProgress) OnComplete(summary *SyncSummary)            {}
func (NullProgress) OnError(err error)                          {}
func (NullProgress) OnLatestDate(date time.Time)                {}
