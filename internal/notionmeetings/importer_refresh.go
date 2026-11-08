package notionmeetings

import (
	"context"
	"encoding/json/v2"
	"errors"
	"maps"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/meetingarchive"
	"go.kenn.io/msgvault/internal/store"
)

// processMeeting never reads sections before matching the refreshed header.
// Its caller removes an admission only after this returns successfully.
func (imp *Importer) processMeeting(ctx context.Context, scoped *store.Store, state *syncState, sum *ImportSummary, opts ImportOptions, id string, progress func(string), hydrator *Hydrator) (bool, error) {
	now := imp.now().UTC()
	stamp := now.Format(time.RFC3339)
	known, exists := state.Known[id]
	known.LastAttemptAt = stamp
	block, err := imp.client.RetrieveBlock(ctx, id)
	if err != nil {
		if errors.Is(err, ErrUnauthorized) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, ErrRequestBudget) {
			sum.Errors++
			return false, hydrationFailure("retrieve Notion meeting header", err)
		}
		sum.Errors++
		reason := errorCategory(err)
		state.reason(reason, "meeting")
		if exists {
			known.FetchError = reason
			state.Known[id] = known
			state.deferTranscriptAttempt(id, now)
		} else {
			state.deferTask(scopeTask{Kind: "meeting", ID: id}, reason, now)
		}
		progress("Notion meeting: fetch deferred")
		return false, nil
	}
	var meeting MeetingNote
	if block == nil || json.Unmarshal(block.Raw, &meeting) != nil || meeting.ID != id || meeting.Type != meetingNotesType {
		sum.Errors++
		state.reason("malformed_response", "meeting")
		if exists {
			known.FetchError = "malformed_response"
			state.Known[id] = known
			state.deferTranscriptAttempt(id, now)
		} else {
			state.deferTask(scopeTask{Kind: "meeting", ID: id}, "malformed_response", now)
		}
		return false, nil
	}
	if !matchesHuman(meeting, state.Identity.UserID) {
		if exists {
			known.FetchError = "not_selected"
			state.Known[id] = known
			state.deferTranscriptAttempt(id, now)
		}
		return false, nil
	}
	if !exists && !opts.CreatedAfter.IsZero() && candidateTime(meeting).Before(opts.CreatedAfter) {
		return false, nil
	}
	sum.MeetingsProcessed++
	if meeting.InTrash || meeting.Archived {
		known.FetchError = "archived_or_trash"
		state.Known[id] = known
		state.reason("archived_or_trash", "meeting")
		state.deferTranscriptAttempt(id, now)
		return true, nil
	}
	hydrated, err := hydrator.HydrateBlock(ctx, block)
	if err != nil {
		sum.Errors++
		if errors.Is(err, ErrUnauthorized) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, ErrRequestBudget) {
			return false, hydrationFailure("hydrate Notion meeting", err)
		}
		reason := errorCategory(err)
		state.reason(reason, "meeting")
		if exists {
			known.FetchError = reason
			state.Known[id] = known
			state.deferTranscriptAttempt(id, now)
		} else {
			state.deferTask(scopeTask{Kind: "meeting", ID: id}, reason, now)
		}
		return true, nil
	}
	providerTranscriptIncomplete := strings.TrimSpace(hydrated.Transcript) == "" || !hydrated.TranscriptTree.Complete || hydrated.TranscriptTree.Filtered
	archived, err := imp.archivedState(ctx, sum.SourceID, id)
	if err != nil {
		sum.Errors++
		return false, hydrationFailure("recover archived Notion evidence", err)
	}
	keepArchive := preserveArchivedSections(hydrated, archived)
	if hydrated.AttendeeResolutionDegraded {
		state.reason("users_unavailable", "users")
		preserveArchivedAttendees(hydrated, archived.ResolvedUsers)
		if archived.MessageID != 0 {
			recipients, err := imp.store.GetMessageRecipientsContext(ctx, archived.MessageID, "to")
			if err != nil {
				sum.Errors++
				return false, hydrationFailure("recover archived Notion attendees", err)
			}
			preserveArchivedAttendeeRelationships(hydrated, recipients, archived.ResolvedUsers)
		}
	}
	if keepArchive {
		known.FetchError = "legacy_unscoped"
		state.Known[id] = known
		state.reason("legacy_unscoped", "meeting")
		state.updateTranscriptPending(id, hydrated, providerTranscriptIncomplete, now)
		progress("Archived meeting retained unchanged; current sections are incomplete")
		return true, nil
	}
	known.LastEditedTime = meeting.LastEditedTime
	known.FetchError = ""
	known.SectionFreshness = maps.Clone(known.SectionFreshness)
	if known.SectionFreshness == nil {
		known.SectionFreshness = map[string]string{}
	}
	for name, section := range map[string]blockTree{"summary": hydrated.SummaryTree, "notes": hydrated.NotesTree, "transcript": hydrated.TranscriptTree} {
		sectionIDs := meeting.MeetingNotes.Children
		if !section.Preserved && ((name == "summary" && sectionIDs.SummaryBlockID == "") || (name == "notes" && sectionIDs.NotesBlockID == "") || (name == "transcript" && sectionIDs.TranscriptBlockID == "")) {
			continue
		}
		if section.Complete && !section.Filtered && !section.Preserved {
			known.SectionFreshness[name] = stamp
		} else {
			known.FetchError = "incomplete_sections"
			state.reason("incomplete_sections", name)
		}
	}
	snapshot, err := hydrated.ArchiveSnapshot(sum.SourceID, opts.Identifier, opts.AccountEmail)
	if err != nil {
		sum.Errors++
		if errors.Is(err, errArchiveEvidenceLimit) {
			state.reason("evidence_limit", "meeting")
			if exists {
				previous := state.Known[id]
				previous.LastAttemptAt = stamp
				previous.FetchError = "evidence_limit"
				state.Known[id] = previous
				state.deferTranscriptAttempt(id, now)
			} else {
				state.deferTask(scopeTask{Kind: "meeting", ID: id}, "evidence_limit", now)
			}
			return true, nil
		}
		return false, hydrationFailure("build Notion archive snapshot", err)
	}
	checksum := checksumBytes(snapshot.Raw)
	archiver := meetingarchive.New(scoped)
	if archived.HasEvidence && exists && !opts.Full && known.SnapshotVersion == notionSnapshotVersion && known.SnapshotSHA256 == checksum {
		if _, err := archiver.LinkIdentities(ctx, sum.SourceID, snapshot.Attendees); err != nil {
			sum.Errors++
			return false, hydrationFailure("link Notion attendee identities", err)
		}
		progress("verified Notion meeting")
	} else {
		result, err := archiver.Upsert(ctx, snapshot, meetingarchive.UpsertOptions{Force: opts.Full})
		if result.Created {
			sum.MeetingsAdded++
		} else if result.Changed {
			sum.MeetingsUpdated++
		}
		if err != nil {
			sum.Errors++
			return false, hydrationFailure("archive Notion meeting", err)
		}
		progress("imported Notion meeting")
	}
	// Archive publication precedes both transcript-pending updates and admission
	// removal; a cancellation here safely replays the same stable message ID.
	known.SnapshotSHA256 = checksum
	known.SnapshotVersion = notionSnapshotVersion
	known.LastSuccessAt = stamp
	state.Known[id] = known
	state.updateTranscriptPending(id, hydrated, providerTranscriptIncomplete, now)
	return true, nil
}
func (s *syncState) updateTranscriptPending(id string, hydrated *HydratedMeeting, incomplete bool, now time.Time) {
	index := -1
	previous := pendingTranscript{}
	hasPrevious := false
	for i, pending := range s.Pending {
		if pending.BlockID == id {
			index = i
			previous = pending
			hasPrevious = true
			break
		}
	}
	for i, pending := range s.PendingBacklog {
		if pending.BlockID == id {
			previous = pending
			hasPrevious = true
			s.PendingBacklog = append(s.PendingBacklog[:i], s.PendingBacklog[i+1:]...)
			break
		}
	}
	if retained := s.TranscriptRetryUntil[id]; retained != "" {
		previous.RetryUntil = retained
		hasPrevious = true
	}
	if incomplete && !terminalStatus(hydrated.Discovery.MeetingNotes.Status) {
		pending, keep := schedulePending(hydrated, now, previous, hasPrevious)
		if keep {
			s.TranscriptRetryUntil[id] = pending.RetryUntil
			if index >= 0 {
				s.Pending[index] = pending
			} else if len(s.Pending) < defaultDiscoveryLimits().Pending {
				s.Pending = append(s.Pending, pending)
			} else {
				s.PendingBacklog = append(s.PendingBacklog, pending)
				s.reason("pending_budget", "transcript")
			}
			return
		}
	}
	if index >= 0 {
		s.Pending = append(s.Pending[:index], s.Pending[index+1:]...)
	}
}
func (s *syncState) deferTranscriptAttempt(id string, now time.Time) {
	for i := range s.Pending {
		if s.Pending[i].BlockID == id {
			next := now.Add(transcriptRetryCadence)
			if deadline, err := time.Parse(time.RFC3339, s.Pending[i].RetryUntil); err == nil && deadline.Before(next) {
				next = deadline
			}
			s.Pending[i].NextAttemptAt = next.Format(time.RFC3339)
		}
	}
}

// Validate source capability explicitly instead of quietly running a narrower
// importer when a test double or embedding application omits broad discovery.
var _ discoverySource = (*Client)(nil)
