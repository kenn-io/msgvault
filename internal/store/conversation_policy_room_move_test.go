package store_test

import (
	"database/sql"
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

// A fact supported only by one message must follow that message between
// rooms: moving it into a Cold room retracts the fact, and moving it back to a
// Full room restores it, without waiting for a tier change.
func TestRoomMoveReconcilesFactsSupportedByTheMessage(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	st := testutil.NewTestStore(t)
	ctx := t.Context()
	source, err := st.GetOrCreateSource("slack", "room-move-facts")
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
	upsert := func(room int64) int64 {
		id, err := st.UpsertMessage(&store.Message{SourceID: source.ID, ConversationID: room, SourceMessageID: "moving-fact",
			MessageType: "slack", SenderID: sql.NullInt64{Int64: participant, Valid: true}, SentAt: sentAt})
		requirements.NoError(err)
		return id
	}
	message := upsert(fullRoom)
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
	before, err := st.LoadPersonSemanticDocumentContext(ctx, person.ID)
	requirements.NoError(err)
	requirements.Contains(before.Text, "tidal energy")

	upsert(coldRoom)
	moved, err := st.LoadPersonSemanticDocumentContext(ctx, person.ID)
	requirements.NoError(err)
	assertions.NotContains(moved.Text, "tidal energy", "a Cold room cannot support the fact")
	assertions.NotEqual(before.Revision, moved.Revision)

	upsert(fullRoom)
	restored, err := st.LoadPersonSemanticDocumentContext(ctx, person.ID)
	requirements.NoError(err)
	assertions.Contains(restored.Text, "tidal energy", "moving back restores the fact's support")
}
