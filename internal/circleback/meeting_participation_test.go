package circleback

import (
	"encoding/json/v2"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/meetingidentity"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestIngestMeetingNameOnlyGuestPreservesMeetingParticipation(t *testing.T) {
	f := storetest.New(t)
	ctx := t.Context()
	source, err := f.Store.GetOrCreateSource(SourceType, "synthetic-meetings")
	require.NoError(t, err)
	require.NoError(t, f.Store.AddAccountIdentityContext(ctx, source.ID, "owner@example.test", "manual"))
	host := f.EnsureParticipant("host@example.test", "Host", "example.test")
	target := f.EnsureParticipant("target@example.test", "Target", "example.test")
	guest := f.EnsureParticipant("guest@example.test", "Guest", "example.test")
	hostPerson, _, err := f.Store.CreatePersonFromParticipantContext(ctx, host)
	require.NoError(t, err)
	targetPerson, _, err := f.Store.CreatePersonFromParticipantContext(ctx, target)
	require.NoError(t, err)
	guestPerson, _, err := f.Store.CreatePersonFromParticipantContext(ctx, guest)
	require.NoError(t, err)
	const raw = `{"id":"synthetic-meeting","startTime":"2026-01-01T00:00:00Z","organizer":{"email":"host@example.test"},"attendees":[{"email":"owner@example.test"},{"email":"target@example.test"},{"name":"Guest"}]}`
	var meeting Meeting
	require.NoError(t, json.Unmarshal([]byte(raw), &meeting))
	meeting.Raw = []byte(raw)
	imp := NewImporter(f.Store, nil)
	result, err := imp.ingestMeeting(ctx, source.ID, ImportOptions{Identifier: source.Identifier, AccountEmail: "owner@example.test"}, meetingidentity.Set{"owner@example.test": {}}, &meeting, nil, transcriptStateUnavailable)
	require.NoError(t, err)
	intro, err := f.Store.PreparePersonIntroductionContext(ctx, store.PersonIntroductionPrepare{
		SourceID: f.Source.ID, PersonAID: hostPerson.ID, PersonBID: targetPerson.ID,
		Title: "Hello", Body: "Host meets Target", MeetingMessageID: &result.MessageID,
	}, "owner")
	require.NoError(t, err, "an unrelated name-only attendee must not invalidate explicit participants")
	assert.Equal(t, &result.MessageID, intro.MeetingMessageID)
	valid, err := f.Store.PersonIntroductionMeetingValidContext(ctx, result.MessageID, hostPerson.ID, guestPerson.ID, nil)
	require.NoError(t, err)
	assert.False(t, valid, "a matching display name must not infer participation")
}
