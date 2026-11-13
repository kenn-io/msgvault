package store_test

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/activity"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestMeetingParticipationFirstAppearance(t *testing.T) {
	for _, tc := range []struct {
		name, format, raw string
		want              store.FirstContactKind
	}{
		{"generic", "meeting_json", `{"organizer":{"email":"host@example.test"},"attendees":[{"email":"owner@example.test"},{"email":"target@example.test"}]}`, store.FirstContactMeetingIntro},
		{"granola", "granola_json", `{"calendar_event":{"organiser":"host@example.test"},"attendees":[{"email":"owner@example.test"},{"email":"target@example.test"}]}`, store.FirstContactMeetingIntro},
		{"fallbacks", "granola_json", `{"owner":{"email":"host@example.test"},"calendar_event":{"invitees":[{"email":"owner@example.test"},{"email":"target@example.test"}]}}`, store.FirstContactMeeting},
		{"mismatch", "meeting_json", `{"organizer":{"email":"other@example.test"},"attendees":[{"email":"owner@example.test"},{"email":"target@example.test"}]}`, store.FirstContactMeeting},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := storetest.New(t)
			ctx := t.Context()
			require.NoError(t, f.Store.AddAccountIdentityContext(ctx, f.Source.ID, "owner@example.test", "manual"))
			owner := f.EnsureParticipant("owner@example.test", "Owner", "example.test")
			host := f.EnsureParticipant("host@example.test", "Host", "example.test")
			target := f.EnsureParticipant("target@example.test", "Target", "example.test")
			person, _, err := f.Store.CreatePersonFromParticipantContext(ctx, target)
			require.NoError(t, err)
			at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
			message := f.NewMessage().WithSourceMessageID("first-meeting").WithSentAt(at).Build()
			message.MessageType = "meeting_transcript"
			id, err := f.Store.UpsertMessage(message)
			require.NoError(t, err)
			_, err = f.Store.DB().ExecContext(ctx, f.Store.Rebind(`INSERT INTO message_recipients (message_id,participant_id,recipient_type,email_address) VALUES (?,?,'from','host@example.test'),(?,?,'to','owner@example.test'),(?,?,'to','target@example.test')`), id, host, id, owner, id, target)
			require.NoError(t, err)
			require.NoError(t, f.Store.UpsertMessageRawWithFormatContext(ctx, id, []byte(tc.raw), tc.format))
			projector, err := activity.NewProjector(f.Store, activity.Options{Timezone: "UTC", BatchSize: 10, MaxDirectCounterparts: 25})
			require.NoError(t, err)
			_, err = projector.RunOnce(ctx)
			require.NoError(t, err)
			state, err := f.Store.ContactStateContext(ctx, person.ID, at)
			require.NoError(t, err)
			assert.Equal(t, tc.want, state.FirstContactKind)
			assert.Equal(t, fmt.Sprintf("meeting:%d", id), state.FirstSeenRef)
			if tc.want == store.FirstContactMeetingIntro {
				require.NotNil(t, state.Introducer)
				assert.Equal(t, host, state.Introducer.ParticipantID)
			} else {
				assert.Nil(t, state.Introducer)
			}
			// A later email cannot displace the earliest event. On a timestamp tie,
			// the lower message ID still wins regardless of introduction kind.
			later := f.NewMessage().WithSourceMessageID("later-email").WithSentAt(at).Create(t, f.Store)
			require.NoError(t, f.Store.ReplaceMessageRecipients(later, "from", []int64{host}, nil))
			require.NoError(t, f.Store.ReplaceMessageRecipients(later, "to", []int64{owner, target}, nil))
			_, err = projector.RunOnce(ctx)
			require.NoError(t, err)
			state, err = f.Store.ContactStateContext(ctx, person.ID, at)
			require.NoError(t, err)
			assert.Equal(t, tc.want, state.FirstContactKind)
			assert.Equal(t, fmt.Sprintf("meeting:%d", id), state.FirstSeenRef)
			if tc.name == "generic" {
				// Reinsert the meeting after the email at the same timestamp: the email's
				// lower ID wins, and the selected-event loader must not search forward.
				_, err = f.Store.DB().ExecContext(ctx, f.Store.Rebind(`DELETE FROM messages WHERE id=?`), id)
				require.NoError(t, err)
				message := f.NewMessage().WithSourceMessageID("replacement-meeting").WithSentAt(at).Build()
				message.MessageType = "meeting_transcript"
				replacement, err := f.Store.UpsertMessage(message)
				require.NoError(t, err)
				_, err = f.Store.DB().ExecContext(ctx, f.Store.Rebind(`INSERT INTO message_recipients (message_id,participant_id,recipient_type,email_address) VALUES (?,?,'from','host@example.test'),(?,?,'to','owner@example.test'),(?,?,'to','target@example.test')`), replacement, host, replacement, owner, replacement, target)
				require.NoError(t, err)
				require.NoError(t, f.Store.UpsertMessageRawWithFormatContext(ctx, replacement, []byte(tc.raw), tc.format))
				_, err = projector.RunOnce(ctx)
				require.NoError(t, err)
				state, err = f.Store.ContactStateContext(ctx, person.ID, at)
				require.NoError(t, err)
				assert.Equal(t, store.FirstContactEmailIntro, state.FirstContactKind)
				assert.Equal(t, fmt.Sprintf("message:%d", later), state.FirstSeenRef)
			}

		})
	}
}
