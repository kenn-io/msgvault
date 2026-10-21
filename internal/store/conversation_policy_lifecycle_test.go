package store_test

import (
	"context"
	"database/sql"
	"fmt"
	"go.kenn.io/msgvault/internal/peoplesweep"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/conversationpolicy"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestConversationPolicyMergeIntoColdInvalidatesTransferredFullWork(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)

	s := testutil.NewTestStore(t)
	from, err := s.GetOrCreateSource("beeper", "history@example.test")
	requirements.NoError(err)
	into, err := s.GetOrCreateSource("beeper", "current@example.test")
	requirements.NoError(err)
	room, err := s.EnsureConversation(from.ID, "!shared:example.test", "Shared")
	requirements.NoError(err)
	dest, err := s.EnsureConversation(into.ID, "!shared:example.test", "Shared")
	requirements.NoError(err)
	settings := conversationpolicy.DefaultSettings()
	settings.Overrides = []conversationpolicy.Override{
		{SourceType: "beeper", SourceIdentifier: from.Identifier, ConversationID: "!shared:example.test", Tier: conversationpolicy.Full},
		{SourceType: "beeper", SourceIdentifier: into.Identifier, ConversationID: "!shared:example.test", Tier: conversationpolicy.Full},
	}
	_, err = s.ConfigureConversationPolicy(t.Context(), settings, time.Now())
	requirements.NoError(err)
	requirements.NoError(s.SetConversationPolicyOverride(t.Context(), into.ID, "!shared:example.test", conversationpolicy.Cold))
	s.DB().SetMaxOpenConns(2) // MergeSources retains its grouped execution-lease baseline.
	id, err := s.UpsertMessage(&store.Message{SourceID: from.ID, ConversationID: room, SourceMessageID: "transferred", MessageType: "beeper"})
	requirements.NoError(err)
	requirements.NoError(s.UpsertMessageBody(id, sql.NullString{String: "Retained synthetic text", Valid: true}, sql.NullString{}))
	before, err := s.ConversationPolicyRevision(t.Context())
	requirements.NoError(err)
	acknowledged, err := s.AcknowledgeConversationPolicyCleanup(t.Context(), before)
	requirements.NoError(err)
	requirements.True(acknowledged)
	_, err = s.MergeSourcesContext(t.Context(), store.MergeSourcesRequest{FromSourceID: from.ID, IntoSourceID: into.ID})
	requirements.NoError(err)
	var actualRoom int64
	requirements.NoError(s.DB().QueryRow(s.Rebind(`SELECT conversation_id FROM messages WHERE id=?`), id).Scan(&actualRoom))
	assertions.Equal(dest, actualRoom)
	var retained string
	requirements.NoError(s.DB().QueryRow(s.Rebind(`SELECT body_text FROM message_bodies WHERE message_id=?`), id).Scan(&retained))
	assertions.Equal("Retained synthetic text", retained)
	full, err := s.ConversationFullEligibleContext(t.Context(), dest)
	requirements.NoError(err)
	assertions.False(full)
	var persisted string
	requirements.NoError(s.DB().QueryRow(s.Rebind(`SELECT override_tier FROM conversation_policy WHERE conversation_id=?`), dest).Scan(&persisted))
	assertions.Equal(string(conversationpolicy.Cold), persisted)
	current, err := s.ConversationPolicySettingsContext(t.Context())
	requirements.NoError(err)
	assertions.ElementsMatch(settings.Overrides, current.Overrides, "source-qualified config choices survive source transfer")
	after, err := s.ConversationPolicyRevision(t.Context())
	requirements.NoError(err)
	assertions.Greater(after, before, "changed provenance must invalidate previously admitted work without a tier delta")
	cleanup, err := s.ConversationPolicyCleanupRevision(t.Context())
	requirements.NoError(err)
	assertions.Equal(after, cleanup)
	assertions.Greater(cleanup, before, "all-lane cleanup must remain pending")
}

func TestConversationPolicySourceRemovalInvalidatesWork(t *testing.T) {
	for _, serialized := range []bool{false, true} {
		t.Run(strconv.FormatBool(serialized), func(t *testing.T) {
			requirements := require.New(t)
			assertions := assert.New(t)

			s := testutil.NewTestStore(t)
			src, err := s.GetOrCreateSource("beeper", "removed@example.test")
			requirements.NoError(err)
			_, err = s.EnsureConversation(src.ID, "!removed:example.test", "Removed")
			requirements.NoError(err)
			requirements.NoError(s.SetConversationPolicyOverride(t.Context(), src.ID, "!removed:example.test", conversationpolicy.Full))
			before, err := s.ConversationPolicyRevision(t.Context())
			requirements.NoError(err)
			s.DB().SetMaxOpenConns(1)
			if serialized {
				_, _, err = s.RemoveSourceSerialized(t.Context(), src.ID)
			} else {
				err = s.RemoveSource(src.ID)
			}
			requirements.NoError(err)
			after, err := s.ConversationPolicyRevision(t.Context())
			requirements.NoError(err)
			assertions.Greater(after, before)
			cleanup, err := s.ConversationPolicyCleanupRevision(t.Context())
			requirements.NoError(err)
			assertions.Equal(after, cleanup)
		})
	}
}

func TestConversationPolicySerializedRemovalWaitsForPublication(t *testing.T) {
	requirements := require.New(t)

	s := testutil.NewTestStore(t)
	src, err := s.GetOrCreateSource("beeper", "locked@example.test")
	requirements.NoError(err)
	var release func()
	if s.IsPostgreSQL() {
		tx, err := s.DB().BeginTx(t.Context(), nil)
		requirements.NoError(err)
		requirements.NoError(s.LockPostgresConversationPolicyTx(t.Context(), tx, store.ConversationPolicyShared))
		release = func() { _ = tx.Rollback() }
	} else {
		var seq int
		var name, path string
		requirements.NoError(s.DB().QueryRow(`PRAGMA database_list`).Scan(&seq, &name, &path))
		other := policyFenceStore(t, path)
		release, err = other.AcquireSQLiteConversationPolicyFence(t.Context(), store.ConversationPolicyShared)
		requirements.NoError(err)
	}
	defer release()
	ctx, cancel := context.WithTimeout(t.Context(), 150*time.Millisecond) //nolint:kennlint // the deadline is the expected result; the held policy read fence blocks serialized source removal
	defer cancel()
	_, _, err = s.RemoveSourceSerialized(ctx, src.ID)
	requirements.Error(err)
	release()
	_, _, err = s.RemoveSourceSerialized(t.Context(), src.ID)
	requirements.NoError(err)
}

func TestConversationPolicySerializedRemovalRollsBackInvalidationFailure(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)

	s := testutil.NewTestStore(t)
	src, err := s.GetOrCreateSource("beeper", "rollback@example.test")
	requirements.NoError(err)
	_, err = s.EnsureConversation(src.ID, "!rollback:example.test", "Rollback")
	requirements.NoError(err)
	before, err := s.ConversationPolicyRevision(t.Context())
	requirements.NoError(err)
	if s.IsPostgreSQL() {
		_, err = s.DB().Exec(`CREATE FUNCTION fail_policy_cleanup() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.key='conversation_policy_cleanup_revision' THEN RAISE EXCEPTION 'synthetic cleanup failure'; END IF; RETURN NEW; END $$`)
		requirements.NoError(err)
		_, err = s.DB().Exec(`CREATE TRIGGER fail_policy_cleanup BEFORE INSERT OR UPDATE ON archive_metadata FOR EACH ROW EXECUTE FUNCTION fail_policy_cleanup()`)
	} else {
		_, err = s.DB().Exec(`CREATE TRIGGER fail_policy_cleanup BEFORE INSERT ON archive_metadata WHEN NEW.key='conversation_policy_cleanup_revision' BEGIN SELECT RAISE(ABORT,'synthetic cleanup failure'); END`)
	}
	requirements.NoError(err)
	s.DB().SetMaxOpenConns(1)
	_, _, err = s.RemoveSourceSerialized(t.Context(), src.ID)
	requirements.ErrorContains(err, "synthetic cleanup failure")
	var id int64
	requirements.NoError(s.DB().QueryRow(s.Rebind(`SELECT id FROM sources WHERE id=?`), src.ID).Scan(&id))
	assertions.Equal(src.ID, id)
	after, err := s.ConversationPolicyRevision(t.Context())
	requirements.NoError(err)
	assertions.Equal(before, after)
}

func TestConversationPolicySerializedRemovalWithdrawsCurrentBrief(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)

	f := newBriefWorkerEndToEndFixture(t, "policy-source-removal")
	s := f.journal.store
	s.DB().SetMaxOpenConns(1)
	person := f.journal.alicePersonID
	result, err := f.worker.Run(t.Context(), peoplesweep.RunRequest{Kind: peoplesweep.RunManual, Mode: peoplesweep.RunIncremental, PersonID: person, Limit: 1, Brief: peoplesweep.BriefModeForce})
	requirements.NoError(err)
	requirements.Equal(1, result.PeopleSucceeded)
	brief, err := s.GetPersonBriefContext(t.Context(), person, 0)
	requirements.NoError(err)
	_, _, err = s.RemoveSourceSerialized(t.Context(), f.journal.sourceID)
	requirements.NoError(err)
	_, err = s.GetPersonBriefContext(t.Context(), person, 0)
	requirements.ErrorIs(err, store.ErrPersonBriefNotFound)
	history, err := s.GetPersonBriefContext(t.Context(), person, brief.Version)
	requirements.NoError(err)
	assertions.Equal(store.PersonBriefStatusSuperseded, history.Status)
	var name sql.NullString
	requirements.NoError(s.DB().QueryRow(s.Rebind(`SELECT display_name FROM persons WHERE id=?`), person).Scan(&name))
	assertions.True(name.Valid, "intentional profile survives source removal")
}

func TestParticipantMergeRepairsExistingSelfProof(t *testing.T) {
	requirements := require.New(t)

	s := testutil.NewTestStore(t)
	s.DB().SetMaxOpenConns(1)
	source, err := s.GetOrCreateSource("beeper", "self-proof-account")
	requirements.NoError(err)
	room, err := s.EnsureConversation(source.ID, "self-proof-room", "Synthetic")
	requirements.NoError(err)
	old, err := s.EnsureParticipantByIdentifier("beeper", "self-alias", "")
	requirements.NoError(err)
	retained, err := s.EnsureParticipantByPhone("+15550100993", "Synthetic self", "beeper")
	requirements.NoError(err)
	raw := fmt.Sprintf(`{"unrelated":{"keep":true},"messaging_route":{"self_participant_ids":[%d],"participant_ids":[%d,%d],"account_id":"self-proof-account","chat_id":"self-proof-room","failure":"membership_fetch_failed","future_proof":"preserve"}}`, old, old, retained)
	bind := "?"
	if s.IsPostgreSQL() {
		bind = "CAST(? AS JSONB)"
	}
	_, err = s.DB().Exec(s.Rebind(`UPDATE conversations SET metadata=`+bind+` WHERE id=?`), raw, room)
	requirements.NoError(err)
	requirements.NoError(s.MergeParticipantsContext(t.Context(), old, retained))
	var got string
	requirements.NoError(s.DB().QueryRow(s.Rebind(`SELECT metadata FROM conversations WHERE id=?`), room).Scan(&got))
	want := fmt.Sprintf(`{"unrelated":{"keep":true},"messaging_route":{"self_participant_ids":[%d],"participant_ids":[%d],"account_id":"self-proof-account","chat_id":"self-proof-room","failure":"membership_fetch_failed","future_proof":"preserve"}}`, retained, retained)
	assert.JSONEq(t, want, got)
}

func TestConversationPolicySourceLifecyclePreservesIndependentSupport(t *testing.T) {
	for _, remove := range []bool{false, true} {
		t.Run(strconv.FormatBool(remove), func(t *testing.T) {
			requirements := require.New(t)

			s := testutil.NewTestStore(t)
			s.DB().SetMaxOpenConns(2)
			source, err := s.GetOrCreateSource("beeper", "support-origin")
			requirements.NoError(err)
			from := source.ID
			room, err := s.EnsureConversation(from, "!support:example.test", "Original Full")
			requirements.NoError(err)
			requirements.NoError(s.SetConversationPolicyOverride(t.Context(), from, "!support:example.test", conversationpolicy.Full))
			pid, err := s.EnsureParticipantByIdentifier("beeper", "support-user", "")
			requirements.NoError(err)
			into, err := s.GetOrCreateSource("beeper", "cold-destination")
			requirements.NoError(err)
			_, err = s.EnsureConversation(into.ID, "!support:example.test", "Cold destination")
			requirements.NoError(err)
			requirements.NoError(s.SetConversationPolicyOverride(t.Context(), into.ID, "!support:example.test", conversationpolicy.Cold))
			independent, err := s.GetOrCreateSource("slack", "independent-full")
			requirements.NoError(err)
			full, err := s.EnsureConversation(independent.ID, "remaining", "Remaining Full")
			requirements.NoError(err)
			requirements.NoError(s.SetConversationPolicyOverride(t.Context(), independent.ID, "remaining", conversationpolicy.Full))
			input := policyObservationInput(from, &room)
			transferred, err := s.RecordContactObservationContext(t.Context(), pid, input)
			requirements.NoError(err)
			input.SourceID = &independent.ID
			input.ConversationID = &full
			remaining, err := s.RecordContactObservationContext(t.Context(), pid, input)
			requirements.NoError(err)
			person, _, err := s.CreatePersonFromParticipant(pid)
			requirements.NoError(err)
			manual, err := s.AddPersonContactPointContext(t.Context(), person.ID, store.PersonContactPointInput{AddressKind: store.ContactAddressEmail, OriginalValue: "manual-support@example.test", Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceUser}})
			requirements.NoError(err)
			if remove {
				s.DB().SetMaxOpenConns(1)
				_, _, err = s.RemoveSourceSerialized(t.Context(), from)
			} else {
				_, err = s.MergeSourcesContext(t.Context(), store.MergeSourcesRequest{FromSourceID: from, IntoSourceID: into.ID})
			}
			requirements.NoError(err)
			eligible, err := s.ObservationFullEligibleContext(t.Context(), transferred.Observation.Envelope.ID)
			requirements.NoError(err)
			assert.False(t, eligible, "transferred evidence inherits destination Cold")
			check := func() {
				t.Helper()
				eligible, err := s.ObservationFullEligibleContext(t.Context(), remaining.Observation.Envelope.ID)
				require.NoError(t, err)
				assert.True(t, eligible, "independent Full support remains usable")
				found, err := s.FindObservationsByProviderUserIDContext(t.Context(), *input.ProviderUserID, 0)
				require.NoError(t, err)
				require.Len(t, found, 1)
				assert.Equal(t, remaining.Observation.Envelope.ID, found[0].Envelope.ID)
				contacts, err := s.ListPersonContactPointsContext(t.Context(), person.ID, true)
				require.NoError(t, err)
				require.Len(t, contacts, 1)
				assert.Equal(t, *manual, contacts[0], "manual contact and envelope remain exact")
			}
			check()
		})
	}
}
