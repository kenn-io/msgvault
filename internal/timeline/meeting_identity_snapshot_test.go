package timeline

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/sourcescope"
	"go.kenn.io/msgvault/internal/store"
)

func TestCanonicalMeetingCopySuppliesDisplayAndSelectedAlias(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	f := newTimelineFixture(t)
	canonical := f.calendar("canonical", singleDayEvent)
	stale := f.calendar("stale", strings.NewReplacer(`"id":"planning"`, `"id":"stale"`, `"Planning"`, `"Stale title"`, `T10:00:00Z`, `T09:00:00Z`, `T11:00:00Z`, `T09:30:00Z`, `alice@example.com`, `other@example.com`, `"Alice"`, `"Other"`).Replace(singleDayEvent))
	_, err := f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`UPDATE messages SET deleted_from_source_at=? WHERE id IN (?,?)`), f.now, canonical, stale)
	requirements.NoError(err)
	f.project()
	ref := fmt.Sprintf("evt:%d", canonical)
	alias := fmt.Sprintf("evt:%d", stale)
	identity, err := f.Store.ResolveMeetingIdentitiesContext(t.Context(), store.MeetingIdentityRequest{OccurrenceRefs: []string{alias}})
	requirements.NoError(err)
	requirements.Equal(ref, identity.ByOccurrence[alias].OccurrenceRef)
	req := dayRequest()
	got, err := Build(t.Context(), f.src, req)
	requirements.NoError(err)
	entries := allDayEntries(got)
	requirements.Len(entries, 1)
	assertions.Equal(ref, entries[0].Ref)
	assertions.Equal("Planning", entries[0].Meeting.Title.Text)
	assertions.Equal(10, entries[0].Time.Hour())
	requirements.Len(entries[0].Meeting.Attendees, 1)
	assertions.Equal(f.peer, entries[0].Meeting.Attendees[0].ParticipantID)
	anchorReq := anchorRequest(alias)
	anchorReq.With = "email:alice@example.com"
	anchored, err := Build(t.Context(), f.src, anchorReq)
	requirements.NoError(err)
	requirements.NotNil(anchored.Anchor)
	assertions.Equal(alias, anchored.Anchor.Ref)
	requirements.Len(anchored.History, 1)
	assertions.True(anchored.History[0].Anchor)
	assertions.Equal(ref, anchored.History[0].Ref)
	assertions.Equal(10, anchored.History[0].Time.Hour())
	// Safety metadata must come from the canonical row as well.
	_, err = f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`UPDATE messages SET deleted_from_source_at=NULL, hidden_text_present=TRUE WHERE id=?`), canonical)
	requirements.NoError(err)
	hidden, err := Build(t.Context(), f.src, dayRequest())
	requirements.NoError(err)
	requirements.Len(allDayEntries(hidden), 1)
	assertions.True(allDayEntries(hidden)[0].HiddenText)
	assertions.Empty(allDayEntries(hidden)[0].Meeting.Title.Text)
}

func TestRelationshipPersonReferences(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	f := newTimelineFixture(t)
	for _, ref := range []string{"person:abc", "participant:0", "unknown:value"} {
		got, err := Build(t.Context(), f.src, relationshipRequest(ref))
		requirements.ErrorIs(err, ErrInvalidRequest, ref)
		assertions.Nil(got)
	}
	const name = "Alex"
	saved := f.EnsureParticipant("saved@example.com", name, "example.com")
	unsaved := f.EnsureParticipant("unsaved@example.com", name, "example.com")
	person, _, err := f.Store.CreatePersonFromParticipant(saved)
	requirements.NoError(err)
	_, err = Build(t.Context(), f.src, relationshipRequest("name:"+name))
	var ambiguous *ErrAmbiguousReference
	requirements.ErrorAs(err, &ambiguous)
	requirements.Len(ambiguous.Candidates, 2)
	assertions.ElementsMatch([]Candidate{
		{PersonID: person.ID, PersonUID: person.VCardUID, ParticipantIDs: []int64{saved}, DisplayName: name, Address: "saved@example.com"},
		{ParticipantIDs: []int64{unsaved}, DisplayName: name, Address: "unsaved@example.com"},
	}, ambiguous.Candidates)
	for _, ref := range []string{fmt.Sprintf("person:%d", person.ID), fmt.Sprintf("participant:%d", unsaved)} {
		got, err := Build(t.Context(), f.src, relationshipRequest(ref))
		requirements.NoError(err, ref)
		assertions.Equal(ModeRelationship, got.Mode)
	}
}

func TestEncryptedMatrixComposerLabelsBurstAndIndividualAnchor(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	f := newTimelineFixture(t)
	source, err := f.Store.GetOrCreateSource("matrix", "matrix-fixture")
	requirements.NoError(err)
	conv, err := f.Store.EnsureConversation(source.ID, "encrypted", "Encrypted room")
	requirements.NoError(err)
	m := f.NewMessage().WithSentAt(f.now.Add(-time.Hour)).Build()
	m.SourceID, m.ConversationID, m.MessageType = source.ID, conv, "matrix"
	m.Snippet = sql.NullString{String: store.MatrixEncryptedPlaceholder, Valid: true}
	m.Subject = sql.NullString{String: "Suppressed subject", Valid: true}
	id, err := f.Store.PersistMatrixMessageContext(t.Context(), &store.MessagePersistData{Message: m}, store.MatrixPersist{SessionID: "synthetic-session"})
	requirements.NoError(err)
	f.project()
	day := dayRequest()
	day.IncludeNoise = true
	got, err := Build(t.Context(), f.src, day)
	requirements.NoError(err)
	requirements.Len(allDayEntries(got), 1)
	assertions.Equal("[encrypted]", allDayEntries(got)[0].Burst.Snippet.Text)
	assertions.Contains(Markdown(got), `\[encrypted\]`)
	assertions.NotContains(Markdown(got), "Suppressed subject")
	req := anchorRequest(messageRef(id))
	req.Conversation = true
	individual, err := Build(t.Context(), f.src, req)
	requirements.NoError(err)
	requirements.Len(individual.History, 1)
	assertions.Equal("[encrypted]", individual.History[0].Message.Snippet.Text)
	assertions.Contains(Markdown(individual), `\[encrypted\]`)
	// A tier filter forces the explicit anchor fallback through its separate path.
	req.Conversation, req.Tiers = false, []int{1}
	fallback, err := Build(t.Context(), f.src, req)
	requirements.NoError(err)
	requirements.Len(fallback.History, 1)
	assertions.True(fallback.History[0].Filtered)
	assertions.Equal("[encrypted]", fallback.History[0].Message.Snippet.Text)
}

func TestAmbiguousTranscriptSuggestionsAppearOnOccurrencesOwnerOnly(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	f := newTimelineFixture(t)
	one := f.calendar("one", singleDayEvent)
	// A series occurrence and a single with the same UID are distinct identities.
	f.calendar("two", strings.ReplaceAll(singleDayEvent, `"id":"planning"`, `"id":"two","recurrence":["RRULE:FREQ=DAILY;COUNT=1"]`))
	note := f.transcript("ambiguous", "planning@example.com", f.now.Add(-2*time.Hour))
	f.project()
	got, err := Build(t.Context(), f.src, dayRequest())
	requirements.NoError(err)
	entries := allDayEntries(got)
	requirements.Len(entries, 3)
	for _, entry := range entries {
		if entry.Kind == "meeting" {
			assertions.False(entry.Meeting.Attended)
			assertions.Empty(entry.Meeting.LinkedTranscriptIDs)
			assertions.Equal([]string{messageRef(note)}, entry.Meeting.PossiblyRelated)
		}
	}
	d, err := f.Store.GetCalendarOccurrenceContext(t.Context(), fmt.Sprintf("evt:%d", one))
	requirements.NoError(err)
	ctx := sourcescope.WithSourceIDs(t.Context(), []int64{d.SourceID})
	req := dayRequest()
	req.Scope.Owner = false
	before, err := Build(ctx, f.src, req)
	requirements.NoError(err)
	for _, entry := range allDayEntries(before) {
		assertions.Empty(entry.Meeting.PossiblyRelated)
	}
	_, err = f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`UPDATE messages SET subject=? WHERE id=?`), "Hidden suggestion changed", note)
	requirements.NoError(err)
	after, err := Build(ctx, f.src, req)
	requirements.NoError(err)
	beforeJSON, err := json.Marshal(before)
	requirements.NoError(err)
	afterJSON, err := json.Marshal(after)
	requirements.NoError(err)
	assertions.True(bytes.Equal(beforeJSON, afterJSON), "restricted suggestions must preserve byte-identical output")
}

func TestRelationshipCalendarAttendeeWithoutTranscriptEnvelope(t *testing.T) {
	for _, behavior := range []string{"attendance", "hidden text", "future clock"} {
		t.Run(behavior, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)
			f := newTimelineFixture(t)
			ref := "email:alice@example.com"
			// An unsent observation resolves the scoped subject without adding contact.
			f.add(f.now, false, func(m *store.Message) { m.IsSent = sql.NullBool{Bool: false, Valid: true} })
			raw := singleDayEvent
			event := f.calendar("calendar-only-attendee", raw)
			f.calendar("calendar-copy", strings.ReplaceAll(raw, `"id":"planning"`, `"id":"calendar-copy"`))
			var notes []int64
			for _, key := range []string{"one", "two"} {
				note := f.transcript(key, "planning@example.com", f.now.Add(-time.Hour))
				recipients := []int64{f.EnsureParticipant("other@example.com", "Other", "example.com")}
				requirements.NoError(f.Store.ReplaceMessageRecipients(note, "to", recipients, nil))
				notes = append(notes, note)
			}
			f.project()
			switch behavior {
			case "attendance":
				meetings := &relationshipMeetingReads{MeetingsSource: f.src.Meetings}
				calendar := &relationshipCalendarReads{CalendarSource: f.src.Calendar}
				f.src.Meetings, f.src.Calendar = meetings, calendar
				got, err := Build(t.Context(), f.src, relationshipRequest(ref))
				requirements.NoError(err)
				requirements.NotNil(got.Person)
				assertions.Equal(1, got.Person.InteractionCount)
				assertions.Equal([]string{"meeting"}, got.Person.Channels)
				requirements.NotNil(got.Person.FirstAt)
				assertions.Equal(10, got.Person.FirstAt.Hour())
				requirements.NotNil(got.Person.LastAt)
				assertions.Equal(10, got.Person.LastAt.Hour())
				assertions.Equal(1, got.Person.Monthly[11])
				requirements.Len(got.History, 1)
				assertions.Equal(fmt.Sprintf("evt:%d", event), got.History[0].Ref)
				assertions.ElementsMatch(notes, got.History[0].Meeting.LinkedTranscriptIDs)
				assertions.Zero(meetings.resolutions)
				assertions.Zero(calendar.details)
			case "hidden text":
				// Hidden text remains attendance evidence, but no hidden title leaks.
				_, err := f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`UPDATE messages SET hidden_text_present=TRUE WHERE id IN (?,?)`), notes[0], notes[1])
				requirements.NoError(err)
				hidden, err := Build(t.Context(), f.src, relationshipRequest(ref))
				requirements.NoError(err)
				assertions.Equal(1, hidden.Person.InteractionCount)
				requirements.Len(hidden.History, 1)
				assertions.True(hidden.History[0].HiddenText)
				assertions.Empty(hidden.History[0].Meeting.Title.Text)
			case "future clock":
				// Request clock excludes meetings in the future even if recording was imported earlier.
				f.src.Clock = func() time.Time { return time.Date(2026, 10, 22, 9, 0, 0, 0, time.UTC) }
				future, err := Build(t.Context(), f.src, relationshipRequest(ref))
				requirements.NoError(err)
				assertions.Zero(future.Person.InteractionCount)
				assertions.Empty(future.History)
			}
		})
	}
}

func TestRelationshipTranscriptOnlyFallback(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	f := newTimelineFixture(t)
	id := f.transcript("standalone", "", f.now.Add(-time.Hour))
	f.project()
	got, err := Build(t.Context(), f.src, relationshipRequest("email:alice@example.com"))
	requirements.NoError(err)
	assertions.Equal(1, got.Person.InteractionCount)
	requirements.Len(got.History, 1)
	assertions.Equal(messageRef(id), got.History[0].Ref)
	assertions.Equal("transcript", got.History[0].Kind)
}

func TestRelationshipCalendarAttendanceOlderThanExpansionWindow(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	f := newTimelineFixture(t)
	f.calendar("old-calendar", strings.ReplaceAll(singleDayEvent, "2026-10-22", "2019-10-22"))
	at := time.Date(2019, 10, 22, 10, 0, 0, 0, time.UTC)
	note := f.transcript("old-recording", "planning@example.com", at)
	requirements.NoError(f.Store.ReplaceMessageRecipients(note, "to", nil, nil))
	f.project()
	got, err := Build(t.Context(), f.src, relationshipRequest("email:alice@example.com"))
	requirements.NoError(err)
	assertions.Equal(1, got.Person.InteractionCount)
	requirements.NotNil(got.Person.FirstAt)
	assertions.Equal(at, *got.Person.FirstAt)
	assertions.Equal([12]int{}, got.Person.Monthly)
	requirements.Len(got.History, 1)
	assertions.Equal(at, got.History[0].Time)
}

func TestSnapshotReadsPreserveScopedDayAndAnchor(t *testing.T) {
	f := newTimelineFixture(t)
	id := f.add(f.now.Add(-time.Hour), false)
	f.calendar("hidden-calendar", singleDayEvent)
	f.project()
	for _, tc := range []struct {
		name string
		req  Request
	}{
		{"day", dayRequest()},
		{"anchor", anchorRequest(fmt.Sprintf("msg:%d", id))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertions, requirements := assert.New(t), require.New(t)
			ctx := sourcescope.WithSourceIDs(t.Context(), []int64{f.Source.ID})
			ctx, release, err := f.Store.BeginReadSnapshotContext(ctx)
			requirements.NoError(err)
			defer release()
			req := tc.req
			req.Scope.Owner = false
			got, err := Build(ctx, f.src, req)
			requirements.NoError(err)
			for _, gap := range got.Gaps {
				assertions.NotEqual("unavailable", gap.Code, gap.Section)
				assertions.NotEqual("timeout", gap.Code, gap.Section)
			}
			if req.Around != "" {
				requirements.NotNil(got.Anchor)
				assertions.False(got.Anchor.Filtered)
			} else {
				entries := allDayEntries(got)
				requirements.Len(entries, 1)
				requirements.NotNil(entries[0].Burst)
				assertions.Equal([]int64{id}, entries[0].Burst.LatestMessageIDs)
			}
		})
	}
}

func TestSnapshotReadWaitsForCanceledReaderAndPreservesTimeout(t *testing.T) {
	f := newTimelineFixture(t)
	for _, tc := range []struct {
		name   string
		cancel bool
		want   error
	}{
		{"canceled", true, context.Canceled},
		{"deadline", false, context.DeadlineExceeded},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertions, requirements := assert.New(t), require.New(t)
			ctx, release, err := f.Store.BeginReadSnapshotContext(t.Context())
			requirements.NoError(err)
			defer release()
			ctx, cancel := context.WithCancel(ctx)
			defer cancel()
			timeout := time.Duration(0)
			if tc.cancel {
				cancel()
				timeout = time.Minute
			}
			type completion struct {
				finished bool
				queryErr error
			}
			ctx, deadlineCancel := context.WithTimeout(ctx, timeout)
			defer deadlineCancel()
			got := readWithin(ctx, func(ctx context.Context) (completion, error) {
				_, queryErr := f.Store.GetSourceByIDContext(ctx, f.Source.ID)
				// A driver may report its own cancellation error during cleanup.
				return completion{finished: true, queryErr: queryErr}, errors.New("driver canceled its read")
			})
			assertions.True(got.value.finished, "the reader must finish before releasing its snapshot")
			requirements.ErrorIs(got.value.queryErr, tc.want)
			requirements.ErrorIs(got.err, tc.want)
		})
	}
}
