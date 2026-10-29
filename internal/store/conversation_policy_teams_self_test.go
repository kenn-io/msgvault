package store_test

import (
	"database/sql"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/conversationpolicy"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestNormalizeTeamsConversationSelf(t *testing.T) {
	for _, existingContact := range []bool{false, true} {
		name := "bare confirmed self"
		if existingContact {
			name = "existing canonical self"
		}
		t.Run(name, func(t *testing.T) {
			requirements := require.New(t)
			assertions := assert.New(t)

			s := testutil.NewTestStore(t)
			s.DB().SetMaxOpenConns(1)
			source, err := s.GetOrCreateSource("teams", "self@example.test")
			requirements.NoError(err)
			room, err := s.EnsureConversation(source.ID, "self-room", "Synthetic")
			requirements.NoError(err)
			requirements.NoError(s.AddAccountIdentity(source.ID, "self@example.test", "manual"))
			requirements.NoError(s.SetConversationPolicyOverride(t.Context(), source.ID, "self-room", conversationpolicy.Cold))
			other, err := s.GetOrCreateSource("teams", "other@example.test")
			requirements.NoError(err)
			otherRoom, err := s.EnsureConversation(other.ID, "self-room", "Other")
			requirements.NoError(err)
			for _, attempt := range []struct {
				room  int64
				email string
			}{{otherRoom, "self@example.test"}, {room, "unconfirmed@example.test"}, {0, "self@example.test"}} {
				pid, confirmed, err := s.NormalizeTeamsConversationSelfContext(t.Context(), attempt.room, "object-self", attempt.email)
				requirements.NoError(err)
				assertions.False(confirmed)
				assertions.Zero(pid)
			}
			var count int
			requirements.NoError(s.DB().QueryRow(`SELECT COUNT(*) FROM participants`).Scan(&count))
			assertions.Zero(count, "unconfirmed or wrong-source proof cannot create participants")
			var canonical int64
			if existingContact {
				canonical, err = s.EnsureParticipant("self@example.test", "Existing self profile", "example.test")
				requirements.NoError(err)
			}
			weak, err := s.EnsureParticipantByIdentifier("teams", "object-self", "")
			requirements.NoError(err)
			message, err := s.UpsertMessage(&store.Message{SourceID: source.ID, ConversationID: room, SourceMessageID: "self-history", MessageType: "teams", SenderID: sql.NullInt64{Int64: weak, Valid: true}})
			requirements.NoError(err)
			work, err := s.CaptureConversationPolicyWork(t.Context())
			requirements.NoError(err)
			pid, confirmed, err := s.NormalizeTeamsConversationSelfContext(t.Context(), room, "object-self", "self@example.test")
			requirements.NoError(err)
			requirements.True(confirmed)
			assertions.NotZero(pid)
			assertions.NotEqual(weak, pid)
			if existingContact {
				assertions.Equal(canonical, pid)
			}
			mapped, _, err := s.ParticipantByIdentifier("teams", "object-self")
			requirements.NoError(err)
			assertions.Equal(pid, mapped)
			var sender int64
			requirements.NoError(s.DB().QueryRow(s.Rebind(`SELECT sender_id FROM messages WHERE id=?`), message).Scan(&sender))
			assertions.Equal(pid, sender)
			var email, display sql.NullString
			requirements.NoError(s.DB().QueryRow(s.Rebind(`SELECT email_address,display_name FROM participants WHERE id=?`), pid).Scan(&email, &display))
			if existingContact {
				assertions.Equal("self@example.test", email.String)
				assertions.Equal("Existing self profile", display.String)
			} else {
				assertions.False(email.Valid)
				assertions.Empty(display.String)
			}
			requirements.NoError(s.DB().QueryRow(s.Rebind(`SELECT COUNT(*) FROM participants WHERE id=?`), weak).Scan(&count))
			assertions.Zero(count)
			requirements.NoError(s.DB().QueryRow(`SELECT COUNT(*) FROM account_identities`).Scan(&count))
			assertions.Equal(1, count, "normalization does not confirm another account identity")
			requirements.ErrorIs(store.CheckConversationPolicyAdmission(work), store.ErrConversationPolicyStale)
			revision, err := s.ConversationPolicyRevision(t.Context())
			requirements.NoError(err)
			again, confirmed, err := s.NormalizeTeamsConversationSelfContext(t.Context(), room, "object-self", "self@example.test")
			requirements.NoError(err)
			assertions.True(confirmed)
			assertions.Equal(pid, again)
			after, err := s.ConversationPolicyRevision(t.Context())
			requirements.NoError(err)
			assertions.Equal(revision, after)
		})
	}
}

func TestNormalizeTeamsConversationSelfRollback(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)

	s := testutil.NewTestStore(t)
	s.DB().SetMaxOpenConns(1)
	source, err := s.GetOrCreateSource("teams", "self@example.test")
	requirements.NoError(err)
	room, err := s.EnsureConversation(source.ID, "self-room", "Synthetic")
	requirements.NoError(err)
	requirements.NoError(s.AddAccountIdentity(source.ID, "self@example.test", "manual"))
	requirements.NoError(s.SetConversationPolicyOverride(t.Context(), source.ID, "self-room", conversationpolicy.Cold))
	weak, err := s.EnsureParticipantByIdentifier("teams", "object-self", "")
	requirements.NoError(err)
	requirements.NoError(s.EnsureConversationParticipant(room, weak, "member"))
	if s.IsPostgreSQL() {
		_, err = s.DB().Exec(`CREATE FUNCTION fail_self_cleanup() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.key='conversation_policy_cleanup_revision' THEN RAISE EXCEPTION 'synthetic self cleanup failure'; END IF; RETURN NEW; END $$`)
		requirements.NoError(err)
		_, err = s.DB().Exec(`CREATE TRIGGER fail_self_cleanup BEFORE INSERT OR UPDATE ON archive_metadata FOR EACH ROW EXECUTE FUNCTION fail_self_cleanup()`)
	} else {
		_, err = s.DB().Exec(`CREATE TRIGGER fail_self_cleanup BEFORE INSERT ON archive_metadata WHEN NEW.key='conversation_policy_cleanup_revision' BEGIN SELECT RAISE(ABORT,'synthetic self cleanup failure'); END`)
	}
	requirements.NoError(err)
	pid, confirmed, err := s.NormalizeTeamsConversationSelfContext(t.Context(), room, "object-self", "self@example.test")
	requirements.ErrorContains(err, "synthetic self cleanup failure")
	assertions.Zero(pid)
	assertions.False(confirmed)
	mapped, _, err := s.ParticipantByIdentifier("teams", "object-self")
	requirements.NoError(err)
	assertions.Equal(weak, mapped)
	var count int
	requirements.NoError(s.DB().QueryRow(`SELECT COUNT(*) FROM participant_identifiers WHERE identifier_type='email' AND identifier_value='self@example.test'`).Scan(&count))
	assertions.Zero(count)
	requirements.NoError(s.DB().QueryRow(`SELECT COUNT(*) FROM participants`).Scan(&count))
	assertions.Equal(1, count)
}

func TestNormalizeTeamsConversationSelfExistingRichProvider(t *testing.T) {
	for _, matches := range []bool{true, false} {
		name := "mismatched rich provider"
		if matches {
			name = "matching legacy self"
		}
		t.Run(name, func(t *testing.T) {
			requirements := require.New(t)
			assertions := assert.New(t)

			s := testutil.NewTestStore(t)
			s.DB().SetMaxOpenConns(1)
			source, err := s.GetOrCreateSource("teams", "self@example.test")
			requirements.NoError(err)
			room, err := s.EnsureConversation(source.ID, "legacy-room", "Synthetic")
			requirements.NoError(err)
			requirements.NoError(s.AddAccountIdentity(source.ID, "self@example.test", "manual"))
			requirements.NoError(s.SetConversationPolicyOverride(t.Context(), source.ID, "legacy-room", conversationpolicy.Cold))
			email := "different@example.test"
			if matches {
				email = "self@example.test"
			}
			existing, err := s.EnsureParticipant(email, "Existing provider profile", "example.test")
			requirements.NoError(err)
			requirements.NoError(s.SetParticipantIdentifier(existing, "teams", "legacy-object"))
			before, err := s.ConversationPolicyRevision(t.Context())
			requirements.NoError(err)
			pid, confirmed, err := s.NormalizeTeamsConversationSelfContext(t.Context(), room, "legacy-object", "self@example.test")
			requirements.NoError(err)
			assertions.Equal(matches, confirmed)
			if matches {
				assertions.Equal(existing, pid)
			} else {
				assertions.Zero(pid)
			}
			mapped, _, err := s.ParticipantByIdentifier("teams", "legacy-object")
			requirements.NoError(err)
			assertions.Equal(existing, mapped)
			var gotEmail, gotName string
			requirements.NoError(s.DB().QueryRow(s.Rebind(`SELECT email_address,display_name FROM participants WHERE id=?`), existing).Scan(&gotEmail, &gotName))
			assertions.Equal(email, gotEmail)
			assertions.Equal("Existing provider profile", gotName)
			var count int
			requirements.NoError(s.DB().QueryRow(`SELECT COUNT(*) FROM participants`).Scan(&count))
			assertions.Equal(1, count)
			after, err := s.ConversationPolicyRevision(t.Context())
			requirements.NoError(err)
			assertions.Equal(before, after)
			now := time.Now().UTC().Truncate(time.Second)
			message, err := s.UpsertMessage(&store.Message{SourceID: source.ID, ConversationID: room, SourceMessageID: "legacy-mention", MessageType: "teams", SentAt: sql.NullTime{Time: now, Valid: true}})
			requirements.NoError(err)
			requirements.NoError(s.ReplaceMessageRecipients(message, "mention", []int64{existing}, []string{""}))
			activity, err := s.ConversationLastActivityContext(t.Context(), room, now)
			requirements.NoError(err)
			if matches {
				assertions.Equal(now, activity, "legacy confirmed self remains attributable while Cold")
			} else {
				assertions.True(activity.IsZero(), "different rich identity cannot acquire self authority")
			}
		})
	}
}
