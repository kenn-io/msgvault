package timeline

import (
	"database/sql"
	"encoding/json/v2"
	"fmt"
	"maps"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestDayWithFiltersEverySection(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	f := newTimelineFixture(t)
	person := f.savePerson()
	f.add(f.now.Add(-time.Hour), false)
	f.call(f.now.Add(-time.Hour))
	peer := f.peer
	f.peer = f.EnsureParticipant("other@example.com", "Other", "example.com")
	conv, err := f.Store.EnsureConversationWithType(f.Source.ID, "unrelated", "email_thread", "Unrelated")
	requirements.NoError(err)
	f.add(f.now.Add(-time.Hour), false, func(m *store.Message) { m.ConversationID = conv })
	f.call(f.now.Add(-time.Hour))
	f.peer = peer
	f.PersistEmail(storetest.EmailSpec{Thread: "related-loop", From: "alice@example.com", To: []string{storetest.OwnerAddress}, Body: "Can you send the contract?", SentAt: f.now.Add(-48 * time.Hour)})
	f.PersistEmail(storetest.EmailSpec{Thread: "unrelated-loop", From: "other@example.com", To: []string{storetest.OwnerAddress}, Body: "Can you send the invoice?", SentAt: f.now.Add(-48 * time.Hour)})
	f.calendar("related", singleDayEvent)
	f.calendar("unrelated", strings.NewReplacer("planning", "unrelated", "alice@example.com", "other@example.com").Replace(singleDayEvent))
	f.calendar("related-future", strings.NewReplacer("planning", "related-future", "T10:00", "T15:00", "T11:00", "T16:00").Replace(singleDayEvent))
	f.calendar("unrelated-future", strings.NewReplacer("planning", "unrelated-future", "alice@example.com", "other@example.com", "T10:00", "T15:00", "T11:00", "T16:00").Replace(singleDayEvent))
	_, err = f.Store.CreateDailyNoteEntryContext(t.Context(), store.DailyNoteEntryInput{LocalDate: "2026-10-22", Body: "Related", Author: "owner", PersonIDs: []int64{person}})
	requirements.NoError(err)
	_, err = f.Store.CreateDailyNoteEntryContext(t.Context(), store.DailyNoteEntryInput{LocalDate: "2026-10-22", Body: "Unrelated", Author: "owner"})
	requirements.NoError(err)
	f.project()
	req := dayRequest()
	req.Mode, req.With = ModeDay, fmt.Sprintf("person:%d", person)
	got, err := Build(t.Context(), f.src, req)
	requirements.NoError(err)
	requirements.Len(got.Days, 1)
	assertions.Len(got.Days[0].Meetings, 2)
	assertions.Len(got.Days[0].Bursts, 1)
	assertions.Len(got.Days[0].Calls, 1)
	requirements.Len(got.Days[0].Notes, 1)
	assertions.Equal("Related", got.Days[0].Notes[0].Note.Text.Text)
	requirements.Len(got.Upcoming, 1)
	assertions.Equal("Planning", got.Upcoming[0].Meeting.Title.Text)
	requirements.NotNil(got.OpenLoops)
	requirements.NotEmpty(got.OpenLoops.Items)
	for _, loop := range got.OpenLoops.Items {
		assertions.Equal(person, loop.Loop.Person.PersonID)
	}
}

func TestIncludeColdCollapsesEmailCopiesBeforeGrouping(t *testing.T) {
	requirements := require.New(t)
	f := newTimelineFixture(t)
	warm := f.add(f.now.Add(-time.Hour), false, func(m *store.Message) {
		m.RFC822MessageID = sql.NullString{String: "<shared@example.com>", Valid: true}
	})
	warmTail := f.add(f.now.Add(-30*time.Minute), false)
	source, err := f.Store.GetOrCreateSource("gmail", "second@example.com")
	requirements.NoError(err)
	requirements.NoError(f.Store.AddAccountIdentity(source.ID, "owner@example.com", "manual"))
	conv, err := f.Store.EnsureConversationWithType(source.ID, "email-copy", "email_thread", "Email copy")
	requirements.NoError(err)
	copyID := f.add(f.now.Add(-time.Hour), false, func(m *store.Message) {
		m.SourceID, m.ConversationID = source.ID, conv
		m.RFC822MessageID = sql.NullString{String: "shared@example.com", Valid: true}
	})
	conv, err = f.Store.EnsureConversationWithType(source.ID, "cold-copy", "direct_chat", "Cold chat")
	requirements.NoError(err)
	coldTail := f.add(f.now.Add(-20*time.Minute), false, func(m *store.Message) {
		m.SourceID, m.ConversationID = source.ID, conv
	})
	requirements.NoError(f.seedConversationPolicy(t.Context(), source.ID, "cold-copy"))
	f.project()
	for _, mode := range []Mode{ModeDay, ModeRelationship, ModeAnchor} {
		t.Run(string(mode), func(t *testing.T) {
			assertions, requirements := assert.New(t), require.New(t)
			req := dayRequest()
			req.Mode, req.IncludeCold = mode, true
			if mode == ModeRelationship {
				req = relationshipRequest("email:alice@example.com")
				req.IncludeCold = true
			}
			if mode == ModeAnchor {
				req = anchorRequest(messageRef(warm))
				req.IncludeCold = true
			}
			got, err := Build(t.Context(), f.src, req)
			requirements.NoError(err)
			entries := got.History
			if mode == ModeDay {
				entries = allDayEntries(got)
			}
			requirements.Len(entries, 2)
			inbound := 0
			copies := map[int64]int{}
			var ids []int64
			for _, e := range entries {
				requirements.NotNil(e.Burst)
				inbound += e.Burst.Inbound
				maps.Copy(copies, e.Burst.Copies)
				ids = append(ids, e.Burst.LatestMessageIDs...)
			}
			assertions.Equal(3, inbound)
			assertions.Equal(map[int64]int{warm: 2, warmTail: 1}, copies)
			assertions.Contains(ids, coldTail)
		})
	}
	got, err := Build(t.Context(), f.src, anchorRequest(messageRef(copyID)))
	requirements.NoError(err)
	requirements.NotNil(got.Anchor)
	assert.False(t, got.Anchor.Filtered)
	requirements.Len(got.History, 1)
	assert.True(t, got.History[0].Anchor)
	requirements.NotNil(got.History[0].Burst)
	assert.Contains(t, got.History[0].Burst.LatestMessageIDs, warm)
}

func TestUpcomingExcludesCancelledAndRemovedOccurrences(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	f := newTimelineFixture(t)
	raw := strings.NewReplacer("T10:00", "T15:00", "T11:00", "T16:00").Replace(singleDayEvent)
	live := f.calendar("live", strings.ReplaceAll(raw, "planning", "live"))
	cancelled := f.calendar("cancelled", strings.ReplaceAll(strings.Replace(raw, `"summary":`, `"status":"cancelled","summary":`, 1), "planning", "cancelled"))
	removed := f.calendar("removed", strings.ReplaceAll(raw, "planning", "removed"))
	_, err := f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`UPDATE messages SET deleted_from_source_at=? WHERE id=?`), f.now, removed)
	requirements.NoError(err)
	got, err := Build(t.Context(), f.src, dayRequest())
	requirements.NoError(err)
	requirements.Len(got.Upcoming, 1)
	assertions.Equal(fmt.Sprintf("evt:%d", live), got.Upcoming[0].Ref)
	entries := allDayEntries(got)
	requirements.Len(entries, 3)
	byRef := map[string]string{}
	for _, e := range entries {
		if e.Ref == fmt.Sprintf("evt:%d", removed) {
			assertions.Equal("Planning", e.Meeting.Title.Text)
			assertions.False(e.HiddenText)
		}
		data, err := json.Marshal(e.Meeting)
		requirements.NoError(err)
		byRef[e.Ref] = string(data)
	}
	assertions.Contains(byRef[fmt.Sprintf("evt:%d", cancelled)], `"status":"cancelled"`)
	assertions.Contains(byRef[fmt.Sprintf("evt:%d", removed)], `"deleted_from_source":true`)
	assertions.Contains(Markdown(got), "· cancelled")
	assertions.Contains(Markdown(got), "· removed from source")
}

func TestICSMeetingExcludesAccountIdentities(t *testing.T) {
	f := newTimelineFixture(t)
	source, err := f.Store.GetOrCreateSource("ics", "synthetic-calendar")
	require.NoError(t, err)
	_, err = f.Store.PersistICalendarContext(t.Context(), source.ID, []byte("BEGIN:VCALENDAR\r\nVERSION:2.0\r\nBEGIN:VEVENT\r\nUID:ics-owner@example.com\r\nDTSTART:20261022T100000Z\r\nDTEND:20261022T110000Z\r\nSUMMARY:Planning\r\nORGANIZER:mailto:owner@example.com\r\nATTENDEE:mailto:owner@example.com\r\nATTENDEE:mailto:alice@example.com\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"))
	require.NoError(t, err)
	got, err := Build(t.Context(), f.src, dayRequest())
	require.NoError(t, err)
	entries := allDayEntries(got)
	require.Len(t, entries, 1)
	require.NotNil(t, entries[0].Meeting)
	require.Len(t, entries[0].Meeting.Attendees, 1)
	assert.Equal(t, f.peer, entries[0].Meeting.Attendees[0].ParticipantID)
}
