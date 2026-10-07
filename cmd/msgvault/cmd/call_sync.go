package cmd

import (
	"errors"
	"fmt"
	"io"

	"go.kenn.io/msgvault/internal/callsync"
)

// callResumeFlags repeats the bounds of a limited run in its resume hint.
func callResumeFlags(limit int, after string, full bool) string {
	flags := ""
	if limit > 0 {
		flags += fmt.Sprintf(" --limit %d", limit)
	}
	if after != "" {
		flags += " --after " + after
	} else if full {
		flags += " --full"
	}
	return flags
}

// writeCallSyncSummary prints one source's sync result, its diagnostics, and
// the command that continues a paused or failed run.
func writeCallSyncSummary(out io.Writer, label, command, identifier string, summary *callsync.Summary, failed bool, resumeFlags string) {
	switch {
	case failed:
		_, _ = fmt.Fprintf(out, "\n%s sync failed; later syncs retry calls that errored while they're within seven days; an older call that failed during a first sync or --full needs --full.\n", label)
	case summary.DiscoveryPending:
		_, _ = fmt.Fprintf(out, "\n%s sync paused with more calls to discover.\n", label)
	default:
		_, _ = fmt.Fprintf(out, "\n%s sync complete!\n", label)
	}
	_, _ = fmt.Fprintf(out, "  Meetings processed: %d\n", summary.MeetingsProcessed)
	_, _ = fmt.Fprintf(out, "  Meetings added:     %d\n", summary.MeetingsAdded)
	_, _ = fmt.Fprintf(out, "  Meetings updated:   %d\n", summary.MeetingsUpdated)
	_, _ = fmt.Fprintf(out, "  Recordings stored: %d\n", summary.AttachmentsStored)
	for _, diagnostic := range summary.Diagnostics {
		_, _ = fmt.Fprintln(out, "  "+diagnostic)
	}
	if summary.DiscoveryPending || failed {
		_, _ = fmt.Fprintf(out, "Run: msgvault %s %s%s\n", command, identifier, resumeFlags)
	}
}

func accumulateCallWrites(total, current *callsync.Summary) {
	if total == nil || current == nil {
		return
	}
	total.MeetingsAdded += current.MeetingsAdded
	total.MeetingsUpdated += current.MeetingsUpdated
	total.AttachmentsStored += current.AttachmentsStored
}

// finishCallSync refreshes caches after a successful sync, or after a failed
// one that still wrote meetings or audio.
func finishCallSync(syncErr error, written *callsync.Summary, refresh func() error) error {
	if syncErr != nil && (written == nil || written.MeetingsAdded+written.MeetingsUpdated+written.AttachmentsStored == 0) {
		return syncErr
	}
	return errors.Join(syncErr, refresh())
}
