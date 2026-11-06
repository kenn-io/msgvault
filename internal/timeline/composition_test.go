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
	f.project()
	req := dayRequest()
	req.Mode, req.With = ModeDay, fmt.Sprintf("person:%d", person)
	got, err := Build(t.Context(), f.src, req)
	requirements.NoError(err)
	requirements.Len(got.Days, 1)
	assertions.Len(got.Days[0].Meetings, 2)
	assertions.Len(got.Days[0].Bursts, 1)
	assertions.Len(got.Days[0].Calls, 1)
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
	require := require.New(t)
	require.NoError(err)
	_, err = f.Store.PersistICalendarContext(t.Context(), source.ID, []byte("BEGIN:VCALENDAR\r\nVERSION:2.0\r\nBEGIN:VEVENT\r\nUID:ics-owner@example.com\r\nDTSTART:20261022T100000Z\r\nDTEND:20261022T110000Z\r\nSUMMARY:Planning\r\nORGANIZER:mailto:owner@example.com\r\nATTENDEE:mailto:owner@example.com\r\nATTENDEE:mailto:alice@example.com\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"))
	require.NoError(err)
	got, err := Build(t.Context(), f.src, dayRequest())
	require.NoError(err)
	entries := allDayEntries(got)
	require.Len(entries, 1)
	require.NotNil(entries[0].Meeting)
	require.Len(entries[0].Meeting.Attendees, 1)
	assert := assert.New(t)
	assert.Equal(f.peer, entries[0].Meeting.Attendees[0].ParticipantID)
}

// Shared flags change history admission and message omissions, while the
// relationship header continues to describe default-filter contact evidence.
func TestSharedColdNoiseAcrossTimelineModes(t *testing.T) {
	for _, saved := range []bool{false, true} {
		t.Run(fmt.Sprintf("saved=%t", saved), func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)
			f := newTimelineFixture(t)
			ref := "email:alice@example.com"
			if saved {
				ref = fmt.Sprintf("person:%d", f.savePerson())
			}

			for i, group := range []struct {
				cold, noise bool
				count       int
			}{
				{false, false, 1}, {false, true, 2}, {true, false, 2}, {true, true, 2},
			} {
				key := fmt.Sprintf("shared-flags-%d", i)
				conv, err := f.Store.EnsureConversationWithType(f.Source.ID, key, "direct_chat", "Shared flags")
				requirements.NoError(err)
				for n := range group.count {
					f.add(f.now.Add(-time.Duration(60+i*5+n)*time.Minute), false, func(m *store.Message) {
						m.ConversationID = conv
						if group.noise {
							m.ListID = sql.NullString{String: "updates.example.com", Valid: true}
						}
					})
				}
				if group.cold {
					requirements.NoError(f.seedConversationPolicy(t.Context(), f.Source.ID, key))
				}
			}
			f.project()
			baseline, err := Build(t.Context(), f.src, relationshipRequest(ref))
			requirements.NoError(err)
			requirements.NotNil(baseline.Person)
			requirements.Equal(1, baseline.Person.InteractionCount)
			if saved {
				assertions.Positive(baseline.Person.Person.PersonID)
			} else {
				assertions.Zero(baseline.Person.Person.PersonID)
			}
			for _, tc := range []struct {
				cold, noise                        bool
				entries, coldOmitted, noiseOmitted int
			}{
				{false, false, 1, 2, 2}, {false, true, 2, 4, 0},
				{true, false, 2, 0, 4}, {true, true, 4, 0, 0},
			} {
				t.Run(fmt.Sprintf("cold=%t/noise=%t", tc.cold, tc.noise), func(t *testing.T) {
					assertions := assert.New(t)
					requirements := require.New(t)
					req := relationshipRequest(ref)
					req.IncludeCold, req.IncludeNoise = tc.cold, tc.noise
					got, err := Build(t.Context(), f.src, req)
					requirements.NoError(err)
					assertions.Equal(baseline.Person, got.Person)
					assertions.Len(got.History, tc.entries)
					assertions.Equal(tc.coldOmitted, got.Omitted.Cold)
					assertions.Equal(tc.noiseOmitted, got.Omitted.Noise)
					req.Around = "2026-10-22T12:00:00Z"
					req.Before, req.After = tc.entries, 0
					req.AfterSet = true
					got, err = Build(t.Context(), f.src, req)
					requirements.NoError(err)
					assertions.Len(got.History, tc.entries)
					assertions.Equal(tc.coldOmitted, got.Omitted.Cold)
					assertions.Equal(tc.noiseOmitted, got.Omitted.Noise)
				})
			}
		})
	}
}
