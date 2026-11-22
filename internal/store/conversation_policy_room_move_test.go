package store_test

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/conversationpolicy"
	"go.kenn.io/msgvault/internal/peoplesweep"
	"go.kenn.io/msgvault/internal/personfacts"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

// roomMoveFact is a tracked person with one fact supported only by one
// message, archived in a Full room. move re-imports that message into room;
// moveWithParticipants does the same through participant resolution, which
// starts with the exclusive identity lock for identifier participants.
type roomMoveFact struct {
	st                   *store.Store
	fullRoom, coldRoom   int64
	person               int64
	move                 func(room int64) error
	moveWithParticipants func(room int64) error
}

func newRoomMoveFact(t *testing.T, name string) roomMoveFact {
	t.Helper()
	requirements := require.New(t)
	st := testutil.NewTestStore(t)
	ctx := t.Context()
	source, err := st.GetOrCreateSource("slack", name)
	requirements.NoError(err)
	fullRoom, err := st.EnsureConversation(ctx, source.ID, "full-room", "Full room")
	requirements.NoError(err)
	coldRoom, err := st.EnsureConversation(ctx, source.ID, "cold-room", "Cold room")
	requirements.NoError(err)
	requirements.NoError(st.SetConversationPolicyOverride(ctx, source.ID, "full-room", conversationpolicy.Full))
	requirements.NoError(st.SetConversationPolicyOverride(ctx, source.ID, "cold-room", conversationpolicy.Cold))
	participant, err := st.EnsureParticipantByIdentifier("slack", "room-mover", "Room Mover")
	requirements.NoError(err)
	person, _, err := st.CreatePersonFromParticipantContext(ctx, participant)
	requirements.NoError(err)
	_, err = st.SetPersonTrackingContext(ctx, person.ID, true)
	requirements.NoError(err)
	sentAt := sql.NullTime{Time: time.Now().UTC().Truncate(time.Second), Valid: true}
	var message int64
	move := func(room int64) error {
		var err error
		message, err = st.UpsertMessage(&store.Message{SourceID: source.ID, ConversationID: room, SourceMessageID: "moving-fact",
			MessageType: "slack", SenderID: sql.NullInt64{Int64: participant, Valid: true}, SentAt: sentAt})
		return err
	}
	requirements.NoError(move(fullRoom))
	requirements.NoError(st.ReplaceMessageRecipients(message, "from", []int64{participant}, []string{"Room Mover"}))
	requirements.NoError(st.UpsertMessageBodyContext(ctx, message, sql.NullString{String: "Ask me about tidal energy.", Valid: true}, sql.NullString{}))
	inputs, err := st.HydratePersonSweepMessages(ctx, person.ID, []int64{message}, 0)
	requirements.NoError(err)
	requirements.Len(inputs, 1)
	evidence, err := peoplesweep.PersonFactEvidenceInput(inputs[0])
	requirements.NoError(err)
	catalog, err := st.BuildPersonFactCatalogContext(ctx, true)
	requirements.NoError(err)
	var target personfacts.TargetDescriptor
	for _, candidate := range catalog.Targets {
		if candidate.Slug == store.AttributeSlugAskMeAbout {
			target = candidate
		}
	}
	requirements.NotEmpty(target.Key)
	_, err = st.ApplyPersonFactGenerationContext(ctx, personfacts.GenerationInput{
		PersonID: person.ID, SourceCursors: []personfacts.SourceCursor{{Lane: "fixture", Start: "room-move", End: "room-move-end"}},
		ProgramID: "room-move", ProgramVersion: "v1", ProgramFingerprint: strings.Repeat("b", 64), CatalogFingerprint: "catalog-fixture",
		Provider: "fixture", ProviderVersion: "v1", Model: "fixture", ModelVersion: "v1", ResolvedAt: time.Now().UTC(),
		Policy: personfacts.PolicyContext{ProviderPolicyFingerprint: "policy-v1"},
		Claims: []personfacts.ProposedClaim{{Target: target, Relation: personfacts.RelationSupport, SubmittedValue: []byte(`"tidal energy"`),
			Evidence: []personfacts.EvidenceInput{evidence}, Origin: personfacts.OriginExtraction, Confidence: personfacts.ConfidenceInputs{ReportedScore: 900}}},
	}, store.PersonSweepEvidenceAligner{Store: st})
	requirements.NoError(err)
	moveWithParticipants := func(room int64) error {
		_, err := st.PersistMessageWithParticipantsContext(t.Context(),
			[]store.ParticipantPersistData{{IdentifierType: "slack", IdentifierValue: "room-mover", DisplayName: "Room Mover"}},
			func(ids []int64) *store.MessagePersistData {
				return &store.MessagePersistData{
					Message: &store.Message{SourceID: source.ID, ConversationID: room, SourceMessageID: "moving-fact",
						MessageType: "slack", SenderID: sql.NullInt64{Int64: ids[0], Valid: true}, SentAt: sentAt},
					BodyText:   sql.NullString{String: "Ask me about tidal energy.", Valid: true},
					Recipients: []store.RecipientSet{{Type: "from", ParticipantIDs: ids, DisplayNames: []string{"Room Mover"}}},
				}
			})
		return err
	}
	return roomMoveFact{st: st, fullRoom: fullRoom, coldRoom: coldRoom, person: person.ID, move: move, moveWithParticipants: moveWithParticipants}
}

// A fact supported only by one message must follow that message between
// rooms: moving it into a Cold room retracts the fact, and moving it back to a
// Full room restores it, without waiting for a tier change.
func TestRoomMoveReconcilesFactsSupportedByTheMessage(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	fx := newRoomMoveFact(t, "room-move-facts")
	ctx := t.Context()
	before, err := fx.st.LoadPersonSemanticDocumentContext(ctx, fx.person)
	requirements.NoError(err)
	requirements.Contains(before.Text, "tidal energy")

	requirements.NoError(fx.move(fx.coldRoom))
	moved, err := fx.st.LoadPersonSemanticDocumentContext(ctx, fx.person)
	requirements.NoError(err)
	assertions.NotContains(moved.Text, "tidal energy", "a Cold room cannot support the fact")
	assertions.NotEqual(before.Revision, moved.Revision)

	requirements.NoError(fx.move(fx.fullRoom))
	restored, err := fx.st.LoadPersonSemanticDocumentContext(ctx, fx.person)
	requirements.NoError(err)
	assertions.Contains(restored.Text, "tidal energy", "moving back restores the fact's support")
}

// A move that changes Full eligibility locks the moved sender's tracked person
// row before that person's fact generation lock, the reverse of fact writers
// such as pins. On PostgreSQL the move can lose the resulting deadlock. It must
// retry instead of failing the import, whichever write path made the move.
func TestRoomMoveRetriesAfterLosingFactLockDeadlock(t *testing.T) {
	for _, path := range []string{"upsert", "participants"} {
		t.Run(path, func(t *testing.T) {
			requirements := require.New(t)
			fx := newRoomMoveFact(t, "room-move-deadlock")
			if !fx.st.IsPostgreSQL() {
				t.Skip("the fact generation lock is a PostgreSQL advisory lock")
			}
			move := fx.move
			if path == "participants" {
				move = fx.moveWithParticipants
			}
			ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
			defer cancel()

			// A fact writer takes the person's generation lock first, as a pin
			// does. Its deadlock check waits, so PostgreSQL aborts the move.
			writer, err := fx.st.DB().BeginTx(ctx, nil)
			requirements.NoError(err)
			defer func() { _ = writer.Rollback() }()
			_, err = writer.ExecContext(ctx, `SET LOCAL deadlock_timeout = '1h'`)
			requirements.NoError(err)
			id := strconv.FormatInt(fx.person, 10)
			_, err = writer.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended(CAST($1 AS TEXT), 0))`,
				fmt.Sprintf("person-fact-generation|int64:%d:%s", len(id), id))
			requirements.NoError(err)
			var writerPID int
			requirements.NoError(writer.QueryRowContext(ctx, `SELECT pg_backend_pid()`).Scan(&writerPID))
			moved := make(chan error, 1)
			go func() { moved <- move(fx.coldRoom) }()
			requirements.Eventually(func() bool {
				var blocked bool
				err := fx.st.DB().QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE $1 = ANY(pg_blocking_pids(pid)))`, writerPID).Scan(&blocked)
				return err == nil && blocked
			}, 30*time.Second, 10*time.Millisecond, "the move must wait for the generation lock")
			// The writer then needs the person row the move holds.
			_, err = writer.ExecContext(ctx, `UPDATE persons SET updated_at = updated_at WHERE id = $1`, fx.person)
			requirements.NoError(err)
			requirements.NoError(writer.Commit())
			requirements.NoError(<-moved, "a move that loses the deadlock must retry")
		})
	}
}
